// Package realtime orders scheduled market activity and participant actions on
// one logical timeline. It does not know about exchange mechanics or transport.
package realtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Source string

const (
	SourceSystem      Source = "system"
	SourceParticipant Source = "participant"
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

type Execution struct {
	Sequence    uint64
	Elapsed     time.Duration
	Action      Action
	Result      any
	Disposition Disposition
	Err         error
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
	StatusRunning   Status = "running"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusClosed    Status = "closed"
)

var (
	ErrClosed       = errors.New("sequencer is closed")
	ErrFailed       = errors.New("sequencer has failed")
	ErrInvalidState = errors.New("sequencer lifecycle transition is invalid")
)

type Snapshot struct {
	Status        Status
	Elapsed       time.Duration
	NextScheduled int
	Sequence      uint64
	Failure       error
}

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
	requestSubmit
	requestPause
	requestResume
	requestSnapshot
	requestClose
)

type request struct {
	kind     requestKind
	received time.Time
	action   Action
	reply    chan response
}

type response struct {
	execution Execution
	snapshot  Snapshot
	err       error
}

type Sequencer struct {
	clock     Clock
	executor  Executor
	schedule  []ScheduledAction
	wake      chan struct{}
	done      chan struct{}
	admission sync.Mutex
	pending   []request
	accepting bool
	shutdown  sync.Mutex
	final     Snapshot
	finalErr  error
	closed    bool
}

func New(schedule []ScheduledAction, executor Executor) (*Sequencer, error) {
	return NewWithClock(schedule, executor, systemClock{})
}

func NewWithClock(schedule []ScheduledAction, executor Executor, clock Clock) (*Sequencer, error) {
	if executor == nil || clock == nil {
		return nil, errors.New("executor and clock are required")
	}
	copySchedule := append([]ScheduledAction(nil), schedule...)
	actionIDs := make(map[string]struct{}, len(copySchedule))
	for i, item := range copySchedule {
		if item.Due < 0 {
			return nil, errors.New("scheduled time must be non-negative")
		}
		if err := validateAction(item.Action, SourceSystem); err != nil {
			return nil, fmt.Errorf("scheduled action %d: %w", i, err)
		}
		if _, exists := actionIDs[item.Action.ID]; exists {
			return nil, fmt.Errorf("scheduled action id %q is duplicated", item.Action.ID)
		}
		actionIDs[item.Action.ID] = struct{}{}
		if i > 0 && item.Due < copySchedule[i-1].Due {
			return nil, errors.New("schedule must be ordered by due time")
		}
	}
	sequencer := &Sequencer{clock: clock, executor: executor, schedule: copySchedule, wake: make(chan struct{}, 1), done: make(chan struct{}), accepting: true}
	go sequencer.run()
	return sequencer, nil
}

func (s *Sequencer) Start(ctx context.Context) error {
	response, err := s.request(ctx, request{kind: requestStart})
	if err != nil {
		return err
	}
	return response.err
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

func (s *Sequencer) Resume(ctx context.Context) error {
	response, err := s.request(ctx, request{kind: requestResume})
	if err != nil {
		return err
	}
	return response.err
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
	req.received = s.clock.Now()
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
	return nil
}

func (s *Sequencer) run() {
	defer func() {
		for _, req := range s.stopAdmission() {
			req.reply <- response{err: ErrClosed}
		}
		close(s.done)
	}()
	status := StatusPreparing
	elapsed := time.Duration(0)
	anchor := s.clock.Now()
	nextScheduled := 0
	sequence := uint64(0)
	var failure error
	var timer Timer

	logicalAt := func(now time.Time) time.Duration {
		if status != StatusRunning || now.Before(anchor) {
			return elapsed
		}
		return elapsed + now.Sub(anchor)
	}
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
	}
	armTimer := func() {
		stopTimer()
		if status != StatusRunning || nextScheduled >= len(s.schedule) {
			return
		}
		delay := s.schedule[nextScheduled].Due - logicalAt(s.clock.Now())
		if delay < 0 {
			delay = 0
		}
		timer = s.clock.NewTimer(delay)
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
		outcome := s.executor(action, at)
		execution.Result, execution.Disposition, execution.Err = outcome.Result, outcome.Disposition, outcome.Err
		switch execution.Disposition {
		case DispositionContinue, DispositionComplete:
			if execution.Err != nil {
				execution.Disposition = DispositionFail
				execution.Err = errors.New("successful executor outcome contains an error")
			}
		case DispositionReject, DispositionFail:
			if execution.Err == nil {
				execution.Disposition = DispositionFail
				execution.Err = errors.New("failed executor outcome is missing an error")
			}
		default:
			execution.Disposition = DispositionFail
			execution.Err = errors.New("executor returned an invalid disposition")
		}
		return execution
	}
	fail := func(at time.Duration, err error) {
		failure, elapsed, status = err, at, StatusFailed
		stopTimer()
	}
	processDue := func(target time.Duration) error {
		for nextScheduled < len(s.schedule) && s.schedule[nextScheduled].Due <= target {
			item := s.schedule[nextScheduled]
			execution := execute(item.Action, item.Due)
			if execution.Disposition == DispositionReject || execution.Disposition == DispositionFail {
				fail(item.Due, fmt.Errorf("scheduled action %q failed: %w", item.Action.ID, execution.Err))
				return failure
			}
			nextScheduled++
			if execution.Disposition == DispositionComplete {
				elapsed, status = item.Due, StatusCompleted
				stopTimer()
				return nil
			}
		}
		return nil
	}
	snapshot := func(now time.Time) Snapshot {
		return Snapshot{Status: status, Elapsed: logicalAt(now), NextScheduled: nextScheduled, Sequence: sequence, Failure: failure}
	}
	handleRequest := func(req request) bool {
		if status == StatusFailed && req.kind != requestSnapshot && req.kind != requestClose {
			req.reply <- response{snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
			return false
		}
		if status == StatusCompleted && req.kind != requestSnapshot && req.kind != requestClose {
			req.reply <- response{snapshot: snapshot(req.received), err: ErrInvalidState}
			return false
		}
		switch req.kind {
		case requestStart:
			if status != StatusPreparing {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			status, elapsed, anchor = StatusRunning, 0, req.received
			armTimer()
			req.reply <- response{}
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
				armTimer()
				req.reply <- response{execution: execution}
			case DispositionComplete:
				elapsed, status = target, StatusCompleted
				stopTimer()
				req.reply <- response{execution: execution, snapshot: snapshot(req.received)}
			case DispositionReject:
				armTimer()
				req.reply <- response{execution: execution, err: execution.Err}
			case DispositionFail:
				fail(target, fmt.Errorf("participant action %q failed: %w", req.action.ID, execution.Err))
				req.reply <- response{execution: execution, snapshot: snapshot(req.received), err: errors.Join(ErrFailed, failure)}
			}
		case requestPause:
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
		case requestResume:
			if status != StatusPaused {
				req.reply <- response{err: ErrInvalidState}
				return false
			}
			status, anchor = StatusRunning, req.received
			armTimer()
			req.reply <- response{}
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
		// over timers. Their receipt timestamp is authoritative, and processDue
		// will still execute every scheduled action due at or before that point.
		if req, ok := s.dequeue(); ok {
			if handleRequest(req) {
				return
			}
			continue
		}
		var timerC <-chan time.Time
		if timer != nil {
			timerC = timer.C()
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
				continue
			}
			if status == StatusRunning {
				_ = processDue(logicalAt(now))
				armTimer()
			}
		}
	}
}
