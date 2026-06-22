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
