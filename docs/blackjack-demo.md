# Blackjack Demo

Regtest BSV blackjack with on-chain settlement via bsvms gRPC. Each hand result is
committed to an OP_RETURN output visible on the local explorer.

## Architecture

```
                    ┌──────────────────────────────────┐
                    │         woc-explorer             │
                    │  (WhatsOnChain, local regtest)   │
                    │  http://localhost:3002            │
                    └──────────┬───────────────────────┘
                               │ RPC (18443)
    ┌──────────────────────────┼───────────────────────────┐
    │                     bsv-node                         │
    │  bitcoind regtest, txindex=1, periodic mining        │
    │  P2P :18444, RPC :18443                              │
    └─────┬────────────────────────────────────────────────┘
          │ P2P :18444
    ┌─────┴──────────────────┐
    │         bsvms           │
    │  bitcoinsv-sdk-go over  │
    │  gRPC :50051            │
    └─────┬───────────────────┘
          │ gRPC :50051
    ┌─────┴───────────────────┐
    │       blackjack          │
    │  Interactive console     │
    │  game client             │
    └─────────────────────────┘

    One-shot:
    ┌─────────────────────────────────┐
    │        blackjack-fund            │
    │  Funds wallets from node RPC     │
    │  coinbase via sendtoaddress      │
    └─────────────────────────────────┘
```

## Services

| Service | Image | Role |
|---|---|---|
| `bsv-node` | `bitcoinsv/bitcoin-sv` | Regtest P2P node with txindex, periodic mining |
| `bsvms` | `ghcr.io/brad1121/bitcoinsv-microservice` | gRPC microservice wrapping bitcoinsv-sdk-go |
| `woc-explorer` | `brad1121/woc-explorer` | Local block explorer (fork of WhatsOnChain) |
| `blackjack-fund` | same as bsvms | One-shot funder: sends coinbase BSV to game wallets |
| `blackjack` | same as bsvms | Interactive console blackjack (profile-gated) |

## Startup Flow

1. **bsv-node starts** – runs `/entrypoint.sh bitcoind` with regtest flags, then a
   background `mine_loop` function mines to `INITIAL_BLOCK_HEIGHT` (default 10001)
   at startup, then mines 1 block every `MINE_INTERVAL_SECONDS` (default 20s).

   Muting to 10001 satisfies two requirements:

   - **Coinbase maturity** (100 blocks) – coinbase outputs become spendable.
   - **Genesis activation** (height 10001) – BSV Genesis consensus rules active.

2. **woc-explorer starts** – connects to `bsv-node:18443` RPC via credentials in
   `docker/woc-explorer/credentials.js`. Serves block explorer at port 3002.

3. **bsvms starts** – connects to `bsv-node:18444` P2P. Registers the peer and
   syncs chain state. Exposes gRPC on port 50051 with tenant/wallet isolation.

4. **blackjack-fund runs** (one-shot, auto-starts with `docker compose up -d`):

   - Clears stale off-chain UTXOs from prior runs (uses `ClearUTXOs` after
     verifying UTXOs are gone from the node via `gettxout`).
   - Waits until the node RPC has enough spendable coinbase balance.
   - Sends `sendtoaddress` to each game wallet address (250000 sats each).
   - Mines 1 block to confirm the funding txs.
   - Calls `ProcessRawTx` on bsvms to import each funding tx into wallet state,
     making UTXOs visible to the game.

   `blackjack` depends on `blackjack-fund: service_completed_successfully`, so
   the game only starts after wallets are funded.

## Playing a Hand

Run `docker compose run --rm blackjack` (or `docker compose -f docker-compose.yml
-f docker-compose.dev.yml run --rm blackjack` for local build).

1. `main()` connects to bsvms gRPC, ensures both `player` and `house` wallets
   exist (using `CreateWallet` and `NewAddress`), waits for a P2P peer if on
   regtest, and verifies wallets are funded via `Balance`.

2. Game loop:

   - Player bets sats (default 1000).
   - Two cards dealt to each side from a shuffled 52-card deck.
   - Player hits or stands.
   - House draws to 17.
   - Winner determined.

3. On non-push outcome, `settleHand()` is called:

   - Calls `P2PKHOutput(to, sats)` on bsvms to get a P2PKH output spec for
     the winner's address.
   - Builds a `handResult` struct with game metadata, card strings, totals,
     bet, and winner.
   - JSON-marshals it and calls `OpReturnOutput(data)` to get a zero-value
     OP_RETURN output spec (`OP_FALSE OP_RETURN <json_bytes>`).
   - Calls `SpendToOutputs(from, [P2PKH, OP_RETURN])` on bsvms.
   - bsvms selects coin from the loser's wallet, builds a transaction with
     both outputs (+ change), signs it, and broadcasts P2P.
   - Returns the txid on success, with retry for "no connected peers".

4. Settlement tx is visible on explorer at `http://localhost:3002/tx/<txid>`.
   Output #2 is the OP_RETURN (0 sats, type `nulldata`). Click the Scripts tab
   to see the decoded hand result.

## OP_RETURN Format

```json
{
  "game": "blackjack",
  "player": ["AS", "KH"],
  "house": ["QD", "7C", "5H"],
  "player_total": 21,
  "house_total": 22,
  "bet": 1000,
  "winner": "player"
}
```

The script is `OP_0 OP_RETURN <pushdata(json_encoded)>` -- 0 value, unspendable,
purely data. The explorer decodes it as type `nulldata` with the hex payload.

## Modes

### Default (public) — GHCR image

```sh
docker compose up -d
docker compose run --rm blackjack
```

Pulls pre-built `ghcr.io/brad1121/bitcoinsv-microservice:latest`. No SDK
checkout needed. Everything works out of the box.

### Dev — local build with SDK override

```sh
docker compose -f docker-compose.yml -f docker-compose.dev.yml build
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d
docker compose -f docker-compose.yml -f docker-compose.dev.yml run --rm blackjack
```

Overrides bsvms / blackjack-fund / blackjack to use `bsvms:dev` image, built
from source with `additional_contexts: sdk: ../bitcoinsv-sdk-go`. Requires a
local checkout of `bitcoinsv-sdk-go` at `../bitcoinsv-sdk-go`.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `BSVMS_IMAGE` | `ghcr.io/brad1121/bitcoinsv-microservice:latest` | Container image for bsvms and blackjack services |
| `BSV_NODE_IMAGE` | `bitcoinsv/bitcoin-sv:latest` | Container image for regtest node |
| `WOC_EXPLORER_IMAGE` | `brad1121/woc-explorer:latest` | Container image for explorer |
| `INITIAL_BLOCK_HEIGHT` | `10001` | Height to mine at startup (Genesis + maturity) |
| `MINE_INTERVAL_SECONDS` | `20` | Seconds between auto-mined blocks during game |
| `BSVMS_ADDR` | `bsvms:50051` | gRPC address for blackjack clients |
| `BSVMS_REGTEST_PEER` | `bsv-node:18444` | P2P address for regtest peer connection |
| `WOC_EXPLORER_URL` | `http://localhost:3002` | Explorer URL for tx/address links |

## Key gRPC Endpoints

See `proto/bsvms/v1/bsvms.proto` for the full API.

| RPC | Request | Response | Purpose |
|---|---|---|---|
| `CreateWallet` | `(tenant_id, wallet_id)` | `Wallet + mnemonic` | Create a new HD wallet |
| `NewAddress` | `(tenant_id, wallet_id)` | `DerivedAddress` | Derive next external address |
| `Balance` | `(tenant_id, wallet_id)` | `(satoshis, bsv)` | Wallet spendable balance |
| `P2PKHOutput` | `(address, value)` | `OutputSpec` | Build P2PKH output script |
| `OpReturnOutput` | `(data)` | `OutputSpec` | Build OP_RETURN output script |
| `SpendToOutputs` | `(tenant_id, wallet_id, outputs[])` | `SpendDetail` | Build, sign, broadcast tx |
| `ProcessRawTx` | `(tenant_id, wallet_id, raw_tx, height)` | — | Import an external tx into wallet state |
| `ConnectPeer` | `(address)` | — | Connect to a P2P peer |
| `Status` | — | `(network, peer_count, etc.)` | Node status |

## Files

| Path | Purpose |
|---|---|
| `docker-compose.yml` | Service definitions, GHCR images, env vars, volumes |
| `docker-compose.dev.yml` | Dev override — local build with SDK context |
| `cmd/blackjack/main.go` | Game client, settleHand, fundMain, node RPC helpers |
| `docker/bsv-node/start.sh` | Node entrypoint — bitcoind with mine_loop |
| `docker/woc-explorer/credentials.js` | Explorer RPC credentials for regtest |
| `internal/service/service.go` | gRPC handler implementations |
| `proto/bsvms/v1/bsvms.proto` | Full gRPC API contract |
