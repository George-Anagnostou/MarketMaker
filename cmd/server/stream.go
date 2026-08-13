package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"market-maker/internal/game"
	"market-maker/internal/realtime"
)

const (
	disconnectGracePeriod = 5 * time.Second
	streamHeartbeatPeriod = 15 * time.Second
	streamBufferSize      = 16
)

type streamMessage struct {
	Sequence uint64
	Event    string
	Data     any
}

type streamSubscriber struct {
	ch         chan streamMessage
	done       chan struct{}
	once       sync.Once
	generation uint64
	ready      bool
	pending    []streamMessage
}

func (s *streamSubscriber) close() {
	s.once.Do(func() { close(s.done) })
}

type controllerState struct {
	nextGeneration  uint64
	active          *streamSubscriber
	graceTimer      *time.Timer
	graceGeneration uint64
	lastError       error
}

func parseStreamCursor(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	if strings.TrimSpace(value) != value {
		return 0, errors.New("Last-Event-ID must be a canonical unsigned integer")
	}
	cursor, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(cursor, 10) != value {
		return 0, errors.New("Last-Event-ID must be a canonical unsigned integer")
	}
	return cursor, nil
}

func (entry *exchangeEntry) streamHandoff(id string, cursor uint64) (*streamSubscriber, streamMessage, []streamMessage, error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.mode != game.PlayModeRealTime || entry.realTime == nil || entry.storageFailed {
		return nil, streamMessage{}, nil, errors.New("real-time stream is unavailable")
	}
	current := entry.replay.DurableRecords
	if cursor > current {
		return nil, streamMessage{}, nil, errors.New("Last-Event-ID is beyond the durable action boundary")
	}
	entry.controller.nextGeneration++
	subscriber := &streamSubscriber{ch: make(chan streamMessage, streamBufferSize), done: make(chan struct{}), generation: entry.controller.nextGeneration, ready: false}
	if entry.controller.active != nil {
		entry.controller.active.close()
	}
	entry.controller.active = subscriber
	entry.metrics.observeStreamConnected()
	if entry.controller.graceTimer != nil {
		entry.controller.graceTimer.Stop()
		entry.controller.graceTimer = nil
	}

	state := entry.realTimeStateResponseLocked(id)
	snapshot := streamMessage{Sequence: cursor, Event: "snapshot", Data: state}
	backfill := make([]streamMessage, 0, current-cursor+1)
	if cursor < current {
		for sequence := cursor + 1; sequence <= current; sequence++ {
			backfill = append(backfill, streamMessageForState(sequence, state))
		}
	}
	subscriber.pending = backfill
	return subscriber, snapshot, backfill, nil
}

func (entry *exchangeEntry) drainPendingControllerMessages(subscriber *streamSubscriber) ([]streamMessage, bool) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.controller.active != subscriber {
		return nil, true
	}
	pending := subscriber.pending
	subscriber.pending = nil
	if len(pending) == 0 {
		subscriber.ready = true
		return nil, true
	}
	return pending, false
}

func (entry *exchangeEntry) connectController() (*streamSubscriber, error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.mode != game.PlayModeRealTime || entry.realTime == nil {
		return nil, errors.New("controller stream requires a real-time game")
	}
	entry.controller.nextGeneration++
	subscriber := &streamSubscriber{ch: make(chan streamMessage, streamBufferSize), done: make(chan struct{}), generation: entry.controller.nextGeneration, ready: true}
	if entry.controller.active != nil {
		entry.controller.active.close()
	}
	entry.controller.active = subscriber
	entry.metrics.observeStreamConnected()
	if entry.controller.graceTimer != nil {
		entry.controller.graceTimer.Stop()
		entry.controller.graceTimer = nil
	}
	return subscriber, nil
}

func (entry *exchangeEntry) disconnectController(subscriber *streamSubscriber) {
	entry.mu.Lock()
	if entry.controller.active != subscriber || entry.controller.graceTimer != nil {
		entry.mu.Unlock()
		return
	}
	entry.controller.active = nil
	entry.controller.graceGeneration = subscriber.generation
	entry.controller.graceTimer = time.AfterFunc(disconnectGracePeriod, func() {
		entry.expireControllerGrace(subscriber.generation)
	})
	entry.metrics.observeStreamClosed()
	entry.mu.Unlock()
}

func (entry *exchangeEntry) expireControllerGrace(generation uint64) {
	entry.mu.Lock()
	if entry.controller.graceGeneration != generation || entry.storageFailed || entry.realTime == nil || entry.realTime.Lifecycle != game.LifecycleRunning {
		entry.mu.Unlock()
		return
	}
	entry.controller.graceTimer = nil
	entry.mu.Unlock()

	id := fmt.Sprintf("%sdisconnect/%d", realtime.SystemActionIDPrefix, generation)
	_, err := entry.systemPauseRealTime(context.Background(), id, realtime.PauseReasonDisconnect)
	if err != nil {
		entry.mu.Lock()
		entry.controller.lastError = err
		entry.mu.Unlock()
	} else {
		entry.metrics.observeStreamAutoPause()
	}
}

func (entry *exchangeEntry) publishStream(message streamMessage) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.publishStreamLocked(message)
}

func (entry *exchangeEntry) publishStreamLocked(message streamMessage) {
	if entry.controller.active == nil {
		return
	}
	if !entry.controller.active.ready {
		entry.controller.active.pending = append(entry.controller.active.pending, message)
		return
	}
	select {
	case entry.controller.active.ch <- message:
	default:
		subscriber := entry.controller.active
		subscriber.close()
		entry.controller.active = nil
		entry.controller.graceGeneration = subscriber.generation
		entry.controller.graceTimer = time.AfterFunc(disconnectGracePeriod, func() {
			entry.expireControllerGrace(subscriber.generation)
		})
	}
}

func writeSSE(w http.ResponseWriter, message streamMessage) error {
	data, err := json.Marshal(message.Data)
	if err != nil {
		return err
	}
	if message.Sequence > 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", message.Sequence); err != nil {
			return err
		}
	}
	if message.Event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", message.Event); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (s *exchangeService) handleExchangeStream(w http.ResponseWriter, r *http.Request, id string, entry *exchangeEntry) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	cursor, err := parseStreamCursor(r.Header.Get("Last-Event-ID"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	subscriber, snapshot, _, err := entry.streamHandoff(id, cursor)
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "beyond") {
			status = http.StatusBadRequest
		}
		writeAPIError(w, status, "invalid_cursor", err.Error())
		return
	}
	defer func() { subscriber.close(); entry.disconnectController(subscriber) }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := writeSSE(w, snapshot); err != nil {
		return
	}
	for {
		pending, ready := entry.drainPendingControllerMessages(subscriber)
		for _, message := range pending {
			if err := writeSSE(w, message); err != nil {
				return
			}
		}
		if ready {
			break
		}
	}

	heartbeat := time.NewTicker(streamHeartbeatPeriod)
	defer heartbeat.Stop()
	for {
		select {
		case message := <-subscriber.ch:
			if err := writeSSE(w, message); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-subscriber.done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func streamMessageForState(sequence uint64, state exchangeStateResponse) streamMessage {
	return streamMessage{Sequence: sequence, Event: "state", Data: state}
}
