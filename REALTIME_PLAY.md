# Real-Time Play Specification

## Status

This document is the proposed specification and implementation roadmap for real-time play. It is intentionally a design artifact, not an implementation. Work should pause for review before the first engine change.

## Product Direction

Real-time play is a distinct game mode alongside the existing turn-based lessons. It does not replace turns or reinterpret the existing `submit_quote` command.

The first release is a local, single-player browser game against deterministic simulated customer flow:

- Each lesson offers both turn-based and real-time modes.
- The first vertical slice uses **First Spread**.
- A real-time round lasts 90 seconds of logical market time.
- The player maintains one live two-sided quote and updates both sides atomically.
- Quote quantity is fixed by the scenario.
- The market receives pseudo-randomly timed, scenario-seeded customer orders and mark changes independently of player actions.
- The player sees the top of book, a compact shallow book, trade tape, mark chart, position, cash, equity, and P&L.
- Coaching stays out of the live decision loop and appears after the round.
- Explicit pause is available in solo play. A browser disconnect pauses after a short grace period.
- Multiplayer and bot rounds are future live sessions that never pause.

## Goals

1. Make inventory and quote management continuous rather than turn-triggered.
2. Preserve deterministic simulation, exact replay, fixed-point accounting, risk checks, and durable command semantics.
3. Preserve all current turn-based behavior and existing persisted games.
4. Establish sequencing, clock, event-stream, and mode boundaries that can support future multiplayer without prematurely building authentication or distributed infrastructure.
5. Keep the solo market deliberately shallow. Synthetic competitors and ambient resting liquidity are deferred to multiplayer and bot-arena work.

## Non-Goals For The First Release

- Multiple human or bot participants.
- Background market makers or a deep synthetic order book.
- Authentication, remote deployment, PostgreSQL, or multi-instance ownership.
- WebSocket order entry.
- Adjustable player order size or independent bid/ask quantities.
- Forced liquidation at normal time expiry.
- Live coaching interruptions.
- Sub-millisecond or high-throughput exchange simulation.
- Changing the command or replay semantics of existing turn-based games.

## Game Rules

### Mode Selection

The lesson catalog exposes supported modes and mode-specific configuration. The lesson identity, objective, tutorial, and scorecard remain shared where appropriate, while pacing text and real-time tuning can differ.

The browser requires an explicit choice between **Turn-based** and **Real-time** before creating a game. Existing game creation defaults and existing persisted snapshots retain turn-based meaning; mode is never inferred from missing data during replay.

### Round Lifecycle

Real-time sessions use the following authoritative states:

```text
preparing -> countdown -> running <-> paused -> completed
```

- **Preparing:** The player edits an opening bid and ask. No market time passes.
- **Countdown:** A start command validates and stages the opening quote, then begins a three-second ready countdown. No customer flow or mark movement occurs during the countdown.
- **Running:** The opening quote becomes live at logical time zero. The 90-second logical clock, scheduled customer flow, mark movement, carry charges, and risk evaluation begin.
- **Paused:** Logical time and all scheduled market activity freeze. Resting orders and account state remain unchanged. Resuming retains the quote.
- **Completed:** New quote commands are rejected. Resting player orders are cancelled, final equity is marked at the final reference price, and the recap is generated.

Normal expiry marks open inventory to market; it does not synthesize a liquidation trade. Margin breach, insolvency, explicit quit, and storage failure remain terminal reasons. A completed or failed session cannot resume.

### Player Actions

The player has these mode-specific actions:

- `start_round`: atomically validates the opening two-sided quote and starts the countdown.
- `update_quote`: atomically replaces the live bid and ask at scenario-fixed quantity.
- `pause_round`: freezes solo logical time after all scheduled work due before receipt of the command has been sequenced.
- `resume_round`: resumes from the exact committed logical time.
- `quit`: ends the round intentionally.

The UI uses an explicit **Update quote** button. Draft input has no market effect until submitted. Accepted replacement receives new order IDs and loses queue priority, matching the existing venue rules.

Real-time quote updates use command IDs for idempotency but do not use the turn-based global `expected_version` precondition. Autonomous market events advance state too often for that contract to be usable. The sequencer defines authoritative order, and an optional quote revision can reject updates based on a stale player quote without conflicting on unrelated market activity.

### Solo Disconnects

The event stream sends heartbeats and represents one browser as the active local controller. If the controller disconnects while the round is running:

1. The server starts a configurable grace period; the initial proposal is three seconds.
2. Reconnection within the grace period preserves continuous play and backfills missed events.
3. Grace expiry enqueues a durable system pause at the authoritative elapsed time.
4. Reconnection hydrates canonical state and requires an explicit resume.

Disconnect auto-pause is a solo policy, not a scheduler behavior. Future multiplayer sessions continue when any participant disconnects.

### Market Model

The real-time scenario owns a deterministic exogenous schedule separate from player input. A schedule contains logical due times and sampled event parameters, not wall-clock timestamps.

Initial event classes are:

- Customer IOC arrival.
- Reference-mark movement.
- Inventory carry charge.
- Round expiry.

Customer inter-arrival times and order parameters are sampled from independent seeded streams. Mark movement has its own stream and schedule. Informed-flow scenarios may inspect the next scheduled mark direction, preserving the current educational model without coupling mark movement to quote submission.

Pacing is scenario configuration and must be playtested. The First Spread slice should begin with a bounded irregular customer cadence and a slower independent mark cadence, targeting dozens rather than hundreds of decisions or tape events in a 90-second round. Exact intervals are not part of the stable wire contract.

The solo book remains shallow:

- The player is the principal source of resting liquidity.
- Simulated customers remain IOC and do not leave residual orders.
- The compact depth view reports honest available levels and ownership; it does not fabricate ambient liquidity.
- The engine API should support multi-level depth projections so future participants can populate them without redesigning the client contract.

### Risk, Accounting, And Scoring

- Order admission continues to use the existing cash reservation, position, and initial-margin rules.
- Settlement remains double-entry and fixed-point.
- Maintenance and insolvency checks run after every fill, mark movement, and carry charge that can alter risk.
- Storage becomes elapsed-time carry. The scenario defines a rate and deterministic charge cadence, with exact fixed-point rounding rules.
- The final score remains marked total P&L and the lesson-specific scorecard.
- Round P&L attribution aggregates execution edge, inventory mark P&L, storage P&L, and informed-flow evidence over the complete event timeline.
- Post-round analysis may group the timeline into mark intervals for explanation, but these groups are not turns and are not exposed as such.

## Engine Architecture

### Boundary Principle

The matching and settlement kernel remains deterministic and unaware of goroutines, sockets, timers, or wall time. A new orchestration layer owns real-time pacing.

```text
HTTP commands       deterministic schedule
       |                    |
       +------> game sequencer <------ pause/disconnect policy
                         |
                  committed engine action
                         |
             exchange kernel + order book
                         |
                durable log, then publish
                         |
                  SSE event subscribers
```

### Exchange Refactor

Extract the indivisible operations currently embedded in turn advancement without changing their existing behavior:

- Atomic two-sided quote replacement.
- One deterministic customer arrival and settlement.
- One reference-mark movement.
- One carry charge.
- Immediate maintenance and terminal evaluation.
- Explicit session completion.

Turn-based `submit_quote` remains an adapter that invokes these operations in its current order and preserves its current random-number consumption, event shape, summaries, versions, and replay results. Golden compatibility tests must prove seeded turn-based runs are byte-for-byte unchanged before real-time orchestration is added.

Real-time mode invokes the same primitives through explicit internal engine actions. The engine remains single-writer and transactional: failed actions do not consume IDs, random state, ledger entries, or book priority.

### Sequencer And Clock

Each active real-time game has one sequencer goroutine. It is the sole caller of that game's engine and owns:

- Session lifecycle state.
- Monotonic logical elapsed time.
- The next deterministic scheduled action.
- The queue of external player commands.
- Server-generated internal action IDs.
- Durable ordering and subscriber publication.

The sequencer uses an injected clock. Production maps monotonic wall time onto logical elapsed time only while running; tests use a manual clock. Wall-clock timestamps are observational metadata and never decide replay outcomes.

Ordering rules are explicit:

1. Stamp an external command with its server receipt point on the sequencer timeline.
2. Execute all scheduled actions whose due time is less than or equal to that receipt point.
3. Execute the external command.
4. Break equal scheduled due times by stable schedule sequence.
5. Persist each accepted action and result before acknowledging or publishing it.

This makes network arrival arbitrary while preserving one authoritative total order. Future multiplayer participants feed the same sequencer; they do not mutate the book concurrently.

Pause stops logical-time accumulation and disarms the next timer. Resume creates a new wall-to-logical-time anchor. Server restart restores the last committed elapsed time and leaves an interrupted solo game paused rather than counting process downtime as market time.

### Deterministic Schedule

The scenario snapshot includes immutable real-time schedule parameters and seed domains. Schedule generation should be independently reproducible and versioned. It may be generated ahead for the finite 90-second round or incrementally from replayable state; the chosen representation must satisfy these invariants:

- Existing scenario catalog edits cannot alter a persisted game.
- Schedule generation does not depend on Go timer wake-up order.
- Player quote frequency does not perturb customer or mark random streams.
- Restart produces the same next due action.
- Informed-flow classification and the next mark direction remain reproducible.
- A replay compares complete action results, not only final state.

Pre-generating the finite exogenous schedule into private game metadata is the preferred first implementation because it is auditable, restart-safe, bounded, and easy to test. Parameters can be stored as relative basis-point moves, sides, quantities, and slippage so execution still resolves against authoritative state at the scheduled time.

### Persistence

Real-time games require a new append-only persistence schema. Existing schemas remain readable and appendable under their current turn-based rules.

Persisted creation metadata includes:

- Explicit play mode.
- Lifecycle and schedule versions.
- Real-time scenario snapshot and complete private schedule or equivalent reproducible schedule state.
- Duration, countdown, grace period, quote quantity, carry cadence, and seed-domain configuration.

The command log records player commands and autonomous system actions in the exact committed order, including logical elapsed time, source, result, events, and ledger entries. System action IDs use a separate canonical namespace from client UUID idempotency keys.

The initial local implementation retains fsync-before-acknowledgement and fsync-before-publication. Before accepting target event rates, benchmark append latency and file growth. If this cannot maintain pacing, optimize persistence explicitly; do not publish uncommitted state or silently weaken durability.

Recovery replays every committed action, verifies complete results and schedule progression, restores the last elapsed time, and fences the game on mismatch. Snapshotting and compaction remain later work.

## Server And API

### Resource Shape

The API keeps games as the durable aggregate and adds explicit mode and lifecycle fields. Exact endpoint naming can be settled during implementation, but the contract needs these operations:

```text
POST /api/v2/games                         create with scenario_id and mode
GET  /api/v2/games/{id}                    canonical state and stream boundary
POST /api/v2/games/{id}/commands           mode-gated player command
GET  /api/v2/games/{id}/events             durable audit/backfill
GET  /api/v2/games/{id}/stream             SSE committed-event stream
```

The existing turn-based request and response shapes remain valid. Real-time responses add a mode-specific projection rather than overloading `turn`:

- Lifecycle state.
- Duration and authoritative elapsed/remaining logical time.
- Player quote revision and active order IDs.
- Shallow depth levels with ownership.
- Recent committed trades or a cursor to retrieve them.
- Stream cursor and connection policy.

### SSE Contract

SSE is used for committed server-to-browser updates; player commands continue over idempotent HTTP.

- Every data message has a monotonically increasing durable cursor.
- Publication occurs only after the corresponding log append is synced.
- `Last-Event-ID` or an explicit cursor resumes without gaps.
- If a requested cursor is too old or invalid, the client rehydrates canonical state and resumes from its boundary.
- Heartbeats detect dead local controllers and keep intermediaries from considering the stream idle.
- Subscribers have bounded buffers. Slow consumers are disconnected and recover by cursor rather than blocking the sequencer.
- Snapshots plus ordered deltas must be sufficient to reconstruct the visible game state.

The internal subscription interface is transport-neutral. A future WebSocket adapter can carry public market data, private account/order events, and participant commands after authentication and multi-instance fan-out exist.

## Browser Experience

### Lesson Selection

Each lesson presents a clear mode choice. Turn-based copy and controls remain unchanged. Real-time selection explains that the market moves independently and the round can be paused only because it is solo practice.

### Preparing And Countdown

- Show lesson briefing and real-time-specific pacing guidance.
- Prefill a sensible scenario opening quote but require deliberate confirmation.
- Display fixed quote size.
- Validate bid, ask, spread, and risk before enabling **Ready**.
- Use a visible three-second countdown after acknowledgement.

### Live Desk

The live view prioritizes decisions over instruction:

- Prominent remaining time and lifecycle state.
- Bid and ask inputs with an explicit **Update quote** action and keyboard-friendly submission.
- Clear distinction between draft quote and acknowledged live quote.
- Current mark and a compact mark chart.
- Honest shallow order-book display with player-order ownership.
- Recent trade tape with player side, price, quantity, and informed status when the lesson reveals it.
- Position, cash, equity, total round P&L, and risk proximity.
- Pause and quit controls separated from quote entry.
- Connection/recovery status that never implies a draft quote was accepted.

Rendering should coalesce visual updates to animation frames while retaining every durable event in the audit. Color cannot be the only signal for side, P&L, connection, or risk state. Keyboard focus, reduced motion, mobile controls, and chart text alternatives are acceptance requirements.

### Post-Round Review

After completion, replace live controls with:

- Existing lesson scorecard adapted to continuous metrics.
- Final equity and exact P&L attribution.
- Peak absolute inventory and time spent near risk limits.
- Fill and mark timeline with quote-change annotations.
- Adverse-selection or informed-flow evidence where applicable.
- Reflection prompt and replay/play-again actions.

No coaching overlay interrupts the running round.

## Testing Strategy

### Compatibility Gates

- Golden deterministic command streams for every existing turn-based lesson before and after extraction.
- Replay existing schema-1 through schema-3 fixtures without migration.
- Existing HTTP and browser tests remain unchanged unless they explicitly select the new mode.

### Engine And Scheduler Tests

- Schedule reproducibility by scenario version and seed.
- Independent random streams under different player quote frequencies.
- Stable tie-breaking for simultaneous internal actions.
- Scheduled-due-before-external-command ordering.
- Pause/resume elapsed-time accounting across repeated cycles.
- Disconnect grace expiry and reconnect cancellation races.
- Expiry, margin breach, insolvency, quit, and storage-failure terminal races.
- No engine mutation after completion.
- Immediate risk evaluation after fills, marks, and carry.
- Exact P&L and balanced-ledger reconciliation over a full round.
- Manual-clock tests with no sleeps.
- Race tests for command submission, stream subscribers, pause, and shutdown.

### Persistence And API Tests

- Fsync-before-publish and no SSE event for failed appends.
- Replay equivalence after interruption at every action type.
- Corrupt schedule, elapsed time, lifecycle transition, checksum, and result detection.
- Idempotent quote update retry and command-ID payload conflict.
- Bounded stream subscriber and cursor backfill behavior.
- Snapshot/stream handoff with no gap or duplicate application.
- Unsupported command/mode combinations.
- Process restart restores interrupted solo sessions as paused.

### Browser Tests

- Mode choice preserves the turn-based path.
- Preparing, countdown, running, pausing, reconnecting, and completion states.
- Draft versus acknowledged quote behavior under latency, rejection, retry, and reconnect.
- Ordered stream application and canonical rehydration.
- Timer correction from authoritative elapsed time without visual backward jumps.
- Tape, chart, book, P&L, and final recap from fixed-point event fixtures.
- Keyboard, reduced-motion, responsive, and accessible-state behavior.

### Performance Checks

- Engine action throughput and allocation profile at expected and 10x solo event rates.
- Event-log fsync latency versus schedule lateness.
- SSE fan-out and slow-consumer behavior.
- Ninety-second file size and full replay time.
- Browser rendering under burst backfill.

## Implementation Roadmap

### Phase 0: Specification And Compatibility Baseline

1. Review and approve this specification.
2. Capture golden outputs for all existing seeded turn-based scenarios.
3. Define target event-rate and durability budgets through a small benchmark.
4. Resolve the open product questions at the end of this document.

Exit criterion: approved contracts, non-goals, state machine, and compatibility fixtures. No production behavior changes.

### Phase 1: Mode And Scenario Contracts

1. Add explicit `turn_based` and `real_time` modes to game creation and persisted metadata.
2. Add versioned real-time scenario configuration and snapshots without altering existing turn configuration.
3. Add First Spread's initial duration, quote size, flow cadence, mark cadence, carry cadence, countdown, and disconnect policy.
4. Keep the browser on turn-based mode while API and persistence tests establish compatibility.

Exit criterion: a real-time game can be created and recovered in `preparing`, but cannot run.

### Phase 2: Exchange Primitive Extraction

1. Isolate quote replacement, customer arrival, mark movement, carry, risk evaluation, and completion operations.
2. Retain the existing turn adapter and random-consumption order.
3. Add real-time internal action types and continuous summaries.
4. Prove ledger, matching, risk, rollback, and turn golden tests.

Exit criterion: deterministic primitives are callable independently and all turn-based outputs remain unchanged.

### Phase 3: Deterministic Scheduler And Sequencer

1. Implement versioned finite schedule generation for First Spread.
2. Add injected production and manual clocks.
3. Implement the per-game actor, ordering rules, lifecycle transitions, timers, and pause/resume.
4. Add idempotent external command envelopes and internal system action identity.
5. Make shutdown restore active solo games as paused.

Exit criterion: a headless 90-second First Spread round runs under a manual clock and replays exactly.

### Phase 4: Durable Real-Time Log And Read Models

1. Introduce the new metadata/log schema and checksum domain.
2. Persist logical time, source, lifecycle, schedule progress, results, and ledger entries.
3. Build state, quote, depth, tape, chart, and P&L projections.
4. Benchmark fsync pacing, file growth, and recovery.

Exit criterion: interruption and replay tests pass at every action boundary within target budgets.

### Phase 5: HTTP Commands And SSE

1. Expose real-time start, update, pause, resume, quit, state, audit, and stream behavior.
2. Implement cursor handoff, heartbeat, bounded subscribers, backfill, and disconnect grace.
3. Preserve the current turn-based API path and mode-gate every command.
4. Add metrics for schedule lateness, queue delay, connected streams, auto-pauses, and dropped slow consumers without unbounded labels.

Exit criterion: an API client can complete, disconnect from, recover, and exactly replay First Spread.

### Phase 6: First Spread Browser Slice

1. Add lesson mode selection, preparing flow, opening quote, and countdown.
2. Add the live quote desk, shallow book, tape, mark chart, account/P&L panel, timer, pause, and connection state.
3. Add post-round continuous recap and timeline.
4. Add browser fixtures, accessibility checks, responsive behavior, and recovery tests.

Exit criterion: First Spread is playable end-to-end in both modes with no turn-based regression.

### Phase 7: Lesson Expansion And Tuning

1. Playtest First Spread and freeze its initial real-time revision.
2. Adapt Inventory Pressure, Volatility Shock, and Informed Flow with independent mode-specific pacing and recap metrics.
3. Test schedule independence, informed-flow lookahead, risk pressure, and storage scaling in each lesson.
4. Update lesson copy so instructions never refer to real-time intervals as turns.

Exit criterion: every current lesson supports both modes and has deterministic replay fixtures.

### Phase 8: Hardening And Release

1. Run formatting, vet, unit, race, browser, fuzz, replay, and performance suites.
2. Test process shutdown, storage fencing, stream reconnect, and long browser suspension manually.
3. Document API contracts, operating limits, and recovery behavior.
4. Keep the feature behind an explicit mode choice until acceptance criteria and pacing review pass.

Exit criterion: the real-time mode is stable for local solo use and the turn-based option remains unchanged.

## Future Multiplayer And Bot Arena

The solo design intentionally establishes reusable foundations but does not claim to solve multiplayer. Future work adds:

1. Server-owned accounts, authentication, authorization, bot credentials, and participant ownership.
2. Unpausable round policy with explicit join, ready, start, disconnect, and forfeit rules.
3. Arbitrary participant place/cancel/replace commands feeding the same single-writer market sequencer.
4. Ambient depth emerging from participant orders and optional benchmark makers, not fabricated solo liquidity.
5. Public market-data and private order/account channels, likely with WebSocket adapters over the same cursor model.
6. PostgreSQL transactions, snapshots, multi-instance market ownership, and stream fan-out.
7. Rate limits, latency profiles, fairness policy, deterministic tie-breaking, anti-cheat controls, and tournament scoring.
8. Per-instrument sequencers and capacity profiling before multi-instrument arenas.

The multiplayer clock remains wall-paced and never pauses, but every accepted order and autonomous action still receives one authoritative sequence and durable timestamp so completed rounds can be audited and replayed.

## Acceptance Criteria

- A player can select either mode for First Spread, and later for every lesson.
- Turn-based seeded outputs and historical logs remain compatible.
- A running solo round progresses without player input and stops after exactly 90 seconds of logical time.
- Quote replacement is atomic, idempotent, risk-checked, and visibly acknowledged.
- Pause and disconnect grace freeze logical time without cancelling the player's quote.
- Restart restores an interrupted solo game as paused at its last committed state.
- State hydration plus SSE cursor replay has no missing or double-applied event.
- No client observes an action before it is durably committed.
- Full-round replay reproduces state, events, ledger, schedule cursor, recap, and terminal reason exactly.
- The live UI remains usable on desktop and mobile and exposes equivalent non-color and text information.
- Expected solo event rates remain within defined schedule-lateness, fsync-latency, file-size, replay-time, and render budgets.

## Open Review Decisions

1. Is three seconds the correct disconnect grace period, or should it be longer to tolerate brief browser/network stalls?
2. Should countdown cancellation return to `preparing`, or should countdown be irreversible once acknowledged?
3. Should the live UI reveal informed-customer status immediately in Informed Flow, or only in the post-round audit?
4. Should quote updates accept an optional expected quote revision to prevent stale-tab replacement, or should last sequenced update always win in local solo play?
5. What schedule-lateness budget is acceptable before the server pauses/fences a round rather than trying to catch up?
