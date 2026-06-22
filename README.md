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
BSVMS_AUTH=false
BSVMS_ENABLE_CUSTOM_SPEND=false
BSVMS_JWT_SECRET=
BSVMS_DATA_KEY=
```

Tenant isolation is request-scoped via `tenant_id` plus `wallet_id`. bsvms stores encrypted wallet metadata in `bsvms-wallets.json`, wallet state in per-wallet SQLite files, and SDK address snapshots in the data directory. Keep data directory private. If `BSVMS_DATA_KEY` is absent, bsvms creates `data.key` with mode `0600`.

Optional JWT auth:

```sh
go run ./cmd/bsvms -auth
```

With auth on, first `CreateWallet` or `RestoreWallet` for a new `(tenant_id, wallet_id)` may be unauthenticated and returns `tokens`. After that, tenant wallet RPCs require `Authorization: Bearer <access_token>`. Use `RefreshToken` with `refresh_token` to rotate token pair. If `BSVMS_JWT_SECRET` is absent, bsvms creates `jwt.secret` with mode `0600`.

`BroadcastCustomSpend` is disabled by default. Enable only when you need caller-supplied custom script spends:

```sh
go run ./cmd/bsvms -enable-custom-spend
```

API contract lives in [proto/bsvms/v1/bsvms.proto](proto/bsvms/v1/bsvms.proto).

## Docker Compose Blackjack Demo

Start regtest node and bsvms:

```sh
docker compose up -d
```

Play blackjack in an interactive console:

```sh
docker compose run --rm blackjack
```

The blackjack service is profile-gated, so plain `docker compose up` starts the node, bsvms, local explorer, and a one-shot `blackjack-fund` container. `docker compose run --rm blackjack` starts the interactive game. This pulls the public bsvms image from GHCR and explorer image from Docker Hub. No SDK checkout is required.

The regtest node mines to height `10001` at startup so coinbase funds are mature and Genesis rules are active, then `blackjack-fund` funds the bsvms blackjack wallets with real on-chain transactions. After that, blackjack settlements are sent through bsvms P2P broadcast. The node mines one new block every minute by default so settlement transactions confirm automatically. The local WhatsOnChain explorer at `http://localhost:3002` shows wallet addresses and settlement transactions.

Compose services:

- `bsv-node`: local BSV regtest P2P node on `18444`
- `bsvms`: gRPC service on `50051`, connected to `bsv-node:18444`
- `woc-explorer`: local explorer on `3002`, using `brad1121/woc-explorer`, connected to `bsv-node` RPC
- `blackjack-fund`: one-shot startup funder for blackjack wallets using node RPC only for initial regtest funding
- `blackjack`: console game using bsvms wallets and settlement transactions

### Local Development

Build and run from source (requires `../bitcoinsv-sdk-go`):

```sh
docker compose -f docker-compose.yml -f docker-compose.dev.yml build
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d
docker compose run --rm blackjack
```

### Options

Pin a release image:

```sh
BSVMS_IMAGE=ghcr.io/brad1121/bitcoinsv-microservice:v0.1.0 docker compose run --rm blackjack
```

Override BSV node image:

```sh
BSV_NODE_IMAGE=your/image:tag docker compose up -d
```

Override mining interval (default 60s):

```sh
MINE_INTERVAL_SECONDS=300 docker compose up -d
```

Override startup chain height (default 10001):

```sh
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
