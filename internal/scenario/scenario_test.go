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
	if first.Revision != "2" || len(first.Snapshot().Tutorial) != 4 || first.Snapshot().Reflection == "" || first.Snapshot().ScorecardKind != "matched_volume" {
		t.Fatalf("first tutorial=%+v", first.Snapshot().Tutorial)
	}
	inventory, ok := Get("inventory-pressure-v1")
	if !ok || inventory.Revision != "2" || len(inventory.Snapshot().Tutorial) != 5 || inventory.Snapshot().Reflection == "" || inventory.Snapshot().ScorecardKind != "peak_inventory" {
		t.Fatalf("inventory tutorial=%+v", inventory.Snapshot().Tutorial)
	}
	volatility, ok := Get("volatility-shock-v1")
	if !ok || volatility.Revision != "2" || len(volatility.Snapshot().Tutorial) != 5 || volatility.Snapshot().Reflection == "" || volatility.Snapshot().ScorecardKind != "adverse_selection_turns" {
		t.Fatalf("volatility tutorial=%+v", volatility.Snapshot().Tutorial)
	}
}

func TestCatalogTutorialsAreCloned(t *testing.T) {
	definition, ok := Get("first-spread-v1")
	if !ok {
		t.Fatal("missing first scenario")
	}
	definition.Tutorial[0].Title = "mutated"
	definition.Tutorial = definition.Tutorial[:1]

	fresh, ok := Get("first-spread-v1")
	if !ok || len(fresh.Tutorial) != 4 || fresh.Tutorial[0].Title == "mutated" {
		t.Fatalf("catalog was mutated: %+v", fresh.Tutorial)
	}

	snapshot := fresh.Snapshot()
	snapshot.Tutorial[0].Title = "mutated snapshot"
	if fresh.Tutorial[0].Title == "mutated snapshot" {
		t.Fatalf("snapshot aliases definition tutorial: %+v", fresh.Tutorial)
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

func TestCoachingHandlesMinInt64Inventory(t *testing.T) {
	result := exchange.Result{State: exchange.State{Position: fixed.Qty(math.MinInt64), Mark: fixed.Price(1_000_000)}}
	if got := Coach(Snapshot{ID: "inventory-pressure-v1"}, exchange.State{Mark: fixed.Price(1_000_000)}, result); got.Code != "short-skew" || got.Body == "" {
		t.Fatalf("coaching=%+v", got)
	}
}

func TestVolatilityShockCoachingPrioritizesProtection(t *testing.T) {
	before := exchange.State{Mark: fixed.Price(1_000_000)}
	result := exchange.Result{State: exchange.State{Position: fixed.Qty(20_000), Mark: fixed.Price(990_000)}, Summary: exchange.Summary{UnitsTraded: fixed.Qty(20_000)}}
	if got := Coach(Snapshot{ID: "volatility-shock-v1"}, before, result); got.Code != "long-adverse-selection" {
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

func TestBuildRecapPropagatesUnrepresentableMagnitudeAndPnL(t *testing.T) {
	snapshot := Snapshot{Objective: "Test arithmetic failures."}
	base := exchange.Config{StartingCash: fixed.Money(1), StartingMark: 0}
	for _, test := range []struct {
		name string
		cfg  exchange.Config
	}{
		{name: "starting inventory magnitude", cfg: exchange.Config{StartingCash: fixed.Money(1), StartingMark: 0, StartingPosition: fixed.Qty(math.MinInt64)}},
		{name: "PnL subtraction", cfg: exchange.Config{StartingCash: fixed.Money(math.MinInt64), StartingMark: 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BuildRecap(snapshot, test.cfg, nil, exchange.Result{}); err == nil {
				t.Fatal("BuildRecap accepted unrepresentable arithmetic")
			}
		})
	}

	if _, err := BuildRecap(snapshot, base, nil, exchange.Result{Events: []exchange.Event{{Trade: &exchange.Trade{SellerID: exchange.PlayerAccount, Quantity: fixed.Qty(math.MinInt64)}}}}); err == nil {
		t.Fatal("BuildRecap accepted unrepresentable trade subtraction")
	}
	if _, err := BuildRecap(snapshot, base, nil, exchange.Result{State: exchange.State{Position: fixed.Qty(math.MinInt64)}}); err == nil {
		t.Fatal("BuildRecap accepted unrepresentable final inventory magnitude")
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
	if recap.Scorecard == nil || recap.Scorecard.FocusLabel != "Matched volume" || recap.Scorecard.FocusValue != recap.UnitsTraded.String() {
		t.Fatalf("scorecard=%+v", recap.Scorecard)
	}
}

func TestVolatilityScorecardCountsAdverseSelection(t *testing.T) {
	cfg := exchange.Config{StartingCash: fixed.Money(10_000_000_000), StartingMark: fixed.Price(100_000)}
	final := exchange.Result{State: exchange.State{Cash: fixed.Money(9_000_000_000), Position: fixed.Qty(10_000), Mark: fixed.Price(90_000)}, Summary: exchange.Summary{UnitsTraded: fixed.Qty(10_000)}}
	recap, err := BuildRecap(Snapshot{ID: "volatility-shock-v1", Reflection: "Reflect."}, cfg, nil, final)
	if err != nil {
		t.Fatal(err)
	}
	if recap.AdverseSelectionTurns != 1 || recap.Scorecard == nil || recap.Scorecard.FocusLabel != "Adverse selection turns" || recap.Scorecard.FocusValue != "1" {
		t.Fatalf("recap=%+v", recap)
	}
	reducedCfg := cfg
	reducedCfg.StartingPosition = fixed.Qty(20_000)
	reduced, err := BuildRecap(Snapshot{ID: "volatility-shock-v1"}, reducedCfg, nil, final)
	if err != nil {
		t.Fatal(err)
	}
	if reduced.AdverseSelectionTurns != 0 {
		t.Fatalf("risk-reducing fill counted as adverse=%+v", reduced)
	}
	flippedCfg := cfg
	flippedCfg.StartingPosition = fixed.Qty(10_000)
	flipped := final
	flipped.State.Position = fixed.Qty(-10_000)
	flipped.State.Mark = fixed.Price(110_000)
	flippedRecap, err := BuildRecap(Snapshot{ID: "volatility-shock-v1"}, flippedCfg, nil, flipped)
	if err != nil {
		t.Fatal(err)
	}
	if flippedRecap.AdverseSelectionTurns != 1 {
		t.Fatalf("sign-flip risk not counted=%+v", flippedRecap)
	}
	before := exchange.State{Position: fixed.Qty(20_000), Mark: fixed.Price(100_000)}
	if got := Coach(Snapshot{ID: "volatility-shock-v1"}, before, final); got.Code != "shock-long-protect" {
		t.Fatalf("risk-reducing coaching=%+v", got)
	}
}

func TestLegacyScorecardFallbacks(t *testing.T) {
	cfg := exchange.Config{StartingCash: fixed.Money(10_000_000_000), StartingMark: fixed.Price(100_000)}
	final := exchange.Result{State: exchange.State{Cash: fixed.Money(10_000_000_000), Mark: fixed.Price(100_000)}}
	for _, test := range []struct {
		id    string
		label string
	}{
		{id: "first-spread-v1", label: "Matched volume"},
		{id: "inventory-pressure-v1", label: "Peak inventory"},
		{id: "volatility-shock-v1", label: "Adverse selection turns"},
	} {
		t.Run(test.id, func(t *testing.T) {
			recap, err := BuildRecap(Snapshot{ID: test.id}, cfg, nil, final)
			if err != nil {
				t.Fatal(err)
			}
			if recap.Scorecard == nil || recap.Scorecard.FocusLabel != test.label || recap.Scorecard.Reflection == "" {
				t.Fatalf("scorecard=%+v", recap.Scorecard)
			}
		})
	}
	unknown, err := BuildRecap(Snapshot{ID: "unknown"}, cfg, nil, final)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Scorecard != nil {
		t.Fatalf("unexpected scorecard=%+v", unknown.Scorecard)
	}
}
