package realtime

import (
	"errors"
	"math"
	"testing"
	"time"

	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
	"market-maker/internal/game"
)

func realtimeExchangeConfig() exchange.Config {
	return exchange.Config{
		Instrument:           "SIM",
		StartingCash:         fixed.Money(10_000_000_000_000),
		StartingMark:         fixed.Price(1_000_000),
		StoragePerUnit:       fixed.Price(10_000),
		NumTurns:             8,
		InitialMarginBps:     5_000,
		MaintenanceMarginBps: 2_500,
		MaxPosition:          fixed.Qty(10_000_000),
		MaxOrdersPerTurn:     5,
		MaxOrderQty:          fixed.Qty(100_000),
		MaxFlowSlippageBps:   200,
		MinMoveBps:           -25,
		MaxMoveBps:           25,
		Seed:                 1,
	}
}

func realtimeGeneratorConfig() GeneratorConfig {
	return GeneratorConfig{
		Version:            game.GeneratorVersion,
		Seed:               99,
		Duration:           30 * time.Millisecond,
		CustomerCadence:    Interval{Minimum: 2 * time.Millisecond, Maximum: 4 * time.Millisecond},
		MarkCadence:        Interval{Minimum: 5 * time.Millisecond, Maximum: 7 * time.Millisecond},
		CarryCadence:       10 * time.Millisecond,
		MaxOrderQuantity:   fixed.Qty(100_000),
		MaxFlowSlippageBps: 200,
		MinMoveBps:         -25,
		MaxMoveBps:         25,
		CustomerDomain:     "customer/v1",
		MarkDomain:         "mark/v1",
		InformedDomain:     "informed/v1",
	}
}

func TestGeneratedActionsDriveExchangeToTradingDayExpiry(t *testing.T) {
	engine, err := exchange.New(realtimeExchangeConfig())
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExchangeExecutor(engine, ExchangeExecutorConfig{QuoteQuantity: fixed.Qty(100_000), CarryPerUnit: fixed.Price(10_000)})
	if err != nil {
		t.Fatal(err)
	}
	generator, err := NewGenerator(realtimeGeneratorConfig())
	if err != nil {
		t.Fatal(err)
	}
	clock := newManualClock()
	sequencer, err := NewWithScheduleAndClock(generator, executor.Execute, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	quote := Action{ID: "quote-1", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: UpdateQuotePayload{Bid: fixed.Price(990_000), Ask: fixed.Price(1_010_000)}}
	quoteExecution, err := sequencer.Submit(t.Context(), quote)
	if err != nil || quoteExecution.Sequence != 1 {
		t.Fatalf("quote execution=%+v err=%v", quoteExecution, err)
	}
	clock.Advance(30 * time.Millisecond)
	if _, err := sequencer.Submit(t.Context(), Action{ID: "late", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: UpdateQuotePayload{Bid: fixed.Price(990_000), Ask: fixed.Price(1_010_000)}}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("late quote error=%v", err)
	}
	snapshot, err := sequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusCompleted || snapshot.Elapsed != 30*time.Millisecond {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	state := engine.State()
	if !state.IsOver || state.Reason != exchange.TimeExpired || state.Turn != 0 || state.Version != uint64(snapshot.NextScheduled+1) || state.BestBid != 0 || state.BestAsk != 0 {
		t.Fatalf("state=%+v snapshot=%+v", state, snapshot)
	}
}

func TestExchangeExecutorClassifiesInformedCustomerAgainstUpcomingMark(t *testing.T) {
	engine, err := exchange.New(realtimeExchangeConfig())
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExchangeExecutor(engine, ExchangeExecutorConfig{QuoteQuantity: fixed.Qty(100_000), CarryPerUnit: fixed.Price(10_000), InformedFlowBps: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	quote := executor.Execute(Action{ID: "quote", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: UpdateQuotePayload{Bid: fixed.Price(990_000), Ask: fixed.Price(1_010_000)}}, 0)
	if quote.Err != nil || quote.Disposition != DispositionContinue {
		t.Fatalf("quote=%+v", quote)
	}
	payload := CustomerArrivalPayload{Buy: false, Quantity: fixed.Qty(10_000), SlippageBps: 200, InformedDraw: 0, HasUpcomingMark: true, UpcomingMarkMoveBps: 25}
	outcome := executor.Execute(Action{ID: "customer", Kind: ActionCustomerArrival, Source: SourceSystem, Payload: payload}, time.Millisecond)
	if outcome.Err != nil || outcome.Disposition != DispositionContinue {
		t.Fatalf("outcome=%+v", outcome)
	}
	result := outcome.Result.(exchange.Result)
	if result.State.Position != fixed.Qty(-10_000) || result.Summary.InformedOrders != 1 || result.Summary.InformedOrdersFilled != 1 || result.Summary.InformedFlowPnL != fixed.Money(75_000_000) || len(result.Events) == 0 || result.Events[0].Order == nil || !result.Events[0].Order.Informed || result.Events[0].Order.Side != exchange.Buy {
		t.Fatalf("result=%+v", result)
	}
}

func TestExchangeExecutorRejectsParticipantErrorsAndFencesSystemErrors(t *testing.T) {
	engine, err := exchange.New(realtimeExchangeConfig())
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExchangeExecutor(engine, ExchangeExecutorConfig{QuoteQuantity: fixed.Qty(100_000), CarryPerUnit: fixed.Price(10_000)})
	if err != nil {
		t.Fatal(err)
	}
	rejected := executor.Execute(Action{ID: "bad", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: UpdateQuotePayload{Bid: fixed.Price(1_010_000), Ask: fixed.Price(990_000)}}, 0)
	if rejected.Disposition != DispositionReject || rejected.Err == nil || engine.State().Version != 0 {
		t.Fatalf("rejected=%+v state=%+v", rejected, engine.State())
	}
	failed := executor.Execute(Action{ID: "bad-system", Kind: ActionMarkMove, Source: SourceSystem, Payload: "invalid"}, 0)
	if failed.Disposition != DispositionFail || failed.Err == nil || engine.State().Version != 0 {
		t.Fatalf("failed=%+v state=%+v", failed, engine.State())
	}
	for _, payload := range []CustomerArrivalPayload{
		{Quantity: fixed.Qty(10_000), InformedDraw: 10_000},
		{Quantity: fixed.Qty(10_000), InformedDraw: 0, UpcomingMarkMoveBps: 1},
		{Quantity: fixed.Qty(10_000), InformedDraw: 0, HasUpcomingMark: true, UpcomingMarkMoveBps: 26},
	} {
		outcome := executor.Execute(Action{ID: "bad-customer", Kind: ActionCustomerArrival, Source: SourceSystem, Payload: payload}, 0)
		if outcome.Disposition != DispositionFail || outcome.Err == nil || engine.State().Version != 0 {
			t.Fatalf("malformed customer outcome=%+v state=%+v", outcome, engine.State())
		}
	}
	accepted := executor.Execute(Action{ID: "good", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: UpdateQuotePayload{Bid: fixed.Price(990_000), Ask: fixed.Price(1_010_000)}}, 0)
	if accepted.Disposition != DispositionContinue || accepted.Err != nil || engine.State().Version != 1 {
		t.Fatalf("accepted=%+v state=%+v", accepted, engine.State())
	}
	if _, err := NewExchangeExecutor(engine, ExchangeExecutorConfig{QuoteQuantity: engine.Config().MaxOrderQty + 1}); err == nil {
		t.Fatal("oversized quote quantity accepted")
	}
	if _, err := NewExchangeExecutor(engine, ExchangeExecutorConfig{QuoteQuantity: fixed.Qty(10_000), CarryPerUnit: fixed.Price(math.MaxInt64)}); err == nil {
		t.Fatal("unrepresentable carry rate accepted")
	}
}
