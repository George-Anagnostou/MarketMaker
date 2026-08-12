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

type recordingSchedule struct {
	actions   []ScheduledAction
	next      int
	nextCalls int
	committed []string
	nextErr   error
	commitErr error
}

type panicSchedule struct {
	action      ScheduledAction
	panicNext   bool
	panicCommit bool
}

func (s *panicSchedule) Next() (ScheduledAction, bool, error) {
	if s.panicNext {
		panic("next panic")
	}
	return s.action, true, nil
}

func (s *panicSchedule) Commit(ScheduledAction) error {
	if s.panicCommit {
		panic("commit panic")
	}
	return nil
}

type panicTimerClock struct{ *manualClock }

func (panicTimerClock) NewTimer(time.Duration) Timer { panic("timer panic") }

type panicNowClock struct{ *manualClock }

func (panicNowClock) Now() time.Time { panic("now panic") }

type faultyTimer struct {
	channel   chan time.Time
	called    chan struct{}
	panicC    bool
	panicStop bool
	nilC      bool
}

func (t *faultyTimer) C() <-chan time.Time {
	if t.called != nil {
		select {
		case <-t.called:
		default:
			close(t.called)
		}
	}
	if t.panicC {
		panic("channel panic")
	}
	if t.nilC {
		return nil
	}
	return t.channel
}

func (t *faultyTimer) Stop() bool {
	if t.panicStop {
		panic("stop panic")
	}
	return true
}

type faultyTimerClock struct {
	*manualClock
	timer *faultyTimer
}

func (c faultyTimerClock) NewTimer(time.Duration) Timer { return c.timer }

func (s *recordingSchedule) Next() (ScheduledAction, bool, error) {
	s.nextCalls++
	if s.nextErr != nil {
		return ScheduledAction{}, false, s.nextErr
	}
	if s.next >= len(s.actions) {
		return ScheduledAction{}, false, nil
	}
	return s.actions[s.next], true, nil
}

func (s *recordingSchedule) Commit(action ScheduledAction) error {
	if s.commitErr != nil {
		return s.commitErr
	}
	if s.next >= len(s.actions) || action.Action.ID != s.actions[s.next].Action.ID {
		return errors.New("wrong action")
	}
	s.committed = append(s.committed, action.Action.ID)
	s.next++
	return nil
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

func startAction(id string) Action {
	return Action{ID: id, Kind: ActionStartSession, Source: SourceParticipant, Payload: StartSessionPayload{}}
}

func pauseAction(id string, source Source) Action {
	reason := PauseReasonPlayer
	if source == SourceSystem {
		reason = PauseReasonShutdown
	}
	return Action{ID: id, Kind: ActionPauseSession, Source: source, Payload: PauseSessionPayload{Reason: reason}}
}

func resumeAction(id string) Action {
	return Action{ID: id, Kind: ActionResumeSession, Source: SourceParticipant}
}

func countdownAction(start Action) Action {
	return Action{ID: "system/countdown/" + start.ID, Kind: ActionCountdownComplete, Source: SourceSystem}
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

func TestSequencerCommitsIncrementalScheduleOnlyAfterSuccess(t *testing.T) {
	clock := newManualClock()
	schedule := &recordingSchedule{actions: []ScheduledAction{systemAction("one", time.Millisecond), systemAction("expiry", 2*time.Millisecond)}}
	sequencer, err := NewWithScheduleAndClock(schedule, func(action Action, _ time.Duration) Outcome {
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
	clock.Advance(3 * time.Millisecond)
	if _, err := sequencer.Submit(context.Background(), participantAction("late")); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("late submit error=%v", err)
	}
	if !reflect.DeepEqual(schedule.committed, []string{"one", "expiry"}) || schedule.next != 2 {
		t.Fatalf("committed=%v next=%d", schedule.committed, schedule.next)
	}

	failing := &recordingSchedule{actions: []ScheduledAction{systemAction("failure", time.Millisecond)}}
	failedSequencer, err := NewWithScheduleAndClock(failing, func(Action, time.Duration) Outcome {
		return Outcome{Disposition: DispositionFail, Err: errors.New("failed")}
	}, newManualClock())
	if err != nil {
		t.Fatal(err)
	}
	defer failedSequencer.Close()
	if err := failedSequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	failedSequencer.clock.(*manualClock).Advance(time.Millisecond)
	if _, err := failedSequencer.Submit(context.Background(), participantAction("after")); !errors.Is(err, ErrFailed) {
		t.Fatalf("failure submit error=%v", err)
	}
	if len(failing.committed) != 0 || failing.next != 0 {
		t.Fatalf("failed action advanced source: committed=%v next=%d", failing.committed, failing.next)
	}
}

func TestSequencerFencesIncrementalScheduleErrors(t *testing.T) {
	clock := newManualClock()
	nextFailure := errors.New("generator failed")
	sequencer, err := NewWithScheduleAndClock(&recordingSchedule{nextErr: nextFailure}, func(Action, time.Duration) Outcome {
		return Outcome{Disposition: DispositionContinue}
	}, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(context.Background()); !errors.Is(err, ErrFailed) || !errors.Is(err, nextFailure) {
		t.Fatalf("start error=%v", err)
	}

	commitFailure := errors.New("cursor commit failed")
	schedule := &recordingSchedule{actions: []ScheduledAction{systemAction("one", time.Millisecond)}, commitErr: commitFailure}
	commitSequencer, err := NewWithScheduleAndClock(schedule, func(Action, time.Duration) Outcome {
		return Outcome{Disposition: DispositionContinue}
	}, newManualClock())
	if err != nil {
		t.Fatal(err)
	}
	defer commitSequencer.Close()
	if err := commitSequencer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	commitSequencer.clock.(*manualClock).Advance(time.Millisecond)
	if _, err := commitSequencer.Submit(context.Background(), participantAction("after")); !errors.Is(err, ErrFailed) || !errors.Is(err, commitFailure) {
		t.Fatalf("commit failure error=%v", err)
	}
}

func TestSequencerFencesScheduleAndTimerPanicsWithoutStrandingRequests(t *testing.T) {
	executor := func(Action, time.Duration) Outcome { return Outcome{Disposition: DispositionContinue} }
	tests := []struct {
		name     string
		schedule Schedule
		clock    Clock
	}{
		{name: "next", schedule: &panicSchedule{panicNext: true}, clock: newManualClock()},
		{name: "timer", schedule: &panicSchedule{action: systemAction("one", time.Millisecond)}, clock: panicTimerClock{newManualClock()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sequencer, err := NewWithScheduleAndClock(test.schedule, executor, test.clock)
			if err != nil {
				t.Fatal(err)
			}
			defer sequencer.Close()
			if err := sequencer.Start(t.Context()); !errors.Is(err, ErrFailed) {
				t.Fatalf("start error=%v", err)
			}
		})
	}

	clock := newManualClock()
	commit := &panicSchedule{action: systemAction("one", time.Millisecond), panicCommit: true}
	sequencer, err := NewWithScheduleAndClock(commit, executor, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Millisecond)
	if _, err := sequencer.Submit(t.Context(), participantAction("after")); !errors.Is(err, ErrFailed) {
		t.Fatalf("commit panic error=%v", err)
	}

	var typedNil *panicSchedule
	if _, err := NewWithScheduleAndClock(typedNil, executor, newManualClock()); err == nil {
		t.Fatal("typed nil schedule accepted")
	}
}

func TestSequencerFencesRemainingClockFailures(t *testing.T) {
	executor := func(Action, time.Duration) Outcome { return Outcome{Disposition: DispositionContinue} }
	if _, err := NewWithScheduleAndClock(&recordingSchedule{}, executor, panicNowClock{newManualClock()}); err == nil {
		t.Fatal("panicking clock accepted")
	}

	for _, test := range []struct {
		name  string
		timer *faultyTimer
	}{
		{name: "channel panic", timer: &faultyTimer{called: make(chan struct{}), panicC: true}},
		{name: "nil channel", timer: &faultyTimer{called: make(chan struct{}), nilC: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := faultyTimerClock{manualClock: newManualClock(), timer: test.timer}
			sequencer, err := NewWithScheduleAndClock(&recordingSchedule{actions: []ScheduledAction{systemAction("one", time.Millisecond)}}, executor, clock)
			if err != nil {
				t.Fatal(err)
			}
			defer sequencer.Close()
			if err := sequencer.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-test.timer.called:
			case <-time.After(time.Second):
				t.Fatal("timer channel was not inspected")
			}
			snapshot, err := sequencer.Snapshot(t.Context())
			if err != nil || snapshot.Status != StatusFailed || snapshot.Failure == nil {
				t.Fatalf("snapshot=%+v err=%v", snapshot, err)
			}
		})
	}

	stopTimer := &faultyTimer{channel: make(chan time.Time), panicStop: true}
	clock := faultyTimerClock{manualClock: newManualClock(), timer: stopTimer}
	sequencer, err := NewWithScheduleAndClock(&recordingSchedule{actions: []ScheduledAction{systemAction("one", time.Millisecond)}}, executor, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if err := sequencer.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := sequencer.Submit(t.Context(), participantAction("player")); !errors.Is(err, ErrFailed) {
		t.Fatalf("stop panic error=%v", err)
	}
}

func TestSequencerDurableStartCountsDownBeforeLoadingMarketSchedule(t *testing.T) {
	clock := newManualClock()
	schedule := &recordingSchedule{actions: []ScheduledAction{systemAction("market", 5*time.Millisecond)}}
	recorder := &executionRecorder{notify: make(chan string, 3)}
	sequencer, err := NewConfigured(Config{
		Schedule: schedule, Executor: recorder.execute, Clock: clock,
		Countdown: 10 * time.Millisecond, CountdownAction: countdownAction,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()

	execution, err := sequencer.StartAction(t.Context(), startAction("start"))
	if err != nil || execution.Sequence != 1 || execution.Elapsed != 0 {
		t.Fatalf("start execution=%+v err=%v", execution, err)
	}
	if action := <-recorder.notify; action != "start" {
		t.Fatalf("first action=%q", action)
	}
	snapshot, err := sequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusCountdown || snapshot.Elapsed != 0 || snapshot.Sequence != 1 || schedule.nextCalls != 0 {
		t.Fatalf("countdown snapshot=%+v next calls=%d err=%v", snapshot, schedule.nextCalls, err)
	}

	clock.Advance(10 * time.Millisecond)
	if action := <-recorder.notify; action != "system/countdown/start" {
		t.Fatalf("countdown action=%q", action)
	}
	snapshot, err = sequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusRunning || snapshot.Elapsed != 0 || snapshot.Sequence != 2 || schedule.nextCalls != 1 {
		t.Fatalf("running snapshot=%+v next calls=%d err=%v", snapshot, schedule.nextCalls, err)
	}

	clock.Advance(5 * time.Millisecond)
	if action := <-recorder.notify; action != "market" {
		t.Fatalf("market action=%q", action)
	}
	snapshot, err = sequencer.Snapshot(t.Context())
	if err != nil || snapshot.NextScheduled != 1 || snapshot.Sequence != 3 {
		t.Fatalf("market snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSequencerDurablePauseDuringCountdownDoesNotCompleteCountdown(t *testing.T) {
	clock := newManualClock()
	schedule := &recordingSchedule{actions: []ScheduledAction{systemAction("market", time.Millisecond)}}
	recorder := &executionRecorder{}
	sequencer, err := NewConfigured(Config{
		Schedule: schedule, Executor: recorder.execute, Clock: clock,
		Countdown: time.Second, CountdownAction: countdownAction,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if _, err := sequencer.StartAction(t.Context(), startAction("start")); err != nil {
		t.Fatal(err)
	}
	execution, err := sequencer.PauseAction(t.Context(), pauseAction("system/pause", SourceSystem))
	if err != nil || execution.Sequence != 2 || execution.Elapsed != 0 {
		t.Fatalf("pause execution=%+v err=%v", execution, err)
	}
	clock.Advance(time.Hour)
	snapshot, err := sequencer.Snapshot(t.Context())
	actions, times := recorder.snapshot()
	if err != nil || snapshot.Status != StatusPaused || snapshot.Elapsed != 0 || snapshot.Sequence != 2 || schedule.nextCalls != 0 {
		t.Fatalf("paused snapshot=%+v next calls=%d err=%v", snapshot, schedule.nextCalls, err)
	}
	if !reflect.DeepEqual(actions, []string{"start", "system/pause"}) || !reflect.DeepEqual(times, []time.Duration{0, 0}) {
		t.Fatalf("actions=%v times=%v", actions, times)
	}
}

func TestSequencerDurablePauseProcessesDueWorkBeforeTransition(t *testing.T) {
	clock := newManualClock()
	schedule := &recordingSchedule{actions: []ScheduledAction{systemAction("due", 5*time.Millisecond)}}
	recorder := &executionRecorder{notify: make(chan string, 4)}
	sequencer, err := NewConfigured(Config{
		Schedule: schedule, Executor: recorder.execute, Clock: clock,
		Countdown: time.Millisecond, CountdownAction: countdownAction,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if _, err := sequencer.StartAction(t.Context(), startAction("start")); err != nil {
		t.Fatal(err)
	}
	<-recorder.notify
	clock.Advance(time.Millisecond)
	if action := <-recorder.notify; action != "system/countdown/start" {
		t.Fatalf("countdown action=%q", action)
	}
	clock.Advance(10 * time.Millisecond)
	execution, err := sequencer.PauseAction(t.Context(), pauseAction("pause", SourceParticipant))
	if err != nil || execution.Sequence != 4 || execution.Elapsed != 10*time.Millisecond {
		t.Fatalf("pause execution=%+v err=%v", execution, err)
	}
	snapshot, err := sequencer.Snapshot(t.Context())
	actions, times := recorder.snapshot()
	if err != nil || snapshot.Status != StatusPaused || snapshot.Elapsed != 10*time.Millisecond || snapshot.NextScheduled != 1 {
		t.Fatalf("paused snapshot=%+v err=%v", snapshot, err)
	}
	if !reflect.DeepEqual(actions, []string{"start", "system/countdown/start", "due", "pause"}) || !reflect.DeepEqual(times, []time.Duration{0, 0, 5 * time.Millisecond, 10 * time.Millisecond}) {
		t.Fatalf("actions=%v times=%v", actions, times)
	}
}

func TestSequencerDurableResumeFromPausedCheckpoint(t *testing.T) {
	clock := newManualClock()
	schedule := &recordingSchedule{actions: []ScheduledAction{systemAction("next", 10*time.Millisecond)}, next: 0}
	recorder := &executionRecorder{notify: make(chan string, 2)}
	sequencer, err := NewConfigured(Config{
		Schedule: schedule, Executor: recorder.execute, Clock: clock,
		Checkpoint: Checkpoint{Status: StatusPaused, Elapsed: 7 * time.Millisecond, NextScheduled: 2, Sequence: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	execution, err := sequencer.ResumeAction(t.Context(), resumeAction("resume"))
	if err != nil || execution.Sequence != 5 || execution.Elapsed != 7*time.Millisecond {
		t.Fatalf("resume execution=%+v err=%v", execution, err)
	}
	if action := <-recorder.notify; action != "resume" {
		t.Fatalf("resume action=%q", action)
	}
	clock.Advance(3 * time.Millisecond)
	if action := <-recorder.notify; action != "next" {
		t.Fatalf("scheduled action=%q", action)
	}
	snapshot, err := sequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusRunning || snapshot.NextScheduled != 3 || snapshot.Sequence != 6 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSequencerReplayPrecedesLifecycleStateAndDoesNotMutateCheckpoint(t *testing.T) {
	conflict := errors.New("action id payload conflict")
	canonicalStart := startAction("start-retry")
	replayed := map[string]Execution{
		"completed-retry": {Sequence: 4, Elapsed: 900 * time.Millisecond, Action: participantAction("completed-retry"), Result: "durable", Disposition: DispositionComplete},
		"pause-retry":     {Sequence: 2, Elapsed: 800 * time.Millisecond, Action: pauseAction("pause-retry", SourceParticipant), Result: "paused", Disposition: DispositionContinue},
		"start-retry":     {Sequence: 1, Elapsed: 0, Action: canonicalStart, Disposition: DispositionReject, Err: conflict},
	}
	lookup := func(action Action) (Execution, bool) {
		execution, ok := replayed[action.ID]
		return execution, ok
	}
	executor := func(Action, time.Duration) Outcome {
		t.Fatal("replayed action reached executor")
		return Outcome{}
	}

	completed, err := NewConfigured(Config{
		Schedule: &recordingSchedule{}, Executor: executor, ReplayLookup: lookup, Clock: newManualClock(),
		Checkpoint: Checkpoint{Status: StatusCompleted, Elapsed: time.Second, NextScheduled: 2, Sequence: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer completed.Close()
	execution, err := completed.Submit(t.Context(), participantAction("completed-retry"))
	if err != nil || execution.Result != "durable" || execution.Sequence != 4 || execution.Elapsed != 900*time.Millisecond {
		t.Fatalf("completed replay=%+v err=%v", execution, err)
	}
	snapshot, err := completed.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusCompleted || snapshot.Sequence != 5 || snapshot.NextScheduled != 2 {
		t.Fatalf("completed snapshot=%+v err=%v", snapshot, err)
	}
	changedStart := canonicalStart
	changedStart.Payload = StartSessionPayload{Bid: 1}
	execution, err = completed.StartAction(t.Context(), changedStart)
	if !errors.Is(err, conflict) || !reflect.DeepEqual(execution.Action, canonicalStart) || execution.Sequence != 1 || execution.Elapsed != 0 {
		t.Fatalf("conflict replay=%+v err=%v", execution, err)
	}

	paused, err := NewConfigured(Config{
		Schedule: &recordingSchedule{}, Executor: executor, ReplayLookup: lookup, Clock: newManualClock(),
		Checkpoint: Checkpoint{Status: StatusPaused, Elapsed: time.Second, Sequence: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer paused.Close()
	execution, err = paused.PauseAction(t.Context(), pauseAction("pause-retry", SourceParticipant))
	if err != nil || execution.Result != "paused" || execution.Sequence != 2 || execution.Elapsed != 800*time.Millisecond {
		t.Fatalf("pause replay=%+v err=%v", execution, err)
	}
	snapshot, err = paused.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusPaused || snapshot.Sequence != 3 {
		t.Fatalf("paused snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSequencerRejectsInvalidConfiguredState(t *testing.T) {
	valid := Config{Schedule: &recordingSchedule{}, Executor: func(Action, time.Duration) Outcome {
		return Outcome{Disposition: DispositionContinue}
	}, Clock: newManualClock()}
	tests := []Config{
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, Countdown: -1},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, Countdown: time.Second},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, DurableLifecycle: true},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, DurableLifecycle: true, Countdown: time.Second, CountdownAction: countdownAction},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, Checkpoint: Checkpoint{Status: StatusRunning}},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, Checkpoint: Checkpoint{Status: StatusCountdown}},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, Checkpoint: Checkpoint{Status: StatusFailed}},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, Checkpoint: Checkpoint{Status: StatusClosed}},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, Checkpoint: Checkpoint{Status: StatusCompleted}},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, Checkpoint: Checkpoint{Status: StatusPreparing, Sequence: 1}},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, Checkpoint: Checkpoint{Status: StatusPaused, Elapsed: -1}},
		{Schedule: valid.Schedule, Executor: valid.Executor, Clock: valid.Clock, Checkpoint: Checkpoint{Status: StatusPaused, NextScheduled: 2, Sequence: 1}},
	}
	for i, config := range tests {
		if sequencer, err := NewConfigured(config); err == nil {
			sequencer.Close()
			t.Fatalf("invalid config %d accepted", i)
		}
	}
}

func TestSequencerCountdownExecutorFailureFences(t *testing.T) {
	clock := newManualClock()
	called := make(chan string, 2)
	failure := errors.New("countdown persistence failed")
	executor := func(action Action, _ time.Duration) Outcome {
		called <- action.ID
		if action.Kind == ActionCountdownComplete {
			return Outcome{Disposition: DispositionFail, Err: failure}
		}
		return Outcome{Disposition: DispositionContinue}
	}
	sequencer, err := NewConfigured(Config{
		Schedule: &recordingSchedule{}, Executor: executor, Clock: clock,
		Countdown: time.Second, CountdownAction: countdownAction,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if _, err := sequencer.StartAction(t.Context(), startAction("start")); err != nil {
		t.Fatal(err)
	}
	<-called
	clock.Advance(time.Second)
	<-called
	snapshot, err := sequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusFailed || snapshot.Elapsed != 0 || snapshot.Sequence != 2 || !errors.Is(snapshot.Failure, failure) {
		t.Fatalf("failed snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSequencerPauseAtCountdownDeadlineCompletesCountdownFirst(t *testing.T) {
	clock := newManualClock()
	recorder := &executionRecorder{notify: make(chan string, 3)}
	sequencer, err := NewConfigured(Config{
		Schedule: &recordingSchedule{}, Executor: recorder.execute, Clock: clock,
		Countdown: 10 * time.Millisecond, CountdownAction: countdownAction,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if _, err := sequencer.StartAction(t.Context(), startAction("start")); err != nil {
		t.Fatal(err)
	}
	<-recorder.notify
	clock.Advance(10 * time.Millisecond)
	execution, err := sequencer.PauseAction(t.Context(), pauseAction("pause", SourceParticipant))
	if err != nil || execution.Sequence != 3 || execution.Elapsed != 0 {
		t.Fatalf("pause execution=%+v err=%v", execution, err)
	}
	actions, times := recorder.snapshot()
	if !reflect.DeepEqual(actions, []string{"start", "system/countdown/start", "pause"}) || !reflect.DeepEqual(times, []time.Duration{0, 0, 0}) {
		t.Fatalf("actions=%v times=%v", actions, times)
	}
	snapshot, err := sequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusPaused || snapshot.Elapsed != 0 || snapshot.Sequence != 3 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSequencerSubmitAtOrAfterCountdownDeadlineCompletesCountdownFirst(t *testing.T) {
	for _, test := range []struct {
		name    string
		advance time.Duration
	}{
		{name: "exactly deadline", advance: 10 * time.Millisecond},
		{name: "after deadline", advance: 15 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock()
			recorder := &executionRecorder{}
			sequencer, err := NewConfigured(Config{
				Schedule: &recordingSchedule{}, Executor: recorder.execute, Clock: clock,
				Countdown: 10 * time.Millisecond, CountdownAction: countdownAction,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer sequencer.Close()
			if _, err := sequencer.StartAction(t.Context(), startAction("start")); err != nil {
				t.Fatal(err)
			}

			clock.Advance(test.advance)
			execution, err := sequencer.Submit(t.Context(), participantAction("participant"))
			if err != nil || execution.Sequence != 3 || execution.Elapsed != 0 {
				t.Fatalf("submit execution=%+v err=%v", execution, err)
			}
			actions, times := recorder.snapshot()
			if !reflect.DeepEqual(actions, []string{"start", "system/countdown/start", "participant"}) || !reflect.DeepEqual(times, []time.Duration{0, 0, 0}) {
				t.Fatalf("actions=%v times=%v", actions, times)
			}
			snapshot, err := sequencer.Snapshot(t.Context())
			if err != nil || snapshot.Status != StatusRunning || snapshot.Elapsed != 0 || snapshot.Sequence != 3 {
				t.Fatalf("snapshot=%+v err=%v", snapshot, err)
			}
		})
	}
}

func TestSequencerSubmitAtCountdownDeadlineFencesCountdownFailure(t *testing.T) {
	clock := newManualClock()
	failure := errors.New("countdown failed")
	recorder := &executionRecorder{}
	sequencer, err := NewConfigured(Config{
		Schedule: &recordingSchedule{}, Clock: clock,
		Countdown: 10 * time.Millisecond, CountdownAction: countdownAction,
		Executor: func(action Action, elapsed time.Duration) Outcome {
			recorder.mu.Lock()
			recorder.actions = append(recorder.actions, action.ID)
			recorder.times = append(recorder.times, elapsed)
			recorder.mu.Unlock()
			if action.Kind == ActionCountdownComplete {
				return Outcome{Disposition: DispositionFail, Err: failure}
			}
			return Outcome{Disposition: DispositionContinue}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if _, err := sequencer.StartAction(t.Context(), startAction("start")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Millisecond)
	if _, err := sequencer.Submit(t.Context(), participantAction("participant")); !errors.Is(err, ErrFailed) || !errors.Is(err, failure) {
		t.Fatalf("submit error=%v", err)
	}
	actions, times := recorder.snapshot()
	if !reflect.DeepEqual(actions, []string{"start", "system/countdown/start"}) || !reflect.DeepEqual(times, []time.Duration{0, 0}) {
		t.Fatalf("actions=%v times=%v", actions, times)
	}
	snapshot, err := sequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusFailed || snapshot.Elapsed != 0 || snapshot.Sequence != 2 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSequencerDurableLifecycleRejectsInvalidExecutorDisposition(t *testing.T) {
	rejection := errors.New("unexpected rejection")

	pauseClock := newManualClock()
	pauseSequencer, err := NewConfigured(Config{
		Schedule: &recordingSchedule{}, Clock: pauseClock,
		Countdown: time.Second, CountdownAction: countdownAction,
		Executor: func(action Action, _ time.Duration) Outcome {
			if action.Kind == ActionPauseSession {
				return Outcome{Disposition: DispositionReject, Err: rejection}
			}
			return Outcome{Disposition: DispositionContinue}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pauseSequencer.Close()
	if _, err := pauseSequencer.StartAction(t.Context(), startAction("start")); err != nil {
		t.Fatal(err)
	}
	if _, err := pauseSequencer.PauseAction(t.Context(), pauseAction("pause", SourceParticipant)); !errors.Is(err, ErrFailed) || !errors.Is(err, rejection) {
		t.Fatalf("pause error=%v", err)
	}
	snapshot, err := pauseSequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusFailed || snapshot.Sequence != 2 {
		t.Fatalf("pause snapshot=%+v err=%v", snapshot, err)
	}

	resumeSequencer, err := NewConfigured(Config{
		Schedule: &recordingSchedule{}, Clock: newManualClock(),
		Checkpoint: Checkpoint{Status: StatusPaused, Sequence: 2},
		Executor: func(Action, time.Duration) Outcome {
			return Outcome{Disposition: DispositionReject, Err: rejection}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resumeSequencer.Close()
	if _, err := resumeSequencer.ResumeAction(t.Context(), resumeAction("resume")); !errors.Is(err, ErrFailed) || !errors.Is(err, rejection) {
		t.Fatalf("resume error=%v", err)
	}
	snapshot, err = resumeSequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusFailed || snapshot.Sequence != 3 {
		t.Fatalf("resume snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSequencerValidatesDurableLifecyclePayloadsAndConfiguration(t *testing.T) {
	executorCalls := 0
	sequencer, err := NewConfigured(Config{
		Schedule: &recordingSchedule{}, Clock: newManualClock(),
		Executor: func(Action, time.Duration) Outcome {
			executorCalls++
			return Outcome{Disposition: DispositionContinue}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	invalid := []func() error{
		func() error {
			_, err := sequencer.StartAction(t.Context(), Action{ID: "start", Kind: ActionStartSession, Source: SourceParticipant})
			return err
		},
		func() error {
			_, err := sequencer.PauseAction(t.Context(), Action{ID: "pause", Kind: ActionPauseSession, Source: SourceParticipant, Payload: PauseSessionPayload{Reason: PauseReasonShutdown}})
			return err
		},
		func() error {
			_, err := sequencer.PauseAction(t.Context(), Action{ID: "system/pause", Kind: ActionPauseSession, Source: SourceSystem, Payload: PauseSessionPayload{Reason: PauseReasonPlayer}})
			return err
		},
		func() error {
			_, err := sequencer.ResumeAction(t.Context(), Action{ID: "resume", Kind: ActionResumeSession, Source: SourceParticipant, Payload: struct{}{}})
			return err
		},
	}
	for i, call := range invalid {
		if err := call(); err == nil {
			t.Fatalf("invalid lifecycle action %d accepted", i)
		}
	}
	if _, err := sequencer.StartAction(t.Context(), startAction("valid")); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero-countdown start error=%v", err)
	}
	if executorCalls != 0 {
		t.Fatalf("executor calls=%d", executorCalls)
	}
}

func TestSequencerDurableLifecycleDisablesInMemoryControls(t *testing.T) {
	lookup := func(Action) (Execution, bool) { return Execution{}, false }
	sequencer, err := NewConfigured(Config{
		Schedule: &recordingSchedule{}, Executor: (&executionRecorder{}).execute, Clock: newManualClock(),
		Countdown: time.Second, CountdownAction: countdownAction, ReplayLookup: lookup, DurableLifecycle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sequencer.Start(t.Context()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("in-memory start error=%v", err)
	}
	if _, err := sequencer.StartAction(t.Context(), startAction("start")); err != nil {
		t.Fatal(err)
	}
	if _, err := sequencer.PauseAction(t.Context(), pauseAction("pause", SourceParticipant)); err != nil {
		t.Fatal(err)
	}
	if err := sequencer.Resume(t.Context()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("in-memory resume error=%v", err)
	}
	if _, err := sequencer.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSequencerCountdownFactoryRequiresPayloadFreeAction(t *testing.T) {
	clock := newManualClock()
	factoryCalled := make(chan struct{}, 1)
	sequencer, err := NewConfigured(Config{
		Schedule: &recordingSchedule{}, Clock: clock, Countdown: time.Second,
		Executor: func(Action, time.Duration) Outcome { return Outcome{Disposition: DispositionContinue} },
		CountdownAction: func(start Action) Action {
			factoryCalled <- struct{}{}
			return Action{ID: "system/countdown/" + start.ID, Kind: ActionCountdownComplete, Source: SourceSystem, Payload: struct{}{}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sequencer.Close()
	if _, err := sequencer.StartAction(t.Context(), startAction("start")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	<-factoryCalled
	snapshot, err := sequencer.Snapshot(t.Context())
	if err != nil || snapshot.Status != StatusFailed || snapshot.Sequence != 1 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}
