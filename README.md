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

Start regtest node, bsvms, and console blackjack:

```sh
docker compose run --rm blackjack
```

This pulls the public bsvms image from GHCR. No SDK checkout is required.

Pin a release image:

```sh
BSVMS_IMAGE=ghcr.io/brad1121/bitcoinsv-microservice:v0.1.0 docker compose run --rm blackjack
```

Compose services:

- `bsv-node`: local BSV regtest P2P node on `18444`
- `bsvms`: gRPC service on `50051`, connected to `bsv-node:18444`
- `blackjack`: console game using bsvms wallets and settlement transactions

If your BSV node image differs, override:

```sh
BSV_NODE_IMAGE=your/image:tag docker compose run --rm blackjack
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
