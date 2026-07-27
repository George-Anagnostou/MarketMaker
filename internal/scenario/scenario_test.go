package scenario

import (
	"math"
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
	first, ok := Get("first-spread-v1")
	if !ok {
		t.Fatal("missing first scenario")
	}
	if first.Revision != "2" || len(first.Snapshot().Tutorial) != 4 || first.Snapshot().Reflection == "" {
		t.Fatalf("first tutorial=%+v", first.Snapshot().Tutorial)
	}
	inventory, ok := Get("inventory-pressure-v1")
	if !ok || inventory.Revision != "2" || len(inventory.Snapshot().Tutorial) != 5 || inventory.Snapshot().Reflection == "" {
		t.Fatalf("inventory tutorial=%+v", inventory.Snapshot().Tutorial)
	}
}

func TestCoachPrioritizesInventory(t *testing.T) {
	before := exchange.State{Position: 0, Mark: fixed.Price(1_000_000)}
	result := exchange.Result{State: exchange.State{Position: fixed.Qty(10_000), Mark: fixed.Price(1_000_000)}}
	if got := Coach(Snapshot{ID: "first-spread-v1"}, before, result); got.Code != "inventory-built" {
		t.Fatalf("coaching=%+v", got)
	}
}

func TestInventoryPressureCoachingSkewsInventory(t *testing.T) {
	result := exchange.Result{State: exchange.State{Position: fixed.Qty(20_000), Mark: fixed.Price(1_000_000)}}
	if got := Coach(Snapshot{ID: "inventory-pressure-v1"}, exchange.State{Mark: fixed.Price(1_000_000)}, result); got.Code != "long-skew" {
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
	recap, err := BuildRecap(snapshot, cfg, records, final)
	if err != nil {
		t.Fatal(err)
	}
	if recap.FinalEquity != fixed.Money(11_000_000_000) || recap.TotalPnL != fixed.Money(1_000_000_000) {
		t.Fatalf("equity=%s pnl=%s", recap.FinalEquity, recap.TotalPnL)
	}
	if recap.MaxAbsInventory != fixed.Qty(20_000) || recap.UnitsTraded != fixed.Qty(30_000) || recap.StoragePaid != fixed.Money(300_000_000) {
		t.Fatalf("recap=%+v", recap)
	}
}

func TestBuildRecapTracksIntraTurnInventoryAndRejectsOverflow(t *testing.T) {
	snapshot := Snapshot{Objective: "Test peak inventory."}
	cfg := exchange.Config{StartingCash: fixed.Money(10_000_000_000), StartingMark: fixed.Price(100_000)}
	final := exchange.Result{
		State: exchange.State{Cash: fixed.Money(10_000_000_000), Mark: fixed.Price(100_000), Reason: exchange.TurnsComplete},
		Events: []exchange.Event{
			{Trade: &exchange.Trade{BuyerID: exchange.PlayerAccount, Quantity: fixed.Qty(20_000)}},
			{Trade: &exchange.Trade{SellerID: exchange.PlayerAccount, Quantity: fixed.Qty(20_000)}},
		},
	}
	recap, err := BuildRecap(snapshot, cfg, nil, final)
	if err != nil {
		t.Fatal(err)
	}
	if recap.MaxAbsInventory != fixed.Qty(20_000) {
		t.Fatalf("peak inventory=%s", recap.MaxAbsInventory)
	}
	_, err = BuildRecap(snapshot, cfg, []exchange.Result{{Summary: exchange.Summary{UnitsTraded: fixed.Qty(math.MaxInt64)}}}, exchange.Result{Summary: exchange.Summary{UnitsTraded: 1}})
	if err == nil {
		t.Fatal("expected recap overflow")
	}
}

func TestBuildRecapIncludesEngineProducedTerminalTurn(t *testing.T) {
	definition, ok := Get("first-spread-v1")
	if !ok {
		t.Fatal("missing first scenario")
	}
	cfg := definition.Config
	cfg.NumTurns = 1
	engine, err := exchange.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Execute(exchange.Command{ID: "terminal-turn", Type: exchange.CommandSubmitQuote, Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.State.IsOver || result.State.Reason != exchange.TurnsComplete {
		t.Fatalf("terminal result=%+v", result.State)
	}
	recap, err := BuildRecap(definition.Snapshot(), cfg, nil, result)
	if err != nil {
		t.Fatal(err)
	}
	if recap.UnitsTraded != result.Summary.UnitsTraded || recap.StoragePaid != result.Summary.StorageCost || recap.MaxAbsInventory < fixed.AbsQty(result.State.Position) {
		t.Fatalf("recap=%+v result=%+v", recap, result.Summary)
	}
}
