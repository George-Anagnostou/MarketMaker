package realtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSequencerConcurrentDuplicateSubmissionExecutesOnce(t *testing.T) {
	schedule := &recordingSchedule{}
	clock := newManualClock()
	var mu sync.Mutex
	executions := 0
	var stored *Execution
	sequencer, err := NewConfigured(Config{
		Schedule: schedule,
		Clock:    clock,
		ReplayLookup: func(action Action) (Execution, bool) {
			mu.Lock()
			defer mu.Unlock()
			if stored == nil || stored.Action.ID != action.ID {
				return Execution{}, false
			}
			return *stored, true
		},
		Executor: func(action Action, _ time.Duration) Outcome {
			mu.Lock()
			executions++
			stored = &Execution{Action: action, Result: action.ID, Disposition: DispositionContinue}
			mu.Unlock()
			return Outcome{Disposition: DispositionContinue, Result: action.ID}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const workers = 16
	results := make(chan Execution, workers)
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := sequencer.Submit(context.Background(), Action{ID: "same-id", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: "immutable"})
			results <- result
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsCh)
	for result := range results {
		if result.Disposition != DispositionContinue {
			t.Fatalf("duplicate result=%+v", result)
		}
	}
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if executions != 1 {
		t.Fatalf("duplicate executions=%d want 1", executions)
	}
}

func TestSequencerDurableReplayAvoidsExecutorAndPreservesOriginalExecution(t *testing.T) {
	clock := newManualClock()
	called := false
	original := Execution{Sequence: 7, Elapsed: 3 * time.Millisecond, Action: Action{ID: "same", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: "original"}, Result: "stored", Disposition: DispositionContinue}
	sequencer, err := NewConfigured(Config{
		Schedule: &recordingSchedule{},
		Clock:    clock,
		ReplayLookup: func(Action) (Execution, bool) {
			return original, true
		},
		Executor: func(Action, time.Duration) Outcome {
			called = true
			return Outcome{Disposition: DispositionFail, Err: errors.New("must not execute")}
		},
		Checkpoint: Checkpoint{Status: StatusCompleted, Sequence: 7, Elapsed: 3 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	got, err := sequencer.Submit(context.Background(), Action{ID: "same", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: "retry"})
	want := original
	want.Replayed = true
	if err != nil || got != want || called {
		t.Fatalf("replay=%+v err=%v called=%v", got, err, called)
	}
}
