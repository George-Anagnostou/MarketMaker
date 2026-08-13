// Package realtime orders scheduled market activity and participant actions on
// one logical timeline. It does not know about exchange mechanics or transport.
package realtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

type Source string

const (
	SourceSystem         Source = "system"
	SourceParticipant    Source = "participant"
	SystemActionIDPrefix        = "system/"
)

type Action struct {
	ID      string
	Kind    string
	Source  Source
	Payload any // Treated as immutable after admission.
}

type ScheduledAction struct {
	Due    time.Duration
	Action Action
}

// Schedule returns one stable next action until Commit advances it. Commit is
// an in-memory cursor update after the executor's durable work succeeds. A
// failed Commit must leave the cursor unchanged; recovery advances it by
// replaying already-durable system actions.
type Schedule interface {
	Next() (ScheduledAction, bool, error)
	Commit(ScheduledAction) error
}

type Execution struct {
	Sequence    uint64
	Elapsed     time.Duration
	Action      Action
	Result      any
	Disposition Disposition
	Err         error
	Replayed    bool
}

type Disposition string

const (
	DispositionContinue Disposition = "continue"
	DispositionComplete Disposition = "complete"
	DispositionReject   Disposition = "reject"
	DispositionFail     Disposition = "fail"
)

type Outcome struct {
	Result      any
	Disposition Disposition
	Err         error
}

// Executor runs synchronously on the sequencer goroutine. It must return and
// must not call back into its Sequencer.
type Executor func(Action, time.Duration) Outcome

type executorPanicError struct{ value any }

func (e *executorPanicError) Error() string { return fmt.Sprintf("executor panicked: %v", e.value) }

type Status string

const (
	StatusPreparing Status = "preparing"
	StatusCountdown Status = "countdown"
	StatusRunning   Status = "running"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusClosed    Status = "closed"
)

var (
	ErrClosed         = errors.New("sequencer is closed")
	ErrFailed         = errors.New("sequencer has failed")
	ErrInvalidState   = errors.New("sequencer lifecycle transition is invalid")
	ErrActionConflict = errors.New("action id has a different payload")
)

type Snapshot struct {
	Status        Status
	Elapsed       time.Duration
	NextScheduled uint64
	Sequence      uint64
	Failure       error
}

// Checkpoint restores a non-active lifecycle state. Schedule must already be
// replay-advanced through NextScheduled. Countdown is intentionally not
// restorable: pausing during countdown freezes at zero and resume starts
// market time directly.
type Checkpoint struct {
	Status        Status
	Elapsed       time.Duration
	NextScheduled uint64
	Sequence      uint64
}

// ReplayLookup runs on the sequencer goroutine and must not call back into its
// Sequencer. The returned Execution is the authoritative original execution.
type ReplayLookup func(Action) (Execution, bool)

// Projector builds an immutable read model while the sequencer is stopped at
// an authoritative point on its logical timeline.
type Projector func() any

type View struct {
	Snapshot Snapshot
	Value    any
}

type Config struct {
	Schedule     Schedule
	Executor     Executor
	ReplayLookup ReplayLookup
	Clock        Clock
	Countdown    time.Duration
	// CountdownAction runs on the sequencer goroutine and must not call back
	// into its Sequencer.
	CountdownAction func(Action) Action
	Checkpoint      Checkpoint
	// DurableLifecycle disables the in-memory Start, Pause, and Resume controls.
	DurableLifecycle bool
}

// Clock must be monotonic for the lifetime of a Sequencer.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTimer(delay time.Duration) Timer {
	return systemTimer{timer: time.NewTimer(delay)}
}

type systemTimer struct{ timer *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.timer.C }
func (t systemTimer) Stop() bool          { return t.timer.Stop() }

type requestKind uint8

const (
	requestStart requestKind = iota
	requestStartAction
	requestSubmit
	requestPause
	requestPauseAction
	requestResume
	requestResumeAction
	requestCompleteAction
	requestView
	requestSnapshot
	requestClose
)

type request struct {
	kind     requestKind
	received time.Time
	action   Action
	project  Projector
	reply    chan response
}

type response struct {
	execution Execution
	snapshot  Snapshot
	view      View
	err       error
}

type Sequencer struct {
	clock            Clock
	executor         Executor
	schedule         Schedule
	replay           ReplayLookup
	countdown        time.Duration
	countdownAction  func(Action) Action
	checkpoint       Checkpoint
	durableLifecycle bool
	wake             chan struct{}
	done             chan struct{}
	admission        sync.Mutex
	pending          []request
	accepting        bool
	shutdown         sync.Mutex
	final            Snapshot
	finalErr         error
	closed           bool
}

func New(schedule []ScheduledAction, executor Executor) (*Sequencer, error) {
	return NewWithClock(schedule, executor, systemClock{})
}

func NewWithClock(schedule []ScheduledAction, executor Executor, clock Clock) (*Sequencer, error) {
	static, err := newStaticSchedule(schedule)
	if err != nil {
		return nil, err
	}
	return NewWithScheduleAndClock(static, executor, clock)
}

func NewWithSchedule(schedule Schedule, executor Executor) (*Sequencer, error) {
	return NewWithScheduleAndClock(schedule, executor, systemClock{})
}

func NewWithScheduleAndClock(schedule Schedule, executor Executor, clock Clock) (*Sequencer, error) {
	return NewConfigured(Config{Schedule: schedule, Executor: executor, Clock: clock})
}

func NewConfigured(config Config) (*Sequencer, error) {
	if isNilInterface(config.Schedule) || config.Executor == nil {
		return nil, errors.New("schedule and executor are required")
	}
	if isNilInterface(config.Clock) {
		config.Clock = systemClock{}
	}
	if config.Countdown < 0 {
		return nil, errors.New("countdown must be non-negative")
	}
	if config.Countdown > 0 && config.CountdownAction == nil {
		return nil, errors.New("countdown action factory is required")
	}
	if config.DurableLifecycle && (config.Countdown <= 0 || config.ReplayLookup == nil || config.CountdownAction == nil) {
		return nil, errors.New("durable lifecycle requires countdown, replay lookup, and countdown action factory")
	}
	checkpoint := config.Checkpoint
	if checkpoint.Status == "" {
		checkpoint.Status = StatusPreparing
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return nil, err
	}
	if _, err := currentTime(config.Clock); err != nil {
		return nil, err
	}
	sequencer := &Sequencer{
		clock: config.Clock, executor: config.Executor, schedule: config.Schedule,
		replay: config.ReplayLookup, countdown: config.Countdown,
		countdownAction: config.CountdownAction, checkpoint: checkpoint,
		durableLifecycle: config.DurableLifecycle,
		wake:             make(chan struct{}, 1), done: make(chan struct{}), accepting: true,
	}
	go sequencer.run()
	return sequencer, nil
}

func validateCheckpoint(checkpoint Checkpoint) error {
	if checkpoint.Elapsed < 0 {
		return errors.New("checkpoint elapsed time must be non-negative")
	}
	if checkpoint.NextScheduled > checkpoint.Sequence {
		return errors.New("checkpoint next scheduled exceeds sequence")
	}
	switch checkpoint.Status {
	case StatusPreparing:
		if checkpoint.Elapsed != 0 || checkpoint.NextScheduled != 0 || checkpoint.Sequence != 0 {
			return errors.New("preparing checkpoint must be empty")
		}
	case StatusPaused:
	case StatusCompleted:
		if checkpoint.Sequence == 0 {
			return errors.New("completed checkpoint must have executed an action")
		}
	default:
		return fmt.Errorf("checkpoint status %q cannot be restored", checkpoint.Status)
	}
	return nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func nextScheduledAction(schedule Schedule) (action ScheduledAction, ok bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("schedule next panicked: %v", recovered)
		}
	}()
	return schedule.Next()
}

func commitScheduledAction(schedule Schedule, action ScheduledAction) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("schedule commit panicked: %v", recovered)
		}
	}()
	return schedule.Commit(action)
}

func createTimer(clock Clock, delay time.Duration) (timer Timer, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("clock timer panicked: %v", recovered)
		}
	}()
	timer = clock.NewTimer(delay)
	if isNilInterface(timer) {
		return nil, errors.New("clock returned a nil timer")
	}
	return timer, nil
}

func currentTime(clock Clock) (now time.Time, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("clock now panicked: %v", recovered)
		}
	}()
	return clock.Now(), nil
}

func clockTimerChannel(timer Timer) (channel <-chan time.Time, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("clock timer channel panicked: %v", recovered)
		}
	}()
	channel = timer.C()
	if channel == nil {
		return nil, errors.New("clock returned a nil timer channel")
	}
	return channel, nil
}

func stopClockTimer(timer Timer) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("clock timer stop panicked: %v", recovered)
		}
	}()
	timer.Stop()
	return nil
}

type staticSchedule struct {
	actions []ScheduledAction
	next    int
}

func newStaticSchedule(schedule []ScheduledAction) (*staticSchedule, error) {
	actions := append([]ScheduledAction(nil), schedule...)
	actionIDs := make(map[string]struct{}, len(actions))
	for i, item := range actions {
		if err := validateScheduledAction(item); err != nil {
			return nil, fmt.Errorf("scheduled action %d: %w", i, err)
		}
		if _, exists := actionIDs[item.Action.ID]; exists {
			return nil, fmt.Errorf("scheduled action id %q is duplicated", item.Action.ID)
		}
		actionIDs[item.Action.ID] = struct{}{}
		if i > 0 && item.Due < actions[i-1].Due {
			return nil, errors.New("schedule must be ordered by due time")
		}
	}
	return &staticSchedule{actions: actions}, nil
}

func (s *staticSchedule) Next() (ScheduledAction, bool, error) {
	if s.next >= len(s.actions) {
		return ScheduledAction{}, false, nil
	}
	return s.actions[s.next], true, nil
}

func (s *staticSchedule) Commit(action ScheduledAction) error {
	if s.next >= len(s.actions) || s.actions[s.next].Due != action.Due || s.actions[s.next].Action.ID != action.Action.ID {
		return errors.New("committed action does not match schedule head")
	}
	s.next++
	return nil
}

func (s *Sequencer) Start(ctx context.Context) error {
	response, err := s.request(ctx, request{kind: requestStart})
	if err != nil {
		return err
	}
	return response.err
}

func (s *Sequencer) StartAction(ctx context.Context, action Action) (Execution, error) {
	if err := validateLifecycleAction(action, ActionStartSession, false); err != nil {
		return Execution{}, err
	}
	response, err := s.request(ctx, request{kind: requestStartAction, action: action})
	if err != nil {
		return Execution{}, err
	}
	return response.execution, response.err
}

func (s *Sequencer) Submit(ctx context.Context, action Action) (Execution, error) {
	if err := validateAction(action, SourceParticipant); err != nil {
		return Execution{}, err
	}
	response, err := s.request(ctx, request{kind: requestSubmit, action: action})
	if err != nil {
		return Execution{}, err
	}
	return response.execution, response.err
}

func (s *Sequencer) Pause(ctx context.Context) (Snapshot, error) {
	response, err := s.request(ctx, request{kind: requestPause})
	if err != nil {
		return Snapshot{}, err
	}
	return response.snapshot, response.err
}

func (s *Sequencer) PauseAction(ctx context.Context, action Action) (Execution, error) {
	if err := validateLifecycleAction(action, ActionPauseSession, true); err != nil {
		return Execution{}, err
	}
	response, err := s.request(ctx, request{kind: requestPauseAction, action: action})
	if err != nil {
		return Execution{}, err
	}
	return response.execution, response.err
}

func (s *Sequencer) Resume(ctx context.Context) error {
	response, err := s.request(ctx, request{kind: requestResume})
	if err != nil {
		return err
	}
	return response.err
}

func (s *Sequencer) ResumeAction(ctx context.Context, action Action) (Execution, error) {
	if err := validateLifecycleAction(action, ActionResumeSession, false); err != nil {
		return Execution{}, err
	}
	response, err := s.request(ctx, request{kind: requestResumeAction, action: action})
	if err != nil {
		return Execution{}, err
	}
	return response.execution, response.err
}

func (s *Sequencer) CompleteAction(ctx context.Context, action Action) (Execution, error) {
	if err := validateLifecycleAction(action, ActionQuitSession, false); err != nil {
		return Execution{}, err
	}
	response, err := s.request(ctx, request{kind: requestCompleteAction, action: action})
	if err != nil {
		return Execution{}, err
	}
	return response.execution, response.err
}

func (s *Sequencer) View(ctx context.Context, project Projector) (View, error) {
	if project == nil {
		return View{}, errors.New("projector is required")
	}
	response, err := s.request(ctx, request{kind: requestView, project: project})
	if err != nil {
		return View{}, err
	}
	return response.view, response.err
}

func (s *Sequencer) Snapshot(ctx context.Context) (Snapshot, error) {
	response, err := s.request(ctx, request{kind: requestSnapshot})
	if err != nil {
		return Snapshot{}, err
	}
	return response.snapshot, response.err
}

func (s *Sequencer) Close() error {
	_, err := s.Shutdown(context.Background())
	return err
}

// Shutdown is an in-memory stop. Durable lifecycle configurations must
// successfully PauseAction before calling Shutdown.
func (s *Sequencer) Shutdown(ctx context.Context) (Snapshot, error) {
	s.shutdown.Lock()
	defer s.shutdown.Unlock()
	if s.closed {
		return s.final, s.finalErr
	}
	response, err := s.request(ctx, request{kind: requestClose})
	if err != nil {
		return Snapshot{}, err
	}
	<-s.done
	s.final, s.finalErr, s.closed = response.snapshot, response.err, true
	return s.final, s.finalErr
}

func (s *Sequencer) request(ctx context.Context, req request) (response, error) {
	if err := ctx.Err(); err != nil {
		return response{}, err
	}
	req.reply = make(chan response, 1)
	s.admission.Lock()
	if !s.accepting {
		s.admission.Unlock()
		return response{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		s.admission.Unlock()
		return response{}, err
	}
	now, err := currentTime(s.clock)
	if err != nil {
		s.admission.Unlock()
		return response{}, err
	}
	req.received = now
	s.pending = append(s.pending, req)
	s.admission.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	// Once admitted, always return the authoritative outcome. Callers use
	// action IDs for higher-level retries rather than abandoning an in-flight
	// mutation when their waiting context expires.
	return <-req.reply, nil
}

func (s *Sequencer) dequeue() (request, bool) {
	s.admission.Lock()
	defer s.admission.Unlock()
	if len(s.pending) == 0 {
		return request{}, false
	}
	req := s.pending[0]
	s.pending[0] = request{}
	s.pending = s.pending[1:]
	return req, true
}

func (s *Sequencer) stopAdmission() []request {
	s.admission.Lock()
	defer s.admission.Unlock()
	s.accepting = false
	pending := s.pending
	s.pending = nil
	return pending
}

func validateAction(action Action, source Source) error {
	if action.ID == "" || action.Kind == "" {
		return errors.New("action id and kind are required")
	}
	if action.Source != source {
		return fmt.Errorf("action source must be %q", source)
	}
	if source == SourceParticipant && strings.HasPrefix(action.ID, SystemActionIDPrefix) {
		return errors.New("participant action id uses the reserved system namespace")
	}
	return nil
}

func validateLifecycleAction(action Action, kind string, allowSystem bool) error {
	if action.Kind != kind {
		return fmt.Errorf("lifecycle action kind must be %q", kind)
	}
	if action.Source == SourceSystem && allowSystem {
		if err := validateAction(action, SourceSystem); err != nil {
			return err
		}
		if err := validatePayloadType(action); err != nil {
			return err
		}
		return validatePauseSourceReason(action)
	}
	if err := validateAction(action, SourceParticipant); err != nil {
		return err
	}
	if err := validatePayloadType(action); err != nil {
		return err
	}
	if kind == ActionPauseSession {
		return validatePauseSourceReason(action)
	}
	return nil
}

func validatePauseSourceReason(action Action) error {
	payload := action.Payload.(PauseSessionPayload)
	if action.Source == SourceParticipant && payload.Reason != PauseReasonPlayer {
		return errors.New("participant pause reason must be player")
	}
	if action.Source == SourceSystem && payload.Reason != PauseReasonShutdown && payload.Reason != PauseReasonRecovery && payload.Reason != PauseReasonDisconnect {
		return errors.New("system pause reason must be shutdown or recovery")
	}
	return nil
}

func validateScheduledAction(action ScheduledAction) error {
	if action.Due < 0 {
		return errors.New("scheduled time must be non-negative")
	}
	return validateAction(action.Action, SourceSystem)
}

func normalizeOutcome(outcome Outcome) Outcome {
	switch outcome.Disposition {
	case DispositionContinue, DispositionComplete:
		if outcome.Err != nil {
			outcome.Disposition = DispositionFail
			outcome.Err = errors.New("successful executor outcome contains an error")
		}
	case DispositionReject, DispositionFail:
		if outcome.Err == nil {
			outcome.Disposition = DispositionFail
			outcome.Err = errors.New("failed executor outcome is missing an error")
		}
	default:
		outcome.Disposition = DispositionFail
		outcome.Err = errors.New("executor returned an invalid disposition")
	}
	return outcome
}

func normalizeExecution(execution Execution) Execution {
	outcome := normalizeOutcome(Outcome{
		Result: execution.Result, Disposition: execution.Disposition, Err: execution.Err,
	})
	execution.Result, execution.Disposition, execution.Err = outcome.Result, outcome.Disposition, outcome.Err
	return execution
}

func (s *Sequencer) run() {
	defer func() {
		for _, req := range s.stopAdmission() {
			req.reply <- response{err: ErrClosed}
		}
		close(s.done)
	}()
	status := s.checkpoint.Status
	elapsed := s.checkpoint.Elapsed
	anchor := time.Time{}
	nextScheduled := s.checkpoint.NextScheduled
	sequence := s.checkpoint.Sequence
	var failure error
	var timer Timer
	var candidate ScheduledAction
	hasCandidate := false
	lastScheduledDue := time.Duration(0)
	scheduledIDs := make(map[string]struct{})
	var countdownStart Action
	var countdownAnchor time.Time

	logicalAt := func(now time.Time) time.Duration {
		if status != StatusRunning || now.Before(anchor) {
			return elapsed
		}
		return elapsed + now.Sub(anchor)
	}
	stopTimer := func() error {
		current := timer
		timer = nil
		if current != nil {
			return stopClockTimer(current)
		}
		return nil
	}
	fail := func(at time.Duration, err error) {
		failure, elapsed, status = err, at, StatusFailed
		_ = stopTimer()
	}
	loadCandidate := func() (ScheduledAction, bool, error) {
		if hasCandidate {
			return candidate, true, nil
		}
		next, ok, err := nextScheduledAction(s.schedule)
		if err != nil || !ok {
			return ScheduledAction{}, ok, err
		}
		if err := validateScheduledAction(next); err != nil {
			return ScheduledAction{}, false, err
		}
		if nextScheduled > 0 && next.Due < lastScheduledDue {
			return ScheduledAction{}, false, errors.New("schedule moved backward")
		}
		if _, exists := scheduledIDs[next.Action.ID]; exists {
			return ScheduledAction{}, false, fmt.Errorf("scheduled action id %q is duplicated", next.Action.ID)
		}
		candidate, hasCandidate = next, true
		return candidate, true, nil
	}
	commitCandidate := func(item ScheduledAction) error {
		if !hasCandidate || candidate.Due != item.Due || candidate.Action.ID != item.Action.ID {
			return errors.New("executed action does not match schedule head")
		}
		if err := commitScheduledAction(s.schedule, item); err != nil {
			return err
		}
		scheduledIDs[item.Action.ID] = struct{}{}
		lastScheduledDue = item.Due
		nextScheduled++
		hasCandidate = false
		candidate = ScheduledAction{}
		return nil
	}
	armTimerAt := func(now time.Time) error {
		if err := stopTimer(); err != nil {
			fail(elapsed, err)
			return failure
		}
		if status != StatusRunning {
			return nil
		}
		next, ok, err := loadCandidate()
		if err != nil {
			fail(logicalAt(now), fmt.Errorf("load next scheduled action: %w", err))
			return failure
		}
		if !ok {
			return nil
		}
		delay := next.Due - logicalAt(now)
		if delay < 0 {
			delay = 0
		}
		created, err := createTimer(s.clock, delay)
		if err != nil {
			fail(logicalAt(now), err)
			return failure
		}
		timer = created
		return nil
	}
	armTimer := func() error {
		now, err := currentTime(s.clock)
		if err != nil {
			fail(elapsed, err)
			return failure
		}
		return armTimerAt(now)
	}
	armCountdownTimer := func(delay time.Duration) error {
		if err := stopTimer(); err != nil {
			fail(0, err)
			return failure
		}
		created, err := createTimer(s.clock, delay)
		if err != nil {
			fail(0, err)
			return failure
		}
		timer = created
		return nil
	}
	rearmCountdownTimer := func() error {
		now, err := currentTime(s.clock)
		if err != nil {
			fail(0, err)
			return failure
		}
		remaining := countdownAnchor.Add(s.countdown).Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		return armCountdownTimer(remaining)
	}
	execute := func(action Action, at time.Duration) (execution Execution) {
		sequence++
		execution = Execution{Sequence: sequence, Elapsed: at, Action: action}
		defer func() {
			if recovered := recover(); recovered != nil {
				execution.Disposition = DispositionFail
				execution.Err = &executorPanicError{value: recovered}
			}
		}()
		outcome := normalizeOutcome(s.executor(action, at))
		execution.Result, execution.Disposition, execution.Err = outcome.Result, outcome.Disposition, outcome.Err
		return execution
	}
	completeCountdown := func() error {
		if err := stopTimer(); err != nil {
			fail(0, err)
			return failure
		}
		var action Action
		var factoryErr error
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					factoryErr = fmt.Errorf("countdown action factory panicked: %v", recovered)
				}
			}()
			action = s.countdownAction(countdownStart)
		}()
		if factoryErr == nil {
			if action.Kind != ActionCountdownComplete {
				factoryErr = fmt.Errorf("countdown action kind must be %q", ActionCountdownComplete)
			} else if factoryErr = validateAction(action, SourceSystem); factoryErr == nil {
				factoryErr = validatePayloadType(action)
			}
		}
		if factoryErr != nil {
			fail(0, factoryErr)
			return failure
		}
		execution := execute(action, 0)
		if execution.Disposition != DispositionContinue {
			cause := execution.Err
			if cause == nil {
				cause = errors.New("countdown action must continue")
			}
			fail(0, fmt.Errorf("countdown action %q failed: %w", action.ID, cause))
			return failure
		}
		now, err := currentTime(s.clock)
		if err != nil {
			fail(0, err)
			return failure
		}
		anchor, status = now, StatusRunning
		return armTimerAt(now)
	}
	replay := func(action Action) (Execution, bool) {
		if s.replay == nil {
			return Execution{}, false
		}
		var execution Execution
		var found bool
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					execution = Execution{Action: action, Disposition: DispositionFail, Err: fmt.Errorf("replay lookup panicked: %v", recovered)}
					found = true
				}
			}()
			execution, found = s.replay(action)
		}()
		if !found {
			return Execution{}, false
		}
		return normalizeExecution(execution), true
	}
	processDue := func(target time.Duration) error {
		for {
			item, ok, err := loadCandidate()
			if err != nil {
				fail(target, fmt.Errorf("load next scheduled action: %w", err))
				return failure
			}
			if !ok || item.Due > target {
				return nil
			}
			execution := execute(item.Action, item.Due)
			if execution.Disposition == DispositionReject || execution.Disposition == DispositionFail {
				fail(item.Due, fmt.Errorf("scheduled action %q failed: %w", item.Action.ID, execution.Err))
				return failure
			}
			if err := commitCandidate(item); err != nil {
				fail(item.Due, fmt.Errorf("commit scheduled action %q: %w", item.Action.ID, err))
				return failure
			}
			if execution.Disposition == DispositionComplete {
				elapsed, status = item.Due, StatusCompleted
				stopTimer()
				return nil
			}
		}
	}
	snapshot := func(now time.Time) Snapshot {
		return Snapshot{Status: status, Elapsed: logicalAt(now), NextScheduled: nextScheduled, Sequence: sequence, Failure: failure}
	}
	handleRequest := func(req request) bool {
		switch req.kind {
		case requestStartAction, requestPauseAction, requestResumeAction, requestCompleteAction, requestSubmit:
			if execution, found := replay(req.action); found {
				execution.Replayed = true
				var err error
				if execution.Disposition == DispositionReject || execution.Disposition == DispositionFail {
					err = execution.Err
				}
				req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: err}
				return false
			}
		}
		if (req.kind == requestPauseAction || req.kind == requestCompleteAction || req.kind == requestSubmit || req.kind == requestView) && status == StatusCountdown && !req.received.Before(countdownAnchor.Add(s.countdown)) {
			_ = completeCountdown()
		}
		if status == StatusFailed && req.kind != requestSnapshot && req.kind != requestClose {
			req.reply <- response{snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
			return false
		}
		if status == StatusCompleted && req.kind != requestSnapshot && req.kind != requestView && req.kind != requestClose {
			req.reply <- response{snapshot: snapshot(req.received), err: ErrInvalidState}
			return false
		}
		switch req.kind {
		case requestStart:
			if s.durableLifecycle {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			if status != StatusPreparing {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			status, elapsed, anchor = StatusRunning, 0, req.received
			if err := armTimer(); err != nil {
				req.reply <- response{snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
				return false
			}
			req.reply <- response{}
		case requestStartAction:
			if status != StatusPreparing || s.countdown <= 0 {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			execution := execute(req.action, 0)
			switch execution.Disposition {
			case DispositionContinue:
				now, err := currentTime(s.clock)
				if err != nil {
					fail(0, err)
					req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
					return false
				}
				status, elapsed, countdownStart, countdownAnchor = StatusCountdown, 0, req.action, now
				if err := armCountdownTimer(s.countdown); err != nil {
					req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
					return false
				}
				req.reply <- response{execution: execution}
			case DispositionReject:
				req.reply <- response{execution: execution, err: execution.Err}
			case DispositionComplete, DispositionFail:
				cause := execution.Err
				if cause == nil {
					cause = errors.New("start action must continue")
				}
				fail(0, fmt.Errorf("start action %q failed: %w", req.action.ID, cause))
				req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
			}
		case requestSubmit:
			if status != StatusRunning {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			target := logicalAt(req.received)
			if err := processDue(target); err != nil {
				req.reply <- response{snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
				return false
			}
			if status == StatusCompleted {
				req.reply <- response{snapshot: snapshot(req.received), err: ErrInvalidState}
				return false
			}
			execution := execute(req.action, target)
			switch execution.Disposition {
			case DispositionContinue:
				if err := armTimer(); err != nil {
					req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
					return false
				}
				req.reply <- response{execution: execution}
			case DispositionComplete:
				elapsed, status = target, StatusCompleted
				stopTimer()
				req.reply <- response{execution: execution, snapshot: snapshot(req.received)}
			case DispositionReject:
				if err := armTimer(); err != nil {
					req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
					return false
				}
				req.reply <- response{execution: execution, err: execution.Err}
			case DispositionFail:
				fail(target, fmt.Errorf("participant action %q failed: %w", req.action.ID, execution.Err))
				req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
			}
		case requestPause:
			if s.durableLifecycle {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			if status != StatusRunning {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			target := logicalAt(req.received)
			if err := processDue(target); err != nil {
				req.reply <- response{snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
				return false
			}
			if status == StatusCompleted {
				req.reply <- response{snapshot: snapshot(req.received)}
				return false
			}
			elapsed, status = target, StatusPaused
			stopTimer()
			req.reply <- response{snapshot: snapshot(req.received)}
		case requestPauseAction:
			if status != StatusRunning && status != StatusCountdown {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			target := time.Duration(0)
			if status == StatusRunning {
				target = logicalAt(req.received)
				if err := processDue(target); err != nil {
					req.reply <- response{snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
					return false
				}
				if status == StatusCompleted {
					req.reply <- response{snapshot: snapshot(req.received), err: ErrInvalidState}
					return false
				}
			}
			execution := execute(req.action, target)
			switch execution.Disposition {
			case DispositionContinue:
				// Countdown progress is intentionally discarded. A durable resume
				// starts market time directly from this elapsed-zero checkpoint.
				elapsed, status = target, StatusPaused
				if err := stopTimer(); err != nil {
					fail(target, err)
					req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
					return false
				}
				req.reply <- response{execution: execution, snapshot: snapshot(req.received)}
			case DispositionReject, DispositionComplete, DispositionFail:
				cause := execution.Err
				if cause == nil {
					cause = errors.New("pause action executor must continue")
				}
				fail(target, fmt.Errorf("pause action %q failed: %w", req.action.ID, cause))
				req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
			}
		case requestResume:
			if s.durableLifecycle {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			if status != StatusPaused {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			status, anchor = StatusRunning, req.received
			if err := armTimer(); err != nil {
				req.reply <- response{snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
				return false
			}
			req.reply <- response{}
		case requestResumeAction:
			if status != StatusPaused {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			execution := execute(req.action, elapsed)
			switch execution.Disposition {
			case DispositionContinue:
				now, err := currentTime(s.clock)
				if err != nil {
					fail(elapsed, err)
					req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
					return false
				}
				status, anchor = StatusRunning, now
				if err := armTimerAt(now); err != nil {
					req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
					return false
				}
				req.reply <- response{execution: execution}
			case DispositionReject, DispositionComplete, DispositionFail:
				cause := execution.Err
				if cause == nil {
					cause = errors.New("resume action executor must continue")
				}
				fail(elapsed, fmt.Errorf("resume action %q failed: %w", req.action.ID, cause))
				req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
			}
		case requestCompleteAction:
			if status != StatusPreparing && status != StatusCountdown && status != StatusRunning && status != StatusPaused {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			target := elapsed
			if status == StatusRunning {
				target = logicalAt(req.received)
				if err := processDue(target); err != nil {
					req.reply <- response{snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
					return false
				}
				if status == StatusCompleted {
					req.reply <- response{snapshot: snapshot(req.received), err: ErrInvalidState}
					return false
				}
			}
			execution := execute(req.action, target)
			switch execution.Disposition {
			case DispositionComplete:
				elapsed, status = target, StatusCompleted
				stopTimer()
				req.reply <- response{execution: execution, snapshot: snapshot(req.received)}
			case DispositionReject:
				if status == StatusRunning {
					if err := armTimer(); err != nil {
						req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
						return false
					}
				}
				req.reply <- response{execution: execution, err: execution.Err}
			case DispositionContinue, DispositionFail:
				cause := execution.Err
				if cause == nil {
					cause = errors.New("completion action executor must complete")
				}
				fail(target, fmt.Errorf("completion action %q failed: %w", req.action.ID, cause))
				req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
			}
		case requestView:
			if status == StatusRunning {
				target := logicalAt(req.received)
				if err := processDue(target); err != nil {
					req.reply <- response{snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
					return false
				}
				if status == StatusRunning {
					if err := armTimer(); err != nil {
						req.reply <- response{snapshot: snapshot(req.received), err: errors.Join(ErrFailed, err)}
						return false
					}
				}
			}
			var value any
			var projectErr error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						projectErr = fmt.Errorf("projector panicked: %v", recovered)
					}
				}()
				value = req.project()
			}()
			if projectErr != nil {
				req.reply <- response{snapshot: snapshot(req.received), err: projectErr}
				return false
			}
			view := View{Snapshot: snapshot(req.received), Value: value}
			req.reply <- response{snapshot: view.Snapshot, view: view}
		case requestSnapshot:
			req.reply <- response{snapshot: snapshot(req.received)}
		case requestClose:
			var shutdownErr error
			if status == StatusFailed {
				shutdownErr = errors.Join(ErrFailed, failure)
			}
			if status == StatusRunning {
				target := logicalAt(req.received)
				if err := processDue(target); err != nil {
					shutdownErr = errors.Join(ErrFailed, err)
				} else if status == StatusRunning {
					elapsed, status = target, StatusPaused
				}
			} else if status == StatusCountdown {
				status, elapsed = StatusPaused, 0
			}
			stopTimer()
			final := snapshot(req.received)
			queued := s.stopAdmission()
			req.reply <- response{snapshot: final, err: shutdownErr}
			for _, pending := range queued {
				pending.reply <- response{err: ErrClosed}
			}
			status = StatusClosed
			return true
		}
		return false
	}

	for {
		// Requests already waiting at the serialized admission point take priority
		// over timers. Their receipt timestamp is authoritative, except that a
		// pause or submit received at the countdown deadline completes countdown
		// first.
		if req, ok := s.dequeue(); ok {
			if handleRequest(req) {
				return
			}
			continue
		}
		var timerC <-chan time.Time
		if timer != nil {
			channel, err := clockTimerChannel(timer)
			if err != nil {
				fail(elapsed, err)
			} else {
				timerC = channel
			}
		}
		select {
		case <-s.wake:
			continue
		case now := <-timerC:
			timer = nil
			if req, ok := s.dequeue(); ok {
				if handleRequest(req) {
					return
				}
				// The timer channel was consumed before the earlier request ran.
				// If that operation left countdown active, use current wall time
				// rather than its stale receipt time to restore the deadline.
				if status == StatusCountdown && timer == nil {
					_ = rearmCountdownTimer()
				}
				continue
			}
			if status == StatusRunning {
				_ = processDue(logicalAt(now))
				_ = armTimer()
			} else if status == StatusCountdown {
				_ = completeCountdown()
			}
		}
	}
}
