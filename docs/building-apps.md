# Building Apps with bsvms

bsvms wraps `bitcoinsv-sdk-go` behind a gRPC API, so applications can use BSV
blockchain features without managing private keys, building transactions, or
depending on a wallet SDK directly.

## What bsvms Provides

- **HD wallet management** — Create, restore, and derive addresses per
  `(tenant_id, wallet_id)`. Private keys never leave the service.
- **Transaction building** — `Send`, `SendAll`, `SpendToOutputs` with arbitrary
  output types (P2PKH, OP_RETURN, anyone-can-spend). Coin selection, signing,
  and P2P broadcast handled internally.
- **UTXO tracking** — `Balance`, `ListUTXOs`, `ProcessRawTx` for importing
  external transactions into wallet state (e.g., funding from an exchange or
  coinbase).
- **Streaming** — Real-time streams for transactions, blocks, payments, and P2P
  traffic. Useful for monitoring, indexing, or triggering downstream logic.
- **Script evaluation** — `ExecuteScript` to test scripts locally without
  broadcasting.

## App Patterns

### Micropayments and Settlements

The blackjack demo (`cmd/blackjack`) is the canonical pattern: a game client
uses `P2PKHOutput` + `OpReturnOutput` + `SpendToOutputs` to settle each hand
on-chain. Replace the game logic with any pay-per-action model — API billing,
content paywalls, machine-to-machine payments.

### Data Anchoring

`OpReturnOutput` embeds arbitrary JSON or hashes into transactions. Use it for
proof-of-existence, document timestamping, supply chain provenance, or
certificate issuance. The OP_RETURN is visible on any BSV block explorer.

### Token and Asset Management

Combine `SpendToOutputs` with custom scripts (via `AnyoneCanSpendOutput` or
`BroadcastCustomSpend`) to implement token protocols, colored coins, or
custom smart contracts on BSV.

### Multi-Tenant Wallets

`tenant_id` + `wallet_id` scoping lets a single bsvms instance serve many
applications or users, each with isolated wallet state and spending authority.
Streaming endpoints (`StreamPayments`, `StreamWalletTransactions`) let each
tenant react to its own on-chain events in real time.

## Why gRPC

- Language-agnostic — any gRPC-capable language can be a client (Go, Python,
  TypeScript, Rust, etc.).
- Strongly-typed contract in `proto/bsvms/v1/bsvms.proto`.
- Streaming built in — no polling for new blocks or payments.

The blackjack demo shows the full cycle: create wallets, fund from a regtest
node, play hands, settle with OP_RETURN, and inspect on a local explorer.
Replace the game with your app logic and the same infrastructure applies.
