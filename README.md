# market-maker

`market-maker` is a deterministic, single-instrument market-making simulation built around a fixed-point central limit order book. It is a local-first foundation for a future real-time bot arena, not a production trading venue.

The web application is the primary interface. The CLI runs the identical exchange kernel for quick deterministic scenarios.

## Run

```sh
go run ./cmd/server
# Open http://127.0.0.1:8080

go run ./cmd/mmg -seed 42 -turns 10
go run ./cmd/mmg -seed 1 -turns 8 -simulation-version 2 -informed-flow-bps 6000
```

The CLI defaults to legacy simulation version 1 so existing seeded runs remain reproducible. Use `-simulation-version 2` for independent random streams, P&L attribution, and informed flow.

Or use the project workflows:

```sh
make run        # local web server
make build      # CLI and server binaries in bin/
make test       # all tests
make test-race  # race-enabled tests
make check      # format, vet, and race tests
```

The server intentionally binds only to loopback. Durable game records are stored under `data/games/` and are excluded from Git.

## Mechanics

- One simulated instrument, `SIM`, has a server-owned reference mark.
- The player maintains one bid and one ask, each with a configured quote size.
- Simulated customers submit deterministic IOC limit orders around the reference mark. An order executes only when its limit crosses a resting quote.
- Simulation version 2 separates the customer-flow, reference-mark, and informed-flow random streams. Informed customers know the direction of the next mark move, but they still trade only when their willingness price crosses a quote.
- The book is price-time priority. Execution occurs at the resting maker price. Partial fills and residual GTC quotes are explicit.
- Cash, quantity, price, and execution notional use fixed-point integers. API decimal values are serialized as strings, never JSON floating-point values.
- The engine reserves full cash for a resting buy quote and applies configurable initial and maintenance margin to gross exposure. Position limits are enforced before quotes are accepted.
- Storage is charged against absolute inventory each simulation turn. Insolvency and maintenance-margin failure end the game; there is no forced liquidation yet.

`net_fill_cash` is cash movement, not realized P&L. Equity is marked as `cash + position * reference_mark`. Version-2 turns reconcile exact fixed-point attribution:

```text
turn P&L = execution edge + inventory mark P&L + storage P&L
```

Execution edge values player fills against the opening mark, inventory mark P&L applies the mark move to ending inventory, and storage P&L is the signed carrying-cost charge. Informed-flow P&L separately values player fills with informed customers at the closing mark.

## Durable API

All mutating v2 requests are versioned and idempotent. IDs are UUIDs.

```text
POST /api/v2/games
GET  /api/v2/scenarios
GET  /api/v2/games/{game_id}
POST /api/v2/games/{game_id}/commands
GET  /api/v2/games/{game_id}/events?after={sequence}
```

The server owns the lesson catalog. Fetch the available scenario snapshots, then create a game with a client-generated `game_id`, `command_id`, and a catalog `scenario_id`:

```json
{
  "game_id": "11111111-1111-4111-8111-111111111111",
  "command_id": "22222222-2222-4222-8222-222222222222",
  "scenario_id": "first-spread-v1"
}
```

Each response includes the persisted scenario snapshot. Quote outcomes also include concise coaching, and terminal sessions include a recap. Submit a quote with an idempotency key and the last observed version:

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

`GET /api/v2/games/{game_id}` returns the durable state, scenario, latest coaching, recap, and the latest completed quote turn with its summary and coaching. Create and command endpoints include their command acknowledgement; command results additionally include the turn summary and events. Event cursors use canonical unsigned integers, for example `?after=42`.

Each accepted command and result is appended and `fsync`ed to a per-game JSONL log. New games are atomically published from a staging directory, and schema-3 records are bound to a checksummed metadata snapshot. Scenario snapshots, simulation configuration, coaching, recaps, scorecards, and P&L attribution are durable parts of that history. Existing schema-1 and schema-2 games remain replayable and appendable; a partial final write is ignored.

Event schemas are append-only wire formats: a new persisted field requires a new schema version rather than silently changing checksum preimages. Each schema retains its checksum domain, and schema 3 is used for new games.

The JSONL store is local-first and single-process. Production multi-instance hosting is deferred to PostgreSQL, where transactional optimistic concurrency or row locking will serialize game mutations.

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
internal/scenario          Curated lessons, deterministic coaching, and recaps
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
