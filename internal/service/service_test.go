package service

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bsv "github.com/brad1121/bitcoinsv-sdk-go/sdk"
	bsvmspb "github.com/brad1121/bsvms/gen/bsvms/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const validTxHex = "0100000001a15d57094aa7a21a28cb20b59aab8fc7d1149a3bdbcddba9c301e9383dcdde9f000000006a4730440220aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0220bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb012102ccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccffffffff0280f0fa02000000001976a914dededededededededededededededededededededede88ac400d0300000000001976a9146b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b88ac00000000"

type testEnv struct {
	ctx context.Context
	svc *Service
}

func newTestEnv(t *testing.T, noBroadcast bool) *testEnv {
	t.Helper()
	cfg := bsv.DefaultConfig()
	cfg.DisableClassicP2P = noBroadcast
	node := bsv.New(bsv.Regtest, cfg)
	svc, err := New(context.Background(), node, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return &testEnv{ctx: context.Background(), svc: svc}
}

func newTestEnvWithOptions(t *testing.T, noBroadcast bool, opts Options) *testEnv {
	t.Helper()
	cfg := bsv.DefaultConfig()
	cfg.DisableClassicP2P = noBroadcast
	node := bsv.New(bsv.Regtest, cfg)
	svc, err := NewWithOptions(context.Background(), node, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return &testEnv{ctx: context.Background(), svc: svc}
}

func TestWalletPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	node := bsv.New(bsv.Regtest)
	svc, err := New(ctx, node, dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.CreateWallet(ctx, &bsvmspb.CreateWalletRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetMnemonic() == "" {
		t.Fatal("mnemonic missing")
	}
	addr, err := svc.NewAddress(ctx, &bsvmspb.NewAddressRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	if addr.GetAddress().GetAddress() == "" {
		t.Fatal("address missing")
	}
	svc.Close()

	node2 := bsv.New(bsv.Regtest)
	svc2, err := New(ctx, node2, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	list, err := svc2.ListWallets(ctx, &bsvmspb.ListWalletsRequest{TenantId: "tenant1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetWallets()) != 1 {
		t.Fatalf("wallet count = %d, want 1", len(list.GetWallets()))
	}
	wallet := list.GetWallets()[0]
	if wallet.GetNextExternalIndex() != 1 {
		t.Fatalf("next external = %d, want 1", wallet.GetNextExternalIndex())
	}
	if got := wallet.GetAddresses(); len(got) != 1 || got[0] != addr.GetAddress().GetAddress() {
		t.Fatalf("addresses = %v, want %q", got, addr.GetAddress().GetAddress())
	}
}

func TestStatusAndPeerValidation(t *testing.T) {
	env := newTestEnv(t, false)
	got, err := env.svc.Status(env.ctx, &bsvmspb.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetNetwork() != "regtest" || got.GetChainHeight() != -1 || got.GetDataDir() == "" {
		t.Fatalf("status = %+v", got)
	}
	if _, err := env.svc.ConnectPeer(env.ctx, &bsvmspb.ConnectPeerRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ConnectPeer empty code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestWalletLifecycleHappyAndUnhappy(t *testing.T) {
	env := newTestEnv(t, false)
	created := createWallet(t, env, "tenant1", "wallet1")
	if created.GetWallet().GetTenantId() != "tenant1" || created.GetWallet().GetWalletId() != "wallet1" {
		t.Fatalf("created wallet = %+v", created.GetWallet())
	}
	if _, err := env.svc.CreateWallet(env.ctx, &bsvmspb.CreateWalletRequest{TenantId: "tenant1", WalletId: "wallet1"}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate create code = %v, want AlreadyExists", status.Code(err))
	}
	if _, err := env.svc.CreateWallet(env.ctx, &bsvmspb.CreateWalletRequest{TenantId: "../bad", WalletId: "wallet2"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad tenant code = %v, want InvalidArgument", status.Code(err))
	}
	got, err := env.svc.GetWallet(env.ctx, &bsvmspb.GetWalletRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetWallet().GetWalletId() != "wallet1" {
		t.Fatalf("GetWallet = %+v", got)
	}
	if _, err := env.svc.GetWallet(env.ctx, &bsvmspb.GetWalletRequest{TenantId: "tenant1", WalletId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing wallet code = %v, want NotFound", status.Code(err))
	}
	list, err := env.svc.ListWallets(env.ctx, &bsvmspb.ListWalletsRequest{TenantId: "tenant1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetWallets()) != 1 {
		t.Fatalf("wallet list len = %d, want 1", len(list.GetWallets()))
	}
	other, err := env.svc.ListWallets(env.ctx, &bsvmspb.ListWalletsRequest{TenantId: "tenant2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(other.GetWallets()) != 0 {
		t.Fatalf("tenant2 wallets = %d, want 0", len(other.GetWallets()))
	}
	restored, err := env.svc.RestoreWallet(env.ctx, &bsvmspb.RestoreWalletRequest{TenantId: "tenant1", WalletId: "restored", Mnemonic: created.GetMnemonic()})
	if err != nil {
		t.Fatal(err)
	}
	if restored.GetWallet().GetWalletId() != "restored" {
		t.Fatalf("restored = %+v", restored.GetWallet())
	}
	if _, err := env.svc.RestoreWallet(env.ctx, &bsvmspb.RestoreWalletRequest{TenantId: "tenant1", WalletId: "empty"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty mnemonic code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := env.svc.RestoreWallet(env.ctx, &bsvmspb.RestoreWalletRequest{TenantId: "../bad", WalletId: "bad", Mnemonic: created.GetMnemonic()}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad restore tenant code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestAddressKeyAndSigningHappyAndUnhappy(t *testing.T) {
	env := newTestEnv(t, false)
	createWallet(t, env, "tenant1", "wallet1")

	addr, err := env.svc.NewAddress(env.ctx, &bsvmspb.NewAddressRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	if addr.GetAddress().GetAddress() == "" || addr.GetAddress().GetPath() != "0/0" || len(addr.GetAddress().GetPubKey()) != 33 {
		t.Fatalf("NewAddress = %+v", addr.GetAddress())
	}
	batch, err := env.svc.BatchNewAddresses(env.ctx, &bsvmspb.BatchNewAddressesRequest{TenantId: "tenant1", WalletId: "wallet1", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.GetAddresses()) != 2 || batch.GetAddresses()[0].GetPath() != "0/1" || batch.GetAddresses()[1].GetPath() != "0/2" {
		t.Fatalf("batch = %+v", batch.GetAddresses())
	}
	if _, err := env.svc.BatchNewAddresses(env.ctx, &bsvmspb.BatchNewAddressesRequest{TenantId: "tenant1", WalletId: "wallet1"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("zero batch code = %v, want InvalidArgument", status.Code(err))
	}
	derived, err := env.svc.DeriveAt(env.ctx, &bsvmspb.DeriveAtRequest{TenantId: "tenant1", WalletId: "wallet1", Branch: 1, Index: 7})
	if err != nil {
		t.Fatal(err)
	}
	if derived.GetAddress().GetPath() != "1/7" || len(derived.GetAddress().GetScript()) == 0 {
		t.Fatalf("DeriveAt = %+v", derived.GetAddress())
	}
	pub, err := env.svc.PubKeyAt(env.ctx, &bsvmspb.PubKeyAtRequest{TenantId: "tenant1", WalletId: "wallet1", Branch: 1, Index: 7})
	if err != nil {
		t.Fatal(err)
	}
	if pub.GetAddress() != derived.GetAddress().GetAddress() || len(pub.GetPubKey()) != 33 {
		t.Fatalf("PubKeyAt = %+v", pub)
	}
	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	sig, err := env.svc.SignHashAt(env.ctx, &bsvmspb.SignHashAtRequest{TenantId: "tenant1", WalletId: "wallet1", Branch: 1, Index: 7, Hash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(sig.GetSignature()) == 0 || len(sig.GetPubKey()) != 33 || len(sig.GetSignature()) == 32 {
		t.Fatalf("SignHashAt leaked/wrong shape: sig=%d pub=%d", len(sig.GetSignature()), len(sig.GetPubKey()))
	}
	if _, err := env.svc.SignHashAt(env.ctx, &bsvmspb.SignHashAtRequest{TenantId: "tenant1", WalletId: "wallet1", Hash: []byte{1, 2, 3}}); status.Code(err) != codes.Internal {
		t.Fatalf("short hash code = %v, want Internal", status.Code(err))
	}
	next, err := env.svc.NextIndex(env.ctx, &bsvmspb.NextIndexRequest{TenantId: "tenant1", WalletId: "wallet1", Branch: 0})
	if err != nil {
		t.Fatal(err)
	}
	if next.GetNextIndex() != 3 {
		t.Fatalf("next external = %d, want 3", next.GetNextIndex())
	}
	if _, err := env.svc.NewAddress(env.ctx, &bsvmspb.NewAddressRequest{TenantId: "tenant1", WalletId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing NewAddress code = %v, want NotFound", status.Code(err))
	}
}

func TestBalanceUTXOAndStoreControlsHappyAndUnhappy(t *testing.T) {
	env := newTestEnv(t, false)
	createWallet(t, env, "tenant1", "wallet1")
	addr := mustNewAddress(t, env, "tenant1", "wallet1")
	script := mustP2PKHScript(t, env, addr, 50_000)

	if _, err := env.svc.ImportUTXO(env.ctx, &bsvmspb.ImportUTXORequest{TenantId: "tenant1", WalletId: "wallet1", Txid: txidHex(1), Vout: 0, Value: 50_000, Script: script, Height: 100}); err != nil {
		t.Fatal(err)
	}
	bal, err := env.svc.Balance(env.ctx, &bsvmspb.BalanceRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	if bal.GetSatoshis() != 50_000 {
		t.Fatalf("balance = %d, want 50000", bal.GetSatoshis())
	}
	utxos, err := env.svc.ListUTXOs(env.ctx, &bsvmspb.ListUTXOsRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(utxos.GetUtxos()) != 1 || utxos.GetUtxos()[0].GetTxid() != txidHex(1) {
		t.Fatalf("utxos = %+v", utxos.GetUtxos())
	}
	if _, err := env.svc.ImportUTXO(env.ctx, &bsvmspb.ImportUTXORequest{TenantId: "tenant1", WalletId: "wallet1", Txid: "bad", Script: script}); status.Code(err) != codes.Internal {
		t.Fatalf("bad import txid code = %v, want Internal", status.Code(err))
	}
	if _, err := env.svc.ProcessRawTx(env.ctx, &bsvmspb.ProcessRawTxRequest{TenantId: "tenant1", WalletId: "wallet1", RawTx: mustHex(t, validTxHex), Height: -1}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.ProcessRawTx(env.ctx, &bsvmspb.ProcessRawTxRequest{TenantId: "tenant1", WalletId: "wallet1", RawTx: []byte{0x01}}); status.Code(err) != codes.Internal {
		t.Fatalf("bad raw tx code = %v, want Internal", status.Code(err))
	}
	if _, err := env.svc.CanCover(env.ctx, &bsvmspb.CanCoverRequest{TenantId: "tenant1", WalletId: "wallet1", TotalOut: 1000, FeePerByte: 1, FixedOutputs: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.CanCover(env.ctx, &bsvmspb.CanCoverRequest{TenantId: "tenant1", WalletId: "wallet1", TotalOut: 1_000_000, FeePerByte: 1, FixedOutputs: 1}); status.Code(err) != codes.Internal {
		t.Fatalf("insufficient CanCover code = %v, want Internal", status.Code(err))
	}
	reloaded, err := env.svc.ReloadFromStore(env.ctx, &bsvmspb.ReloadFromStoreRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.GetLoaded() != 0 {
		t.Fatalf("reloaded = %d, want 0 for in-memory import path", reloaded.GetLoaded())
	}
	if _, err := env.svc.IgnoreOutpoint(env.ctx, &bsvmspb.IgnoreOutpointRequest{TenantId: "tenant1", WalletId: "wallet1", Txid: txidHex(1), Vout: 0}); err != nil {
		t.Fatal(err)
	}
	ignored, err := env.svc.IsOutpointIgnored(env.ctx, &bsvmspb.IsOutpointIgnoredRequest{TenantId: "tenant1", WalletId: "wallet1", Txid: txidHex(1), Vout: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !ignored.GetIgnored() {
		t.Fatal("outpoint not ignored")
	}
	untracked, err := env.svc.UntrackUTXO(env.ctx, &bsvmspb.UntrackUTXORequest{TenantId: "tenant1", WalletId: "wallet1", Txid: txidHex(1), Vout: 0})
	if err != nil {
		t.Fatal(err)
	}
	if untracked.GetRemovedValue() != 0 {
		t.Fatalf("ignored outpoint untrack value = %d, want 0", untracked.GetRemovedValue())
	}
	cleared, err := env.svc.ClearUTXOs(env.ctx, &bsvmspb.ClearUTXOsRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.GetCleared() != 0 {
		t.Fatalf("cleared = %d, want 0 after ignore", cleared.GetCleared())
	}
	pruned, err := env.svc.PruneUnknownUTXOs(env.ctx, &bsvmspb.PruneUnknownUTXOsRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	if pruned.GetRemoved() != 0 {
		t.Fatalf("pruned = %d, want 0", pruned.GetRemoved())
	}
	wiped, err := env.svc.WipeWallet(env.ctx, &bsvmspb.WipeWalletRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	if wiped.GetRemoved() < 0 {
		t.Fatalf("wipe removed = %d", wiped.GetRemoved())
	}
	if _, err := env.svc.Balance(env.ctx, &bsvmspb.BalanceRequest{TenantId: "tenant1", WalletId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing balance code = %v, want NotFound", status.Code(err))
	}
}

func TestTransactionOutputScriptHelpersHappyAndUnhappy(t *testing.T) {
	env := newTestEnv(t, true)
	createWallet(t, env, "tenant1", "wallet1")
	addr := mustNewAddress(t, env, "tenant1", "wallet1")

	p2pkh, err := env.svc.P2PKHOutput(env.ctx, &bsvmspb.P2PKHOutputRequest{Address: addr, Value: 1234})
	if err != nil {
		t.Fatal(err)
	}
	if p2pkh.GetValue() != 1234 || len(p2pkh.GetScript()) == 0 {
		t.Fatalf("P2PKHOutput = %+v", p2pkh)
	}
	if _, err := env.svc.P2PKHOutput(env.ctx, &bsvmspb.P2PKHOutputRequest{Address: "bad", Value: 1}); status.Code(err) != codes.Internal {
		t.Fatalf("bad P2PKH code = %v, want Internal", status.Code(err))
	}
	opret, err := env.svc.OpReturnOutput(env.ctx, &bsvmspb.OpReturnOutputRequest{Data: []byte("hello")})
	if err != nil {
		t.Fatal(err)
	}
	if opret.GetValue() != 0 || len(opret.GetScript()) < 3 || opret.GetScript()[1] != 0x6a {
		t.Fatalf("OpReturnOutput = %x", opret.GetScript())
	}
	acs, err := env.svc.AnyoneCanSpendOutput(env.ctx, &bsvmspb.AnyoneCanSpendOutputRequest{Value: 99})
	if err != nil {
		t.Fatal(err)
	}
	if acs.GetValue() != 99 || hex.EncodeToString(acs.GetScript()) != "51" {
		t.Fatalf("AnyoneCanSpend = %+v", acs)
	}
	decoded, err := env.svc.DecodeOutputAddress(env.ctx, &bsvmspb.DecodeOutputAddressRequest{ScriptPubKey: p2pkh.GetScript()})
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.GetOk() || decoded.GetAddress() != addr {
		t.Fatalf("DecodeOutputAddress = %+v", decoded)
	}
	badDecoded, err := env.svc.DecodeOutputAddress(env.ctx, &bsvmspb.DecodeOutputAddressRequest{ScriptPubKey: []byte{0x51}})
	if err != nil {
		t.Fatal(err)
	}
	if badDecoded.GetOk() {
		t.Fatalf("bad decode ok = true")
	}
	tx, err := env.svc.ParseTransaction(env.ctx, &bsvmspb.ParseTransactionRequest{RawTx: mustHex(t, validTxHex)})
	if err != nil {
		t.Fatal(err)
	}
	if tx.GetTxid() == "" || len(tx.GetInputs()) != 1 || len(tx.GetOutputs()) != 2 || len(tx.GetRawTx()) == 0 {
		t.Fatalf("ParseTransaction = %+v", tx)
	}
	if _, err := env.svc.ParseTransaction(env.ctx, &bsvmspb.ParseTransactionRequest{RawTx: []byte{0x01}}); status.Code(err) != codes.Internal {
		t.Fatalf("bad parse code = %v, want Internal", status.Code(err))
	}
	ok, err := env.svc.ExecuteScript(env.ctx, &bsvmspb.ExecuteScriptRequest{ScriptPubKey: []byte{0x51}})
	if err != nil {
		t.Fatal(err)
	}
	if !ok.GetOk() || ok.GetError() != "" {
		t.Fatalf("ExecuteScript ok = %+v", ok)
	}
	fail, err := env.svc.ExecuteScript(env.ctx, &bsvmspb.ExecuteScriptRequest{ScriptPubKey: []byte{0x00}})
	if err != nil {
		t.Fatal(err)
	}
	if fail.GetOk() || fail.GetError() == "" {
		t.Fatalf("ExecuteScript fail = %+v", fail)
	}
	if _, err := env.svc.ExecuteScript(env.ctx, &bsvmspb.ExecuteScriptRequest{RawTx: []byte{0x01}, ScriptPubKey: []byte{0x51}}); status.Code(err) != codes.Internal {
		t.Fatalf("bad script tx code = %v, want Internal", status.Code(err))
	}
}

func TestSpendingAndBroadcastHappyAndUnhappy(t *testing.T) {
	env := newTestEnv(t, true)
	createWallet(t, env, "tenant1", "wallet1")
	addr := mustNewAddress(t, env, "tenant1", "wallet1")
	script := mustP2PKHScript(t, env, addr, 200_000)
	for i := 0; i < 4; i++ {
		if _, err := env.svc.ImportUTXO(env.ctx, &bsvmspb.ImportUTXORequest{TenantId: "tenant1", WalletId: "wallet1", Txid: txidHex(10 + i), Vout: 0, Value: 200_000, Script: script, Height: 100, Force: true}); err != nil {
			t.Fatal(err)
		}
	}
	dest := mustNewAddress(t, env, "tenant1", "wallet1")
	sent, err := env.svc.Send(env.ctx, &bsvmspb.SendRequest{TenantId: "tenant1", WalletId: "wallet1", ToAddress: dest, AmountSatoshis: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if sent.GetTxid() == "" {
		t.Fatal("Send txid missing")
	}
	if _, err := env.svc.Send(env.ctx, &bsvmspb.SendRequest{TenantId: "tenant1", WalletId: "wallet1", ToAddress: dest, AmountSatoshis: -1}); status.Code(err) != codes.Internal {
		t.Fatalf("negative send code = %v, want Internal", status.Code(err))
	}
	detail, err := env.svc.SpendToOutputs(env.ctx, &bsvmspb.SpendToOutputsRequest{
		TenantId: "tenant1",
		WalletId: "wallet1",
		Outputs:  []*bsvmspb.OutputSpec{{Script: []byte{0x51}, Value: 1000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if detail.GetDetail().GetTxid() == "" || len(detail.GetDetail().GetSpentUtxos()) == 0 || len(detail.GetDetail().GetRawTx()) == 0 {
		t.Fatalf("SpendToOutputs detail = %+v", detail.GetDetail())
	}
	if _, err := env.svc.SpendToOutputs(env.ctx, &bsvmspb.SpendToOutputsRequest{TenantId: "tenant1", WalletId: "wallet1"}); status.Code(err) != codes.Internal {
		t.Fatalf("empty SpendToOutputs code = %v, want Internal", status.Code(err))
	}
	if _, err := env.svc.BroadcastCustomSpend(env.ctx, &bsvmspb.BroadcastCustomSpendRequest{
		Inputs:  []*bsvmspb.CustomInput{{Txid: txidHex(100), Vout: 0, ScriptSig: nil}},
		Outputs: []*bsvmspb.OutputSpec{{Script: []byte{0x51}, Value: 500}},
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("disabled BroadcastCustomSpend code = %v, want FailedPrecondition", status.Code(err))
	}
	customEnv := newTestEnvWithOptions(t, true, Options{EnableCustomSpend: true})
	custom, err := customEnv.svc.BroadcastCustomSpend(customEnv.ctx, &bsvmspb.BroadcastCustomSpendRequest{
		Inputs:  []*bsvmspb.CustomInput{{Txid: txidHex(100), Vout: 0, ScriptSig: nil}},
		Outputs: []*bsvmspb.OutputSpec{{Script: []byte{0x51}, Value: 500}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if custom.GetTxid() == "" {
		t.Fatal("BroadcastCustomSpend txid missing")
	}
	if _, err := customEnv.svc.BroadcastCustomSpend(customEnv.ctx, &bsvmspb.BroadcastCustomSpendRequest{Inputs: []*bsvmspb.CustomInput{{Txid: "bad"}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad custom txid code = %v, want InvalidArgument", status.Code(err))
	}
	bcast, err := env.svc.BroadcastRaw(env.ctx, &bsvmspb.BroadcastRawRequest{RawTx: mustHex(t, validTxHex)})
	if err != nil {
		t.Fatal(err)
	}
	if bcast.GetTxid() == "" {
		t.Fatal("BroadcastRaw txid missing")
	}
	if _, err := env.svc.BroadcastRaw(env.ctx, &bsvmspb.BroadcastRawRequest{RawTx: []byte{0x01}}); status.Code(err) != codes.Internal {
		t.Fatalf("bad broadcast code = %v, want Internal", status.Code(err))
	}
	pending, err := env.svc.PendingTransactions(env.ctx, &bsvmspb.PendingTransactionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.GetTransactions() == nil {
		t.Fatal("pending tx slice nil")
	}
	rebroadcast, err := env.svc.RebroadcastPendingTransactions(env.ctx, &bsvmspb.RebroadcastPendingTransactionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if rebroadcast.GetCount() != 0 {
		t.Fatalf("rebroadcast count = %d, want 0 with disabled p2p", rebroadcast.GetCount())
	}
}

func TestSendAllHappyAndUnhappy(t *testing.T) {
	env := newTestEnv(t, true)
	createWallet(t, env, "tenant1", "wallet1")
	dest := mustNewAddress(t, env, "tenant1", "wallet1")
	if _, err := env.svc.SendAll(env.ctx, &bsvmspb.SendAllRequest{TenantId: "tenant1", WalletId: "wallet1", ToAddress: dest}); status.Code(err) != codes.Internal {
		t.Fatalf("empty SendAll code = %v, want Internal", status.Code(err))
	}
	script := mustP2PKHScript(t, env, dest, 50_000)
	if _, err := env.svc.ImportUTXO(env.ctx, &bsvmspb.ImportUTXORequest{TenantId: "tenant1", WalletId: "wallet1", Txid: txidHex(200), Vout: 0, Value: 50_000, Script: script, Height: 100, Force: true}); err != nil {
		t.Fatal(err)
	}
	swept, err := env.svc.SendAll(env.ctx, &bsvmspb.SendAllRequest{TenantId: "tenant1", WalletId: "wallet1", ToAddress: dest})
	if err != nil {
		t.Fatal(err)
	}
	if swept.GetTxid() == "" {
		t.Fatal("SendAll txid missing")
	}
}

func TestEventStreamsHappyAndCancel(t *testing.T) {
	env := newTestEnv(t, false)

	txCtx, txCancel := context.WithCancel(env.ctx)
	txStream := newTestStream[bsvmspb.Transaction](txCtx, txCancel, 1)
	txDone := callStream(t, func() error { return env.svc.StreamTransactions(&bsvmspb.StreamTransactionsRequest{}, txStream) })
	waitSubscribers(t, env.svc.txBroker, 1)
	env.svc.txBroker.publish(&bsvmspb.Transaction{Txid: "tx1"})
	txDone()
	if got := txStream.items; len(got) != 1 || got[0].GetTxid() != "tx1" {
		t.Fatalf("tx stream = %+v", got)
	}

	payCtx, payCancel := context.WithCancel(env.ctx)
	payStream := newTestStream[bsvmspb.Payment](payCtx, payCancel, 1)
	payDone := callStream(t, func() error {
		return env.svc.StreamPayments(&bsvmspb.StreamPaymentsRequest{TenantId: "tenant1", WalletId: "wallet1"}, payStream)
	})
	waitSubscribers(t, env.svc.paymentBroker, 1)
	env.svc.paymentBroker.publish(&bsvmspb.Payment{TenantId: "tenant2", WalletId: "wallet1", Txid: "wrong"})
	env.svc.paymentBroker.publish(&bsvmspb.Payment{TenantId: "tenant1", WalletId: "wallet1", Txid: "right"})
	payDone()
	if got := payStream.items; len(got) != 1 || got[0].GetTxid() != "right" {
		t.Fatalf("payment stream = %+v", got)
	}

	blockCtx, blockCancel := context.WithCancel(env.ctx)
	blockStream := newTestStream[bsvmspb.Block](blockCtx, blockCancel, 1)
	blockDone := callStream(t, func() error { return env.svc.StreamBlocks(&bsvmspb.StreamBlocksRequest{}, blockStream) })
	waitSubscribers(t, env.svc.blockBroker, 1)
	env.svc.blockBroker.publish(&bsvmspb.Block{Hash: "block1"})
	blockDone()
	if len(blockStream.items) != 1 {
		t.Fatalf("block stream len = %d", len(blockStream.items))
	}

	wtxCtx, wtxCancel := context.WithCancel(env.ctx)
	wtxStream := newTestStream[bsvmspb.WalletTransaction](wtxCtx, wtxCancel, 1)
	wtxDone := callStream(t, func() error {
		return env.svc.StreamWalletTransactions(&bsvmspb.StreamWalletTransactionsRequest{TenantId: "tenant1"}, wtxStream)
	})
	waitSubscribers(t, env.svc.walletTxBroker, 1)
	env.svc.walletTxBroker.publish(&bsvmspb.WalletTransaction{TenantId: "tenant1", WalletId: "wallet2"})
	wtxDone()
	if len(wtxStream.items) != 1 {
		t.Fatalf("wallet tx stream len = %d", len(wtxStream.items))
	}

	trafficCtx, trafficCancel := context.WithCancel(env.ctx)
	trafficStream := newTestStream[bsvmspb.P2PTraffic](trafficCtx, trafficCancel, 1)
	trafficDone := callStream(t, func() error { return env.svc.StreamP2PTraffic(&bsvmspb.StreamP2PTrafficRequest{}, trafficStream) })
	waitSubscribers(t, env.svc.trafficBroker, 1)
	env.svc.trafficBroker.publish(&bsvmspb.P2PTraffic{Peer: "peer1"})
	trafficDone()
	if len(trafficStream.items) != 1 {
		t.Fatalf("traffic stream len = %d", len(trafficStream.items))
	}

	rejectCtx, rejectCancel := context.WithCancel(env.ctx)
	rejectStream := newTestStream[bsvmspb.Reject](rejectCtx, rejectCancel, 1)
	rejectDone := callStream(t, func() error { return env.svc.StreamRejects(&bsvmspb.StreamRejectsRequest{}, rejectStream) })
	waitSubscribers(t, env.svc.rejectBroker, 1)
	env.svc.rejectBroker.publish(&bsvmspb.Reject{Reason: "bad"})
	rejectDone()
	if len(rejectStream.items) != 1 {
		t.Fatalf("reject stream len = %d", len(rejectStream.items))
	}
}

func TestTenantIsolationAndPrivateMaterialExposure(t *testing.T) {
	env := newTestEnv(t, true)
	alice := createWallet(t, env, "alice", "main")
	bob := createWallet(t, env, "bob", "main")
	aliceAddr := mustNewAddress(t, env, "alice", "main")
	bobAddr := mustNewAddress(t, env, "bob", "main")
	if aliceAddr == bobAddr {
		t.Fatal("tenant addresses equal")
	}
	aliceList, err := env.svc.ListWallets(env.ctx, &bsvmspb.ListWalletsRequest{TenantId: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceList.GetWallets()) != 1 || aliceList.GetWallets()[0].GetTenantId() != "alice" {
		t.Fatalf("alice list = %+v", aliceList.GetWallets())
	}
	bobAsAlice, err := env.svc.GetWallet(env.ctx, &bsvmspb.GetWalletRequest{TenantId: "alice", WalletId: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(bobAsAlice.GetWallet().GetAddresses(), bobAddr) {
		t.Fatalf("alice response contains bob address")
	}
	if strings.Contains(fmt.Sprintf("%+v", bobAsAlice), alice.GetMnemonic()) || strings.Contains(fmt.Sprintf("%+v", bobAsAlice), bob.GetMnemonic()) {
		t.Fatalf("wallet response exposes mnemonic/private material")
	}
	info, err := os.Stat(env.svc.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %o, want 0600", info.Mode().Perm())
	}

	bobScript := mustP2PKHScript(t, env, bobAddr, 100_000)
	if _, err := env.svc.ImportUTXO(env.ctx, &bsvmspb.ImportUTXORequest{TenantId: "bob", WalletId: "main", Txid: txidHex(500), Vout: 0, Value: 100_000, Script: bobScript, Height: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Send(env.ctx, &bsvmspb.SendRequest{TenantId: "alice", WalletId: "main", ToAddress: aliceAddr, AmountSatoshis: 1000}); status.Code(err) != codes.Internal {
		t.Fatalf("alice spend bob funds code = %v, want Internal", status.Code(err))
	}
}

func TestStateEncryptsMnemonicAtRestAndReloads(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	key := bytesOf('k', 32)
	node := bsv.New(bsv.Regtest)
	svc, err := NewWithOptions(ctx, node, dir, Options{DataKey: key})
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.CreateWallet(ctx, &bsvmspb.CreateWalletRequest{TenantId: "tenant1", WalletId: "wallet1"})
	if err != nil {
		t.Fatal(err)
	}
	addr := mustNewAddress(t, &testEnv{ctx: ctx, svc: svc}, "tenant1", "wallet1")
	svc.Close()

	raw, err := os.ReadFile(filepath.Join(dir, "bsvms-wallets.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), created.GetMnemonic()) {
		t.Fatalf("state file contains plaintext mnemonic")
	}
	if !strings.Contains(string(raw), "mnemonic_ciphertext") || strings.Contains(string(raw), `"mnemonic"`) {
		t.Fatalf("state file not encrypted shape: %s", raw)
	}
	node2 := bsv.New(bsv.Regtest)
	svc2, err := NewWithOptions(ctx, node2, dir, Options{DataKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	list, err := svc2.ListWallets(ctx, &bsvmspb.ListWalletsRequest{TenantId: "tenant1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetWallets()) != 1 || !containsString(list.GetWallets()[0].GetAddresses(), addr) {
		t.Fatalf("reload wallets = %+v, want address %s", list.GetWallets(), addr)
	}
}

func TestJWTAuthFlowOptionalBootstrapRefreshAndTenantScope(t *testing.T) {
	ctx := context.Background()
	noAuth := newTestEnv(t, false)
	if err := noAuth.svc.authorize(ctx, "/bsvms.v1.BSVMS/GetWallet", &bsvmspb.GetWalletRequest{TenantId: "tenant1", WalletId: "wallet1"}); err != nil {
		t.Fatalf("auth disabled authorize: %v", err)
	}

	dir := t.TempDir()
	svc, err := NewWithOptions(ctx, bsv.New(bsv.Regtest), dir, Options{
		AuthEnabled: true,
		JWTSecret:   bytesOf('j', 32),
		DataKey:     bytesOf('d', 32),
		AccessTTL:   time.Hour,
		RefreshTTL:  2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	createReq := &bsvmspb.CreateWalletRequest{TenantId: "tenant1", WalletId: "wallet1"}
	if err := svc.authorize(ctx, "/bsvms.v1.BSVMS/CreateWallet", createReq); err != nil {
		t.Fatalf("bootstrap create authorize: %v", err)
	}
	created, err := svc.CreateWallet(ctx, createReq)
	if err != nil {
		t.Fatal(err)
	}
	if created.GetTokens().GetAccessToken() == "" || created.GetTokens().GetRefreshToken() == "" || created.GetTokens().GetTenantId() != "tenant1" || created.GetTokens().GetWalletId() != "wallet1" {
		t.Fatalf("tokens = %+v", created.GetTokens())
	}
	if err := svc.authorize(ctx, "/bsvms.v1.BSVMS/GetWallet", &bsvmspb.GetWalletRequest{TenantId: "tenant1", WalletId: "wallet1"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing token code = %v, want Unauthenticated", status.Code(err))
	}
	accessCtx := authContext(created.GetTokens().GetAccessToken())
	if err := svc.authorize(accessCtx, "/bsvms.v1.BSVMS/GetWallet", &bsvmspb.GetWalletRequest{TenantId: "tenant1", WalletId: "wallet1"}); err != nil {
		t.Fatalf("same wallet auth: %v", err)
	}
	if err := svc.authorize(accessCtx, "/bsvms.v1.BSVMS/GetWallet", &bsvmspb.GetWalletRequest{TenantId: "tenant1", WalletId: "wallet2"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wallet mismatch code = %v, want PermissionDenied", status.Code(err))
	}
	if err := svc.authorize(accessCtx, "/bsvms.v1.BSVMS/GetWallet", &bsvmspb.GetWalletRequest{TenantId: "tenant2", WalletId: "wallet1"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("tenant mismatch code = %v, want PermissionDenied", status.Code(err))
	}
	refreshCtx := authContext(created.GetTokens().GetRefreshToken())
	if err := svc.authorize(refreshCtx, "/bsvms.v1.BSVMS/GetWallet", &bsvmspb.GetWalletRequest{TenantId: "tenant1", WalletId: "wallet1"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("refresh-as-access code = %v, want Unauthenticated", status.Code(err))
	}
	refreshed, err := svc.RefreshToken(ctx, &bsvmspb.RefreshTokenRequest{RefreshToken: created.GetTokens().GetRefreshToken()})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.GetAccessToken() == "" || refreshed.GetAccessToken() == created.GetTokens().GetAccessToken() {
		t.Fatalf("refreshed token invalid")
	}
	if _, err := svc.RefreshToken(ctx, &bsvmspb.RefreshTokenRequest{RefreshToken: created.GetTokens().GetAccessToken()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("access-as-refresh code = %v, want Unauthenticated", status.Code(err))
	}
	if err := svc.authorize(ctx, "/bsvms.v1.BSVMS/CreateWallet", createReq); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("existing bootstrap without token code = %v, want Unauthenticated", status.Code(err))
	}
}

func createWallet(t *testing.T, env *testEnv, tenantID, walletID string) *bsvmspb.CreateWalletResponse {
	t.Helper()
	created, err := env.svc.CreateWallet(env.ctx, &bsvmspb.CreateWalletRequest{TenantId: tenantID, WalletId: walletID})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetMnemonic() == "" {
		t.Fatal("mnemonic missing")
	}
	return created
}

func mustNewAddress(t *testing.T, env *testEnv, tenantID, walletID string) string {
	t.Helper()
	addr, err := env.svc.NewAddress(env.ctx, &bsvmspb.NewAddressRequest{TenantId: tenantID, WalletId: walletID})
	if err != nil {
		t.Fatal(err)
	}
	if addr.GetAddress().GetAddress() == "" {
		t.Fatal("address missing")
	}
	return addr.GetAddress().GetAddress()
}

func mustP2PKHScript(t *testing.T, env *testEnv, address string, value int64) []byte {
	t.Helper()
	out, err := env.svc.P2PKHOutput(env.ctx, &bsvmspb.P2PKHOutputRequest{Address: address, Value: value})
	if err != nil {
		t.Fatal(err)
	}
	return out.GetScript()
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func txidHex(n int) string {
	return fmt.Sprintf("%064x", n)
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

type testStream[T any] struct {
	ctx       context.Context
	cancel    context.CancelFunc
	stopAfter int
	items     []*T
}

func newTestStream[T any](ctx context.Context, cancel context.CancelFunc, stopAfter int) *testStream[T] {
	return &testStream[T]{ctx: ctx, cancel: cancel, stopAfter: stopAfter}
}

func (s *testStream[T]) Send(item *T) error {
	s.items = append(s.items, item)
	if s.stopAfter > 0 && len(s.items) >= s.stopAfter {
		s.cancel()
	}
	return nil
}

func (s *testStream[T]) SetHeader(metadata.MD) error  { return nil }
func (s *testStream[T]) SendHeader(metadata.MD) error { return nil }
func (s *testStream[T]) SetTrailer(metadata.MD)       {}
func (s *testStream[T]) Context() context.Context     { return s.ctx }
func (s *testStream[T]) SendMsg(any) error            { return nil }
func (s *testStream[T]) RecvMsg(any) error            { return errors.New("not implemented") }

func callStream(t *testing.T, fn func() error) func() {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	return func() {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("stream error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("stream did not stop")
		}
	}
}

func waitSubscribers[T any](t *testing.T, b *broker[T], want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		got := len(b.subs)
		b.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("subscribers did not reach %d", want)
}
