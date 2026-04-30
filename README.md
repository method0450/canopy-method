# Subex — $SBX

> Programmable subscription infrastructure on-chain.

Subex is a Nested Chain built on [Canopy Network](https://canopy.network) that provides the on-chain infrastructure for recurring payment subscriptions. Creators, protocols, and businesses can deploy programmable subscription tiers with automated billing cycles, cancellation enforcement, and subscriber management — without intermediaries, without platform extraction.

Payments are validated by the network. Revenue goes directly to creators.

---

## Why Subex

Every subscription platform today is a middleman. Stripe, Patreon, Substack — they own the billing relationship, take a cut, and can deplatform you at any time. Subex removes them from the equation entirely.

With Subex:
- Subscription terms are enforced by the chain, not a company
- Billing is automated and transparent
- Creators retain full ownership of their subscriber relationships
- Anyone can run a billing processor and earn rewards for doing so

---

## Architecture

Subex is built as a **Nested Chain** on Canopy Network using the Go plugin framework. It runs its own state machine, processes its own transaction types, and settles security on Canopy's root chain via NestBFT consensus.

```
Canopy Root Chain (security layer)
        │
        └── Subex Nested Chain (plugin/go)
                ├── State Machine (contract.go)
                ├── Transaction Types (tx.proto)
                ├── Frontend UI (Plugin/frontend/index.html)
                └── Token: $SBX
```

The plugin communicates with the Canopy FSM over a Unix socket, handling CheckTx and DeliverTx for all custom transaction types. State (plans, subscriptions, counters) is stored in Canopy's key-value store via the plugin state read/write API.

---

## Token

**Symbol:** `$SBX`  
**Unit:** `uSBX` (1 SBX = 1,000,000 uSBX)  
**Genesis supply:** 1,000,000 SBX  

Fees are collected per transaction and distributed to the fee pool. Billing processors earn **1 SBX per processed billing cycle** as an incentive to keep subscriptions active.

---

## Transaction Types

Subex defines 7 custom transaction types on top of the standard `send`:

### `create_plan`
Deploy a new subscription tier on-chain.

| Field | Description |
|---|---|
| `creatorAddress` | Plan owner (bytes) |
| `name` | Plan name (max 64 chars) |
| `description` | What subscribers get (max 280 chars) |
| `price` | Cost per billing cycle in uSBX |
| `intervalDays` | Billing cycle length in days |
| `trialDays` | Free trial period (0 = none) |
| `maxSubscribers` | Subscriber cap (0 = unlimited) |

### `subscribe`
Subscribe a wallet to a plan. Deducts the first billing payment immediately (or starts trial).

| Field | Description |
|---|---|
| `subscriberAddress` | Subscriber wallet |
| `planId` | ID of the plan to subscribe to |

### `process_billing`
Process a due billing cycle for a subscription. Anyone can call this — the processor earns 1 SBX reward.

| Field | Description |
|---|---|
| `processorAddress` | Wallet receiving the reward |
| `subscriptionId` | ID of the subscription to bill |

### `cancel_subscription`
Cancel an active subscription. Enforced immediately on-chain.

| Field | Description |
|---|---|
| `subscriberAddress` | Must match subscription owner |
| `subscriptionId` | Subscription to cancel |

### `pause_subscription`
Pause billing without cancelling. Billing cycles are skipped while paused.

| Field | Description |
|---|---|
| `subscriberAddress` | Must match subscription owner |
| `subscriptionId` | Subscription to pause |

### `resume_subscription`
Resume a paused subscription, restarting the billing cycle.

| Field | Description |
|---|---|
| `subscriberAddress` | Must match subscription owner |
| `subscriptionId` | Subscription to resume |

### `update_plan`
Update an existing plan's name, description, price, or subscriber cap.

| Field | Description |
|---|---|
| `creatorAddress` | Must match plan creator |
| `planId` | Plan to update |
| `name` | New name (blank = no change) |
| `description` | New description (blank = no change) |
| `newPrice` | New price in uSBX (0 = no change) |
| `maxSubscribers` | New cap (0 = no change) |

---

## Repo Structure

```
canopy-method/
├── plugin/
│   └── go/
│       ├── main.go               # Plugin entry point
│       ├── chain.json            # Chain config (name: Subex, symbol: SBX)
│       ├── go.mod                # Module: github.com/method0450/canopy-method/plugin/go
│       ├── contract/
│       │   ├── contract.go       # State machine — CheckTx, DeliverTx, all handlers
│       │   ├── tx.pb.go          # Generated protobuf types
│       │   ├── account.pb.go     # Account/Pool types
│       │   ├── plugin.go         # Plugin framework (Canopy SDK)
│       │   └── error.go          # Error codes
│       └── proto/
│           ├── tx.proto          # Subex message definitions
│           ├── plugin.proto      # Canopy plugin protocol
│           └── account.proto     # Account types
└── Plugin/
    └── frontend/
        └── index.html            # Live frontend UI
```

---

## Running Locally

### Prerequisites
- [Canopy Network](https://github.com/canopy-network/canopy) node binary (`canopy`)
- Go 1.24+
- protoc v5.29.3
- protoc-gen-go

### 1. Clone and build

```bash
git clone https://github.com/method0450/canopy-method.git
cd canopy-method/plugin/go

# Fix module paths in all .go files
grep -rl "canopy-network/go-plugin" . | xargs sed -i \
  's|github.com/canopy-network/go-plugin|github.com/method0450/canopy-method/plugin/go|g'

# Tidy dependencies
GOTOOLCHAIN=local go mod tidy

# Build the plugin binary
go build -o go-plugin .
```

### 2. Copy plugin binary to Canopy

The `canopy` binary resolves the plugin via `pluginctl.sh` relative to its own location. Copy the built binary to the correct path:

```bash
cp go-plugin ~/canopy/plugin/go/go-plugin
```

### 3. Start the chain

```bash
canopy start
```

The node will automatically launch the plugin via `pluginctl.sh` and complete the FSM handshake. You should see:

```
Plugin go started: go-plugin started successfully
Plugin service listening on socket: /tmp/plugin/plugin.sock
```

### 4. Serve the frontend

```bash
cd Plugin/frontend
python3 -m http.server 8080
```

Open `http://localhost:8080` — the frontend auto-loads the validator wallet and connects to the live chain.

---

## Protobuf Regeneration

If you modify `tx.proto`, regenerate with protoc v5.29.3 (required — older versions produce incompatible descriptors):

```bash
cd plugin/go/proto
protoc \
  --plugin=protoc-gen-go=$HOME/go/bin/protoc-gen-go \
  --go_out=../contract \
  --go_opt=paths=source_relative \
  tx.proto account.proto event.proto plugin.proto

protoc-go-inject-tag -input=../contract/tx.pb.go
```

---

## Frontend

The frontend is a single-file HTML/JS app with no build step. It connects directly to the Canopy RPC at `http://localhost:50002` and the admin keystore at `http://localhost:50003`.

Features:
- Auto-loads validator wallet on boot
- Polls block height every 10s
- Rebuilds plan and subscription state from `txs-by-sender` tx history
- Full tx submission for all 7 Subex transaction types
- Live activity log with RPC connection status
- Loading spinners and toast notifications on all actions

For remote access, tunnel both ports via cloudflared:

```bash
cloudflared tunnel --url http://localhost:8080   # Frontend
cloudflared tunnel --url http://localhost:50002  # Public RPC
cloudflared tunnel --url http://localhost:50003  # Admin RPC
```

Then update `const RPC` and `const ARPC` in `index.html` with the tunnel URLs.

---

## Built On

- [Canopy Network](https://canopy.network) — NestBFT consensus, Progressive Sovereignty, Nested Chain framework
- Go plugin SDK — CheckTx/DeliverTx/BeginBlock/EndBlock lifecycle
- BLS12-381 signatures via `@noble/curves`
- Protobuf v3

---

## Author

Built by [@MakDaVeli](https://twitter.com/MakDaVeli) — Canopy Builder #6, Trader #10 on testnet.

> Subex is part of the Canopy Creators Hub Week 5 submission.
