# market-maker

`market-maker` is a deterministic, single-instrument market-making simulation built around a fixed-point central limit order book. It is a local-first foundation for a future real-time bot arena, not a production trading venue.

The web application is the primary interface. The CLI runs the identical exchange kernel for quick deterministic scenarios.

## Run

```sh
go run ./cmd/server
# Open http://127.0.0.1:8080

go run ./cmd/mmg -seed 42 -turns 10
```

The server intentionally binds only to loopback. Durable game records are stored under `data/games/` and are excluded from Git.

## Mechanics

- One simulated instrument, `SIM`, has a server-owned reference mark.
- The player maintains one bid and one ask, each with a configured quote size.
- Simulated customers submit deterministic IOC limit orders around the reference mark. An order executes only when its limit crosses a resting quote.
- The book is price-time priority. Execution occurs at the resting maker price. Partial fills and residual GTC quotes are explicit.
- Cash, quantity, price, and execution notional use fixed-point integers. API decimal values are serialized as strings, never JSON floating-point values.
- The engine reserves full cash for a resting buy quote and applies configurable initial and maintenance margin to gross exposure. Position limits are enforced before quotes are accepted.
- Storage is charged against absolute inventory each simulation turn. Insolvency and maintenance-margin failure end the game; there is no forced liquidation yet.

`net_fill_cash` is cash movement, not realized P&L. Equity is marked as `cash + position * reference_mark`.

## Durable API

All mutating v2 requests are versioned and idempotent. IDs are UUIDs.

```text
POST /api/v2/games
GET  /api/v2/games/{game_id}
POST /api/v2/games/{game_id}/commands
GET  /api/v2/games/{game_id}/events?after={sequence}
```

Create a game with a client-generated `game_id` and `command_id`. The response preserves the resolved non-zero seed. Submit a quote with an idempotency key and the last observed version:

```json
{
  "id": "33333333-3333-4333-8333-333333333333",
  "type": "submit_quote",
  "expected_version": 4,
  "bid": "99.5000",
  "ask": "100.5000"
}
```

Retrying the same command returns its original result without advancing the market. Reusing a command ID with another payload and submitting a stale version both return `409`.

Each accepted command and result is appended and `fsync`ed to a per-game JSONL log. Restarting the server replays that log from the persisted scenario configuration; a partial final write is ignored.

## Venue Boundary

The matching foundation has three deliberate layers:

- `orderbook.Book` is pure price-level/FIFO matching. It knows only orders, prices, quantities, time-in-force, and an explicit self-trade policy.
- `exchange.Engine` is the single-instrument venue adapter. It owns accounts, live reservations, risk admission, settlement, trade IDs, and exchange events.
- Scenario code submits quotes and synthetic flow through venue commands. It does not implement matching or mutate balances directly.

Venue account state separates settled cash from `reserved_cash`, and reports open buy/sell quantity plus available cash. Live orders derive reservations, so cancellation and partial/full fill release the correct amount automatically.

The current durable venue commands are `open_account`, `place_order`, `cancel_order`, and `replace_order`. A replacement atomically removes one live order and accepts a new same-side order after risk and self-trade checks against a preview without the old order. It receives a new order ID and loses queue priority.

The venue also has an append-only double-entry journal. Opening balances, reservation movements, settled trades, and storage charges each balance independently in cash and instrument units. Account fields remain verified projections during this transition; deposits, withdrawals, fees, and liquidation remain deferred until ledger persistence and authorization are introduced. `submit_quote` and `quit` are temporary scenario commands, not part of the future bot-facing venue contract.

## Architecture

```text
cmd/server                 Local web/API process
cmd/mmg                    Local CLI adapter
internal/fixed             Exact decimal parsing and arithmetic
internal/orderbook         Pure price-level FIFO matching and depth snapshots
internal/exchange          Account/risk, settlement, scenario policy, event projection
internal/eventlog          Durable committed-command JSONL store
web/static                 Browser projection of structured API events
```

There is no v1 game or API compatibility layer. All new work targets `internal/exchange` and `/api/v2`.

## Roadmap

The detailed current status and next-wave sequencing is in [ROADMAP.md](ROADMAP.md).

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```
