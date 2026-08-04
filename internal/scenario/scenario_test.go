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
	wantInformedConfig := exchange.Config{
		Instrument:           "SIM",
		StartingCash:         fixed.Money(10_000_000_000_000),
		StartingPosition:     0,
		StartingMark:         fixed.Price(1_000_000),
		StoragePerUnit:       fixed.Price(10_000),
		NumTurns:             8,
		InitialMarginBps:     5_000,
		MaintenanceMarginBps: 2_500,
		MaxPosition:          fixed.Qty(10_000_000),
		MaxOrdersPerTurn:     4,
		MaxOrderQty:          fixed.Qty(120_000),
		MaxFlowSlippageBps:   200,
		MinMoveBps:           -300,
		MaxMoveBps:           425,
		Seed:                 1,
		SimulationVersion:    exchange.SimulationVersionAdverseSelection,
		InformedFlowBps:      6_000,
	}
	if !reflect.DeepEqual(informed.Config, wantInformedConfig) {
		t.Fatalf("informed config=%+v", informed.Config)
	}
	tutorialText := informed.Briefing
	for _, step := range informed.Tutorial {
		tutorialText += " " + step.Body
	}
	for _, text := range []string{"direction", "not its magnitude", "overpay", "negative result", "zero means", "positive result"} {
		if !strings.Contains(tutorialText, text) {
			t.Fatalf("informed tutorial does not explain %q: %s", text, tutorialText)
		}
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
	containedBuy := buyFill
	containedBuy.Summary.InformedFlowPnL = fixed.Money(10_000_000)
	matchedBuy := buyFill
	matchedBuy.Summary.InformedFlowPnL = 0

	for _, test := range []struct {
		name       string
		result     exchange.Result
		wantCode   string
		wantTitle  string
		wantInBody []string
	}{
		{name: "informed buy", result: buyFill, wantCode: "informed-buy-filled", wantTitle: "You sold before the rise", wantInBody: []string{"1.0000", "-0.10000000", "value conceded"}},
		{name: "informed sell", result: sellFill, wantCode: "informed-sell-filled", wantTitle: "You bought before the fall", wantInBody: []string{"2.0000", "-0.20000000", "value conceded"}},
		{name: "avoided informed order", result: avoided, wantCode: "informed-flow-avoided", wantInBody: []string{"0.0000", "0.00000000"}},
		{name: "contained informed buy", result: containedBuy, wantCode: "informed-buy-contained", wantTitle: "Your ask contained the informed buy", wantInBody: []string{"0.10000000", "not by how much", "overpaid"}},
		{name: "matched informed buy", result: matchedBuy, wantCode: "informed-buy-contained", wantTitle: "Your ask contained the informed buy", wantInBody: []string{"0.00000000", "no measured informed edge"}},
		{name: "ordinary flow", result: ordinary, wantCode: "ordinary-flow"},
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

func TestInformedFlowCoachingUsesNoInformedCounterfactual(t *testing.T) {
	for _, test := range []struct {
		name   string
		before fixed.Qty
		result exchange.Result
		want   string
	}{
		{
			name:   "mixed flow informed buy adds short risk",
			before: fixed.Qty(50_000),
			result: exchange.Result{
				State:   exchange.State{Position: fixed.Qty(-30_000), Mark: fixed.Price(101_000)},
				Summary: exchange.Summary{UnitsTraded: fixed.Qty(80_000), InformedOrders: 1, InformedOrdersFilled: 1, InformedUnitsTraded: fixed.Qty(10_000), InformedFlowPnL: fixed.Money(-10_000_000)},
				Events: []exchange.Event{
					{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.FlowAccount, SellerID: exchange.PlayerAccount, Quantity: fixed.Qty(70_000)}},
					{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.FlowAccount, SellerID: exchange.PlayerAccount, Quantity: fixed.Qty(10_000), Informed: true}},
				},
			},
			want: "increased inventory risk to short 3.0000 units",
		},
		{
			name:   "mixed flow informed sell reduces short risk",
			before: fixed.Qty(-10_000),
			result: exchange.Result{
				State:   exchange.State{Position: fixed.Qty(-30_000), Mark: fixed.Price(99_000)},
				Summary: exchange.Summary{UnitsTraded: fixed.Qty(60_000), InformedOrders: 1, InformedOrdersFilled: 1, InformedUnitsTraded: fixed.Qty(20_000), InformedFlowPnL: fixed.Money(-20_000_000)},
				Events: []exchange.Event{
					{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.FlowAccount, SellerID: exchange.PlayerAccount, Quantity: fixed.Qty(40_000)}},
					{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.PlayerAccount, SellerID: exchange.FlowAccount, Quantity: fixed.Qty(20_000), Informed: true}},
				},
			},
			want: "reduced the prior short inventory from 5.0000 to 3.0000 units",
		},
		{
			name:   "mixed flow informed buy flips long to short",
			before: fixed.Qty(-50_000),
			result: exchange.Result{
				State:   exchange.State{Position: fixed.Qty(-10_000), Mark: fixed.Price(101_000)},
				Summary: exchange.Summary{UnitsTraded: fixed.Qty(80_000), InformedOrders: 1, InformedOrdersFilled: 1, InformedUnitsTraded: fixed.Qty(20_000), InformedFlowPnL: fixed.Money(-20_000_000)},
				Events: []exchange.Event{
					{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.PlayerAccount, SellerID: exchange.FlowAccount, Quantity: fixed.Qty(60_000)}},
					{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.FlowAccount, SellerID: exchange.PlayerAccount, Quantity: fixed.Qty(20_000), Informed: true}},
				},
			},
			want: "flipped inventory from long 1.0000 units to short 1.0000 units",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Coach(Snapshot{ID: "volatility-shock-v2"}, exchange.State{Position: test.before, Mark: fixed.Price(100_000)}, test.result)
			if !strings.Contains(got.Body, test.want) {
				t.Fatalf("coaching=%+v", got)
			}
		})
	}
}

func TestInformedFlowTerminalCoachingRetainsEvidenceWithoutNextTurnAdvice(t *testing.T) {
	adverse := exchange.Result{
		State:   exchange.State{Position: fixed.Qty(-10_000), Mark: fixed.Price(101_000), IsOver: true, Reason: exchange.TurnsComplete},
		Summary: exchange.Summary{InformedOrders: 1, InformedOrdersFilled: 1, InformedUnitsTraded: fixed.Qty(10_000), InformedFlowPnL: fixed.Money(-10_000_000)},
		Events:  []exchange.Event{{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.FlowAccount, SellerID: exchange.PlayerAccount, Quantity: fixed.Qty(10_000), Informed: true}}},
	}
	contained := adverse
	contained.Summary.InformedFlowPnL = fixed.Money(10_000_000)
	avoided := exchange.Result{
		State:   exchange.State{IsOver: true, Reason: exchange.TurnsComplete},
		Summary: exchange.Summary{InformedOrders: 1},
		Events:  []exchange.Event{{Type: "flow_order", Order: &exchange.Order{AccountID: exchange.FlowAccount, Side: exchange.Buy, Informed: true}}},
	}
	ordinary := exchange.Result{
		State:   exchange.State{IsOver: true, Reason: exchange.TurnsComplete},
		Summary: exchange.Summary{UnitsTraded: fixed.Qty(10_000)},
		Events:  []exchange.Event{{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.PlayerAccount, SellerID: exchange.FlowAccount, Quantity: fixed.Qty(10_000)}}},
	}
	for _, test := range []struct {
		name     string
		result   exchange.Result
		wantCode string
		evidence string
	}{
		{name: "adverse fill", result: adverse, wantCode: "informed-buy-filled", evidence: "value conceded"},
		{name: "contained fill", result: contained, wantCode: "informed-buy-contained", evidence: "overpaid"},
		{name: "avoided flow", result: avoided, wantCode: "informed-flow-avoided", evidence: "none filled"},
		{name: "ordinary flow", result: ordinary, wantCode: "ordinary-flow", evidence: "No measured informed evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Coach(Snapshot{ID: "volatility-shock-v2"}, exchange.State{Mark: fixed.Price(100_000)}, test.result)
			if got.Code != test.wantCode || !strings.Contains(got.Body, test.evidence) {
				t.Fatalf("coaching=%+v", got)
			}
			body := strings.ToLower(got.Body)
			for _, instruction := range []string{"consider", "keep testing", "compare", "decide", "next quote", "next turn", "invite more"} {
				if strings.Contains(body, instruction) {
					t.Fatalf("terminal coaching contains next-turn instruction %q: %s", instruction, got.Body)
				}
			}
		})
	}
}

func TestQuitCoachingReviewsPriorTurns(t *testing.T) {
	result := exchange.Result{State: exchange.State{IsOver: true, Reason: exchange.PlayerQuit}}
	for _, scenarioID := range []string{"first-spread-v1", "inventory-pressure-v1", "volatility-shock-v1", "volatility-shock-v2"} {
		t.Run(scenarioID, func(t *testing.T) {
			got := Coach(Snapshot{ID: scenarioID}, exchange.State{}, result)
			if got.Code != "player-quit" || !strings.Contains(got.Body, "You ended the session") || !strings.Contains(got.Body, "prior turns") {
				t.Fatalf("coaching=%+v", got)
			}
			for _, incorrect := range []string{"final turn", "No customer fill", "no customer fill"} {
				if strings.Contains(got.Body, incorrect) {
					t.Fatalf("quit coaching describes a turn that did not happen: %+v", got)
				}
			}
		})
	}
}

func TestInformedFlowNaturalTerminalNoFillCoaching(t *testing.T) {
	for _, reason := range []exchange.EndReason{exchange.TurnsComplete, exchange.MarginBreach, exchange.Insolvent} {
		t.Run(string(reason), func(t *testing.T) {
			result := exchange.Result{State: exchange.State{IsOver: true, Reason: reason}}
			got := Coach(Snapshot{ID: "volatility-shock-v2"}, exchange.State{}, result)
			if got.Code != "terminal" || !strings.Contains(got.Body, "no customer fill on the final turn") || strings.Contains(got.Body, "You ended the session") {
				t.Fatalf("coaching=%+v", got)
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

func TestInformedScenarioActivePolicyBeatsAbstentionBenchmark(t *testing.T) {
	definition, ok := Get("volatility-shock-v2")
	if !ok {
		t.Fatal("missing informed-flow scenario")
	}
	type benchmarkResult struct {
		recap             *Recap
		informedBuys      int
		informedSells     int
		informedBuyFills  int
		informedSellFills int
	}
	run := func(t *testing.T, bidFactor, askFactor int64) benchmarkResult {
		t.Helper()
		engine, err := exchange.New(definition.Config)
		if err != nil {
			t.Fatal(err)
		}
		results := make([]exchange.Result, 0, definition.Config.NumTurns)
		benchmark := benchmarkResult{}
		for !engine.State().IsOver {
			mark := engine.State().Mark
			bid, err := fixed.ScalePrice(mark, bidFactor, 10_000)
			if err != nil {
				t.Fatal(err)
			}
			ask, err := fixed.ScalePrice(mark, askFactor, 10_000)
			if err != nil {
				t.Fatal(err)
			}
			result, err := engine.SubmitQuote(bid, ask)
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, result)
			for _, event := range result.Events {
				if event.Type == "flow_order" && event.Order != nil && event.Order.Informed {
					if event.Order.Side == exchange.Buy {
						benchmark.informedBuys++
					} else {
						benchmark.informedSells++
					}
				}
				if event.Type != "trade" || event.Trade == nil || !event.Trade.Informed {
					continue
				}
				if event.Trade.SellerID == exchange.PlayerAccount {
					benchmark.informedBuyFills++
				}
				if event.Trade.BuyerID == exchange.PlayerAccount {
					benchmark.informedSellFills++
				}
			}
		}
		final := results[len(results)-1]
		benchmark.recap, err = BuildRecap(definition.Snapshot(), definition.Config, results[:len(results)-1], final)
		if err != nil {
			t.Fatal(err)
		}
		return benchmark
	}

	// Both policies use only the current public mark. The active policy quotes
	// symmetrically at 150 bps; the wide policy intentionally abstains.
	active := run(t, 9_850, 10_150)
	abstention := run(t, 5_000, 15_000)
	t.Logf("active: pnl=%s volume=%s informed=%d/%d fills=%d/%d; abstention: pnl=%s volume=%s", active.recap.TotalPnL, active.recap.UnitsTraded, active.informedBuys, active.informedSells, active.informedBuyFills, active.informedSellFills, abstention.recap.TotalPnL, abstention.recap.UnitsTraded)
	if active.recap.UnitsTraded <= 0 || active.recap.TotalPnL <= 0 {
		t.Fatalf("active policy did not make a profitable market: %+v", active.recap)
	}
	if active.informedBuys == 0 || active.informedSells == 0 || active.informedBuyFills == 0 || active.informedSellFills == 0 {
		t.Fatalf("active policy missed informed-flow coverage: buys=%d sells=%d buy fills=%d sell fills=%d", active.informedBuys, active.informedSells, active.informedBuyFills, active.informedSellFills)
	}
	if abstention.recap.UnitsTraded != 0 || abstention.recap.TotalPnL != 0 || active.recap.TotalPnL <= abstention.recap.TotalPnL {
		t.Fatalf("benchmark active=%+v abstention=%+v", active.recap, abstention.recap)
	}
	if active.recap.Scorecard == nil {
		t.Fatal("active benchmark has no scorecard")
	}
	for _, context := range []string{"total P&L", "matched volume"} {
		if !strings.Contains(active.recap.Scorecard.FocusNote, context) {
			t.Fatalf("scorecard note does not contextualize informed P&L with %q: %s", context, active.recap.Scorecard.FocusNote)
		}
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
			UnitsTraded: fixed.Qty(10_000), InformedOrders: 2, InformedOrdersFilled: 1, InformedUnitsTraded: fixed.Qty(10_000), InformedFlowPnL: fixed.Money(-100_000_000),
		},
	}}
	final := exchange.Result{
		State: exchange.State{Cash: fixed.Money(10_000_000_000), Mark: fixed.Price(100_000), Reason: exchange.TurnsComplete},
		Summary: exchange.Summary{
			UnitsTraded: fixed.Qty(20_000), InformedOrders: 3, InformedOrdersFilled: 2, InformedUnitsTraded: fixed.Qty(20_000), InformedFlowPnL: fixed.Money(50_000_000),
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
		{name: "informed quantity", prior: exchange.Summary{UnitsTraded: fixed.Qty(math.MaxInt64), InformedUnitsTraded: fixed.Qty(math.MaxInt64)}, latest: exchange.Summary{UnitsTraded: 1, InformedUnitsTraded: 1}},
		{name: "informed order count", prior: exchange.Summary{InformedOrders: maxInt}, latest: exchange.Summary{InformedOrders: 1}},
		{name: "informed filled count", prior: exchange.Summary{InformedOrders: maxInt, InformedOrdersFilled: maxInt}, latest: exchange.Summary{InformedOrders: 1, InformedOrdersFilled: 1}},
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

func TestBuildRecapRejectsNegativeInformedMetrics(t *testing.T) {
	snapshot := Snapshot{ID: "volatility-shock-v2"}
	cfg := exchange.Config{StartingCash: fixed.Money(1), StartingMark: fixed.Price(1)}
	for _, test := range []struct {
		name    string
		summary exchange.Summary
	}{
		{name: "order count", summary: exchange.Summary{InformedOrders: -1}},
		{name: "filled count", summary: exchange.Summary{InformedOrdersFilled: -1}},
		{name: "units", summary: exchange.Summary{InformedUnitsTraded: fixed.Qty(-1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BuildRecap(snapshot, cfg, nil, exchange.Result{Summary: test.summary}); err == nil {
				t.Fatal("BuildRecap accepted a negative informed metric")
			}
		})
	}
}

func TestBuildRecapRejectsContradictoryMetrics(t *testing.T) {
	cfg := exchange.Config{StartingCash: fixed.Money(1), StartingMark: fixed.Price(1)}
	for _, test := range []struct {
		name    string
		summary exchange.Summary
	}{
		{name: "filled informed orders exceed arrivals", summary: exchange.Summary{InformedOrders: 1, InformedOrdersFilled: 2}},
		{name: "informed units exceed player units", summary: exchange.Summary{UnitsTraded: fixed.Qty(10_000), InformedOrders: 1, InformedOrdersFilled: 1, InformedUnitsTraded: fixed.Qty(20_000)}},
		{name: "negative total units", summary: exchange.Summary{UnitsTraded: fixed.Qty(-1)}},
		{name: "negative storage", summary: exchange.Summary{StorageCost: fixed.Money(-1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BuildRecap(Snapshot{}, cfg, nil, exchange.Result{Summary: test.summary}); err == nil {
				t.Fatal("BuildRecap accepted contradictory metrics")
			}
		})
	}
}

func TestBuildRecapRejectsDuplicateFinalVersion(t *testing.T) {
	cfg := exchange.Config{StartingCash: fixed.Money(1), StartingMark: fixed.Price(1)}
	recorded := exchange.Result{State: exchange.State{Version: 8}}
	final := exchange.Result{State: exchange.State{Version: 8}}
	if _, err := BuildRecap(Snapshot{}, cfg, []exchange.Result{recorded}, final); err == nil {
		t.Fatal("BuildRecap accepted a final result already present in records")
	}
}

func TestBuildRecapRejectsDuplicateRecordedVersion(t *testing.T) {
	cfg := exchange.Config{StartingCash: fixed.Money(1), StartingMark: fixed.Price(1)}
	records := []exchange.Result{{State: exchange.State{Version: 8}}, {State: exchange.State{Version: 8}}}
	if _, err := BuildRecap(Snapshot{}, cfg, records, exchange.Result{State: exchange.State{Version: 9}}); err == nil {
		t.Fatal("BuildRecap accepted duplicate versions within records")
	}
}

func TestBuildRecapAllowsQuitFinalAndVersionZeroFixtures(t *testing.T) {
	cfg := exchange.Config{StartingCash: fixed.Money(1), StartingMark: fixed.Price(1)}
	recorded := exchange.Result{State: exchange.State{Version: 1, Cash: fixed.Money(1), Mark: fixed.Price(1)}}
	quit := exchange.Result{State: exchange.State{Version: 2, Cash: fixed.Money(1), Mark: fixed.Price(1), IsOver: true, Reason: exchange.PlayerQuit}}
	if _, err := BuildRecap(Snapshot{}, cfg, []exchange.Result{recorded}, quit); err != nil {
		t.Fatalf("BuildRecap rejected a distinct quit result: %v", err)
	}
	zero := exchange.Result{State: exchange.State{Cash: fixed.Money(1), Mark: fixed.Price(1)}}
	if _, err := BuildRecap(Snapshot{}, cfg, []exchange.Result{zero, zero}, zero); err != nil {
		t.Fatalf("BuildRecap rejected version-zero fixtures: %v", err)
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
