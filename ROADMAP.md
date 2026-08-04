# Roadmap

## Current Status

The project now has a coherent local exchange foundation:

- A fixed-point, deterministic, single-instrument CLOB with maker-price execution, price-time priority, partial fills, IOC/GTC behavior, self-trade prevention, and explicit order IDs.
- Versioned deterministic simulated limit flow with independent customer, reference-mark, and informed-classification random streams. A customer trades only when its willingness price crosses a posted quote.
- Cash reservations, position limits, configurable initial/maintenance margin, storage costs, and terminal margin/insolvency states.
- A venue/account boundary with settled cash, book-derived reserved cash, position, open-order exposure, durable account-open/order/cancel/replace commands, and balanced journal entries persisted with their command result and replay-verified.
- Versioned, idempotent commands and structured events. A retry never executes another turn.
- A fsynced JSONL command/result record per local game, replay on restart, retained terminal sessions, atomic publication, and recovery that rejects replacement or rollback of the active event file.
- A server-owned catalog of curated lessons with persisted snapshots, measured informed-flow coaching, exact turn P&L attribution, and terminal scorecards.
- A local-only v2 HTTP API, browser client, and CLI that all use the same exchange kernel.

This is a sound simulation kernel, not yet a shared real-time venue. Known constraints are intentional and should guide the next work:

- The HTTP scenario API intentionally exposes only player quote/quit commands until authenticated account ownership exists. The venue supports multi-account order primitives internally.
- The book uses deterministic price levels and FIFO queues. It is correct for a single instrument, but it still needs per-instrument sharding and performance profiling before high order counts.
- Time advances when a player submits a quote. There is no independent market clock, latency model, or scheduled event queue.
- JSONL storage is single-process and local. New games use atomic directory publication and schema-3 metadata-to-record binding; schema-1 and schema-2 games remain readable and appendable. It still has no compaction snapshots, cross-process lock, or PostgreSQL transaction layer.
- Risk has a balanced in-memory journal and conservative order-entry checks, but still lacks collateral/borrow models, forced liquidation, and kill switches.
- The browser restores its durable turn audit, session path, latest coaching, and authoritative P&L attribution after refresh. It validates API envelopes before rendering and pins paginated audit hydration to the authoritative event boundary, but it has no charting or offline replay export.
- CI checks formatting, vet, dependency-free browser runtime tests, race tests, and builds. The local server has storage-aware readiness, structured JSON request and error logs, bounded graceful shutdown, and local Prometheus metrics. Tracing, packaged static assets, and end-to-end browser testing remain deferred.

## Next Wave: Exchange Correctness

1. Add per-instrument venue sharding, full depth snapshots, deterministic best-book updates, and performance profiling at larger order counts.
2. Promote authenticated account provisioning, place, cancel, replace, and account-query operations. Keep account identity server-owned.
3. Add client order IDs, order-status history, reject reasons, and explicit trade/settlement records.
4. Add durable journal projections and audit queries, then add collateral, realized P&L, and fees as separate balances.
5. Define maintenance handling: margin calls, cancel-on-breach, forced liquidation, bankruptcy waterfall, and venue loss limits.
6. Add maker/taker fees, tick/lot schedules, price bands, trading halts, self-trade policy modes, and per-account rate/position/notional limits.
7. Build property and fuzz tests for conservation, reservation release, matching priority, replay equivalence, overflow boundaries, and command idempotency.

## Next Wave: Simulation Quality

1. Extend the versioned independent random streams from customer flow, reference price, and informed classification to news and regime changes; define public/private seed policy.
2. Add an independent simulation clock, scheduled arrivals, cancel/replace latency, exchange sequencing, and deterministic tie-breaking.
3. Expand directional informed flow into inventory-sensitive customer behavior, market regimes, volatility clusters, outages, and liquidity shocks.
4. Add benchmark market-makers and replayable historical runs to the curated tutorial scenarios.
5. Add bot execution constraints: API quotas, CPU/wall-clock budgets, network latency profiles, fault injection, and simulation isolation.

## Platform Wave

1. Move committed commands, events, snapshots, users, accounts, and scenarios to PostgreSQL with transactional optimistic concurrency.
2. Add authentication, account ownership, bot credentials, secrets handling, authorization, rate limits, audit trails, and abuse controls before any non-loopback deployment.
3. Add WebSocket market-data and private-order streams with cursors, reconnect/backfill, slow-consumer policy, and multi-instance fan-out.
4. Package static assets into the binary or a versioned frontend build; add graceful shutdown, health/readiness, structured logging, metrics, tracing, backups, and restore drills.
5. Extend CI with fuzz/property tests, dependency/vulnerability checks, reproducible builds, and release artifacts.

## Local Operations Observability

Completed foundation:

1. The server creates and validates the configured game-storage root at startup. `/healthz` reports liveness and `/readyz` performs a tiny durable probe plus checks loaded-game storage fences.
2. JSON request logs go to stderr with a server-generated request ID, method, route template, status, duration, and response size. Fixed-name error events cover recovered handler panics and internal HTTP-server errors. They exclude raw paths, query strings, client addresses, command payloads, account balances, durable game data, and raw error values.
3. `SIGINT` and `SIGTERM` trigger a 10-second graceful drain. The server stops accepting new connections, logs lifecycle outcomes, and exits with failure if the drain deadline is reached.
4. `/metrics` exposes dependency-free Prometheus text for bounded-cardinality request, command, recovery-failure, event-log append, process, and shutdown metrics. It is available only through the loopback listener.

Next dependency-light pull requests:

1. Add tracing and deployment-specific exporters after the metrics contract and authentication boundary are stable.

## Game Wave

1. Complete onboarding/tutorial content for queue position and margin; inventory risk and measured adverse selection now have guided lessons.
2. Add player accounts, progression, challenge rules, leaderboards, replay sharing, and deterministic tournament scoring.
3. Add bot-versus-bot arenas with fair scenario assignment, observability, anti-cheat controls, and post-game analysis.
4. Add multi-instrument and cross-asset scenarios only after single-instrument accounting, risk, and replay are proven.

## Recommended Sequence

1. Exchange correctness and ledger/risk work.
2. Simulation clock and realistic flow model.
3. PostgreSQL, authentication, and multi-account API.
4. WebSocket transport and operational infrastructure.
5. Bot arena, scenarios, and game mechanics.
