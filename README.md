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
