package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	bsv "github.com/brad1121/bitcoinsv-sdk-go/sdk"
	walletsqlite "github.com/brad1121/bitcoinsv-sdk-go/walletstore/sqlite"
	bsvmspb "github.com/brad1121/bsvms/gen/bsvms/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type Service struct {
	bsvmspb.UnimplementedBSVMSServer

	node    *bsv.Node
	dataDir string
	opts    Options

	mu      sync.Mutex
	state   stateFile
	wallets map[string]*tenantWallet

	txBroker       *broker[*bsvmspb.Transaction]
	blockBroker    *broker[*bsvmspb.Block]
	paymentBroker  *broker[*bsvmspb.Payment]
	walletTxBroker *broker[*bsvmspb.WalletTransaction]
	trafficBroker  *broker[*bsvmspb.P2PTraffic]
	rejectBroker   *broker[*bsvmspb.Reject]
}

type stateFile struct {
	Wallets []walletMeta `json:"wallets"`
}

type walletMeta struct {
	TenantID             string `json:"tenant_id"`
	WalletID             string `json:"wallet_id"`
	SDKName              string `json:"sdk_name"`
	Mnemonic             string `json:"mnemonic,omitempty"`   // legacy plaintext, migrated on next save
	Passphrase           string `json:"passphrase,omitempty"` // legacy plaintext, migrated on next save
	MnemonicCiphertext   string `json:"mnemonic_ciphertext,omitempty"`
	PassphraseCiphertext string `json:"passphrase_ciphertext,omitempty"`
}

type tenantWallet struct {
	meta  walletMeta
	w     *bsv.Wallet
	store *walletsqlite.Store
}

func New(ctx context.Context, node *bsv.Node, dataDir string) (*Service, error) {
	return NewWithOptions(ctx, node, dataDir, defaultOptions())
}

func NewWithOptions(ctx context.Context, node *bsv.Node, dataDir string, opts Options) (*Service, error) {
	resolved, err := opts.withDefaults(dataDir)
	if err != nil {
		return nil, err
	}
	s := &Service{
		node:           node,
		dataDir:        dataDir,
		opts:           resolved,
		wallets:        make(map[string]*tenantWallet),
		txBroker:       newBroker[*bsvmspb.Transaction](),
		blockBroker:    newBroker[*bsvmspb.Block](),
		paymentBroker:  newBroker[*bsvmspb.Payment](),
		walletTxBroker: newBroker[*bsvmspb.WalletTransaction](),
		trafficBroker:  newBroker[*bsvmspb.P2PTraffic](),
		rejectBroker:   newBroker[*bsvmspb.Reject](),
	}
	if err := os.MkdirAll(s.walletDir(), 0o700); err != nil {
		return nil, err
	}
	if err := s.loadState(); err != nil {
		return nil, err
	}
	s.registerNodeEvents()
	migrated := false
	for _, meta := range s.state.Wallets {
		if _, err := s.restoreLoadedWallet(ctx, meta); err != nil {
			return nil, fmt.Errorf("load wallet %s/%s: %w", meta.TenantID, meta.WalletID, err)
		}
		if meta.Mnemonic != "" || meta.Passphrase != "" {
			migrated = true
		}
	}
	if migrated {
		s.mu.Lock()
		err := s.saveStateLocked()
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tw := range s.wallets {
		_ = tw.store.Close()
	}
}

func (s *Service) Status(context.Context, *bsvmspb.StatusRequest) (*bsvmspb.StatusResponse, error) {
	return &bsvmspb.StatusResponse{
		Network:        s.node.Network(),
		ChainHeight:    s.node.ChainHeight(),
		BestPeerHeight: s.node.BestPeerHeight(),
		PeerCount:      int32(s.node.PeerCount()),
		DataDir:        s.dataDir,
		PeerHeights:    s.node.PeerHeights(),
	}, nil
}

func (s *Service) ConnectPeer(ctx context.Context, req *bsvmspb.ConnectPeerRequest) (*bsvmspb.ConnectPeerResponse, error) {
	if req.GetAddress() == "" {
		return nil, invalid("address required")
	}
	if err := s.node.ConnectPeer(req.GetAddress()); err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.ConnectPeerResponse{}, nil
}

func (s *Service) ListWallets(ctx context.Context, req *bsvmspb.ListWalletsRequest) (*bsvmspb.ListWalletsResponse, error) {
	if err := validateTenant(req.GetTenantId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var keys []string
	for key, tw := range s.wallets {
		if tw.meta.TenantID == req.GetTenantId() {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var wallets []*bsvmspb.Wallet
	for _, key := range keys {
		wallets = append(wallets, s.walletProtoLocked(s.wallets[key]))
	}
	s.mu.Unlock()
	return &bsvmspb.ListWalletsResponse{Wallets: wallets}, nil
}

func (s *Service) CreateWallet(ctx context.Context, req *bsvmspb.CreateWalletRequest) (*bsvmspb.CreateWalletResponse, error) {
	if err := validateTenantWallet(req.GetTenantId(), req.GetWalletId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := walletKey(req.GetTenantId(), req.GetWalletId())
	if _, ok := s.wallets[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "wallet exists")
	}
	sdkName := sdkWalletName(req.GetTenantId(), req.GetWalletId())
	w, mnemonic, err := s.node.CreateWallet(sdkName)
	if err != nil {
		return nil, internal(err)
	}
	meta, err := s.newWalletMeta(req.GetTenantId(), req.GetWalletId(), sdkName, mnemonic, "")
	if err != nil {
		return nil, internal(err)
	}
	tw, err := s.attachWalletLocked(ctx, meta, w)
	if err != nil {
		return nil, err
	}
	s.state.Wallets = append(s.state.Wallets, meta)
	if err := s.saveStateLocked(); err != nil {
		return nil, internal(err)
	}
	_ = s.saveSnapshotLocked(tw)
	tokens, err := s.tokensForWallet(meta)
	if err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.CreateWalletResponse{Wallet: s.walletProtoLocked(tw), Mnemonic: mnemonic, Tokens: tokens}, nil
}

func (s *Service) RestoreWallet(ctx context.Context, req *bsvmspb.RestoreWalletRequest) (*bsvmspb.WalletResponse, error) {
	if err := validateTenantWallet(req.GetTenantId(), req.GetWalletId()); err != nil {
		return nil, err
	}
	if req.GetMnemonic() == "" {
		return nil, invalid("mnemonic required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := walletKey(req.GetTenantId(), req.GetWalletId())
	if _, ok := s.wallets[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "wallet exists")
	}
	meta, err := s.newWalletMeta(req.GetTenantId(), req.GetWalletId(), sdkWalletName(req.GetTenantId(), req.GetWalletId()), req.GetMnemonic(), req.GetPassphrase())
	if err != nil {
		return nil, internal(err)
	}
	w, err := s.node.RestoreWallet(meta.SDKName, req.GetMnemonic(), req.GetPassphrase())
	if err != nil {
		return nil, internal(err)
	}
	tw, err := s.attachWalletLocked(ctx, meta, w)
	if err != nil {
		return nil, err
	}
	s.state.Wallets = append(s.state.Wallets, meta)
	if err := s.saveStateLocked(); err != nil {
		return nil, internal(err)
	}
	_ = s.saveSnapshotLocked(tw)
	tokens, err := s.tokensForWallet(meta)
	if err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.WalletResponse{Wallet: s.walletProtoLocked(tw), Tokens: tokens}, nil
}

func (s *Service) GetWallet(ctx context.Context, req *bsvmspb.GetWalletRequest) (*bsvmspb.WalletResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	return &bsvmspb.WalletResponse{Wallet: s.walletProto(tw)}, nil
}

func (s *Service) NewAddress(ctx context.Context, req *bsvmspb.NewAddressRequest) (*bsvmspb.AddressResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	addr, err := tw.w.NewDerivedAddress()
	if err != nil {
		return nil, internal(err)
	}
	s.saveSnapshot(tw)
	return &bsvmspb.AddressResponse{Address: derivedAddress(addr)}, nil
}

func (s *Service) BatchNewAddresses(ctx context.Context, req *bsvmspb.BatchNewAddressesRequest) (*bsvmspb.BatchNewAddressesResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	if req.GetCount() <= 0 {
		return nil, invalid("count must be positive")
	}
	addrs, err := tw.w.BatchNewDerivedAddresses(int(req.GetCount()))
	if err != nil {
		return nil, internal(err)
	}
	s.saveSnapshot(tw)
	out := make([]*bsvmspb.DerivedAddress, len(addrs))
	for i, addr := range addrs {
		out[i] = derivedAddress(addr)
	}
	return &bsvmspb.BatchNewAddressesResponse{Addresses: out}, nil
}

func (s *Service) DeriveAt(ctx context.Context, req *bsvmspb.DeriveAtRequest) (*bsvmspb.AddressResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	address, err := tw.w.DeriveAt(req.GetBranch(), req.GetIndex())
	if err != nil {
		return nil, internal(err)
	}
	pub, _, err := tw.w.PubKeyAt(req.GetBranch(), req.GetIndex())
	if err != nil {
		return nil, internal(err)
	}
	spec, err := s.node.P2PKHOutput(address, 0)
	if err != nil {
		return nil, internal(err)
	}
	s.saveSnapshot(tw)
	return &bsvmspb.AddressResponse{Address: &bsvmspb.DerivedAddress{
		Address: address,
		Path:    fmt.Sprintf("%d/%d", req.GetBranch(), req.GetIndex()),
		PubKey:  pub,
		Script:  spec.Script,
		Branch:  req.GetBranch(),
		Index:   req.GetIndex(),
	}}, nil
}

func (s *Service) PubKeyAt(ctx context.Context, req *bsvmspb.PubKeyAtRequest) (*bsvmspb.PubKeyAtResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	pub, address, err := tw.w.PubKeyAt(req.GetBranch(), req.GetIndex())
	if err != nil {
		return nil, internal(err)
	}
	s.saveSnapshot(tw)
	return &bsvmspb.PubKeyAtResponse{PubKey: pub, Address: address}, nil
}

func (s *Service) SignHashAt(ctx context.Context, req *bsvmspb.SignHashAtRequest) (*bsvmspb.SignHashAtResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	sig, pub, err := tw.w.SignHashAt(req.GetBranch(), req.GetIndex(), req.GetHash())
	if err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.SignHashAtResponse{Signature: sig, PubKey: pub}, nil
}

func (s *Service) NextIndex(ctx context.Context, req *bsvmspb.NextIndexRequest) (*bsvmspb.NextIndexResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	return &bsvmspb.NextIndexResponse{NextIndex: tw.w.NextIndex(req.GetBranch())}, nil
}

func (s *Service) Balance(ctx context.Context, req *bsvmspb.BalanceRequest) (*bsvmspb.BalanceResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	return &bsvmspb.BalanceResponse{Satoshis: tw.w.Balance(), Bsv: tw.w.BalanceBSV()}, nil
}

func (s *Service) ListUTXOs(ctx context.Context, req *bsvmspb.ListUTXOsRequest) (*bsvmspb.ListUTXOsResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	return &bsvmspb.ListUTXOsResponse{Utxos: utxos(tw.w.UTXOs())}, nil
}

func (s *Service) ImportUTXO(ctx context.Context, req *bsvmspb.ImportUTXORequest) (*bsvmspb.ImportUTXOResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	if req.GetForce() {
		err = tw.w.ForceImportUTXO(req.GetTxid(), req.GetVout(), req.GetValue(), req.GetScript(), req.GetHeight())
	} else {
		err = tw.w.ImportUTXO(req.GetTxid(), req.GetVout(), req.GetValue(), req.GetScript(), req.GetHeight())
	}
	if err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.ImportUTXOResponse{}, nil
}

func (s *Service) ProcessRawTx(ctx context.Context, req *bsvmspb.ProcessRawTxRequest) (*bsvmspb.ProcessRawTxResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	if err := tw.w.ProcessRawTx(req.GetRawTx(), req.GetHeight()); err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.ProcessRawTxResponse{}, nil
}

func (s *Service) ClearUTXOs(ctx context.Context, req *bsvmspb.ClearUTXOsRequest) (*bsvmspb.ClearUTXOsResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	return &bsvmspb.ClearUTXOsResponse{Cleared: int32(tw.w.ClearUTXOs())}, nil
}

func (s *Service) ReloadFromStore(ctx context.Context, req *bsvmspb.ReloadFromStoreRequest) (*bsvmspb.ReloadFromStoreResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	n, err := tw.w.ReloadFromStore(ctx)
	if err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.ReloadFromStoreResponse{Loaded: int32(n)}, nil
}

func (s *Service) WipeWallet(ctx context.Context, req *bsvmspb.WipeWalletRequest) (*bsvmspb.WipeWalletResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	n, err := tw.w.Wipe(ctx)
	if err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.WipeWalletResponse{Removed: int32(n)}, nil
}

func (s *Service) PruneUnknownUTXOs(ctx context.Context, req *bsvmspb.PruneUnknownUTXOsRequest) (*bsvmspb.PruneUnknownUTXOsResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	return &bsvmspb.PruneUnknownUTXOsResponse{Removed: int32(tw.w.PruneUnknownUTXOs())}, nil
}

func (s *Service) IgnoreOutpoint(ctx context.Context, req *bsvmspb.IgnoreOutpointRequest) (*bsvmspb.IgnoreOutpointResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	if req.GetUnignore() {
		tw.w.UnignoreOutpoint(req.GetTxid(), req.GetVout())
	} else {
		tw.w.IgnoreOutpoint(req.GetTxid(), req.GetVout())
	}
	return &bsvmspb.IgnoreOutpointResponse{}, nil
}

func (s *Service) IsOutpointIgnored(ctx context.Context, req *bsvmspb.IsOutpointIgnoredRequest) (*bsvmspb.IsOutpointIgnoredResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	return &bsvmspb.IsOutpointIgnoredResponse{Ignored: tw.w.IsOutpointIgnored(req.GetTxid(), req.GetVout())}, nil
}

func (s *Service) UntrackUTXO(ctx context.Context, req *bsvmspb.UntrackUTXORequest) (*bsvmspb.UntrackUTXOResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	return &bsvmspb.UntrackUTXOResponse{RemovedValue: tw.w.UntrackUTXO(req.GetTxid(), req.GetVout())}, nil
}

func (s *Service) CanCover(ctx context.Context, req *bsvmspb.CanCoverRequest) (*bsvmspb.CanCoverResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	if err := tw.w.CanCover(req.GetTotalOut(), req.GetFeePerByte(), int(req.GetFixedOutputs())); err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.CanCoverResponse{}, nil
}

func (s *Service) Send(ctx context.Context, req *bsvmspb.SendRequest) (*bsvmspb.SpendResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	txid, err := tw.w.Send(req.GetToAddress(), req.GetAmountSatoshis())
	if err != nil {
		return nil, internal(err)
	}
	s.saveSnapshot(tw)
	return &bsvmspb.SpendResponse{Txid: txid}, nil
}

func (s *Service) SendAll(ctx context.Context, req *bsvmspb.SendAllRequest) (*bsvmspb.SpendResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	txid, err := tw.w.SendAll(req.GetToAddress())
	if err != nil {
		return nil, internal(err)
	}
	s.saveSnapshot(tw)
	return &bsvmspb.SpendResponse{Txid: txid}, nil
}

func (s *Service) SpendToOutputs(ctx context.Context, req *bsvmspb.SpendToOutputsRequest) (*bsvmspb.SpendDetailResponse, error) {
	tw, err := s.wallet(ctx, req.GetTenantId(), req.GetWalletId())
	if err != nil {
		return nil, err
	}
	outs := outputSpecs(req.GetOutputs())
	var detail *bsv.SpendDetail
	if req.GetIgnoreFixedOutputs() {
		detail, err = tw.w.SpendToOutputsDetailedIgnoringFixed(outs)
	} else {
		detail, err = tw.w.SpendToOutputsDetailed(outs)
	}
	if err != nil {
		return nil, internal(err)
	}
	s.saveSnapshot(tw)
	return &bsvmspb.SpendDetailResponse{Detail: spendDetail(detail)}, nil
}

func (s *Service) BroadcastCustomSpend(ctx context.Context, req *bsvmspb.BroadcastCustomSpendRequest) (*bsvmspb.SpendResponse, error) {
	if !s.opts.EnableCustomSpend {
		return nil, status.Error(codes.FailedPrecondition, "BroadcastCustomSpend disabled")
	}
	inputs := make([]bsv.CustomInput, len(req.GetInputs()))
	for i, in := range req.GetInputs() {
		txid, err := bsv.TxIDFromHex(in.GetTxid())
		if err != nil {
			return nil, invalid("invalid input txid")
		}
		inputs[i] = bsv.CustomInput{TxID: txid, Vout: in.GetVout(), ScriptSig: in.GetScriptSig(), Sequence: in.GetSequence()}
	}
	txid, err := s.node.SpendCustom(inputs, outputSpecs(req.GetOutputs()))
	if err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.SpendResponse{Txid: txid}, nil
}

func (s *Service) P2PKHOutput(ctx context.Context, req *bsvmspb.P2PKHOutputRequest) (*bsvmspb.OutputSpec, error) {
	out, err := s.node.P2PKHOutput(req.GetAddress(), req.GetValue())
	if err != nil {
		return nil, internal(err)
	}
	return outputSpec(out), nil
}

func (s *Service) OpReturnOutput(ctx context.Context, req *bsvmspb.OpReturnOutputRequest) (*bsvmspb.OutputSpec, error) {
	return outputSpec(bsv.OpReturnOutput(req.GetData())), nil
}

func (s *Service) AnyoneCanSpendOutput(ctx context.Context, req *bsvmspb.AnyoneCanSpendOutputRequest) (*bsvmspb.OutputSpec, error) {
	return outputSpec(bsv.AnyoneCanSpendOutput(req.GetValue())), nil
}

func (s *Service) ParseTransaction(ctx context.Context, req *bsvmspb.ParseTransactionRequest) (*bsvmspb.Transaction, error) {
	tx, err := bsv.ParseTransaction(req.GetRawTx())
	if err != nil {
		return nil, internal(err)
	}
	return s.transactionProto(tx), nil
}

func (s *Service) BroadcastRaw(ctx context.Context, req *bsvmspb.BroadcastRawRequest) (*bsvmspb.BroadcastRawResponse, error) {
	txid, err := s.node.BroadcastRaw(req.GetRawTx())
	if err != nil {
		return nil, internal(err)
	}
	return &bsvmspb.BroadcastRawResponse{Txid: txid}, nil
}

func (s *Service) ExecuteScript(ctx context.Context, req *bsvmspb.ExecuteScriptRequest) (*bsvmspb.ExecuteScriptResponse, error) {
	var tx *bsv.Transaction
	if len(req.GetRawTx()) > 0 {
		parsed, err := bsv.ParseTransaction(req.GetRawTx())
		if err != nil {
			return nil, internal(err)
		}
		tx = parsed
	}
	if err := s.node.ExecuteScript(req.GetScriptSig(), req.GetScriptPubKey(), tx, int(req.GetInputIndex()), req.GetAmount()); err != nil {
		return &bsvmspb.ExecuteScriptResponse{Ok: false, Error: err.Error()}, nil
	}
	return &bsvmspb.ExecuteScriptResponse{Ok: true}, nil
}

func (s *Service) DecodeOutputAddress(ctx context.Context, req *bsvmspb.DecodeOutputAddressRequest) (*bsvmspb.DecodeOutputAddressResponse, error) {
	addr, ok := s.node.DecodeOutputAddress(req.GetScriptPubKey())
	return &bsvmspb.DecodeOutputAddressResponse{Address: addr, Ok: ok}, nil
}

func (s *Service) PendingTransactions(context.Context, *bsvmspb.PendingTransactionsRequest) (*bsvmspb.PendingTransactionsResponse, error) {
	src := s.node.PendingTransactions()
	out := make([]*bsvmspb.PendingTransaction, len(src))
	for i, tx := range src {
		out[i] = &bsvmspb.PendingTransaction{
			Txid:           tx.TxID,
			FirstBroadcast: tx.FirstBroadcast.Format(time.RFC3339Nano),
			LastBroadcast:  tx.LastBroadcast.Format(time.RFC3339Nano),
			LastAnnounce:   tx.LastAnnounce.Format(time.RFC3339Nano),
			Rebroadcasts:   int32(tx.Rebroadcasts),
		}
	}
	return &bsvmspb.PendingTransactionsResponse{Transactions: out}, nil
}

func (s *Service) RebroadcastPendingTransactions(context.Context, *bsvmspb.RebroadcastPendingTransactionsRequest) (*bsvmspb.RebroadcastPendingTransactionsResponse, error) {
	return &bsvmspb.RebroadcastPendingTransactionsResponse{Count: int32(s.node.RebroadcastPendingTransactions())}, nil
}

func (s *Service) StreamTransactions(req *bsvmspb.StreamTransactionsRequest, stream grpc.ServerStreamingServer[bsvmspb.Transaction]) error {
	return streamAll(stream.Context(), s.txBroker, func(v *bsvmspb.Transaction) error { return stream.Send(v) })
}

func (s *Service) StreamBlocks(req *bsvmspb.StreamBlocksRequest, stream grpc.ServerStreamingServer[bsvmspb.Block]) error {
	return streamAll(stream.Context(), s.blockBroker, func(v *bsvmspb.Block) error { return stream.Send(v) })
}

func (s *Service) StreamPayments(req *bsvmspb.StreamPaymentsRequest, stream grpc.ServerStreamingServer[bsvmspb.Payment]) error {
	if err := s.authorize(stream.Context(), "/bsvms.v1.BSVMS/StreamPayments", req); err != nil {
		return err
	}
	return streamFiltered(stream.Context(), s.paymentBroker, func(v *bsvmspb.Payment) bool {
		return matches(req.GetTenantId(), req.GetWalletId(), v.GetTenantId(), v.GetWalletId())
	}, func(v *bsvmspb.Payment) error { return stream.Send(v) })
}

func (s *Service) StreamWalletTransactions(req *bsvmspb.StreamWalletTransactionsRequest, stream grpc.ServerStreamingServer[bsvmspb.WalletTransaction]) error {
	if err := s.authorize(stream.Context(), "/bsvms.v1.BSVMS/StreamWalletTransactions", req); err != nil {
		return err
	}
	return streamFiltered(stream.Context(), s.walletTxBroker, func(v *bsvmspb.WalletTransaction) bool {
		return matches(req.GetTenantId(), req.GetWalletId(), v.GetTenantId(), v.GetWalletId())
	}, func(v *bsvmspb.WalletTransaction) error { return stream.Send(v) })
}

func (s *Service) StreamP2PTraffic(req *bsvmspb.StreamP2PTrafficRequest, stream grpc.ServerStreamingServer[bsvmspb.P2PTraffic]) error {
	return streamAll(stream.Context(), s.trafficBroker, func(v *bsvmspb.P2PTraffic) error { return stream.Send(v) })
}

func (s *Service) StreamRejects(req *bsvmspb.StreamRejectsRequest, stream grpc.ServerStreamingServer[bsvmspb.Reject]) error {
	return streamAll(stream.Context(), s.rejectBroker, func(v *bsvmspb.Reject) error { return stream.Send(v) })
}

func (s *Service) registerNodeEvents() {
	s.node.OnTransaction(func(tx *bsv.Transaction) {
		s.txBroker.publish(s.transactionProto(tx))
	})
	s.node.OnBlock(func(b *bsv.Block) {
		spends := b.InputSpends()
		outSpends := make([]*bsvmspb.BlockSpend, len(spends))
		for i, sp := range spends {
			outSpends[i] = &bsvmspb.BlockSpend{
				PrevTxid:     sp.PrevTxID,
				PrevVout:     sp.PrevVout,
				SpendingTxid: sp.SpendingTxID,
			}
		}
		s.blockBroker.publish(&bsvmspb.Block{Hash: b.Hash(), Height: b.Height, Txids: b.TxIDs(), Spends: outSpends})
	})
	s.node.OnP2PTraffic(func(t bsv.P2PTraffic) {
		s.trafficBroker.publish(&bsvmspb.P2PTraffic{
			Peer:         t.Peer,
			Direction:    t.Direction,
			Command:      t.Command,
			PayloadBytes: int32(t.PayloadBytes),
			Summary:      t.Summary,
		})
	})
	s.node.OnReject(func(r bsv.Reject) {
		s.rejectBroker.publish(&bsvmspb.Reject{
			Peer:     r.Peer,
			Command:  r.Command,
			Code:     uint32(r.Code),
			CodeName: r.CodeName,
			Reason:   r.Reason,
			Hash:     r.Hash,
		})
	})
}

func (s *Service) registerWalletEvents(tw *tenantWallet) {
	tenantID := tw.meta.TenantID
	walletID := tw.meta.WalletID
	tw.w.OnPayment(func(p bsv.Payment) {
		s.paymentBroker.publish(&bsvmspb.Payment{
			TenantId: tenantID,
			WalletId: walletID,
			Txid:     p.TxID,
			Amount:   p.Amount,
			Address:  p.Address,
			Vout:     p.Vout,
			Script:   p.Script,
		})
	})
	tw.w.OnWalletTx(func(tx *bsv.Transaction, owned []bsv.OwnedOutput, spent []bsv.SpentOutPoint) {
		o := make([]*bsvmspb.OwnedOutput, len(owned))
		for i, item := range owned {
			o[i] = &bsvmspb.OwnedOutput{Vout: item.Vout, Value: item.Value, Script: item.Script, Address: item.Address}
		}
		sp := make([]*bsvmspb.SpentOutPoint, len(spent))
		for i, item := range spent {
			sp[i] = &bsvmspb.SpentOutPoint{Txid: item.TxID, Vout: item.Vout}
		}
		s.walletTxBroker.publish(&bsvmspb.WalletTransaction{
			TenantId:    tenantID,
			WalletId:    walletID,
			Transaction: s.transactionProto(tx),
			Owned:       o,
			Spent:       sp,
		})
	})
}

func (s *Service) wallet(ctx context.Context, tenantID, walletID string) (*tenantWallet, error) {
	if err := validateTenantWallet(tenantID, walletID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tw, ok := s.wallets[walletKey(tenantID, walletID)]
	if !ok {
		return nil, status.Error(codes.NotFound, "wallet not found")
	}
	return tw, nil
}

func (s *Service) restoreLoadedWallet(ctx context.Context, meta walletMeta) (*tenantWallet, error) {
	mnemonic, passphrase, err := s.openWalletSecrets(meta)
	if err != nil {
		return nil, err
	}
	w, err := s.node.RestoreWallet(meta.SDKName, mnemonic, passphrase)
	if err != nil {
		return nil, err
	}
	if meta.Mnemonic != "" || meta.Passphrase != "" {
		migrated, err := s.newWalletMeta(meta.TenantID, meta.WalletID, meta.SDKName, mnemonic, passphrase)
		if err != nil {
			return nil, err
		}
		meta = migrated
		for i := range s.state.Wallets {
			if s.state.Wallets[i].TenantID == meta.TenantID && s.state.Wallets[i].WalletID == meta.WalletID {
				s.state.Wallets[i] = meta
			}
		}
	}
	return s.attachWalletLocked(ctx, meta, w)
}

func (s *Service) newWalletMeta(tenantID, walletID, sdkName, mnemonic, passphrase string) (walletMeta, error) {
	sealedMnemonic, err := sealString(s.opts.DataKey, mnemonic)
	if err != nil {
		return walletMeta{}, err
	}
	sealedPassphrase, err := sealString(s.opts.DataKey, passphrase)
	if err != nil {
		return walletMeta{}, err
	}
	return walletMeta{
		TenantID:             tenantID,
		WalletID:             walletID,
		SDKName:              sdkName,
		MnemonicCiphertext:   sealedMnemonic,
		PassphraseCiphertext: sealedPassphrase,
	}, nil
}

func (s *Service) openWalletSecrets(meta walletMeta) (mnemonic, passphrase string, err error) {
	if meta.Mnemonic != "" || meta.Passphrase != "" {
		return meta.Mnemonic, meta.Passphrase, nil
	}
	mnemonic, err = openString(s.opts.DataKey, meta.MnemonicCiphertext)
	if err != nil {
		return "", "", err
	}
	passphrase, err = openString(s.opts.DataKey, meta.PassphraseCiphertext)
	if err != nil {
		return "", "", err
	}
	return mnemonic, passphrase, nil
}

func (s *Service) tokensForWallet(meta walletMeta) (*bsvmspb.AuthTokens, error) {
	if !s.opts.AuthEnabled {
		return nil, nil
	}
	return s.issueTokens(meta.TenantID, meta.WalletID)
}

func (s *Service) attachWalletLocked(ctx context.Context, meta walletMeta, w *bsv.Wallet) (*tenantWallet, error) {
	store, err := walletsqlite.Open(ctx, s.walletStorePath(meta))
	if err != nil {
		return nil, internal(err)
	}
	w.SetStore(store)
	if nextExternal, nextChange, entries, err := bsv.ReadAddressSnapshot(s.walletSnapshotPath(meta)); err == nil {
		w.LoadAddressSnapshot(entries)
		w.SetNextIndices(nextExternal, nextChange)
	} else if !errors.Is(err, bsv.ErrSnapshotNotFound) {
		_ = store.Close()
		return nil, internal(err)
	}
	if _, err := w.LoadFromStore(ctx); err != nil {
		_ = store.Close()
		return nil, internal(err)
	}
	tw := &tenantWallet{meta: meta, w: w, store: store}
	s.wallets[walletKey(meta.TenantID, meta.WalletID)] = tw
	s.registerWalletEvents(tw)
	return tw, nil
}

func (s *Service) loadState() error {
	path := s.statePath()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.state = stateFile{}
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &s.state); err != nil {
		return err
	}
	return nil
}

func (s *Service) saveStateLocked() error {
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath())
}

func (s *Service) saveSnapshot(tw *tenantWallet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.saveSnapshotLocked(tw)
}

func (s *Service) saveSnapshotLocked(tw *tenantWallet) error {
	nextExternal, nextChange := tw.w.NextIndices()
	return bsv.WriteAddressSnapshot(s.walletSnapshotPath(tw.meta), nextExternal, nextChange, tw.w.SaveAddressSnapshot())
}

func (s *Service) walletProto(tw *tenantWallet) *bsvmspb.Wallet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.walletProtoLocked(tw)
}

func (s *Service) walletProtoLocked(tw *tenantWallet) *bsvmspb.Wallet {
	nextExternal, nextChange := tw.w.NextIndices()
	return &bsvmspb.Wallet{
		TenantId:          tw.meta.TenantID,
		WalletId:          tw.meta.WalletID,
		Addresses:         walletAddresses(tw.w),
		BalanceSatoshis:   tw.w.Balance(),
		BalanceBsv:        tw.w.BalanceBSV(),
		NextExternalIndex: nextExternal,
		NextChangeIndex:   nextChange,
		Selector:          fmt.Sprintf("%v", tw.w.Selector()),
	}
}

func (s *Service) transactionProto(tx *bsv.Transaction) *bsvmspb.Transaction {
	if tx == nil {
		return nil
	}
	inputs := make([]*bsvmspb.TxInput, tx.InputCount())
	for i := 0; i < tx.InputCount(); i++ {
		prevTxid, prevVout := tx.InputPrevOutPoint(i)
		sender, _ := s.node.SenderAddressFromInput(tx, i)
		inputs[i] = &bsvmspb.TxInput{
			PrevTxid:      prevTxid,
			PrevVout:      prevVout,
			ScriptSig:     tx.InputScriptSig(i),
			Coinbase:      tx.InputIsCoinbase(i),
			SenderAddress: sender,
		}
	}
	outputs := make([]*bsvmspb.TxOutput, tx.OutputCount())
	for i := 0; i < tx.OutputCount(); i++ {
		script := tx.OutputScript(i)
		address, _ := s.node.DecodeOutputAddress(script)
		outputs[i] = &bsvmspb.TxOutput{Vout: uint32(i), Value: tx.OutputValue(i), Script: script, Address: address}
	}
	return &bsvmspb.Transaction{Txid: tx.TxID(), RawTx: tx.Bytes(), Inputs: inputs, Outputs: outputs}
}

func (s *Service) statePath() string { return filepath.Join(s.dataDir, "bsvms-wallets.json") }
func (s *Service) walletDir() string { return filepath.Join(s.dataDir, "wallets") }

func (s *Service) walletStorePath(meta walletMeta) string {
	return filepath.Join(s.walletDir(), walletFileID(meta)+".sqlite")
}

func (s *Service) walletSnapshotPath(meta walletMeta) string {
	return filepath.Join(s.walletDir(), walletFileID(meta)+".addrsnap")
}

func walletFileID(meta walletMeta) string {
	sum := sha256.Sum256([]byte(meta.TenantID + "\x00" + meta.WalletID))
	return hex.EncodeToString(sum[:])
}

func walletKey(tenantID, walletID string) string { return tenantID + "\x00" + walletID }

func sdkWalletName(tenantID, walletID string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + walletID))
	return "bsvms:" + hex.EncodeToString(sum[:16])
}

func validateTenantWallet(tenantID, walletID string) error {
	if err := validateTenant(tenantID); err != nil {
		return err
	}
	if !idRE.MatchString(walletID) {
		return invalid("wallet_id must match [A-Za-z0-9][A-Za-z0-9_.-]{0,127}")
	}
	return nil
}

func validateTenant(tenantID string) error {
	if !idRE.MatchString(tenantID) {
		return invalid("tenant_id must match [A-Za-z0-9][A-Za-z0-9_.-]{0,127}")
	}
	return nil
}

func matches(reqTenant, reqWallet, tenant, wallet string) bool {
	if reqTenant != "" && reqTenant != tenant {
		return false
	}
	if reqWallet != "" && reqWallet != wallet {
		return false
	}
	return true
}

func invalid(msg string) error { return status.Error(codes.InvalidArgument, msg) }

func internal(err error) error {
	if err == nil {
		return nil
	}
	return status.Error(codes.Internal, err.Error())
}

func derivedAddress(a *bsv.DerivedAddress) *bsvmspb.DerivedAddress {
	return &bsvmspb.DerivedAddress{
		Address: a.Address,
		Path:    a.Path,
		PubKey:  a.PubKey,
		Script:  a.Script,
		Branch:  a.Branch,
		Index:   a.Index,
	}
}

func outputSpec(o bsv.OutputSpec) *bsvmspb.OutputSpec {
	return &bsvmspb.OutputSpec{Script: o.Script, Value: o.Value}
}

func outputSpecs(src []*bsvmspb.OutputSpec) []bsv.OutputSpec {
	out := make([]bsv.OutputSpec, len(src))
	for i, item := range src {
		out[i] = bsv.OutputSpec{Script: item.GetScript(), Value: item.GetValue()}
	}
	return out
}

func utxos(src []bsv.UTXO) []*bsvmspb.UTXO {
	out := make([]*bsvmspb.UTXO, len(src))
	for i, u := range src {
		out[i] = &bsvmspb.UTXO{Txid: u.TxID, Vout: u.Vout, Value: u.Value, Script: u.Script, Height: u.Height}
	}
	return out
}

func walletAddresses(w *bsv.Wallet) []string {
	entries := w.SaveAddressSnapshot()
	if len(entries) == 0 {
		return w.Addresses()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Index < entries[j].Index })
	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if _, ok := seen[entry.Address]; ok {
			continue
		}
		seen[entry.Address] = struct{}{}
		out = append(out, entry.Address)
	}
	return out
}

func spendDetail(d *bsv.SpendDetail) *bsvmspb.SpendDetail {
	if d == nil {
		return nil
	}
	out := &bsvmspb.SpendDetail{Txid: d.TxID, SpentUtxos: utxos(d.SpentUTXOs), RawTx: d.RawTx}
	if d.ChangeUTXO != nil {
		u := utxos([]bsv.UTXO{*d.ChangeUTXO})[0]
		out.ChangeUtxo = u
	}
	return out
}

type broker[T any] struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]chan T
}

func newBroker[T any]() *broker[T] {
	return &broker[T]{subs: make(map[int]chan T)}
}

func (b *broker[T]) subscribe() (int, <-chan T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan T, 64)
	b.subs[id] = ch
	return id, ch
}

func (b *broker[T]) unsubscribe(id int) {
	b.mu.Lock()
	ch := b.subs[id]
	delete(b.subs, id)
	b.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (b *broker[T]) publish(v T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- v:
		default:
		}
	}
}

func streamAll[T any](ctx context.Context, b *broker[T], send func(T) error) error {
	return streamFiltered(ctx, b, func(T) bool { return true }, send)
}

func streamFiltered[T any](ctx context.Context, b *broker[T], keep func(T) bool, send func(T) error) error {
	id, ch := b.subscribe()
	defer b.unsubscribe(id)
	for {
		select {
		case <-ctx.Done():
			return nil
		case v, ok := <-ch:
			if !ok {
				return nil
			}
			if keep(v) {
				if err := send(v); err != nil {
					return err
				}
			}
		}
	}
}
