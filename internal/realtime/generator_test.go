package realtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"market-maker/internal/fixed"
)

func testGeneratorConfig() GeneratorConfig {
	return GeneratorConfig{
		Version:            1,
		Seed:               42,
		Duration:           100 * time.Millisecond,
		CustomerCadence:    Interval{Minimum: 5 * time.Millisecond, Maximum: 12 * time.Millisecond},
		MarkCadence:        Interval{Minimum: 15 * time.Millisecond, Maximum: 25 * time.Millisecond},
		CarryCadence:       20 * time.Millisecond,
		MaxOrderQuantity:   fixed.Qty(100_000),
		MaxFlowSlippageBps: 200,
		MinMoveBps:         -25,
		MaxMoveBps:         25,
		CustomerDomain:     "customer/v1",
		MarkDomain:         "mark/v1",
		InformedDomain:     "informed/v1",
	}
}

func collectGenerator(t *testing.T, config GeneratorConfig) []ScheduledAction {
	t.Helper()
	generator, err := NewGenerator(config)
	if err != nil {
		t.Fatal(err)
	}
	var actions []ScheduledAction
	for {
		action, ok, err := generator.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		actions = append(actions, action)
		if err := generator.Commit(action); err != nil {
			t.Fatal(err)
		}
	}
	return actions
}

func TestGeneratorIsDeterministicAndSeededPerGame(t *testing.T) {
	config := testGeneratorConfig()
	first := collectGenerator(t, config)
	second := collectGenerator(t, config)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same seed produced different actions")
	}
	config.Seed++
	third := collectGenerator(t, config)
	if reflect.DeepEqual(first, third) {
		t.Fatal("different seeds produced the same actions")
	}
}

func TestGeneratorV1GoldenActions(t *testing.T) {
	actions := collectGenerator(t, testGeneratorConfig())
	encoded, err := json.Marshal(actions)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	got := hex.EncodeToString(digest[:])
	const want = "e83e2e96f756eb3b4d3a1e99403dbad0670ae2f8c289a1474f55455bc40abbab"
	if got != want {
		t.Fatalf("generator v1 digest=%s", got)
	}
}

func TestGeneratorOrdersRuntimeEventsAndReservesExpiryBoundary(t *testing.T) {
	config := testGeneratorConfig()
	actions := collectGenerator(t, config)
	if len(actions) == 0 || actions[len(actions)-1].Action.Kind != ActionTimeExpired || actions[len(actions)-1].Due != config.Duration {
		t.Fatalf("terminal action=%+v", actions)
	}
	ids := make(map[string]struct{}, len(actions))
	previous := time.Duration(0)
	for index, action := range actions {
		if action.Due < previous {
			t.Fatalf("action %d moved backward: %+v", index, action)
		}
		if action.Action.Kind != ActionTimeExpired && action.Due >= config.Duration {
			t.Fatalf("market action reached expiry boundary: %+v", action)
		}
		if _, exists := ids[action.Action.ID]; exists {
			t.Fatalf("duplicate action id %q", action.Action.ID)
		}
		ids[action.Action.ID] = struct{}{}
		previous = action.Due
	}
}

func TestGeneratorNextIsStableAndCommitAdvancesOneEvent(t *testing.T) {
	generator, err := NewGenerator(testGeneratorConfig())
	if err != nil {
		t.Fatal(err)
	}
	before := generator.Snapshot()
	first, ok, err := generator.Next()
	if err != nil || !ok {
		t.Fatalf("next=%+v ok=%v err=%v", first, ok, err)
	}
	again, ok, err := generator.Next()
	if err != nil || !ok || !reflect.DeepEqual(again, first) {
		t.Fatalf("unstable next=%+v again=%+v err=%v", first, again, err)
	}
	wrong := first
	wrong.Action.ID = "wrong"
	if err := generator.Commit(wrong); err == nil {
		t.Fatal("mismatched commit accepted")
	}
	if !reflect.DeepEqual(generator.Snapshot(), before) {
		t.Fatal("rejected commit advanced generator")
	}
	if err := generator.Commit(first); err != nil {
		t.Fatal(err)
	}
	after := generator.Snapshot()
	if after.Committed != before.Committed+1 || after.Generated < before.Generated {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
}

func TestCustomerActionUsesTheUpcomingMarkCandidate(t *testing.T) {
	config := testGeneratorConfig()
	config.CustomerCadence = Interval{Minimum: time.Millisecond, Maximum: time.Millisecond}
	config.MarkCadence = Interval{Minimum: 10 * time.Millisecond, Maximum: 10 * time.Millisecond}
	generator, err := NewGenerator(config)
	if err != nil {
		t.Fatal(err)
	}
	action, ok, err := generator.Next()
	if err != nil || !ok || action.Action.Kind != ActionCustomerArrival {
		t.Fatalf("action=%+v ok=%v err=%v", action, ok, err)
	}
	payload, ok := action.Action.Payload.(CustomerArrivalPayload)
	if !ok || !payload.HasUpcomingMark || payload.UpcomingMarkMoveBps < config.MinMoveBps || payload.UpcomingMarkMoveBps > config.MaxMoveBps {
		t.Fatalf("payload=%+v", action.Action.Payload)
	}
}

func TestCustomerLookaheadMatchesTheNextEmittedMark(t *testing.T) {
	config := testGeneratorConfig()
	config.CustomerCadence = Interval{Minimum: 10 * time.Millisecond, Maximum: 10 * time.Millisecond}
	config.MarkCadence = Interval{Minimum: 10 * time.Millisecond, Maximum: 10 * time.Millisecond}
	actions := collectGenerator(t, config)
	for index, action := range actions {
		if action.Action.Kind != ActionCustomerArrival {
			continue
		}
		payload := action.Action.Payload.(CustomerArrivalPayload)
		var nextMark *MarkMovePayload
		for _, later := range actions[index+1:] {
			if later.Action.Kind == ActionMarkMove {
				mark := later.Action.Payload.(MarkMovePayload)
				nextMark = &mark
				break
			}
		}
		if nextMark == nil {
			if payload.HasUpcomingMark {
				t.Fatalf("customer without a future mark has lookahead: %+v", action)
			}
			continue
		}
		if !payload.HasUpcomingMark || payload.UpcomingMarkMoveBps != nextMark.BasisPoints {
			t.Fatalf("customer lookahead=%+v next mark=%+v", payload, nextMark)
		}
	}
	if actions[0].Action.Kind != ActionCustomerArrival || actions[1].Action.Kind != ActionMarkMove || actions[0].Due != actions[1].Due {
		t.Fatalf("equal-time generation order changed: %v then %v", actions[0], actions[1])
	}
}

func TestGeneratorStreamsRemainIndependent(t *testing.T) {
	config := testGeneratorConfig()
	baseline := collectGenerator(t, config)
	informedConfig := config
	informedConfig.InformedDomain = "different-informed/v1"
	informed := collectGenerator(t, informedConfig)
	stripInformed := func(actions []ScheduledAction) []ScheduledAction {
		copy := append([]ScheduledAction(nil), actions...)
		for index := range copy {
			if payload, ok := copy[index].Action.Payload.(CustomerArrivalPayload); ok {
				payload.InformedDraw = 0
				copy[index].Action.Payload = payload
			}
		}
		return copy
	}
	if !reflect.DeepEqual(stripInformed(baseline), stripInformed(informed)) {
		t.Fatal("informed stream changed market event timing or parameters")
	}

	customerConfig := config
	customerConfig.CustomerDomain = "different-customer/v1"
	differentCustomer := collectGenerator(t, customerConfig)
	type mark struct {
		due     time.Duration
		payload MarkMovePayload
	}
	marks := func(actions []ScheduledAction) []mark {
		var result []mark
		for _, action := range actions {
			if payload, ok := action.Action.Payload.(MarkMovePayload); ok {
				result = append(result, mark{due: action.Due, payload: payload})
			}
		}
		return result
	}
	if !reflect.DeepEqual(marks(baseline), marks(differentCustomer)) {
		t.Fatal("customer stream changed mark timing or values")
	}
}

func TestGeneratorReplaysCommittedPrefix(t *testing.T) {
	config := testGeneratorConfig()
	original, err := NewGenerator(config)
	if err != nil {
		t.Fatal(err)
	}
	var prefix []ScheduledAction
	for range 7 {
		action, ok, err := original.Next()
		if err != nil || !ok {
			t.Fatalf("prefix action=%+v ok=%v err=%v", action, ok, err)
		}
		prefix = append(prefix, action)
		if err := original.Commit(action); err != nil {
			t.Fatal(err)
		}
	}
	wantNext, ok, err := original.Next()
	if err != nil || !ok {
		t.Fatalf("next=%+v ok=%v err=%v", wantNext, ok, err)
	}
	replayed, err := NewGenerator(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range prefix {
		got, ok, err := replayed.Next()
		if err != nil || !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("replay got=%+v want=%+v ok=%v err=%v", got, want, ok, err)
		}
		if err := replayed.Commit(got); err != nil {
			t.Fatal(err)
		}
	}
	gotNext, ok, err := replayed.Next()
	if err != nil || !ok || !reflect.DeepEqual(gotNext, wantNext) {
		t.Fatalf("replayed next=%+v want=%+v ok=%v err=%v", gotNext, wantNext, ok, err)
	}
}

func TestGeneratorValidatesConfiguration(t *testing.T) {
	valid := testGeneratorConfig()
	tests := map[string]func(*GeneratorConfig){
		"version":          func(config *GeneratorConfig) { config.Version = 0 },
		"future version":   func(config *GeneratorConfig) { config.Version++ },
		"seed":             func(config *GeneratorConfig) { config.Seed = 0 },
		"duration":         func(config *GeneratorConfig) { config.Duration = 0 },
		"long duration":    func(config *GeneratorConfig) { config.Duration = 24*time.Hour + 1 },
		"customer cadence": func(config *GeneratorConfig) { config.CustomerCadence.Minimum = 0 },
		"mark cadence":     func(config *GeneratorConfig) { config.MarkCadence.Maximum = config.Duration + 1 },
		"carry cadence":    func(config *GeneratorConfig) { config.CarryCadence = 0 },
		"quantity":         func(config *GeneratorConfig) { config.MaxOrderQuantity = 0 },
		"slippage":         func(config *GeneratorConfig) { config.MaxFlowSlippageBps = 10_001 },
		"mark range":       func(config *GeneratorConfig) { config.MinMoveBps = config.MaxMoveBps + 1 },
		"empty domain":     func(config *GeneratorConfig) { config.CustomerDomain = "" },
		"duplicate domain": func(config *GeneratorConfig) { config.InformedDomain = config.MarkDomain },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := NewGenerator(config); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestGeneratorIntegratesWithCommitAfterSuccessSequencing(t *testing.T) {
	config := testGeneratorConfig()
	generator, err := NewGenerator(config)
	if err != nil {
		t.Fatal(err)
	}
	clock := newManualClock()
	sequencer, err := NewWithScheduleAndClock(generator, func(action Action, _ time.Duration) Outcome {
		if action.Kind == ActionTimeExpired {
			return Outcome{Disposition: DispositionComplete}
		}
		return Outcome{Disposition: DispositionContinue}
	}, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(config.Duration)
	if _, err := sequencer.Submit(t.Context(), participantAction("late")); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("late submit error=%v", err)
	}
	snapshot, err := sequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusCompleted || snapshot.Elapsed != config.Duration || generator.Snapshot().Committed == 0 {
		t.Fatalf("sequencer=%+v generator=%+v err=%v", snapshot, generator.Snapshot(), err)
	}
}
