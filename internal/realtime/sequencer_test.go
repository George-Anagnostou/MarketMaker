package realtime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*manualTimer]struct{}
	notify chan time.Time
}

type manualTimer struct {
	clock   *manualClock
	due     time.Time
	channel chan time.Time
	stopped bool
	fired   bool
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), timers: make(map[*manualTimer]struct{})}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	now, notify := c.now, c.notify
	c.mu.Unlock()
	if notify != nil {
		notify <- now
	}
	return now
}

func (c *manualClock) NewTimer(delay time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &manualTimer{clock: c, due: c.now.Add(delay), channel: make(chan time.Time, 1)}
	c.timers[timer] = struct{}{}
	if delay <= 0 {
		timer.fired = true
		timer.channel <- c.now
	}
	return timer
}

func (c *manualClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if delta < 0 {
		panic("manual clock cannot move backward")
	}
	c.now = c.now.Add(delta)
	for timer := range c.timers {
		if timer.stopped || timer.fired || timer.due.After(c.now) {
			continue
		}
		timer.fired = true
		timer.channel <- c.now
	}
}

func (t *manualTimer) C() <-chan time.Time { return t.channel }

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

type executionRecorder struct {
	mu      sync.Mutex
	actions []string
	times   []time.Duration
	fail    map[string]error
	notify  chan string
}

func (r *executionRecorder) execute(action Action, elapsed time.Duration) Outcome {
	r.mu.Lock()
	r.actions = append(r.actions, action.ID)
	r.times = append(r.times, elapsed)
	err := r.fail[action.ID]
	r.mu.Unlock()
	if r.notify != nil {
		r.notify <- action.ID
	}
	if err != nil {
		disposition := DispositionReject
		if action.Source == SourceSystem {
			disposition = DispositionFail
		}
		return Outcome{Disposition: disposition, Err: err}
	}
	return Outcome{Result: action.ID + "-result", Disposition: DispositionContinue}
}

func (r *executionRecorder) snapshot() ([]string, []time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.actions...), append([]time.Duration(nil), r.times...)
}

func systemAction(id string, due time.Duration) ScheduledAction {
	return ScheduledAction{Due: due, Action: Action{ID: id, Kind: "market_event", Source: SourceSystem}}
}

func participantAction(id string) Action {
	return Action{ID: id, Kind: "quote", Source: SourceParticipant}
}

func TestSequencerOrdersDueActionsBeforeParticipantAtSameTime(t *testing.T) {
	clock := newManualClock()
	recorder := &executionRecorder{}
	sequencer, err := NewWithClock([]ScheduledAction{systemAction("system-1", 10*time.Millisecond), systemAction("system-2", 10*time.Millisecond), systemAction("system-3", 20*time.Millisecond)}, recorder.execute, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	clock.Advance(10 * time.Millisecond)
	execution, err := sequencer.Submit(context.Background(), participantAction("player-1"))
	if err != nil {
		t.Fatal(err)
	}
	if execution.Sequence != 3 || execution.Elapsed != 10*time.Millisecond || execution.Result != "player-1-result" {
		t.Fatalf("participant execution=%+v", execution)
	}
	clock.Advance(15 * time.Millisecond)
	if _, err := sequencer.Submit(context.Background(), participantAction("player-2")); err != nil {
		t.Fatal(err)
	}
	actions, times := recorder.snapshot()
	if !reflect.DeepEqual(actions, []string{"system-1", "system-2", "player-1", "system-3", "player-2"}) {
		t.Fatalf("actions=%v", actions)
	}
	if !reflect.DeepEqual(times, []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 25 * time.Millisecond}) {
		t.Fatalf("times=%v", times)
	}
}

func TestSequencerPauseFreezesLogicalTimeAndRetainsSchedule(t *testing.T) {
	clock := newManualClock()
	recorder := &executionRecorder{}
	sequencer, err := NewWithClock([]ScheduledAction{systemAction("system-1", 5*time.Millisecond), systemAction("system-2", 10*time.Millisecond)}, recorder.execute, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(6 * time.Millisecond)
	snapshot, err := sequencer.Pause(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != StatusPaused || snapshot.Elapsed != 6*time.Millisecond || snapshot.NextScheduled != 1 {
		t.Fatalf("paused snapshot=%+v", snapshot)
	}
	clock.Advance(time.Hour)
	if _, err := sequencer.Submit(context.Background(), participantAction("paused-player")); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("paused submit error=%v", err)
	}
	snapshot, err = sequencer.Snapshot(context.Background())
	if err != nil || snapshot.Elapsed != 6*time.Millisecond || snapshot.NextScheduled != 1 {
		t.Fatalf("frozen snapshot=%+v err=%v", snapshot, err)
	}
	if err := sequencer.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(4 * time.Millisecond)
	if _, err := sequencer.Submit(context.Background(), participantAction("player")); err != nil {
		t.Fatal(err)
	}
	actions, times := recorder.snapshot()
	if !reflect.DeepEqual(actions, []string{"system-1", "system-2", "player"}) || !reflect.DeepEqual(times, []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}) {
		t.Fatalf("actions=%v times=%v", actions, times)
	}
}

func TestSequencerCatchesUpEveryOverdueActionInOrder(t *testing.T) {
	clock := newManualClock()
	recorder := &executionRecorder{}
	sequencer, err := NewWithClock([]ScheduledAction{systemAction("one", time.Millisecond), systemAction("two", 2*time.Millisecond), systemAction("three", 3*time.Millisecond)}, recorder.execute, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Millisecond)
	if _, err := sequencer.Submit(context.Background(), participantAction("player")); err != nil {
		t.Fatal(err)
	}
	actions, _ := recorder.snapshot()
	if !reflect.DeepEqual(actions, []string{"one", "two", "three", "player"}) {
		t.Fatalf("actions=%v", actions)
	}
}

func TestSequencerExecutesTimersWithoutParticipantTraffic(t *testing.T) {
	clock := newManualClock()
	recorder := &executionRecorder{notify: make(chan string, 1)}
	sequencer, err := NewWithClock([]ScheduledAction{systemAction("timer", 5*time.Millisecond)}, recorder.execute, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(5 * time.Millisecond)
	select {
	case action := <-recorder.notify:
		if action != "timer" {
			t.Fatalf("action=%q", action)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduled action did not execute")
	}
}

func TestSequencerFencesSystemFailuresButNotParticipantRejections(t *testing.T) {
	clock := newManualClock()
	rejected := errors.New("quote rejected")
	recorder := &executionRecorder{fail: map[string]error{"bad-player": rejected, "bad-system": errors.New("storage failed")}}
	sequencer, err := NewWithClock([]ScheduledAction{systemAction("bad-system", 10*time.Millisecond)}, recorder.execute, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if execution, err := sequencer.Submit(context.Background(), participantAction("before-start")); !errors.Is(err, ErrInvalidState) || execution.Sequence != 0 {
		t.Fatalf("before-start execution=%+v err=%v", execution, err)
	}
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	execution, err := sequencer.Submit(context.Background(), participantAction("bad-player"))
	if !errors.Is(err, rejected) || execution.Sequence != 1 {
		t.Fatalf("rejected execution=%+v err=%v", execution, err)
	}
	if _, err := sequencer.Submit(context.Background(), participantAction("good-player")); err != nil {
		t.Fatalf("sequencer fenced participant rejection: %v", err)
	}
	clock.Advance(10 * time.Millisecond)
	if _, err := sequencer.Submit(context.Background(), participantAction("after-system")); !errors.Is(err, ErrFailed) {
		t.Fatalf("system failure error=%v", err)
	}
	snapshot, err := sequencer.Snapshot(context.Background())
	if err != nil || snapshot.Status != StatusFailed || snapshot.Elapsed != 10*time.Millisecond || snapshot.NextScheduled != 0 || snapshot.Sequence != 3 || snapshot.Failure == nil {
		t.Fatalf("failed snapshot=%+v err=%v", snapshot, err)
	}
	final, err := sequencer.Shutdown(context.Background())
	if !errors.Is(err, ErrFailed) || final.Status != StatusFailed || final.Failure == nil {
		t.Fatalf("failed shutdown=%+v err=%v", final, err)
	}
}

func TestSequencerValidatesScheduleAndLifecycle(t *testing.T) {
	clock := newManualClock()
	executor := func(Action, time.Duration) Outcome { return Outcome{Disposition: DispositionContinue} }
	tests := map[string][]ScheduledAction{
		"negative time": {{Due: -1, Action: Action{ID: "one", Kind: "event", Source: SourceSystem}}},
		"unordered":     {systemAction("two", 2*time.Millisecond), systemAction("one", time.Millisecond)},
		"missing id":    {{Action: Action{Kind: "event", Source: SourceSystem}}},
		"wrong source":  {{Action: participantAction("player")}},
		"duplicate id":  {systemAction("same", time.Millisecond), systemAction("same", 2*time.Millisecond)},
	}
	for name, schedule := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewWithClock(schedule, executor, clock); err == nil {
				t.Fatal("invalid schedule accepted")
			}
		})
	}
	if _, err := NewWithClock(nil, nil, clock); err == nil {
		t.Fatal("nil executor accepted")
	}
	sequencer, err := NewWithClock(nil, executor, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequencer.Resume(context.Background()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("resume-before-start error=%v", err)
	}
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sequencer.Start(context.Background()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second start error=%v", err)
	}
	if err := sequencer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sequencer.Snapshot(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("snapshot-after-close error=%v", err)
	}
}

func TestSequencerDoesNotAdmitAlreadyCanceledRequest(t *testing.T) {
	clock := newManualClock()
	recorder := &executionRecorder{}
	sequencer, err := NewWithClock(nil, recorder.execute, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sequencer.Submit(ctx, participantAction("canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("submit error=%v", err)
	}
	actions, _ := recorder.snapshot()
	if len(actions) != 0 {
		t.Fatalf("canceled action executed: %v", actions)
	}
}

func TestSequencerFencesExecutorPanics(t *testing.T) {
	clock := newManualClock()
	sequencer, err := NewWithClock(nil, func(Action, time.Duration) Outcome {
		panic("broken executor")
	}, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(7 * time.Millisecond)
	execution, err := sequencer.Submit(context.Background(), participantAction("panic"))
	if !errors.Is(err, ErrFailed) || execution.Sequence != 1 || execution.Err == nil {
		t.Fatalf("execution=%+v err=%v", execution, err)
	}
	snapshot, err := sequencer.Snapshot(context.Background())
	if err != nil || snapshot.Status != StatusFailed || snapshot.Elapsed != 7*time.Millisecond || snapshot.Failure == nil {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSequencerCompletionFreezesTimeAndRejectsLaterActions(t *testing.T) {
	clock := newManualClock()
	sequencer, err := NewWithClock([]ScheduledAction{systemAction("expiry", 10*time.Millisecond)}, func(action Action, _ time.Duration) Outcome {
		if action.ID == "expiry" {
			return Outcome{Disposition: DispositionComplete}
		}
		return Outcome{Disposition: DispositionContinue}
	}, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(20 * time.Millisecond)
	if _, err := sequencer.Submit(context.Background(), participantAction("late")); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("late submit error=%v", err)
	}
	snapshot, err := sequencer.Snapshot(context.Background())
	if err != nil || snapshot.Status != StatusCompleted || snapshot.Elapsed != 10*time.Millisecond || snapshot.NextScheduled != 1 || snapshot.Sequence != 1 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	clock.Advance(time.Hour)
	snapshot, err = sequencer.Snapshot(context.Background())
	if err != nil || snapshot.Elapsed != 10*time.Millisecond {
		t.Fatalf("completed time advanced: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSequencerShutdownReturnsPausedCheckpoint(t *testing.T) {
	clock := newManualClock()
	recorder := &executionRecorder{}
	sequencer, err := NewWithClock([]ScheduledAction{systemAction("due", 5*time.Millisecond), systemAction("future", 10*time.Millisecond)}, recorder.execute, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(6 * time.Millisecond)
	final, err := sequencer.Shutdown(context.Background())
	if err != nil || final.Status != StatusPaused || final.Elapsed != 6*time.Millisecond || final.NextScheduled != 1 {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	again, err := sequencer.Shutdown(context.Background())
	if err != nil || !reflect.DeepEqual(again, final) {
		t.Fatalf("second shutdown=%+v err=%v", again, err)
	}
	if _, err := sequencer.Snapshot(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("snapshot-after-shutdown error=%v", err)
	}
}

func TestSequencerPrioritizesAdmittedRequestsAndAcknowledgesBeforeShutdown(t *testing.T) {
	clock := newManualClock()
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var actions []string
	var times []time.Duration
	executor := func(action Action, elapsed time.Duration) Outcome {
		if action.ID == "first" {
			close(started)
			<-release
		}
		mu.Lock()
		actions = append(actions, action.ID)
		times = append(times, elapsed)
		mu.Unlock()
		return Outcome{Result: action.ID, Disposition: DispositionContinue}
	}
	sequencer, err := NewWithClock([]ScheduledAction{systemAction("scheduled", 7*time.Millisecond)}, executor, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(5 * time.Millisecond)
	firstResult := make(chan Execution, 1)
	firstErr := make(chan error, 1)
	go func() {
		execution, err := sequencer.Submit(context.Background(), participantAction("first"))
		firstResult <- execution
		firstErr <- err
	}()
	<-started

	clock.Advance(5 * time.Millisecond)
	clock.mu.Lock()
	clock.notify = make(chan time.Time, 1)
	nowNotify := clock.notify
	clock.mu.Unlock()
	secondResult := make(chan Execution, 1)
	secondErr := make(chan error, 1)
	go func() {
		execution, err := sequencer.Submit(context.Background(), participantAction("second"))
		secondResult <- execution
		secondErr <- err
	}()
	if received := <-nowNotify; received != time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Add(10*time.Millisecond) {
		t.Fatalf("second receipt=%v", received)
	}
	sequencer.admission.Lock()
	if len(sequencer.pending) != 1 || sequencer.pending[0].action.ID != "second" {
		sequencer.admission.Unlock()
		t.Fatalf("pending requests=%+v", sequencer.pending)
	}
	sequencer.admission.Unlock()
	clock.mu.Lock()
	clock.notify = nil
	clock.mu.Unlock()
	clock.Advance(10 * time.Millisecond)
	shutdownResult := make(chan Snapshot, 1)
	shutdownErr := make(chan error, 1)
	go func() {
		final, err := sequencer.Shutdown(context.Background())
		shutdownResult <- final
		shutdownErr <- err
	}()
	close(release)

	if execution, err := <-firstResult, <-firstErr; err != nil || execution.Result != "first" {
		t.Fatalf("first execution=%+v err=%v", execution, err)
	}
	if execution, err := <-secondResult, <-secondErr; err != nil || execution.Result != "second" {
		t.Fatalf("second execution=%+v err=%v", execution, err)
	}
	if final, err := <-shutdownResult, <-shutdownErr; err != nil || final.Status != StatusPaused || final.Elapsed != 20*time.Millisecond {
		t.Fatalf("shutdown=%+v err=%v", final, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(actions, []string{"first", "scheduled", "second"}) || !reflect.DeepEqual(times, []time.Duration{5 * time.Millisecond, 7 * time.Millisecond, 10 * time.Millisecond}) {
		t.Fatalf("actions=%v times=%v", actions, times)
	}
}
