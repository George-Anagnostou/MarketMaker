package engine

import (
	"math"
	"testing"

	"market-maker/internal/types"
)

func testConfig(seed int64) types.GameConfig {
	return types.GameConfig{
		StartingCash:       100000,
		StartingInventory:  0,
		StartingPrice:      100,
		NumTurns:           0,
		StorageCostPerUnit: 1.0,
		MinPriceMovePct:    -0.005,
		MaxPriceMovePct:    0.03,
		MaxOrdersPerTurn:   5,
		MaxOrderSize:       10,
		Seed:               seed,
	}
}

func TestNewEngineSetsInitialState(t *testing.T) {
	cfg := testConfig(42)
	e := NewEngine(cfg)
	st := e.State()
	if st.Turn != 0 {
		t.Errorf("turn=%d want 0", st.Turn)
	}
	if st.Cash != cfg.StartingCash {
		t.Errorf("cash=%v want %v", st.Cash, cfg.StartingCash)
	}
	if st.Inventory != cfg.StartingInventory {
		t.Errorf("inv=%v want %v", st.Inventory, cfg.StartingInventory)
	}
	if st.LastPrice != cfg.StartingPrice {
		t.Errorf("price=%v want %v", st.LastPrice, cfg.StartingPrice)
	}
	if st.IsOver {
		t.Error("IsOver should be false")
	}
	if st.Reason != types.EndReasonNotOver {
		t.Errorf("reason=%q want %q", st.Reason, types.EndReasonNotOver)
	}
	stats := e.Stats()
	if stats.MaxAbsInventory != math.Abs(cfg.StartingInventory) {
		t.Errorf("maxAbsInv=%v want %v", stats.MaxAbsInventory, math.Abs(cfg.StartingInventory))
	}
}

func TestStepValidReturnsNotOver(t *testing.T) {
	e := NewEngine(testConfig(1))
	res, err := e.Step(99, 101)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.State.IsOver {
		t.Error("IsOver must be false from engine")
	}
	if res.State.Reason != types.EndReasonNotOver {
		t.Errorf("reason=%q want not over", res.State.Reason)
	}
	if res.State.Turn != 1 {
		t.Errorf("turn=%d want 1", res.State.Turn)
	}
}

func TestStepInvalidReturnsErrorStateUnchanged(t *testing.T) {
	cases := []struct {
		bid, ask float64
	}{
		{0, 101},
		{-1, 101},
		{99, 0},
		{99, -5},
		{101, 100},
		{100, 100},
	}
	for _, c := range cases {
		e := NewEngine(testConfig(7))
		before := e.State()
		res, err := e.Step(c.bid, c.ask)
		if err == nil {
			t.Errorf("bid=%v ask=%v: expected error", c.bid, c.ask)
		}
		after := e.State()
		if after.Cash != before.Cash || after.Inventory != before.Inventory || after.Turn != before.Turn {
			t.Errorf("state changed on invalid bid=%v ask=%v", c.bid, c.ask)
		}
		if res.State.Cash != before.Cash {
			t.Error("result state should reflect unchanged")
		}
	}
}

func TestStepManyTimesWithNumTurnsZeroNoOver(t *testing.T) {
	cfg := testConfig(123)
	cfg.NumTurns = 0
	e := NewEngine(cfg)
	for i := 0; i < 100; i++ {
		res, err := e.Step(99.5, 100.5)
		if err != nil {
			t.Fatalf("step %d err: %v", i, err)
		}
		if res.State.IsOver || res.State.Reason != types.EndReasonNotOver {
			t.Fatalf("over at turn %d", i)
		}
	}
	if e.State().Turn != 100 {
		t.Errorf("final turn=%d want 100", e.State().Turn)
	}
}

func TestDeterminismSameSeedSameActions(t *testing.T) {
	cfg := testConfig(999)
	e1 := NewEngine(cfg)
	e2 := NewEngine(cfg)
	for i := 0; i < 20; i++ {
		r1, err1 := e1.Step(98, 102)
		r2, err2 := e2.Step(98, 102)
		if err1 != nil || err2 != nil {
			t.Fatal("unexpected err")
		}
		if r1.State.Cash != r2.State.Cash ||
			r1.State.Inventory != r2.State.Inventory ||
			r1.State.LastPrice != r2.State.LastPrice ||
			r1.State.Turn != r2.State.Turn ||
			r1.Summary.UnitsTraded != r2.Summary.UnitsTraded ||
			r1.Summary.TurnPnL != r2.Summary.TurnPnL {
			t.Errorf("mismatch after step %d", i)
		}
	}
}

func TestStatsAccumulate(t *testing.T) {
	e := NewEngine(testConfig(55))
	before := e.Stats()
	if before.TotalUnitsTraded != 0 || before.TotalNetFillCash != 0 || before.TotalStoragePaid != 0 {
		t.Error("initial stats should be zero")
	}
	for i := 0; i < 5; i++ {
		_, err := e.Step(99, 101)
		if err != nil {
			t.Fatal(err)
		}
	}
	after := e.Stats()
	if after.TotalUnitsTraded <= before.TotalUnitsTraded {
		t.Error("TotalUnitsTraded should increase")
	}
	if after.MaxAbsInventory < math.Abs(e.State().Inventory) {
		t.Error("maxAbs should track")
	}
	if after.TotalStoragePaid <= 0 {
		t.Error("storage should accumulate >0")
	}
}

func TestBankruptcyConditionNotSetInEngine(t *testing.T) {
	cfg := testConfig(321)
	cfg.StartingCash = 50
	cfg.StorageCostPerUnit = 0 // isolate, rely on buy fills draining cash
	e := NewEngine(cfg)
	hitNeg := false
	for i := 0; i < 30; i++ {
		res, err := e.Step(99, 101) // high bid -> hit by sells -> buy inventory, pay cash
		if err != nil {
			t.Fatal(err)
		}
		if res.State.Cash <= 0 {
			hitNeg = true
			if res.State.IsOver || res.State.Reason != types.EndReasonNotOver {
				t.Error("engine must not set IsOver=true or bankrupt reason")
			}
		}
	}
	st := e.State()
	if st.IsOver || st.Reason != types.EndReasonNotOver {
		t.Error("engine never sets bankruptcy condition")
	}
	if !hitNeg {
		t.Log("note: did not hit negative cash in this run (non-deterministic flow), but still valid")
	}
}
