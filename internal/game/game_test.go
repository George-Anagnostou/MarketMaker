package game

import (
	"math"
	"testing"
)

func TestNewGameDefaults(t *testing.T) {
	cfg := DefaultConfig()
	g := NewGame(cfg)

	st := g.State()
	if st.Turn != 0 {
		t.Errorf("initial turn = %d, want 0", st.Turn)
	}
	if st.Cash != cfg.StartingCash {
		t.Errorf("initial cash = %v, want %v", st.Cash, cfg.StartingCash)
	}
	if st.Inventory != cfg.StartingInventory {
		t.Errorf("initial inventory = %v, want %v", st.Inventory, cfg.StartingInventory)
	}
	if st.LastPrice != cfg.StartingPrice {
		t.Errorf("initial price = %v, want %v", st.LastPrice, cfg.StartingPrice)
	}
	if st.IsOver {
		t.Error("game should not be over at start")
	}
}

func TestSubmitTurnValidation(t *testing.T) {
	g := NewGame(DefaultConfig())

	_, err := g.SubmitTurn(0, 101)
	if err == nil {
		t.Error("expected error for non-positive bid")
	}

	_, err = g.SubmitTurn(99, 0)
	if err == nil {
		t.Error("expected error for non-positive ask")
	}

	_, err = g.SubmitTurn(101, 100)
	if err == nil {
		t.Error("expected error for bid >= ask")
	}
}

func TestBasicTurnExecutionDeterministic(t *testing.T) {
	cfg := TestConfigWithSeed(42)
	g := NewGame(cfg)

	// Post a reasonable spread around 100
	bid, ask := 99.0, 101.0

	res, err := g.SubmitTurn(bid, ask)
	if err != nil {
		t.Fatalf("SubmitTurn error: %v", err)
	}

	st := res.State
	if st.Turn != 1 {
		t.Errorf("turn = %d, want 1", st.Turn)
	}
	if st.LastPrice == cfg.StartingPrice {
		t.Error("price should have moved")
	}
	// With seed 42 and defaults, we can at least assert invariants
	if st.Cash > cfg.StartingCash+10000 || st.Cash < cfg.StartingCash-10000 {
		t.Errorf("cash moved too wildly: %v", st.Cash)
	}
	if len(res.Events) == 0 {
		t.Error("expected some events")
	}
}

func TestBankruptcyEndsGame(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StartingCash = 50
	cfg.StorageCostPerUnit = 0 // isolate from storage
	cfg.NumTurns = 0           // unlimited; we want to hit bankruptcy, not turn limit
	cfg.Seed = 123
	g := NewGame(cfg)

	// High bid near current price (~100) so market sells will hit us and drain cash fast.
	// With small starting cash, a few fills on the bid side will bankrupt.
	for i := 0; i < 20; i++ {
		if g.IsOver() {
			break
		}
		_, _ = g.SubmitTurn(99, 101)
	}

	if !g.IsOver() {
		t.Error("expected game to end in bankruptcy with bad strategy")
	}
	if g.Reason() != "bankrupt" {
		t.Errorf("reason = %q, want bankrupt", g.Reason())
	}
}

func TestFiniteTurnsComplete(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NumTurns = 3
	cfg.Seed = 7
	g := NewGame(cfg)

	// Play conservatively with no inventory build
	for i := 0; i < 10; i++ {
		if g.IsOver() {
			break
		}
		_, err := g.SubmitTurn(99, 101)
		if err != nil {
			t.Fatal(err)
		}
	}

	if !g.IsOver() {
		t.Error("game should be over after exactly configured turns")
	}
	if g.Reason() != "turns_complete" {
		t.Errorf("reason = %q, want turns_complete", g.Reason())
	}
	if g.State().Turn != 3 {
		t.Errorf("final turn = %d, want 3", g.State().Turn)
	}
}

func TestUnlimitedModeDoesNotEndOnTurns(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NumTurns = 0
	cfg.Seed = 99
	g := NewGame(cfg)

	for i := 0; i < 25; i++ {
		if g.IsOver() {
			break
		}
		_, err := g.SubmitTurn(99.5, 100.5)
		if err != nil {
			t.Fatal(err)
		}
	}

	if g.IsOver() {
		t.Error("unlimited game should not end from turn count")
	}
}

func TestEquityCalculation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StartingCash = 1000
	cfg.StartingInventory = 10
	cfg.StartingPrice = 50
	g := NewGame(cfg)

	want := 1000.0 + 10*50.0
	if math.Abs(g.State().Equity()-want) > 1e-9 {
		t.Errorf("equity = %v, want %v", g.State().Equity(), want)
	}
}

func TestPnlIsEquityDelta(t *testing.T) {
	cfg := TestConfigWithSeed(2024)
	g := NewGame(cfg)

	startEquity := g.State().Equity()

	res, err := g.SubmitTurn(99, 101)
	if err != nil {
		t.Fatal(err)
	}

	endEquity := res.State.Equity()
	if math.Abs(res.Summary.TurnPnL-(endEquity-startEquity)) > 1e-6 {
		t.Errorf("TurnPnL %v != equity delta %v", res.Summary.TurnPnL, endEquity-startEquity)
	}
}

func TestStorageIsChargedOnPostFillInventory(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StorageCostPerUnit = 3.0
	cfg.Seed = 42
	cfg.MinPriceMovePct = 0
	cfg.MaxPriceMovePct = 0 // isolate from price move effects on cash accounting
	cfg.MaxOrdersPerTurn = 3
	cfg.MaxOrderSize = 5

	g := NewGame(cfg)

	// Submit a turn; storage is charged on inventory after fills this turn.
	res, err := g.SubmitTurn(99, 101)
	if err != nil {
		t.Fatal(err)
	}

	inv := res.State.Inventory
	expectedStorage := cfg.StorageCostPerUnit * math.Abs(inv)
	if math.Abs(res.Summary.StorageCost-expectedStorage) > 1e-9 {
		t.Errorf("storage charged = %v, want %v (based on end-of-turn inv)", res.Summary.StorageCost, expectedStorage)
	}

	// Cash must reflect: starting cash + net from fills - storage
	startCash := cfg.StartingCash
	wantCash := startCash + res.Summary.NetFillCash - res.Summary.StorageCost
	if math.Abs(res.State.Cash-wantCash) > 1e-9 {
		t.Errorf("cash = %v, want %v", res.State.Cash, wantCash)
	}
}
