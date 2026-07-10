package game

import (
	"market-maker/internal/types"
	"testing"
)

// Test helpers for deterministic testing.

func TestConfigWithSeed(seed int64) types.GameConfig {
	cfg := DefaultConfig()
	cfg.Seed = seed
	return cfg
}

// RunTurns is a helper for tests to drive a game to completion or N turns.
func RunTurns(t *testing.T, g *Game, turns int, bid, ask float64) {
	t.Helper()
	for i := 0; i < turns; i++ {
		if g.IsOver() {
			break
		}
		_, err := g.SubmitTurn(bid, ask)
		if err != nil {
			t.Fatalf("SubmitTurn failed at turn %d: %v", i, err)
		}
	}
}
