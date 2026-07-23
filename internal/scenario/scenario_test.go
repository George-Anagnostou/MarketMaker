package scenario

import (
	"testing"

	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
)

func TestCatalogIsValidAndStable(t *testing.T) {
	if err := ValidateCatalog(); err != nil {
		t.Fatal(err)
	}
	if len(List()) < 3 {
		t.Fatal("expected initial lesson catalog")
	}
	if _, ok := Get("first-spread-v1"); !ok {
		t.Fatal("missing first scenario")
	}
}

func TestCoachPrioritizesInventory(t *testing.T) {
	before := exchange.State{Position: 0, Mark: fixed.Price(1_000_000)}
	result := exchange.Result{State: exchange.State{Position: fixed.Qty(10_000), Mark: fixed.Price(1_000_000)}}
	if got := Coach(before, result); got.Code != "inventory-built" {
		t.Fatalf("coaching=%+v", got)
	}
}

func TestBuildRecapIncludesTerminalTurn(t *testing.T) {
	snapshot := Snapshot{Objective: "Test the final turn."}
	cfg := exchange.Config{StartingCash: fixed.Money(10_000_000_000), StartingMark: fixed.Price(100_000)}
	records := []exchange.Result{{
		State:   exchange.State{Position: fixed.Qty(-10_000)},
		Summary: exchange.Summary{UnitsTraded: fixed.Qty(10_000), StorageCost: fixed.Money(100_000_000)},
	}}
	final := exchange.Result{
		State:   exchange.State{Cash: fixed.Money(8_000_000_000), Position: fixed.Qty(20_000), Mark: fixed.Price(150_000), Reason: exchange.PlayerQuit},
		Summary: exchange.Summary{UnitsTraded: fixed.Qty(20_000), StorageCost: fixed.Money(200_000_000)},
	}
	recap := BuildRecap(snapshot, cfg, records, final)
	if recap.FinalEquity != fixed.Money(11_000_000_000) || recap.TotalPnL != fixed.Money(1_000_000_000) {
		t.Fatalf("equity=%s pnl=%s", recap.FinalEquity, recap.TotalPnL)
	}
	if recap.MaxAbsInventory != fixed.Qty(20_000) || recap.UnitsTraded != fixed.Qty(30_000) || recap.StoragePaid != fixed.Money(300_000_000) {
		t.Fatalf("recap=%+v", recap)
	}
}
