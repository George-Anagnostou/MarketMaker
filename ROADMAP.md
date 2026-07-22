# Roadmap

## Current Status

The project now has a coherent local exchange foundation:

- A fixed-point, deterministic, single-instrument CLOB with maker-price execution, price-time priority, partial fills, IOC/GTC behavior, self-trade prevention, and explicit order IDs.
- Deterministic simulated limit flow. A customer trades only when its willingness price crosses a posted quote, so spread competitiveness now matters.
- Cash reservations, position limits, configurable initial/maintenance margin, storage costs, and terminal margin/insolvency states.
- Versioned, idempotent commands and structured events. A retry never executes another turn.
- A fsynced JSONL command/result record per local game, replay on restart, and retained terminal sessions.
- A local-only v2 HTTP API, browser client, and CLI that all use the same exchange kernel.

This is a sound simulation kernel, not yet a shared real-time venue. Known constraints are intentional and should guide the next work:

- Only the player and synthetic-flow accounts are provisioned through the application. Multi-account primitives are internal only.
- The book uses deterministic price levels and FIFO queues. It is correct for a single instrument, but it still needs per-instrument sharding and performance profiling before high order counts.
- Time advances when a player submits a quote. There is no independent market clock, latency model, or scheduled event queue.
- JSONL storage is single-process and local. It has no snapshots, integrity hash chain, schema migration, or cross-process lock.
- Risk is conservative at order entry but has no double-entry ledger, collateral model, borrow model, forced liquidation, or kill switch.
- The browser restores an active game state but does not yet rebuild full chart history after refresh.
- There is no browser test suite, CI pipeline, metrics, tracing, structured logs, health endpoint, graceful shutdown, or packaged static assets.

## Next Wave: Exchange Correctness

1. Replace map scans with per-instrument price levels and FIFO queues. Add depth snapshots and deterministic best-book updates.
2. Promote account provisioning, place, cancel, replace, and account-query operations into authenticated API commands. Keep account identity server-owned.
3. Add atomic replace semantics, client order IDs, order-status history, reject reasons, and explicit trade/settlement records.
4. Introduce a double-entry ledger with cash, reserved cash, position, reserved position, collateral, realized P&L, and fees as separate balances.
5. Define maintenance handling: margin calls, cancel-on-breach, forced liquidation, bankruptcy waterfall, and venue loss limits.
6. Add maker/taker fees, tick/lot schedules, price bands, trading halts, self-trade policy modes, and per-account rate/position/notional limits.
7. Build property and fuzz tests for conservation, reservation release, matching priority, replay equivalence, overflow boundaries, and command idempotency.

## Next Wave: Simulation Quality

1. Split deterministic random streams for customer flow, reference price, news, and regime changes; persist the effective scenario and engine version.
2. Add an independent simulation clock, scheduled arrivals, cancel/replace latency, exchange sequencing, and deterministic tie-breaking.
3. Model informed flow, adverse selection, inventory-sensitive customer behavior, market regimes, volatility clusters, outages, and liquidity shocks.
4. Add scenario definitions with server-approved parameters, public/private seeds, scorecards, benchmark market-makers, and replayable historical runs.
5. Add bot execution constraints: API quotas, CPU/wall-clock budgets, network latency profiles, fault injection, and simulation isolation.

## Platform Wave

1. Move committed commands, events, snapshots, users, accounts, and scenarios to PostgreSQL with transactional optimistic concurrency.
2. Add authentication, account ownership, bot credentials, secrets handling, authorization, rate limits, audit trails, and abuse controls before any non-loopback deployment.
3. Add WebSocket market-data and private-order streams with cursors, reconnect/backfill, slow-consumer policy, and multi-instance fan-out.
4. Package static assets into the binary or a versioned frontend build; add graceful shutdown, health/readiness, structured logging, metrics, tracing, backups, and restore drills.
5. Add CI for formatting, vet, race, fuzz/property tests, dependency/vulnerability checks, reproducible builds, and release artifacts.

## Game Wave

1. Turn scenarios into onboarding/tutorial content that explains queue position, inventory risk, adverse selection, and margin.
2. Add player accounts, progression, challenge rules, leaderboards, replay sharing, and deterministic tournament scoring.
3. Add bot-versus-bot arenas with fair scenario assignment, observability, anti-cheat controls, and post-game analysis.
4. Add multi-instrument and cross-asset scenarios only after single-instrument accounting, risk, and replay are proven.

## Recommended Sequence

1. Exchange correctness and ledger/risk work.
2. Simulation clock and realistic flow model.
3. PostgreSQL, authentication, and multi-account API.
4. WebSocket transport and operational infrastructure.
5. Bot arena, scenarios, and game mechanics.
