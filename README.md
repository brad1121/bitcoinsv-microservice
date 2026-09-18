# bsvms

Bitcoin SV micro-service exposing `bitcoinsv-sdk-go` over gRPC.

Default run is mainnet, `:50051`, data in `./data`:

```sh
go run ./cmd/bsvms
```

Useful local smoke run:

```sh
go run ./cmd/bsvms -network regtest -connect=false -addr 127.0.0.1:50051 -data-dir /tmp/bsvms
grpcurl -plaintext -d '{}' 127.0.0.1:50051 bsvms.v1.BSVMS/Status
```

Config:

```sh
BSVMS_ADDR=:50051
BSVMS_NETWORK=mainnet
BSVMS_DATA_DIR=data
BSVMS_CONNECT=true
BSVMS_PEERS=
BSVMS_AUTH=false
BSVMS_ENABLE_CUSTOM_SPEND=false
BSVMS_JWT_SECRET=
BSVMS_DATA_KEY=
BSVMS_BROADCAST_FANOUT=0
BSVMS_NO_PENDING_TX_TRACKING=false
```

Tenant isolation is request-scoped via `tenant_id` plus `wallet_id`. bsvms stores encrypted wallet metadata in `bsvms-wallets.json`, wallet state in per-wallet SQLite files, and SDK address snapshots in the data directory. Keep data directory private. If `BSVMS_DATA_KEY` is absent, bsvms creates `data.key` with mode `0600`.

Optional JWT auth:

```sh
go run ./cmd/bsvms -auth
```

With auth on, first `CreateWallet` or `RestoreWallet` for a new `(tenant_id, wallet_id)` may be unauthenticated and returns `tokens`. After that every RPC requires `Authorization: Bearer <access_token>` — node-scoped calls such as `Status` included — and tenant-scoped calls are additionally checked against the token's tenant and wallet. `RefreshToken` is the one exception, and rotates the token pair from `refresh_token`. If `BSVMS_JWT_SECRET` is absent, bsvms creates `jwt.secret` with mode `0600`.

`BroadcastCustomSpend` is disabled by default. Enable only when you need caller-supplied custom script spends:

```sh
go run ./cmd/bsvms -enable-custom-spend
```

## Balances and coinbase maturity

`Balance` returns `satoshis` (everything the wallet holds), `spendable`, and
`immature`. A coinbase output counts towards `satoshis` from the moment it is
seen but cannot be spent until `coinbase_maturity` confirmations have passed
(`Status` reports the figure). Size a payment against `spendable`, not
`satoshis`. `ListUTXOs` marks each such output with `is_coinbase`, and
`ImportUTXO` accepts the same flag alongside `force` — restoring a coinbase
from your own records without it puts an unspendable coin straight back into
coin selection.

## Reorgs

`StreamBlocks` carries abandoned blocks as well as new ones. A message with
`disconnected` set names a block the chain dropped in a reorganisation, emitted
deepest first before its replacement arrives; `txids` and `spends` are empty on
those. Wallets have already demoted their transactions from that block to
unconfirmed, and a consumer keeping its own mined index should do the same.

## Delivery verification

A peer never announces a transaction back to whoever sent it, so a broadcast is
only checkable when some connected peer was left out of it. Start with
`BSVMS_BROADCAST_FANOUT=1`, then use `WaitForTxRelay` to wait for a peer that
did not receive the tx from us to announce it back, or `VerifyTxSeen` to ask
peers directly. Both report `false` rather than an error when nothing answers
before the timeout.

`BSVMS_NO_PENDING_TX_TRACKING=true` stops the node holding every locally
broadcast transaction in memory until a block confirms it. Turn it on only if
you persist and rebroadcast transactions yourself — `PendingTransactions` and
`RebroadcastPendingTransactions` report nothing once it is off.

## Stuck transactions

`TxState` reports the store's view of a txid: `confirmed`, the `height` it
confirmed at, and `conflicted` — meaning it can no longer confirm, because a
confirmed transaction took one of its inputs, an ancestor is conflicted, or it
was abandoned. `AbandonTransaction` gives up on an unconfirmed transaction of
ours: the coins it spent return to the balance and the node stops announcing
it. Use it for a transaction the chain will never rule on, such as one whose
parent was never relayed — peers hold that as an orphan without ever sending a
reject, so nothing else will clear it.

## Rescan and crash recovery

`GetIncompleteCursor` reports a block the wallet started applying but never
finished — how a crash mid-block shows up. Call it at startup; when it returns
`found`, rescan from `height - 1`.

`Rescan` walks forward from a block hash, pulling every block in full over P2P
and replaying its transactions through the wallet. It is a recovery path, not a
hot path: on mainnet a few thousand blocks can be hundreds of MB and minutes of
wall time. It streams `RescanEvent` — progress heartbeats, then exactly one
`stats` message at the end. Cancelling the stream stops the walk; blocks already
replayed stay applied. One rescan runs per wallet at a time, and a second
returns `FailedPrecondition`.

Set `start_height` to the real height of `start_block_hash` whenever you know
it. Without it every replayed transaction is stamped unconfirmed, and spends
already mined deep in the chain are not reconciled by the walk itself.

```sh
grpcurl -plaintext -d '{"tenant_id":"t1","wallet_id":"w1","start_block_hash":"<hash>","start_height":800000,"max_blocks":2000}' \
  127.0.0.1:50051 bsvms.v1.BSVMS/Rescan
```

## Authorization model

With auth on, authorization is closed by default. A request carrying
`tenant_id` is checked against the token's tenant and wallet; an empty
`tenant_id` is rejected rather than treated as a wildcard. Requests with no
`tenant_id` are only accepted for the RPCs on the node-scoped list in
`internal/service/auth.go`, so an RPC added later is refused until it is
classified deliberately. Streaming RPCs are checked against the request message
itself, not just the connection.

Services other than `bsvms.v1.BSVMS` on the same server — notably gRPC server
reflection — still require a valid token but are not tenant-scoped. Reflection
therefore needs one too, so the first `CreateWallet` against an auth-enabled
server has to be made from the proto file rather than by reflection:

```sh
grpcurl -plaintext -import-path proto -proto bsvms/v1/bsvms.proto \
  -d '{"tenant_id":"t1","wallet_id":"w1"}' 127.0.0.1:50051 bsvms.v1.BSVMS/CreateWallet
```

API contract lives in [proto/bsvms/v1/bsvms.proto](proto/bsvms/v1/bsvms.proto).
For app patterns and what the service provides, see
[docs/building-apps.md](docs/building-apps.md).

## Docker Compose Blackjack Demo

Regtest BSV blackjack with on-chain settlement. Each hand result is committed to
an OP_RETURN output visible on the local explorer at `http://localhost:3002`.

Quick start:

```sh
docker compose up -d
docker compose run --rm blackjack
```

Pulls pre-built images from GHCR and Docker Hub. No SDK checkout required.

For a detailed walkthrough of the architecture, startup flow, settlement
mechanics, OP_RETURN format, env vars, and gRPC endpoints, see
[docs/blackjack-demo.md](docs/blackjack-demo.md).

### Local Development

Build from source (requires `../bitcoinsv-sdk-go`):

```sh
docker compose -f docker-compose.yml -f docker-compose.dev.yml build
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d
docker compose -f docker-compose.yml -f docker-compose.dev.yml run --rm blackjack
```

### Options

```sh
BSVMS_IMAGE=ghcr.io/brad1121/bitcoinsv-microservice:v0.1.0 docker compose run --rm blackjack
BSV_NODE_IMAGE=your/image:tag docker compose up -d
MINE_INTERVAL_SECONDS=300 docker compose up -d
INITIAL_BLOCK_HEIGHT=10001 docker compose up -d
```

## Releases

Releases are published by GitHub Actions from semantic version tags:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The release pipeline builds `bsvms` and `blackjack` for Linux, macOS, and Windows, then attaches archives to the GitHub release.
It also publishes the container image to `ghcr.io/brad1121/bitcoinsv-microservice` with both the semver tag and `latest`.

Private SDK access:

- Add a read-only deploy key to `brad1121/bitcoinsv-sdk-go`.
- Add the matching private key as this repository secret: `BSV_SDK_DEPLOY_KEY`.
