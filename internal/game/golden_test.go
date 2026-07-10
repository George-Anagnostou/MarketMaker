package game

import (
	"math"
	"testing"

	"market-maker/internal/types"
)

// TestDeterministicGolden locks the exact behavior for a known seed + sequence of actions.
// If someone changes core logic (fill math, storage timing, price walk, etc.) this will catch it.
func TestDeterministicGolden(t *testing.T) {
	cfg := TestConfigWithSeed(424242)
	cfg.NumTurns = 5
	cfg.StartingCash = 10000
	cfg.StartingInventory = 0
	cfg.StartingPrice = 100
	cfg.StorageCostPerUnit = 1
	cfg.MinPriceMovePct = -0.01
	cfg.MaxPriceMovePct = 0.01
	cfg.MaxOrdersPerTurn = 3
	cfg.MaxOrderSize = 5

	g := NewGame(cfg)

	// Fixed player strategy: always post 99 / 101
	actions := []struct{ bid, ask float64 }{
		{99, 101},
		{99, 101},
		{99, 101},
		{99, 101},
		{99, 101},
	}

	var lastResult types.TurnResult
	for _, a := range actions {
		if g.IsOver() {
			break
		}
		res, err := g.SubmitTurn(a.bid, a.ask)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		lastResult = res
	}

	st := g.State()
	if !st.IsOver {
		t.Error("expected game over after 5 turns")
	}
	if st.Reason != "turns_complete" {
		t.Errorf("reason = %s, want turns_complete", st.Reason)
	}

	// Locked golden values from a known-good run (seed 424242, correct [1..N] flow).
	// These are the source of truth. Only change when you mean to change behavior.
	wantCash := 10497.681978410616
	wantInv := -4.821907458214
	wantPrice := 98.722949832643
	wantEquity := 10021.649050315697

	if math.Abs(st.Cash-wantCash) > 1e-9 {
		t.Errorf("final cash = %.12f, want %.12f", st.Cash, wantCash)
	}
	if math.Abs(st.Inventory-wantInv) > 1e-9 {
		t.Errorf("final inventory = %.12f, want %.12f", st.Inventory, wantInv)
	}
	if math.Abs(st.LastPrice-wantPrice) > 1e-9 {
		t.Errorf("final price = %.12f, want %.12f", st.LastPrice, wantPrice)
	}
	if math.Abs(st.Equity()-wantEquity) > 1e-6 {
		t.Errorf("final equity = %.6f, want %.6f", st.Equity(), wantEquity)
	}

	// Also sanity check the last turn's summary makes basic sense
	if lastResult.Summary.OrdersReceived < 0 || lastResult.Summary.OrdersReceived > cfg.MaxOrdersPerTurn {
		t.Errorf("orders received out of range: %d", lastResult.Summary.OrdersReceived)
	}
}
