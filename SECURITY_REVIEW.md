# Security Review

Scope: bsvms gRPC surface in `proto/bsvms/v1/bsvms.proto`, implementation in `internal/service`, SDK treated as dependency.

## Findings

### Resolved For Opt-In Mode: JWT authentication and tenant authorization

JWT auth is optional because bsvms is intended for local deployment. When enabled, service issues HS256 access/refresh tokens scoped to `(tenant_id, wallet_id)`.

Current behavior:

- Auth off: local trusted mode. Tenant fields are routing only, not security boundary.
- Auth on: first `CreateWallet` or `RestoreWallet` for a new wallet can bootstrap without token and returns access/refresh tokens.
- Existing tenant wallet operations require bearer access token matching tenant and wallet.
- `RefreshToken` accepts refresh token and returns new token pair.

Residual risks:

- Bootstrap is open by design; any local client can create a new tenant wallet when auth is on.
- HS256 secret stored locally when not provided. If data directory leaks, attacker can mint tokens.
- No refresh-token revocation list yet. Rotate `jwt.secret` to invalidate all tokens.
- Admin/global RPCs and tenantless `BroadcastCustomSpend` need policy work before public/network exposure.

### Resolved With Caveat: mnemonic encrypted at rest

`bsvms-wallets.json` stores AES-256-GCM ciphertext for mnemonics/passphrases. If `BSVMS_DATA_KEY` is absent, service creates local `data.key` with mode `0600`.

Residual risks:

- Local generated data key protects against accidental metadata-file disclosure, not full data-directory compromise.
- Production deployments should provide `BSVMS_DATA_KEY` from KMS/HSM/secret manager and exclude local key files from backups.

### High: `CreateWallet` returns mnemonic

`CreateWallet` returns mnemonic once. This is operationally useful, but clients, proxies, traces, and logs can capture it.

Required fix:

- Keep return only for development or explicit export flow.
- Prefer server-owned key custody with no mnemonic returned.
- Redact request/response logging for seed-bearing RPCs.

### Reduced: `BroadcastCustomSpend` is global and tenantless, disabled by default

`BroadcastCustomSpend` takes arbitrary inputs and scripts, then broadcasts. It does not spend tenant wallet UTXOs through bsvms signing, but it can relay any caller-provided transaction.

Impact:

- Service can be abused as public transaction relay.
- No tenant-level accounting or policy applies.

Current control:

- Runtime disabled by default.
- Enable with `--enable-custom-spend` / `BSVMS_ENABLE_CUSTOM_SPEND=true`.

Required fix before broad use:

- Add tenant context and policy check.
- Consider disabling by default or requiring admin scope.

### Medium: streaming APIs can leak metadata when auth is off

When auth is enabled, tenant event streams require matching tenant/wallet token. When auth is off, empty filter streams all tenant events by local-trusted-mode design.

Required fix:

- Apply same tenant authorization to streams.
- Remove cross-tenant stream mode unless caller has admin scope.

### Medium: no transport security by default

CLI starts plain gRPC. That is acceptable for localhost dev, unsafe on shared networks.

Required fix:

- Support TLS/mTLS.
- Bind default to `127.0.0.1:50051` or require explicit `--addr :50051` for public listen.

## Current Positive Controls

- Service never imports SDK internal packages.
- Tenant wallet keys use `(tenant_id, wallet_id)` map keys and hashed storage filenames, avoiding path traversal.
- ID validation rejects slashes and path-like tenant/wallet IDs.
- Wallet response DTOs do not include mnemonic, private keys, or seed bytes.
- Address and pubkey endpoints expose public key material only.
- Unit tests cover auth bootstrap, refresh, cross-tenant/wallet denial, encrypted state, accidental isolation, and response-level mnemonic leakage.

## Production Gate

Do not expose bsvms outside local trusted development unless auth is enabled, TLS/mTLS is configured at deployment edge, `BSVMS_DATA_KEY` and `BSVMS_JWT_SECRET` come from a real secret manager, and `BroadcastCustomSpend` remains disabled or gets admin policy.
