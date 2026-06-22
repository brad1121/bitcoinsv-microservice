package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math/rand"
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
	addr := env("BSVMS_ADDR", "bsvms:50051")
	ctx := context.Background()
	client, closeConn, err := connect(ctx, addr)
	if err != nil {
		log.Fatal(err)
	}
	defer closeConn()

	player := ensureWallet(ctx, client, playerWallet)
	house := ensureWallet(ctx, client, houseWallet)
	seedWallet(ctx, client, player, seedSatoshis)
	seedWallet(ctx, client, house, seedSatoshis)

	fmt.Println("BSV blackjack")
	fmt.Println("Network: regtest via bsvms")
	fmt.Printf("Player address: %s\n", player.address)
	fmt.Printf("House address:  %s\n", house.address)
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		playHand(ctx, client, reader, rng, player, house)
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

func seedWallet(ctx context.Context, client bsvmspb.BSVMSClient, w wallet, minBalance int64) {
	bal, err := client.Balance(ctx, &bsvmspb.BalanceRequest{TenantId: tenantID, WalletId: w.id})
	if err != nil {
		log.Fatalf("balance %s: %v", w.id, err)
	}
	if bal.GetSatoshis() >= minBalance/2 {
		return
	}
	out, err := client.P2PKHOutput(ctx, &bsvmspb.P2PKHOutputRequest{Address: w.address, Value: minBalance})
	if err != nil {
		log.Fatalf("p2pkh %s: %v", w.id, err)
	}
	txid := fakeTxID(w.id, time.Now().UnixNano())
	if _, err := client.ImportUTXO(ctx, &bsvmspb.ImportUTXORequest{
		TenantId: tenantID,
		WalletId: w.id,
		Txid:     txid,
		Vout:     0,
		Value:    minBalance,
		Script:   out.GetScript(),
		Height:   1,
		Force:    true,
	}); err != nil {
		log.Fatalf("seed %s: %v", w.id, err)
	}
}

func playHand(ctx context.Context, client bsvmspb.BSVMSClient, reader *bufio.Reader, rng *rand.Rand, player, house wallet) {
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
		txid := send(ctx, client, house, player.address, int64(bet))
		fmt.Printf("You win %d sats. Settlement tx: %s\n\n", bet, txid)
	case "house":
		txid := send(ctx, client, player, house.address, int64(bet))
		fmt.Printf("House wins %d sats. Settlement tx: %s\n\n", bet, txid)
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

func send(ctx context.Context, client bsvmspb.BSVMSClient, from wallet, to string, sats int64) string {
	resp, err := client.Send(ctx, &bsvmspb.SendRequest{
		TenantId:       tenantID,
		WalletId:       from.id,
		ToAddress:      to,
		AmountSatoshis: sats,
	})
	if err != nil {
		log.Fatalf("settle from %s: %v", from.id, err)
	}
	return resp.GetTxid()
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
	parts := make([]string, len(hand))
	for i, c := range hand {
		parts[i] = c.String()
	}
	return strings.Join(parts, " ")
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

func fakeTxID(walletID string, nonce int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", walletID, nonce)))
	return hex.EncodeToString(sum[:])
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
