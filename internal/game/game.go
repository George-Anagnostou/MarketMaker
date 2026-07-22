package game

import (
	"errors"
	"fmt"

	"market-maker/internal/engine"
	"market-maker/internal/types"
)

// Game wraps the pure engine to provide a "game session" with explicit lifecycle.
// It owns IsOver / end reasons (including player-initiated quit) on top of the
// reusable market-making simulation.
//
// The underlying engine (internal/engine) has no notion of "over". Game decides:
// - after a Step, check for bankruptcy (cash <= 0) or turns_complete
// - explicit Quit() for player/session end
//
// This separation lets the engine be used for simulations, bots, MC, etc. without
// session concepts. Game + server provide the playable "game".
//
// Determinism preserved: same config/seed + actions = same results.
type Game struct {
	eng *engine.Engine
	cfg types.GameConfig

	// session lifecycle (owned here, not in engine)
	isOver bool
	reason types.EndReason

	// cached stats (updated from engine)
	stats types.GameStats
}

// NewGame creates a new game session (engine + lifecycle rules).
// Note: if cfg.StartingCash <= 0, the session starts not-over; first SubmitTurn will end it bankrupt.
// (This matches historical behavior and is accepted by server create.)
func NewGame(cfg types.GameConfig) *Game {
	eng := engine.NewEngine(cfg)
	g := &Game{
		eng:    eng,
		cfg:    cfg,
		isOver: false,
		reason: types.EndReasonNotOver,
	}
	// Seed initial stats from engine (handles starting inventory risk)
	g.refreshStats()
	return g
}

func (g *Game) refreshStats() {
	g.stats = g.eng.Stats()
}

// State returns current observable state (with IsOver/Reason from this session layer).
func (g *Game) State() types.GameState {
	st := g.eng.State()
	st.IsOver = g.isOver
	st.Reason = g.reason
	return st
}

// Config returns the game config.
func (g *Game) Config() types.GameConfig {
	return g.cfg
}

// SubmitTurn executes a turn via the engine, then applies session end rules.
func (g *Game) SubmitTurn(bid, ask float64) (types.TurnResult, error) {
	if g.isOver {
		return types.TurnResult{State: g.State()}, errors.New("game is already over")
	}

	res, err := g.eng.Step(bid, ask)
	if err != nil {
		return res, err
	}

	// Apply terminal conditions from engine result (bankruptcy can happen due to storage/fills)
	g.applyTerminalRules(res.State)

	// Overlay session lifecycle on the result
	res.State.IsOver = g.isOver
	res.State.Reason = g.reason

	// If we just ended, append a game_over event (for CLI/web parity)
	if g.isOver {
		msg := "Game over."
		switch g.reason {
		case types.EndReasonBankrupt:
			msg = "BANKRUPTCY! Cash <= 0. Game over."
		case types.EndReasonTurnsComplete:
			msg = fmt.Sprintf("Completed %d turns.", g.cfg.NumTurns)
		}
		res.Events = append(res.Events, types.Event{
			Type:    "game_over",
			Message: msg,
		})
	}

	g.refreshStats()
	return res, nil
}

// Quit ends the game/session explicitly (player choice). Idempotent if already over.
func (g *Game) Quit() (types.TurnResult, error) {
	if g.isOver {
		return types.TurnResult{State: g.State()}, nil
	}
	g.isOver = true
	g.reason = types.EndReasonPlayerQuit

	st := g.State()
	// Return a result for convenience (no new "turn" happened)
	return types.TurnResult{
		State:   st,
		Events:  []types.Event{{Type: "game_over", Message: "Player quit. Game ended."}},
		Summary: types.TurnSummary{},
	}, nil
}

// IsOver reports whether the session has ended (by rules or Quit).
func (g *Game) IsOver() bool {
	return g.isOver
}

// Reason returns why it ended (or NotOver).
func (g *Game) Reason() types.EndReason {
	return g.reason
}

// Stats returns lifetime aggregates (from engine).
func (g *Game) Stats() types.GameStats {
	return g.stats
}

// applyTerminalRules checks the post-step state and sets isOver/reason if needed.
// Only sets if not already over. Bankruptcy checked on post-storage cash.
func (g *Game) applyTerminalRules(st types.GameState) {
	if g.isOver {
		return
	}
	if st.Cash <= 0 {
		g.isOver = true
		g.reason = types.EndReasonBankrupt
	} else if g.cfg.NumTurns > 0 && st.Turn >= g.cfg.NumTurns {
		g.isOver = true
		g.reason = types.EndReasonTurnsComplete
	}
}
