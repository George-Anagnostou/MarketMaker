# market-maker

A fast, simple, single-player market making game / trading simulation engine.

You act as a market maker. Each turn you post a bid and ask. Pseudo-random market orders (aggressive flow) hit your quotes. You capture spread but take on inventory risk. Price walks randomly. You pay storage on your position. Go bankrupt or survive the turns.

Built in Go with zero non-stdlib dependencies. Designed to be extended into a full multiplayer real-time exchange simulator later.

## Quick Start

```bash
# Build the CLI
go build -o market-maker ./cmd/mmg

# Play a 10-turn game with a fixed seed (reproducible)
./market-maker -turns 10 -seed 42

# Unlimited practice mode
./market-maker -turns 0

# Tune parameters
./market-maker -cash 50000 -storage 2 -vol "-1,4" -turns 15
```

In-game:
- Enter `bid ask` (e.g. `99.5 100.5`)
- `q` to quit early

## Core Rules (Phase 1)

- You post prices only. Size is implicit ("at size").
- Market buys hit your ask → you sell.
- Market sells hit your bid → you buy.
- Price does a uniform random walk after execution.
- Storage cost = `storage_cost_per_unit * |inventory|` each turn.
- Equity (MTM) = `cash + inventory * current_reference_price`
- Game ends on `cash <= 0` (bankrupt) or after N turns (or never if unlimited).
- Goal: maximize P&L while managing inventory risk. Wide spreads = less volume, less risk. Tight spreads = more volume, more risk.

The reference price used for marking inventory is an exogenous simulated market price (the random walk), not your own fill prices. This is the standard way to teach the inventory risk lesson in market making simulations.

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-turns` | 10 | Number of turns. 0 = unlimited |
| `-cash` | 100000 | Starting cash |
| `-inventory` | 0 | Starting inventory (can be negative) |
| `-price` | 100 | Starting reference price |
| `-storage` | 1 | Storage cost per unit per turn |
| `-seed` | 0 | RNG seed (0 = random, non-reproducible) |
| `-vol` | "-0.5,3" | Price move range per turn in percent, "min,max" |

## Determinism

If you provide `-seed`, the same sequence of bids/asks will produce the exact same outcome every time. This is extremely useful for testing, teaching, and later for bot strategy development.

## Architecture (for future you)

```
cmd/mmg          - CLI entrypoint (thin)
internal/
  game/          - Core engine. SubmitTurn, state, P&L, bankruptcy, lifetime stats.
  market/        - Flow generation + price random walk.
  types/         - GameConfig, GameState, TurnResult, Event, TurnSummary (JSON-ready).
```

The engine has no I/O. It is fully deterministic given config + player actions. This makes it reusable for:
- Web single-player (Phase 3+)
- Multiplayer exchange (later)
- Bot arena / strategy backtesting (later)
- Monte Carlo parameter tuning

## Roadmap (high level)

1. ✅ Phase 1: Playable CLI, correct mechanics, deterministic, tested.
2. CLI polish + scenarios + better end-of-game analytics.
3. Web single-player (pure stdlib net/http first).
4. Web polish, charts, config UI.
5. Multiplayer + real matching engine + quote allocation.
6. Real-time, bots, informed flow, historical replay, etc.

## Development

```bash
go test ./...          # all tests
go test -race ./...    # with race detector
go build ./cmd/mmg     # build CLI
```

Golden deterministic test (`internal/game/golden_test.go`) locks exact outcomes for a known seed + strategy. Change core logic only if you intend to change behavior.

## Notes on Money and Floats

Prices, quantities, and cash are `float64`. This is intentional for speed and simplicity in a teaching simulation. It is not intended for production settlement or real money movement. If we ever need exact decimal arithmetic, we will introduce a dependency then.

## License

Unlicensed / throwaway for now. Rewrite aggressively.
