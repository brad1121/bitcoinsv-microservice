package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	bsvmspb "github.com/brad1121/bsvms/gen/bsvms/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var version = "dev"

const (
	tenantID     = "blackjack"
	playerWallet = "player"
	houseWallet  = "house"
	seedSatoshis = 250_000
	defaultBet   = 1_000
	regtestPeer  = "bsv-node:18444"
)

type wallet struct {
	id      string
	address string
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-version" {
		fmt.Println(version)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "-fund" {
		fundMain()
		return
	}
	addr := env("BSVMS_ADDR", "bsvms:50051")
	explorerURL := strings.TrimRight(env("WOC_EXPLORER_URL", "http://localhost:3002"), "/")
	ctx := context.Background()
	client, closeConn, err := connect(ctx, addr)
	if err != nil {
		log.Fatal(err)
	}
	defer closeConn()

	player := ensureWallet(ctx, client, playerWallet)
	house := ensureWallet(ctx, client, houseWallet)
	network := statusNetwork(ctx, client)
	if network == "regtest" {
		ensureRegtestPeer(ctx, client)
	}
	requireFunded(ctx, client, player)
	requireFunded(ctx, client, house)

	fmt.Println("BSV blackjack")
	fmt.Printf("Network: %s via bsvms\n", network)
	fmt.Printf("Player address: %s\n", player.address)
	fmt.Printf("House address:  %s\n", house.address)
	printAddressLinks(network, explorerURL, player, house)
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		playHand(ctx, client, reader, rng, explorerURL, player, house)
		fmt.Print("Play again? [y/N]: ")
		line, _ := reader.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			return
		}
	}
}

func connect(ctx context.Context, addr string) (bsvmspb.BSVMSClient, func(), error) {
	var lastErr error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			client := bsvmspb.NewBSVMSClient(conn)
			if _, err := client.Status(ctx, &bsvmspb.StatusRequest{}); err == nil {
				return client, func() { _ = conn.Close() }, nil
			} else {
				lastErr = err
				_ = conn.Close()
			}
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	return nil, nil, fmt.Errorf("connect bsvms %s: %w", addr, lastErr)
}

func ensureWallet(ctx context.Context, client bsvmspb.BSVMSClient, id string) wallet {
	got, err := client.GetWallet(ctx, &bsvmspb.GetWalletRequest{TenantId: tenantID, WalletId: id})
	if status.Code(err) == codes.NotFound {
		if _, err := client.CreateWallet(ctx, &bsvmspb.CreateWalletRequest{TenantId: tenantID, WalletId: id}); err != nil {
			log.Fatalf("create wallet %s: %v", id, err)
		}
		got, err = client.GetWallet(ctx, &bsvmspb.GetWalletRequest{TenantId: tenantID, WalletId: id})
	}
	if err != nil {
		log.Fatalf("get wallet %s: %v", id, err)
	}
	addresses := got.GetWallet().GetAddresses()
	if len(addresses) > 0 {
		return wallet{id: id, address: addresses[0]}
	}
	addr, err := client.NewAddress(ctx, &bsvmspb.NewAddressRequest{TenantId: tenantID, WalletId: id})
	if err != nil {
		log.Fatalf("new address %s: %v", id, err)
	}
	return wallet{id: id, address: addr.GetAddress().GetAddress()}
}

func requireFunded(ctx context.Context, client bsvmspb.BSVMSClient, w wallet) {
	bal, err := client.Balance(ctx, &bsvmspb.BalanceRequest{TenantId: tenantID, WalletId: w.id})
	if err != nil {
		log.Fatalf("balance %s: %v", w.id, err)
	}
	if bal.GetSatoshis() >= defaultBet {
		return
	}
	log.Fatalf("%s wallet has %d sats; run `docker compose up -d` and wait for blackjack-fund to complete", w.id, bal.GetSatoshis())
}

func playHand(ctx context.Context, client bsvmspb.BSVMSClient, reader *bufio.Reader, rng *rand.Rand, explorerURL string, player, house wallet) {
	bet := askBet(reader)
	deck := newDeck(rng)
	playerHand := []card{draw(&deck), draw(&deck)}
	houseHand := []card{draw(&deck), draw(&deck)}

	for {
		fmt.Printf("\nHouse: %s [?]\n", houseHand[0])
		fmt.Printf("You:   %s (%d)\n", handString(playerHand), handValue(playerHand))
		if handValue(playerHand) >= 21 {
			break
		}
		fmt.Print("Hit or stand? [h/S]: ")
		line, _ := reader.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "h" {
			break
		}
		playerHand = append(playerHand, draw(&deck))
	}

	for handValue(playerHand) <= 21 && handValue(houseHand) < 17 {
		houseHand = append(houseHand, draw(&deck))
	}

	playerTotal := handValue(playerHand)
	houseTotal := handValue(houseHand)
	fmt.Printf("\nHouse: %s (%d)\n", handString(houseHand), houseTotal)
	fmt.Printf("You:   %s (%d)\n", handString(playerHand), playerTotal)

	switch outcome(playerTotal, houseTotal) {
	case "player":
		txid := settleHand(ctx, client, house, player.address, int64(bet), playerHand, houseHand, "player")
		fmt.Printf("You win %d sats. Settlement tx: %s\n", bet, txid)
		printTxLink(ctx, client, explorerURL, txid)
		fmt.Println()
	case "house":
		txid := settleHand(ctx, client, player, house.address, int64(bet), playerHand, houseHand, "house")
		fmt.Printf("House wins %d sats. Settlement tx: %s\n", bet, txid)
		printTxLink(ctx, client, explorerURL, txid)
		fmt.Println()
	default:
		fmt.Println("Push. No settlement.")
		fmt.Println()
	}
	showBalances(ctx, client)
}

func askBet(reader *bufio.Reader) int {
	fmt.Printf("Bet sats [%d]: ", defaultBet)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultBet
	}
	n, err := strconv.Atoi(line)
	if err != nil || n <= 0 {
		return defaultBet
	}
	return n
}

type handResult struct {
	Game        string   `json:"game"`
	Player      []string `json:"player"`
	House       []string `json:"house"`
	PlayerTotal int      `json:"player_total"`
	HouseTotal  int      `json:"house_total"`
	Bet         int64    `json:"bet"`
	Winner      string   `json:"winner"`
}

func settleHand(ctx context.Context, client bsvmspb.BSVMSClient, from wallet, to string, sats int64, playerHand, houseHand []card, winner string) string {
	ensureRegtestPeer(ctx, client)

	p2pkh, err := client.P2PKHOutput(ctx, &bsvmspb.P2PKHOutputRequest{Address: to, Value: sats})
	if err != nil {
		log.Fatalf("p2pkh output: %v", err)
	}

	result := handResult{
		Game:        "blackjack",
		Player:      handStringParts(playerHand),
		House:       handStringParts(houseHand),
		PlayerTotal: handValue(playerHand),
		HouseTotal:  handValue(houseHand),
		Bet:         sats,
		Winner:      winner,
	}
	raw, err := json.Marshal(result)
	if err != nil {
		log.Fatalf("encode hand result: %v", err)
	}
	opReturn, err := client.OpReturnOutput(ctx, &bsvmspb.OpReturnOutputRequest{Data: raw})
	if err != nil {
		log.Fatalf("op_return output: %v", err)
	}

	for deadline := time.Now().Add(60 * time.Second); ; {
		detail, err := client.SpendToOutputs(ctx, &bsvmspb.SpendToOutputsRequest{
			TenantId: tenantID,
			WalletId: from.id,
			Outputs:  []*bsvmspb.OutputSpec{p2pkh, opReturn},
		})
		if err == nil {
			return detail.GetDetail().GetTxid()
		}
		if strings.Contains(err.Error(), "no connected peers") {
			ensureRegtestPeer(ctx, client)
			detail, err := client.SpendToOutputs(ctx, &bsvmspb.SpendToOutputsRequest{
				TenantId: tenantID,
				WalletId: from.id,
				Outputs:  []*bsvmspb.OutputSpec{p2pkh, opReturn},
			})
			if err == nil {
				return detail.GetDetail().GetTxid()
			}
		}
		if time.Now().After(deadline) {
			log.Fatalf("settle from %s: %v", from.id, err)
		}
		time.Sleep(time.Second)
	}
}

func ensureRegtestPeer(ctx context.Context, client bsvmspb.BSVMSClient) {
	peer := env("BSVMS_REGTEST_PEER", regtestPeer)
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		st, err := client.Status(ctx, &bsvmspb.StatusRequest{})
		if err == nil && strings.ToLower(st.GetNetwork()) != "regtest" {
			return
		}
		if err == nil && st.GetPeerCount() > 0 {
			return
		}
		if _, err := client.ConnectPeer(ctx, &bsvmspb.ConnectPeerRequest{Address: peer}); err != nil {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		log.Fatalf("connect regtest peer %s: %v", peer, lastErr)
	}
	log.Fatalf("connect regtest peer %s: timed out", peer)
}

func showBalances(ctx context.Context, client bsvmspb.BSVMSClient) {
	for _, id := range []string{playerWallet, houseWallet} {
		bal, err := client.Balance(ctx, &bsvmspb.BalanceRequest{TenantId: tenantID, WalletId: id})
		if err != nil {
			log.Fatalf("balance %s: %v", id, err)
		}
		fmt.Printf("%s balance: %d sats\n", id, bal.GetSatoshis())
	}
}

func statusNetwork(ctx context.Context, client bsvmspb.BSVMSClient) string {
	status, err := client.Status(ctx, &bsvmspb.StatusRequest{})
	if err != nil {
		log.Fatalf("status: %v", err)
	}
	return status.GetNetwork()
}

func printAddressLinks(network, explorerURL string, player, house wallet) {
	playerURL := wocAddressURL(network, explorerURL, player.address)
	houseURL := wocAddressURL(network, explorerURL, house.address)
	if playerURL == "" || houseURL == "" {
		return
	}
	fmt.Printf("Player explorer: %s\n", playerURL)
	fmt.Printf("House explorer:  %s\n", houseURL)
}

func printTxLink(ctx context.Context, client bsvmspb.BSVMSClient, explorerURL, txid string) {
	network := statusNetwork(ctx, client)
	if url := wocTxURL(network, explorerURL, txid); url != "" {
		fmt.Printf("Explorer: %s\n", url)
		return
	}
	fmt.Printf("Inspect after next mined block: docker compose exec bsv-node bitcoin-cli -regtest -rpcuser=bsv -rpcpassword=bsv getrawtransaction %s 1\n", txid)
}

func wocAddressURL(network, explorerURL, address string) string {
	base := wocBaseURL(network, explorerURL)
	if base == "" {
		return ""
	}
	return base + "/address/" + address
}

func wocTxURL(network, explorerURL, txid string) string {
	base := wocBaseURL(network, explorerURL)
	if base == "" {
		return ""
	}
	return base + "/tx/" + txid
}

func wocBaseURL(network, explorerURL string) string {
	switch strings.ToLower(network) {
	case "regtest":
		return explorerURL
	case "mainnet":
		return "https://whatsonchain.com"
	case "testnet":
		return "https://test.whatsonchain.com"
	case "stn":
		return "https://stn.whatsonchain.com"
	default:
		return ""
	}
}

func fundMain() {
	ctx := context.Background()
	client, closeConn, err := connect(ctx, env("BSVMS_ADDR", "bsvms:50051"))
	if err != nil {
		log.Fatal(err)
	}
	defer closeConn()

	ensureRegtestPeer(ctx, client)
	rpc := nodeRPC{
		url:      env("BSV_NODE_RPC_URL", "http://bsv-node:18443"),
		username: env("BSV_NODE_RPC_USER", "bsv"),
		password: env("BSV_NODE_RPC_PASSWORD", "bsv"),
		client:   &http.Client{Timeout: 30 * time.Second},
	}
	if err := rpc.waitReady(ctx); err != nil {
		log.Fatal(err)
	}

	wallets := []wallet{ensureWallet(ctx, client, playerWallet), ensureWallet(ctx, client, houseWallet)}
	for _, w := range wallets {
		if err := clearMissingUTXOs(ctx, client, rpc, w); err != nil {
			log.Fatalf("check %s utxos: %v", w.id, err)
		}
	}

	funded := map[string]string{}
	var needed int64
	for _, w := range wallets {
		bal, err := client.Balance(ctx, &bsvmspb.BalanceRequest{TenantId: tenantID, WalletId: w.id})
		if err != nil {
			log.Fatalf("balance %s: %v", w.id, err)
		}
		if bal.GetSatoshis() < seedSatoshis {
			needed += seedSatoshis - bal.GetSatoshis()
		}
	}
	if needed > 0 {
		if err := rpc.waitSpendable(ctx, needed); err != nil {
			log.Fatalf("node funding balance: %v", err)
		}
	}
	for _, w := range wallets {
		bal, err := client.Balance(ctx, &bsvmspb.BalanceRequest{TenantId: tenantID, WalletId: w.id})
		if err != nil {
			log.Fatalf("balance %s: %v", w.id, err)
		}
		if bal.GetSatoshis() >= seedSatoshis {
			continue
		}
		txid, err := rpc.sendToAddress(ctx, w.address, seedSatoshis-bal.GetSatoshis())
		if err != nil {
			log.Fatalf("fund %s: %v", w.id, err)
		}
		funded[w.id] = txid
		fmt.Printf("funded %s wallet: %s\n", w.id, txid)
	}
	if len(funded) == 0 {
		fmt.Println("blackjack wallets already funded")
		return
	}
	if err := rpc.generate(ctx, 1); err != nil {
		log.Fatalf("mine funding block: %v", err)
	}
	height, err := rpc.blockCount(ctx)
	if err != nil {
		log.Fatalf("funding block height: %v", err)
	}
	for _, w := range wallets {
		txid, ok := funded[w.id]
		if !ok {
			continue
		}
		raw, err := rpc.rawTransaction(ctx, txid)
		if err != nil {
			log.Fatalf("funding tx %s: %v", txid, err)
		}
		if _, err := client.ProcessRawTx(ctx, &bsvmspb.ProcessRawTxRequest{TenantId: tenantID, WalletId: w.id, RawTx: raw, Height: int32(height)}); err != nil {
			log.Fatalf("process funding tx for %s: %v", w.id, err)
		}
	}
	fmt.Println("blackjack wallets funded from regtest coinbase")
}

func clearMissingUTXOs(ctx context.Context, client bsvmspb.BSVMSClient, rpc nodeRPC, w wallet) error {
	utxos, err := client.ListUTXOs(ctx, &bsvmspb.ListUTXOsRequest{TenantId: tenantID, WalletId: w.id})
	if err != nil {
		return err
	}
	for _, utxo := range utxos.GetUtxos() {
		ok, err := rpc.txOutExists(ctx, utxo.GetTxid(), utxo.GetVout())
		if err != nil {
			return err
		}
		if !ok {
			cleared, err := client.ClearUTXOs(ctx, &bsvmspb.ClearUTXOsRequest{TenantId: tenantID, WalletId: w.id})
			if err != nil {
				return err
			}
			fmt.Printf("cleared %d off-chain %s utxos\n", cleared.GetCleared(), w.id)
			return nil
		}
	}
	return nil
}

type nodeRPC struct {
	url      string
	username string
	password string
	client   *http.Client
}

func (r nodeRPC) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := r.blockCount(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("connect bsv-node RPC: %w", lastErr)
}

func (r nodeRPC) waitSpendable(ctx context.Context, sats int64) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		balance, err := r.balanceSats(ctx)
		if err == nil && balance >= sats {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("spendable balance %d sats, need %d", balance, sats)
		}
		time.Sleep(time.Second)
	}
	return lastErr
}

func (r nodeRPC) balanceSats(ctx context.Context) (int64, error) {
	var bsv float64
	if err := r.call(ctx, "getbalance", nil, &bsv); err != nil {
		return 0, err
	}
	return int64(bsv * 100_000_000), nil
}

func (r nodeRPC) blockCount(ctx context.Context) (int, error) {
	var out int
	return out, r.call(ctx, "getblockcount", nil, &out)
}

func (r nodeRPC) sendToAddress(ctx context.Context, address string, sats int64) (string, error) {
	var txid string
	return txid, r.call(ctx, "sendtoaddress", []any{address, float64(sats) / 100_000_000}, &txid)
}

func (r nodeRPC) generate(ctx context.Context, blocks int) error {
	var hashes []string
	return r.call(ctx, "generate", []any{blocks}, &hashes)
}

func (r nodeRPC) rawTransaction(ctx context.Context, txid string) ([]byte, error) {
	var rawHex string
	if err := r.call(ctx, "getrawtransaction", []any{txid}, &rawHex); err != nil {
		return nil, err
	}
	return hex.DecodeString(rawHex)
}

func (r nodeRPC) txOutExists(ctx context.Context, txid string, vout uint32) (bool, error) {
	var raw json.RawMessage
	if err := r.call(ctx, "gettxout", []any{txid, vout, true}, &raw); err != nil {
		return false, err
	}
	return string(raw) != "null", nil
}

func (r nodeRPC) call(ctx context.Context, method string, params []any, result any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": "blackjack", "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(r.username, r.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s RPC %d: %s", method, envelope.Error.Code, envelope.Error.Message)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, result)
}

func outcome(playerTotal, houseTotal int) string {
	switch {
	case playerTotal > 21:
		return "house"
	case houseTotal > 21:
		return "player"
	case playerTotal > houseTotal:
		return "player"
	case houseTotal > playerTotal:
		return "house"
	default:
		return "push"
	}
}

type card struct {
	rank string
	suit string
}

func (c card) String() string { return c.rank + c.suit }

func newDeck(rng *rand.Rand) []card {
	ranks := []string{"A", "2", "3", "4", "5", "6", "7", "8", "9", "10", "J", "Q", "K"}
	suits := []string{"S", "H", "D", "C"}
	var deck []card
	for _, suit := range suits {
		for _, rank := range ranks {
			deck = append(deck, card{rank: rank, suit: suit})
		}
	}
	rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	return deck
}

func draw(deck *[]card) card {
	c := (*deck)[0]
	*deck = (*deck)[1:]
	return c
}

func handString(hand []card) string {
	return strings.Join(handStringParts(hand), " ")
}

func handStringParts(hand []card) []string {
	parts := make([]string, len(hand))
	for i, c := range hand {
		parts[i] = c.String()
	}
	return parts
}

func handValue(hand []card) int {
	total := 0
	aces := 0
	for _, c := range hand {
		switch c.rank {
		case "A":
			total += 11
			aces++
		case "K", "Q", "J":
			total += 10
		default:
			n, _ := strconv.Atoi(c.rank)
			total += n
		}
	}
	for total > 21 && aces > 0 {
		total -= 10
		aces--
	}
	return total
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
