package scenario

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
)

func TestCatalogIsValidAndStable(t *testing.T) {
	if err := ValidateCatalog(); err != nil {
		t.Fatal(err)
	}
	if len(List()) != 4 {
		t.Fatal("expected four lessons in catalog")
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
	if volatility.Title != "Volatility Shock" || !reflect.DeepEqual(volatility.Config, config(8, 303, -300, 425, 4, 12)) {
		t.Fatalf("volatility v1 changed: %+v", volatility)
	}
	informed, ok := Get("volatility-shock-v2")
	if !ok || informed.Revision != "1" || informed.Title != "Volatility Shock: Informed Flow" || len(informed.Snapshot().Tutorial) != 5 || informed.Snapshot().Reflection == "" || informed.Snapshot().ScorecardKind != "informed_flow_pnl" {
		t.Fatalf("informed scenario=%+v", informed)
	}
	if informed.Config.NumTurns != 8 || informed.Config.Seed != 304 || informed.Config.SimulationVersion != exchange.SimulationVersionAdverseSelection || informed.Config.InformedFlowBps != 6_000 || informed.Config.MinMoveBps != volatility.Config.MinMoveBps || informed.Config.MaxMoveBps != volatility.Config.MaxMoveBps {
		t.Fatalf("informed config=%+v", informed.Config)
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

func TestInformedFlowCoachingUsesAuthoritativeEvidence(t *testing.T) {
	buyFill := exchange.Result{
		State:   exchange.State{Position: fixed.Qty(-10_000), Mark: fixed.Price(101_000)},
		Summary: exchange.Summary{InformedOrders: 1, InformedOrdersFilled: 1, InformedUnitsTraded: fixed.Qty(10_000), InformedFlowPnL: fixed.Money(-10_000_000)},
		Events:  []exchange.Event{{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.FlowAccount, SellerID: exchange.PlayerAccount, Quantity: fixed.Qty(10_000), Informed: true}}},
	}
	sellFill := exchange.Result{
		State:   exchange.State{Position: fixed.Qty(20_000), Mark: fixed.Price(99_000)},
		Summary: exchange.Summary{InformedOrders: 1, InformedOrdersFilled: 1, InformedUnitsTraded: fixed.Qty(20_000), InformedFlowPnL: fixed.Money(-20_000_000)},
		Events:  []exchange.Event{{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.PlayerAccount, SellerID: exchange.FlowAccount, Quantity: fixed.Qty(20_000), Informed: true}}},
	}
	avoided := exchange.Result{
		Summary: exchange.Summary{InformedOrders: 1},
		Events:  []exchange.Event{{Type: "flow_order", Order: &exchange.Order{AccountID: exchange.FlowAccount, Side: exchange.Buy, Informed: true}}},
	}
	ordinary := exchange.Result{
		State:   exchange.State{Mark: fixed.Price(90_000)},
		Summary: exchange.Summary{UnitsTraded: fixed.Qty(10_000)},
		Events:  []exchange.Event{{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.PlayerAccount, SellerID: exchange.FlowAccount, Quantity: fixed.Qty(10_000)}}},
	}
	terminalBuy := buyFill
	terminalBuy.State.IsOver = true
	terminalBuy.State.Reason = exchange.TurnsComplete
	containedBuy := buyFill
	containedBuy.Summary.InformedFlowPnL = fixed.Money(10_000_000)

	for _, test := range []struct {
		name       string
		result     exchange.Result
		wantCode   string
		wantTitle  string
		wantInBody []string
	}{
		{name: "informed buy", result: buyFill, wantCode: "informed-buy-filled", wantTitle: "You sold before the rise", wantInBody: []string{"1.0000", "-0.10000000"}},
		{name: "informed sell", result: sellFill, wantCode: "informed-sell-filled", wantTitle: "You bought before the fall", wantInBody: []string{"2.0000", "-0.20000000"}},
		{name: "avoided informed order", result: avoided, wantCode: "informed-flow-avoided", wantInBody: []string{"0.0000", "0.00000000"}},
		{name: "contained informed buy", result: containedBuy, wantCode: "informed-buy-contained", wantTitle: "Your ask contained the informed buy", wantInBody: []string{"0.10000000"}},
		{name: "ordinary flow", result: ordinary, wantCode: "ordinary-flow"},
		{name: "terminal informed evidence", result: terminalBuy, wantCode: "informed-buy-filled", wantTitle: "You sold before the rise"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Coach(Snapshot{ID: "volatility-shock-v2"}, exchange.State{Mark: fixed.Price(100_000)}, test.result)
			if got.Code != test.wantCode || test.wantTitle != "" && got.Title != test.wantTitle {
				t.Fatalf("coaching=%+v", got)
			}
			for _, text := range test.wantInBody {
				if !strings.Contains(got.Body, text) {
					t.Fatalf("coaching body %q does not contain %q", got.Body, text)
				}
			}
		})
	}
}

func TestInformedScenarioSeedExercisesBothDirections(t *testing.T) {
	definition, ok := Get("volatility-shock-v2")
	if !ok {
		t.Fatal("missing informed-flow scenario")
	}
	engine, err := exchange.New(definition.Config)
	if err != nil {
		t.Fatal(err)
	}
	informedBuys, informedSells, informedFills := 0, 0, 0
	for !engine.State().IsOver {
		mark := engine.State().Mark
		result, err := engine.SubmitQuote(mark-fixed.Price(5_000), mark+fixed.Price(5_000))
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range result.Events {
			if event.Type == "flow_order" && event.Order != nil && event.Order.Informed {
				if event.Order.Side == exchange.Buy {
					informedBuys++
				} else {
					informedSells++
				}
			}
			if event.Type == "trade" && event.Trade != nil && event.Trade.Informed && (event.Trade.BuyerID == exchange.PlayerAccount || event.Trade.SellerID == exchange.PlayerAccount) {
				informedFills++
			}
		}
	}
	if informedBuys == 0 || informedSells == 0 || informedFills == 0 {
		t.Fatalf("seed did not exercise lesson: buys=%d sells=%d fills=%d", informedBuys, informedSells, informedFills)
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

func TestBuildRecapAggregatesMeasuredInformedFlow(t *testing.T) {
	cfg := exchange.Config{StartingCash: fixed.Money(10_000_000_000), StartingMark: fixed.Price(100_000)}
	records := []exchange.Result{{
		State: exchange.State{Cash: fixed.Money(10_000_000_000), Mark: fixed.Price(100_000)},
		Summary: exchange.Summary{
			InformedOrders: 2, InformedOrdersFilled: 1, InformedUnitsTraded: fixed.Qty(10_000), InformedFlowPnL: fixed.Money(-100_000_000),
		},
	}}
	final := exchange.Result{
		State: exchange.State{Cash: fixed.Money(10_000_000_000), Mark: fixed.Price(100_000), Reason: exchange.TurnsComplete},
		Summary: exchange.Summary{
			InformedOrders: 3, InformedOrdersFilled: 2, InformedUnitsTraded: fixed.Qty(20_000), InformedFlowPnL: fixed.Money(50_000_000),
		},
	}
	recap, err := BuildRecap(Snapshot{ID: "volatility-shock-v2", ScorecardKind: "informed_flow_pnl", Reflection: "Reflect."}, cfg, records, final)
	if err != nil {
		t.Fatal(err)
	}
	if recap.InformedOrders != 5 || recap.InformedOrdersFilled != 3 || recap.InformedUnitsTraded != fixed.Qty(30_000) || recap.InformedFlowPnL != fixed.Money(-50_000_000) || recap.AdverseSelectionTurns != 1 {
		t.Fatalf("recap=%+v", recap)
	}
	if recap.Scorecard == nil || recap.Scorecard.FocusLabel != "Informed-flow P&L" || recap.Scorecard.FocusValue != recap.InformedFlowPnL.String() {
		t.Fatalf("scorecard=%+v", recap.Scorecard)
	}
	for _, text := range []string{"5 informed orders", "3 filled", "3.0000 units", "More negative", "total P&L", "matched volume"} {
		if !strings.Contains(recap.Scorecard.FocusNote, text) {
			t.Fatalf("scorecard note %q does not contain %q", recap.Scorecard.FocusNote, text)
		}
	}
}

func TestBuildRecapRejectsInformedMetricOverflow(t *testing.T) {
	snapshot := Snapshot{ID: "volatility-shock-v2"}
	cfg := exchange.Config{StartingCash: fixed.Money(1), StartingMark: fixed.Price(1)}
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name   string
		prior  exchange.Summary
		latest exchange.Summary
	}{
		{name: "informed PnL", prior: exchange.Summary{InformedFlowPnL: fixed.Money(math.MaxInt64)}, latest: exchange.Summary{InformedFlowPnL: 1}},
		{name: "informed quantity", prior: exchange.Summary{InformedUnitsTraded: fixed.Qty(math.MaxInt64)}, latest: exchange.Summary{InformedUnitsTraded: 1}},
		{name: "informed order count", prior: exchange.Summary{InformedOrders: maxInt}, latest: exchange.Summary{InformedOrders: 1}},
		{name: "informed filled count", prior: exchange.Summary{InformedOrdersFilled: maxInt}, latest: exchange.Summary{InformedOrdersFilled: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BuildRecap(snapshot, cfg, []exchange.Result{{Summary: test.prior}}, exchange.Result{Summary: test.latest})
			if err == nil {
				t.Fatal("BuildRecap accepted overflowing informed metrics")
			}
		})
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

func TestBuildRecapIncludesEngineProducedV2TerminalLesson(t *testing.T) {
	definition, ok := Get("volatility-shock-v2")
	if !ok {
		t.Fatal("missing informed-flow scenario")
	}
	cfg := definition.Config
	cfg.NumTurns = 1
	engine, err := exchange.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.SubmitQuote(fixed.Price(995_000), fixed.Price(1_005_000))
	if err != nil {
		t.Fatal(err)
	}
	if !result.State.IsOver || result.State.Reason != exchange.TurnsComplete || result.Summary.PnLAttribution == nil {
		t.Fatalf("terminal result=%+v summary=%+v", result.State, result.Summary)
	}
	recap, err := BuildRecap(definition.Snapshot(), cfg, nil, result)
	if err != nil {
		t.Fatal(err)
	}
	if recap.InformedOrders != result.Summary.InformedOrders || recap.InformedOrdersFilled != result.Summary.InformedOrdersFilled || recap.InformedUnitsTraded != result.Summary.InformedUnitsTraded || recap.InformedFlowPnL != result.Summary.InformedFlowPnL {
		t.Fatalf("recap=%+v summary=%+v", recap, result.Summary)
	}
	if recap.Scorecard == nil || recap.Scorecard.FocusLabel != "Informed-flow P&L" || recap.Scorecard.FocusValue != result.Summary.InformedFlowPnL.String() {
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
		{id: "volatility-shock-v2", label: "Informed-flow P&L"},
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
