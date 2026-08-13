package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"market-maker/internal/eventlog"
	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
	"market-maker/internal/game"
	"market-maker/internal/realtime"
	"market-maker/internal/scenario"
)

const testGameID = "11111111-1111-4111-8111-111111111111"
const testOtherGameID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const testCreateID = "22222222-2222-4222-8222-222222222222"
const testQuoteID = "33333333-3333-4333-8333-333333333333"
const testQuitID = "44444444-4444-4444-8444-444444444444"

type serverManualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*serverManualTimer]struct{}
}

type serverManualTimer struct {
	clock   *serverManualClock
	due     time.Time
	channel chan time.Time
	stopped bool
	fired   bool
}

type nilTimerClock struct{ now time.Time }

func (c nilTimerClock) Now() time.Time                      { return c.now }
func (nilTimerClock) NewTimer(time.Duration) realtime.Timer { return nil }

func newServerManualClock() *serverManualClock {
	return &serverManualClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), timers: make(map[*serverManualTimer]struct{})}
}

func (c *serverManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *serverManualClock) NewTimer(delay time.Duration) realtime.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &serverManualTimer{clock: c, due: c.now.Add(delay), channel: make(chan time.Time, 1)}
	c.timers[timer] = struct{}{}
	if delay <= 0 {
		timer.fired = true
		timer.channel <- c.now
	}
	return timer
}

func (c *serverManualClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if delta < 0 {
		panic("manual clock cannot move backward")
	}
	c.now = c.now.Add(delta)
	for timer := range c.timers {
		if !timer.stopped && !timer.fired && !timer.due.After(c.now) {
			timer.fired = true
			timer.channel <- c.now
		}
	}
}

func (t *serverManualTimer) C() <-chan time.Time { return t.channel }

func (t *serverManualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

func installManualSequencer(service *exchangeService, clock *serverManualClock, calls *int, observed *realtime.Config) {
	service.newSequencer = func(config realtime.Config) (*realtime.Sequencer, error) {
		(*calls)++
		config.Clock = clock
		if observed != nil {
			*observed = config
		}
		return realtime.NewConfigured(config)
	}
}

func waitForRealTime(t *testing.T, entry *exchangeEntry, condition func(*exchangeEntry) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		entry.mu.Lock()
		ready := condition(entry)
		entry.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for real-time state")
		}
		time.Sleep(time.Millisecond)
	}
}

func closeRealTimeSequencer(t *testing.T, entry *exchangeEntry) {
	t.Helper()
	entry.mu.Lock()
	sequencer := entry.sequencer
	entry.mu.Unlock()
	if sequencer == nil {
		return
	}
	snapshot, err := sequencer.Snapshot(t.Context())
	if err == nil && (snapshot.Status == realtime.StatusRunning || snapshot.Status == realtime.StatusCountdown) {
		_, _ = entry.systemPauseRealTime(t.Context(), fmt.Sprintf("system/test_pause/%d", snapshot.Sequence), realtime.PauseReasonShutdown)
	}
	_, _ = sequencer.Shutdown(t.Context())
}

func executionsEqualIgnoringReplay(left, right realtime.Execution) bool {
	left.Replayed, right.Replayed = false, false
	return reflect.DeepEqual(left, right)
}

func postRealTimeCommand(t *testing.T, server *httptest.Server, body string, target any) int {
	t.Helper()
	resp, err := http.Post(server.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestV2RealTimeHTTPCommandsAndState(t *testing.T) {
	service := newExchangeService(t.TempDir())
	service.newSeed = func() (uint64, error) { return 99, nil }
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	server := v2ServerForService(service)
	defer server.Close()

	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	startBody := `{"id":"` + testQuoteID + `","type":"start_session","bid":"99.5000","ask":"100.5000"}`
	var started realTimeCommandResponse
	if status := postRealTimeCommand(t, server, startBody, &started); status != http.StatusOK || started.Command.Replayed || started.Sequence != 1 || started.Elapsed != 0 {
		t.Fatalf("start status/state=%+v", started)
	}
	countdownState := getRealTimeState(t, server)
	if countdownState.RealTime.Lifecycle != game.LifecycleCountdown || countdownState.RealTime.QuoteRevision != 0 || countdownState.RealTime.BidOrderID != nil || countdownState.RealTime.AskOrderID != nil || len(countdownState.RealTime.Depth.Bids) != 0 || len(countdownState.RealTime.Depth.Asks) != 0 {
		t.Fatalf("opening quote became live during countdown: %+v", countdownState)
	}
	var replayed realTimeCommandResponse
	if status := postRealTimeCommand(t, server, startBody, &replayed); status != http.StatusOK || !replayed.Command.Replayed || replayed.Sequence != started.Sequence || replayed.Elapsed != started.Elapsed {
		t.Fatalf("replay status=%d response=%+v", status, replayed)
	}

	clock.Advance(3 * time.Second)
	entry, err := service.load(testGameID)
	if err != nil {
		t.Fatal(err)
	}
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecycleRunning })

	updateID := "55555555-5555-4555-8555-555555555555"
	updateBody := `{"id":"` + updateID + `","type":"update_quote","expected_quote_revision":1,"bid":"99.6000","ask":"100.4000"}`
	var updated realTimeCommandResponse
	if status := postRealTimeCommand(t, server, updateBody, &updated); status != http.StatusOK || updated.Sequence < 3 {
		t.Fatalf("update status=%d response=%+v", status, updated)
	}
	staleBody := `{"id":"66666666-6666-4666-8666-666666666666","type":"update_quote","expected_quote_revision":1,"bid":"99.7000","ask":"100.3000"}`
	var stale apiError
	if status := postRealTimeCommand(t, server, staleBody, &stale); status != http.StatusConflict || stale.Error.Code != "quote_revision_conflict" {
		t.Fatalf("stale status=%d response=%+v", status, stale)
	}
	var replayedStale apiError
	if status := postRealTimeCommand(t, server, staleBody, &replayedStale); status != http.StatusConflict || replayedStale.Error.Code != "quote_revision_conflict" {
		t.Fatalf("replayed stale status=%d response=%+v", status, replayedStale)
	}
	conflictingBody := `{"id":"` + updateID + `","type":"update_quote","expected_quote_revision":2,"bid":"99.7000","ask":"100.3000"}`
	var conflicting apiError
	if status := postRealTimeCommand(t, server, conflictingBody, &conflicting); status != http.StatusConflict || conflicting.Error.Code != "idempotency_key_reused" {
		t.Fatalf("conflicting status=%d response=%+v", status, conflicting)
	}

	var state exchangeStateResponse
	stateResp, err := http.Get(server.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(stateResp.Body).Decode(&state); err != nil {
		stateResp.Body.Close()
		t.Fatal(err)
	}
	stateResp.Body.Close()
	if state.RealTime == nil || state.RealTime.Lifecycle != game.LifecycleRunning || state.RealTime.QuoteRevision != 2 || state.RealTime.ActionsThrough < 3 || state.RealTime.EventsThrough != state.EventsThrough || state.RealTime.DurationMillis != 90_000 {
		t.Fatalf("state=%+v", state)
	}
	pauseBody := `{"id":"77777777-7777-4777-8777-777777777777","type":"pause_session"}`
	var paused realTimeCommandResponse
	if status := postRealTimeCommand(t, server, pauseBody, &paused); status != http.StatusOK {
		t.Fatalf("pause status=%d response=%+v", status, paused)
	}
	pausedState := getRealTimeState(t, server)
	if pausedState.RealTime.Lifecycle != game.LifecyclePaused || pausedState.RealTime.QuoteRevision != 2 {
		t.Fatalf("paused state=%+v", pausedState)
	}
	clock.Advance(10 * time.Second)
	frozen := getRealTimeState(t, server)
	if frozen.RealTime.ElapsedMillis != pausedState.RealTime.ElapsedMillis || frozen.RealTime.RemainingMillis != pausedState.RealTime.RemainingMillis {
		t.Fatalf("paused clock advanced: before=%+v after=%+v", pausedState.RealTime, frozen.RealTime)
	}
	resumeBody := `{"id":"88888888-8888-4888-8888-888888888888","type":"resume_session"}`
	var resumed realTimeCommandResponse
	if status := postRealTimeCommand(t, server, resumeBody, &resumed); status != http.StatusOK {
		t.Fatalf("resume status=%d response=%+v", status, resumed)
	}
	if running := getRealTimeState(t, server); running.RealTime.Lifecycle != game.LifecycleRunning || running.RealTime.ElapsedMillis != pausedState.RealTime.ElapsedMillis {
		t.Fatalf("resumed state=%+v", running)
	}

	quitBody := `{"id":"` + testQuitID + `","type":"quit"}`
	var quit realTimeCommandResponse
	if status := postRealTimeCommand(t, server, quitBody, &quit); status != http.StatusOK || quit.Sequence <= updated.Sequence {
		t.Fatalf("quit status=%d response=%+v", status, quit)
	}
	completedResp, err := http.Get(server.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	defer completedResp.Body.Close()
	var completed exchangeStateResponse
	if err := json.NewDecoder(completedResp.Body).Decode(&completed); err != nil {
		t.Fatal(err)
	}
	if completed.RealTime.Lifecycle != game.LifecycleCompleted || !completed.State.IsOver || completed.State.Reason != exchange.PlayerQuit || completed.RealTime.BidOrderID != nil || completed.RealTime.AskOrderID != nil {
		t.Fatalf("completed=%+v", completed)
	}
}

func TestV2RealTimeCountdownPauseResumeActivatesOpeningQuote(t *testing.T) {
	service := newExchangeService(t.TempDir())
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	server := v2ServerForService(service)
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var acknowledgement realTimeCommandResponse
	if status := postRealTimeCommand(t, server, `{"id":"`+testQuoteID+`","type":"start_session","bid":"99.5000","ask":"100.5000"}`, &acknowledgement); status != http.StatusOK {
		t.Fatalf("start status=%d response=%+v", status, acknowledgement)
	}
	if status := postRealTimeCommand(t, server, `{"id":"77777777-7777-4777-8777-777777777777","type":"pause_session"}`, &acknowledgement); status != http.StatusOK {
		t.Fatalf("pause status=%d response=%+v", status, acknowledgement)
	}
	paused := getRealTimeState(t, server)
	if paused.RealTime.Lifecycle != game.LifecyclePaused || paused.RealTime.QuoteRevision != 0 || paused.State.BestBid != 0 || paused.State.BestAsk != 0 {
		t.Fatalf("paused=%+v", paused)
	}
	if status := postRealTimeCommand(t, server, `{"id":"88888888-8888-4888-8888-888888888888","type":"resume_session"}`, &acknowledgement); status != http.StatusOK {
		t.Fatalf("resume status=%d response=%+v", status, acknowledgement)
	}
	running := getRealTimeState(t, server)
	if running.RealTime.Lifecycle != game.LifecycleRunning || running.RealTime.QuoteRevision != 1 || running.State.BestBid != fixed.Price(995_000) || running.State.BestAsk != fixed.Price(1_005_000) || running.RealTime.BidOrderID == nil || running.RealTime.AskOrderID == nil {
		t.Fatalf("running=%+v", running)
	}
}

func TestV2RealTimeStreamReconnectCancelsDisconnectGrace(t *testing.T) {
	service := newExchangeService(t.TempDir())
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	server := v2ServerForService(service)
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var acknowledgement realTimeCommandResponse
	postRealTimeCommand(t, server, `{"id":"`+testQuoteID+`","type":"start_session","bid":"99.5000","ask":"100.5000"}`, &acknowledgement)
	clock.Advance(3 * time.Second)
	entry, err := service.load(testGameID)
	if err != nil {
		t.Fatal(err)
	}
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecycleRunning })

	first, err := entry.connectController()
	if err != nil {
		t.Fatal(err)
	}
	entry.disconnectController(first)
	second, err := entry.connectController()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { second.close(); entry.disconnectController(second) }()
	time.Sleep(10 * time.Millisecond)
	entry.mu.Lock()
	if entry.controller.graceTimer != nil || entry.realTime.Lifecycle != game.LifecycleRunning {
		entry.mu.Unlock()
		t.Fatalf("reconnect did not cancel grace: %+v", entry.controller)
	}
	entry.mu.Unlock()
}

func TestV2RealTimeDisconnectGraceAutoPausesDurably(t *testing.T) {
	service := newExchangeService(t.TempDir())
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	server := v2ServerForService(service)
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var acknowledgement realTimeCommandResponse
	postRealTimeCommand(t, server, `{"id":"`+testQuoteID+`","type":"start_session","bid":"99.5000","ask":"100.5000"}`, &acknowledgement)
	clock.Advance(3 * time.Second)
	entry, err := service.load(testGameID)
	if err != nil {
		t.Fatal(err)
	}
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecycleRunning })
	controller, err := entry.connectController()
	if err != nil {
		t.Fatal(err)
	}
	entry.disconnectController(controller)
	time.Sleep(disconnectGracePeriod + 100*time.Millisecond)
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecyclePaused })
	entry.mu.Lock()
	if entry.realTime.PauseReason != realtime.PauseReasonDisconnect {
		entry.mu.Unlock()
		t.Fatalf("pause reason=%q", entry.realTime.PauseReason)
	}
	found := false
	for _, record := range entry.commands {
		if record.Action != nil && record.Action.Kind == realtime.ActionPauseSession {
			decoded, decodeErr := record.Action.Decode()
			if decodeErr == nil {
				if payload, ok := decoded.Payload.(realtime.PauseSessionPayload); ok && payload.Reason == realtime.PauseReasonDisconnect {
					found = true
				}
			}
		}
	}
	entry.mu.Unlock()
	if !found {
		t.Fatal("disconnect pause was not durable")
	}
}

func TestRealTimeControllerPublishesCommittedStateAndReplacesStaleStream(t *testing.T) {
	entry := &exchangeEntry{mode: game.PlayModeRealTime, realTime: &realTimeState{Lifecycle: game.LifecycleRunning}, metrics: newMetrics()}
	first, err := entry.connectController()
	if err != nil {
		t.Fatal(err)
	}
	second, err := entry.connectController()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { second.close(); entry.disconnectController(second) }()
	select {
	case <-first.done:
	case <-time.After(time.Second):
		t.Fatal("old controller was not closed")
	}
	entry.publishStream(streamMessage{Sequence: 7, Event: "state", Data: map[string]string{"status": "running"}})
	select {
	case message := <-second.ch:
		if message.Sequence != 7 || message.Event != "state" {
			t.Fatalf("message=%+v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("committed state was not published")
	}
}

func TestParseStreamCursorRequiresCanonicalUnsignedInteger(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  uint64
		valid bool
	}{
		{name: "missing", valid: true},
		{name: "zero", value: "0", valid: true},
		{name: "positive", value: "42", want: 42, valid: true},
		{name: "negative", value: "-1"},
		{name: "leading zero", value: "042"},
		{name: "fraction", value: "4.2"},
		{name: "space", value: " 4"},
		{name: "overflow", value: "18446744073709551616"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseStreamCursor(test.value)
			if test.valid {
				if err != nil || got != test.want {
					t.Fatalf("cursor=%d err=%v", got, err)
				}
			} else if err == nil {
				t.Fatalf("cursor=%d was accepted", got)
			}
		})
	}
}

func TestRealTimeStreamHandoffBackfillsCommittedActionBoundaries(t *testing.T) {
	service := newExchangeService(t.TempDir())
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	server := v2ServerForService(service)
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var acknowledgement realTimeCommandResponse
	postRealTimeCommand(t, server, `{"id":"`+testQuoteID+`","type":"start_session","bid":"99.5000","ask":"100.5000"}`, &acknowledgement)
	clock.Advance(3 * time.Second)
	entry, err := service.load(testGameID)
	if err != nil {
		t.Fatal(err)
	}
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecycleRunning })
	entry.mu.Lock()
	current := entry.replay.DurableRecords
	entry.mu.Unlock()
	if current < 2 {
		t.Fatalf("durable actions=%d", current)
	}
	subscriber, snapshot, backfill, err := entry.streamHandoff(testGameID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { subscriber.close(); entry.disconnectController(subscriber) }()
	if snapshot.Sequence != 1 || len(backfill) != int(current-1) {
		t.Fatalf("snapshot=%+v backfill=%d current=%d", snapshot, len(backfill), current)
	}
	for index, message := range backfill {
		want := uint64(index) + 2
		if message.Sequence != want || message.Event != "state" {
			t.Fatalf("index=%d message=%+v", index, message)
		}
	}
	if _, _, _, err := entry.streamHandoff(testGameID, current+1); err == nil {
		t.Fatal("future cursor was accepted")
	}
}

func TestRealTimeStreamRejectsInvalidAndFutureCursors(t *testing.T) {
	service := newExchangeService(t.TempDir())
	server := v2ServerForService(service)
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, value := range []string{"042", "-1", "1.5", " 1"} {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v2/games/"+testGameID+"/stream", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Last-Event-ID", value)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var apiErr apiError
		if err := json.NewDecoder(response.Body).Decode(&apiErr); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || apiErr.Error.Code != "invalid_cursor" {
			t.Fatalf("cursor=%q status=%d error=%+v", value, response.StatusCode, apiErr)
		}
	}
	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v2/games/"+testGameID+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("future cursor status=%d", response.StatusCode)
	}
}

func getRealTimeState(t *testing.T, server *httptest.Server) exchangeStateResponse {
	t.Helper()
	resp, err := http.Get(server.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state exchangeStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state status=%d response=%+v", resp.StatusCode, state)
	}
	return state
}

func TestV2RealTimeStateRejectsFencedStorage(t *testing.T) {
	service := newExchangeService(t.TempDir())
	server := v2ServerForService(service)
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	entry, err := service.load(testGameID)
	if err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	entry.storageFailed = true
	entry.mu.Unlock()
	resp, err = http.Get(server.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var apiErr apiError
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError || apiErr.Error.Code != "storage_failure" {
		t.Fatalf("status=%d error=%+v", resp.StatusCode, apiErr)
	}
}

func TestV2RealTimeQuoteRevisionSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	service := newExchangeService(root)
	service.newSeed = func() (uint64, error) { return 99, nil }
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	server := v2ServerForService(service)
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var acknowledged realTimeCommandResponse
	postRealTimeCommand(t, server, `{"id":"`+testQuoteID+`","type":"start_session","bid":"99.5000","ask":"100.5000"}`, &acknowledged)
	clock.Advance(3 * time.Second)
	entry, err := service.load(testGameID)
	if err != nil {
		t.Fatal(err)
	}
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecycleRunning })
	postRealTimeCommand(t, server, `{"id":"55555555-5555-4555-8555-555555555555","type":"update_quote","expected_quote_revision":1,"bid":"99.6000","ask":"100.4000"}`, &acknowledged)
	postRealTimeCommand(t, server, `{"id":"77777777-7777-4777-8777-777777777777","type":"pause_session"}`, &acknowledged)
	closeRealTimeSequencer(t, entry)
	server.Close()

	reloaded := newExchangeService(root)
	restartClock := newServerManualClock()
	restartCalls := 0
	installManualSequencer(reloaded, restartClock, &restartCalls, nil)
	restartedServer := v2ServerForService(reloaded)
	defer restartedServer.Close()
	state := getRealTimeState(t, restartedServer)
	if state.RealTime.QuoteRevision != 2 || state.RealTime.Lifecycle != game.LifecyclePaused {
		t.Fatalf("restarted state=%+v", state)
	}
	postRealTimeCommand(t, restartedServer, `{"id":"88888888-8888-4888-8888-888888888888","type":"resume_session"}`, &acknowledged)
	postRealTimeCommand(t, restartedServer, `{"id":"99999999-9999-4999-8999-999999999999","type":"update_quote","expected_quote_revision":2,"bid":"99.7000","ask":"100.3000"}`, &acknowledged)
	if state = getRealTimeState(t, restartedServer); state.RealTime.QuoteRevision != 3 || state.State.BestBid != fixed.Price(997_000) {
		t.Fatalf("updated state=%+v", state)
	}
	staleBody := `{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaab","type":"update_quote","expected_quote_revision":2,"bid":"99.8000","ask":"100.2000"}`
	var stale apiError
	if status := postRealTimeCommand(t, restartedServer, staleBody, &stale); status != http.StatusConflict || stale.Error.Code != "quote_revision_conflict" {
		t.Fatalf("stale status=%d response=%+v", status, stale)
	}
	var replayed apiError
	if status := postRealTimeCommand(t, restartedServer, staleBody, &replayed); status != http.StatusConflict || replayed.Error.Code != "quote_revision_conflict" {
		t.Fatalf("replayed stale status=%d response=%+v", status, replayed)
	}
}

func TestV2RealTimeCommandValidationAndModeGating(t *testing.T) {
	server := v2Server(t.TempDir())
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, test := range []struct {
		body string
		code string
	}{
		{`{"id":"` + testQuoteID + `","type":"update_quote","bid":"99.5000","ask":"100.5000"}`, "invalid_request"},
		{`{"id":"` + testQuoteID + `","type":"pause_session","bid":"99.5000"}`, "invalid_request"},
		{`{"id":"` + testQuoteID + `","type":"pause_session","expected_quote_revision":null}`, "invalid_request"},
		{`{"id":"` + testQuoteID + `","type":"update_quote","expected_quote_revision":null,"bid":"99.5000","ask":"100.5000"}`, "invalid_request"},
		{`{"id":"` + testQuoteID + `","type":null}`, "invalid_request"},
		{`{"id":"` + testQuoteID + `"}`, "invalid_request"},
		{`{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99.5000","ask":"100.5000"}`, "command_unavailable_for_mode"},
	} {
		var apiErr apiError
		status := postRealTimeCommand(t, server, test.body, &apiErr)
		if status < 400 || apiErr.Error.Code != test.code {
			t.Fatalf("body=%s status=%d error=%+v", test.body, status, apiErr)
		}
	}
}

func TestV2RealTimeCreateIDConflictDoesNotConsumeSequence(t *testing.T) {
	service := newExchangeService(t.TempDir())
	server := v2ServerForService(service)
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	conflictBody := `{"id":"` + testCreateID + `","type":"start_session","bid":"99.5000","ask":"100.5000"}`
	var conflict apiError
	if status := postRealTimeCommand(t, server, conflictBody, &conflict); status != http.StatusConflict || conflict.Error.Code != "idempotency_key_reused" {
		t.Fatalf("conflict status=%d response=%+v", status, conflict)
	}
	validBody := `{"id":"` + testQuoteID + `","type":"start_session","bid":"99.5000","ask":"100.5000"}`
	var accepted realTimeCommandResponse
	if status := postRealTimeCommand(t, server, validBody, &accepted); status != http.StatusOK || accepted.Sequence != 1 {
		t.Fatalf("accepted status=%d response=%+v", status, accepted)
	}
	var replayed realTimeCommandResponse
	if status := postRealTimeCommand(t, server, validBody, &replayed); status != http.StatusOK || !replayed.Command.Replayed || replayed.Sequence != 1 {
		t.Fatalf("replayed status=%d response=%+v", status, replayed)
	}
}

func TestV2RealTimeConcurrentQuoteRevisionAndReads(t *testing.T) {
	service := newExchangeService(t.TempDir())
	service.newSeed = func() (uint64, error) { return 99, nil }
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	server := v2ServerForService(service)
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var started realTimeCommandResponse
	postRealTimeCommand(t, server, `{"id":"`+testQuoteID+`","type":"start_session","bid":"99.5000","ask":"100.5000"}`, &started)
	clock.Advance(3 * time.Second)
	entry, err := service.load(testGameID)
	if err != nil {
		t.Fatal(err)
	}
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecycleRunning })

	type commandResult struct {
		status int
		body   []byte
	}
	commandBodies := []string{
		`{"id":"55555555-5555-4555-8555-555555555555","type":"update_quote","expected_quote_revision":1,"bid":"99.6000","ask":"100.4000"}`,
		`{"id":"66666666-6666-4666-8666-666666666666","type":"update_quote","expected_quote_revision":1,"bid":"99.7000","ask":"100.3000"}`,
	}
	results := make(chan commandResult, len(commandBodies))
	for _, body := range commandBodies {
		go func() {
			response, requestErr := http.Post(server.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", strings.NewReader(body))
			if requestErr != nil {
				results <- commandResult{status: 0, body: []byte(requestErr.Error())}
				return
			}
			defer response.Body.Close()
			data, _ := io.ReadAll(response.Body)
			results <- commandResult{status: response.StatusCode, body: data}
		}()
	}
	statuses := map[int]int{}
	for range commandBodies {
		result := <-results
		statuses[result.status]++
		if result.status != http.StatusOK && result.status != http.StatusConflict {
			t.Fatalf("unexpected command result: status=%d body=%s", result.status, result.body)
		}
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("statuses=%v", statuses)
	}
	state := getRealTimeState(t, server)
	if state.RealTime.QuoteRevision != 2 || state.RealTime.ActionsThrough != 4 {
		t.Fatalf("state=%+v", state)
	}
	validQuote := state.State.BestBid == fixed.Price(996_000) && state.State.BestAsk == fixed.Price(1_004_000) || state.State.BestBid == fixed.Price(997_000) && state.State.BestAsk == fixed.Price(1_003_000)
	if !validQuote {
		t.Fatalf("mixed quote state=%+v", state.State)
	}
}

func TestV2RealTimeConcurrentDuplicateCommandExecutesOnce(t *testing.T) {
	service := newExchangeService(t.TempDir())
	server := v2ServerForService(service)
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	startBody := `{"id":"` + testQuoteID + `","type":"start_session","bid":"99.5000","ask":"100.5000"}`
	const workers = 12
	type result struct {
		status int
		body   realTimeCommandResponse
	}
	results := make(chan result, workers)
	for range workers {
		go func() {
			response, requestErr := http.Post(server.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", strings.NewReader(startBody))
			if requestErr != nil {
				results <- result{}
				return
			}
			defer response.Body.Close()
			var body realTimeCommandResponse
			_ = json.NewDecoder(response.Body).Decode(&body)
			results <- result{status: response.StatusCode, body: body}
		}()
	}
	nonReplayed := 0
	for range workers {
		result := <-results
		if result.status != http.StatusOK || result.body.Sequence != 1 {
			t.Fatalf("result=%+v", result)
		}
		if !result.body.Command.Replayed {
			nonReplayed++
		}
	}
	if nonReplayed != 1 {
		t.Fatalf("non-replayed responses=%d", nonReplayed)
	}
	_, records, err := eventlog.Open(service.root, testGameID)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
}

func TestV2RealTimeAcknowledgesCommitBeforeTimerFailure(t *testing.T) {
	service := newExchangeService(t.TempDir())
	service.newSequencer = func(config realtime.Config) (*realtime.Sequencer, error) {
		config.Clock = nilTimerClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
		return realtime.NewConfigured(config)
	}
	server := v2ServerForService(service)
	defer server.Close()
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var acknowledged realTimeCommandResponse
	startBody := `{"id":"` + testQuoteID + `","type":"start_session","bid":"99.5000","ask":"100.5000"}`
	if status := postRealTimeCommand(t, server, startBody, &acknowledged); status != http.StatusOK || acknowledged.Sequence != 1 {
		t.Fatalf("acknowledgement status=%d response=%+v", status, acknowledged)
	}
	_, records, err := eventlog.Open(service.root, testGameID)
	if err != nil || len(records) != 1 || records[0].Action.ID != testQuoteID {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	resp, err = http.Get(server.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed runtime state status=%d", resp.StatusCode)
	}
	if service.storageHealthy() {
		t.Fatal("failed runtime remained ready")
	}
}

func TestExchangeServiceShutdownDurablyPausesRunningGame(t *testing.T) {
	root := t.TempDir()
	service := newExchangeService(root)
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	entry, _, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.startRealTime(t.Context(), "start", fixed.Price(995_000), fixed.Price(1_005_000)); err != nil {
		t.Fatal(err)
	}
	clock.Advance(3 * time.Second)
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecycleRunning })
	clock.Advance(time.Second)
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatalf("idempotent shutdown: %v", err)
	}
	_, records, err := eventlog.Open(root, testGameID)
	if err != nil {
		t.Fatal(err)
	}
	last := records[len(records)-1]
	action, err := last.Action.Decode()
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := action.Payload.(realtime.PauseSessionPayload)
	if !ok || action.Source != realtime.SourceSystem || payload.Reason != realtime.PauseReasonShutdown || last.Lifecycle != game.LifecyclePaused || last.ElapsedNanoseconds != int64(time.Second) {
		t.Fatalf("shutdown record=%+v action=%+v", last, action)
	}
}

func TestExchangeServiceShutdownRetriesAfterCancelledContext(t *testing.T) {
	service := newExchangeService(t.TempDir())
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	entry, _, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.ensureRealTimeSequencer(); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Shutdown(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled shutdown error=%v", err)
	}
	if service.shutdownDone {
		t.Fatal("failed shutdown was latched as complete")
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatalf("retried shutdown: %v", err)
	}
}

func v2Server(root string) *httptest.Server {
	svc := newExchangeService(root)
	return v2ServerForService(svc)
}

func v2ServerForService(svc *exchangeService) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/scenarios", svc.handleScenarios)
	mux.HandleFunc("/api/v2/games", svc.handleGames)
	mux.HandleFunc("/api/v2/games/", svc.handleGame)
	return httptest.NewServer(mux)
}

func TestCommandMetricsIncludeDurableAppend(t *testing.T) {
	svc := newExchangeService(t.TempDir())
	svc.metrics = newMetrics()
	create := httptest.NewRequest(http.MethodPost, "/api/v2/games", strings.NewReader(`{"game_id":"`+testGameID+`","command_id":"`+testCreateID+`","scenario_id":"first-spread-v1"}`))
	create.Header.Set("Content-Type", "application/json")
	svc.handleGames(httptest.NewRecorder(), create)
	entry := svc.entries[testGameID]
	if entry == nil {
		t.Fatal("game was not created")
	}
	quote := httptest.NewRequest(http.MethodPost, "/api/v2/games/"+testGameID+"/commands", strings.NewReader(`{"id":"`+testQuoteID+`","type":"submit_quote","expected_version":0,"bid":"99.5000","ask":"100.5000"}`))
	quote.Header.Set("Content-Type", "application/json")
	svc.handleExchangeCommand(httptest.NewRecorder(), quote, testGameID, entry)
	output := string(svc.metrics.prometheus())
	for _, expected := range []string{
		`mmg_game_commands_total{command="create_game",outcome="accepted"} 1`,
		`mmg_game_commands_total{command="submit_quote",outcome="accepted"} 1`,
		`mmg_event_log_append_duration_seconds_count{outcome="success"} 1`,
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("metrics output missing %q:\n%s", expected, output)
		}
	}
}

func TestRecoveryFailureMetric(t *testing.T) {
	svc := newExchangeService(t.TempDir())
	svc.metrics = newMetrics()
	create := httptest.NewRequest(http.MethodPost, "/api/v2/games", strings.NewReader(`{"game_id":"`+testGameID+`","command_id":"`+testCreateID+`","scenario_id":"first-spread-v1"}`))
	create.Header.Set("Content-Type", "application/json")
	svc.handleGames(httptest.NewRecorder(), create)
	entry := svc.entries[testGameID]
	entry.syncLog = func(*eventlog.Log) error { return errors.New("sync failed") }
	if err := entry.recover(); err == nil {
		t.Fatal("recover succeeded")
	}
	if !strings.Contains(string(svc.metrics.prometheus()), "mmg_game_recovery_failures_total 1") {
		t.Fatalf("recovery failure missing from metrics:\n%s", svc.metrics.prometheus())
	}
}

func TestV2ListsServerOwnedScenarios(t *testing.T) {
	ts := v2Server(t.TempDir())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v2/scenarios")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body struct {
		Scenarios []struct {
			ID         string                   `json:"id"`
			Turns      int                      `json:"turns"`
			Tutorial   []scenario.TutorialStep  `json:"tutorial"`
			Reflection string                   `json:"reflection"`
			Modes      []game.PlayMode          `json:"modes"`
			RealTime   *scenario.RealTimeConfig `json:"real_time"`
		} `json:"scenarios"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Scenarios) != 4 || body.Scenarios[0].ID == "" || body.Scenarios[0].Turns == 0 || len(body.Scenarios[0].Tutorial) != 4 || body.Scenarios[0].Reflection == "" || body.Scenarios[1].ID != "inventory-pressure-v1" || len(body.Scenarios[1].Tutorial) != 5 || body.Scenarios[1].Reflection == "" || body.Scenarios[2].ID != "volatility-shock-v1" || len(body.Scenarios[2].Tutorial) != 5 || body.Scenarios[2].Reflection == "" || body.Scenarios[3].ID != "volatility-shock-v2" || body.Scenarios[3].Turns != 8 || len(body.Scenarios[3].Tutorial) != 5 || body.Scenarios[3].Reflection == "" {
		t.Fatalf("scenarios=%+v", body.Scenarios)
	}
	if !reflect.DeepEqual(body.Scenarios[0].Modes, []game.PlayMode{game.PlayModeTurnBased, game.PlayModeRealTime}) || body.Scenarios[0].RealTime == nil || body.Scenarios[0].RealTime.DurationMilliseconds != 90_000 {
		t.Fatalf("first real-time scenario=%+v", body.Scenarios[0])
	}
	for _, item := range body.Scenarios[1:] {
		if !reflect.DeepEqual(item.Modes, []game.PlayMode{game.PlayModeTurnBased}) || item.RealTime != nil {
			t.Fatalf("unexpected real-time scenario=%+v", item)
		}
	}
}

func TestV2CreatesAndRecoversRealTimePreparingGame(t *testing.T) {
	root := t.TempDir()
	server := v2Server(root)
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	var created exchangeCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.Mode != game.PlayModeRealTime || created.RealTime == nil || created.RealTime.Lifecycle != game.LifecyclePreparing || created.State.Version != 0 || created.State.Turn != 0 {
		t.Fatalf("created status=%d response=%+v", resp.StatusCode, created)
	}
	log, records, err := eventlog.Open(root, testGameID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 || log.Meta().EffectiveMode() != game.PlayModeRealTime || log.Meta().RealTime.Seed == 0 {
		t.Fatalf("persisted records=%+v meta=%+v", records, log.Meta())
	}
	server.Close()

	reloaded := v2Server(root)
	defer reloaded.Close()
	resp, err = http.Get(reloaded.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	var state exchangeStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || state.Mode != game.PlayModeRealTime || state.RealTime == nil || state.RealTime.Lifecycle != game.LifecyclePreparing || state.EventsThrough != 0 {
		t.Fatalf("recovered status=%d state=%+v", resp.StatusCode, state)
	}

	req, err := http.NewRequest(http.MethodPost, reloaded.URL+"/api/v2/games/"+testGameID+"/commands", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var rejected apiError
	if err := json.NewDecoder(resp.Body).Decode(&rejected); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || rejected.Error.Code != "invalid_request" {
		t.Fatalf("command status=%d error=%+v", resp.StatusCode, rejected)
	}
	_, records, err = eventlog.Open(root, testGameID)
	if err != nil || len(records) != 0 {
		t.Fatalf("rejected command mutated log: records=%+v err=%v", records, err)
	}
}

func TestRealTimeSequencerIsLazyAndOrdersCountdown(t *testing.T) {
	service := newExchangeService(t.TempDir())
	service.newSeed = func() (uint64, error) { return 99, nil }
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	entry, created, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil || !created || calls != 0 || entry.sequencer != nil {
		t.Fatalf("create created=%v calls=%d sequencer=%v err=%v", created, calls, entry.sequencer, err)
	}
	defer closeRealTimeSequencer(t, entry)

	start, err := entry.startRealTime(t.Context(), "start", fixed.Price(995_000), fixed.Price(1_005_000))
	if err != nil || start.Sequence != 1 || calls != 1 {
		t.Fatalf("start=%+v calls=%d err=%v", start, calls, err)
	}
	if replayed, replayErr := entry.startRealTime(t.Context(), "start", fixed.Price(995_000), fixed.Price(1_005_000)); replayErr != nil || !replayed.Replayed || !executionsEqualIgnoringReplay(replayed, start) || calls != 1 {
		t.Fatalf("replayed start=%+v calls=%d err=%v", replayed, calls, replayErr)
	}
	countdown := time.Duration(entry.scenario.RealTime.CountdownMilliseconds) * time.Millisecond
	clock.Advance(countdown - time.Nanosecond)
	time.Sleep(time.Millisecond)
	entry.mu.Lock()
	recordsBeforeDeadline := len(entry.commands)
	lifecycleBeforeDeadline := entry.realTime.Lifecycle
	entry.mu.Unlock()
	if recordsBeforeDeadline != 1 || lifecycleBeforeDeadline != game.LifecycleCountdown {
		t.Fatalf("before deadline records=%d lifecycle=%s", recordsBeforeDeadline, lifecycleBeforeDeadline)
	}
	clock.Advance(time.Nanosecond)
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecycleRunning })
	entry.mu.Lock()
	startRecord := entry.commands["start"]
	countdownRecord := entry.commands["system/countdown_complete/start"]
	recordCount := len(entry.commands)
	entry.mu.Unlock()
	if recordCount != 2 || startRecord.Version != 1 || startRecord.Lifecycle != game.LifecycleCountdown || countdownRecord.Version != 2 || countdownRecord.ElapsedNanoseconds != 0 || countdownRecord.Lifecycle != game.LifecycleRunning {
		t.Fatalf("records=%d start=%+v countdown=%+v", recordCount, startRecord, countdownRecord)
	}
}

func TestRealTimeSequencerConstructionIsSingleFlight(t *testing.T) {
	service := newExchangeService(t.TempDir())
	service.newSeed = func() (uint64, error) { return 99, nil }
	clock := newServerManualClock()
	var calls int
	installManualSequencer(service, clock, &calls, nil)
	entry, _, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	results := make(chan *realtime.Sequencer, workers)
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			sequencer, err := entry.ensureRealTimeSequencer()
			results <- sequencer
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsCh)
	var first *realtime.Sequencer
	for sequencer := range results {
		if first == nil {
			first = sequencer
		} else if sequencer != first {
			t.Fatal("concurrent construction returned different sequencers")
		}
	}
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("factory calls=%d want 1", calls)
	}
	closeRealTimeSequencer(t, entry)
}

func TestRealTimePauseResumeUsesLogicalTimeAndRunsDueWorkFirst(t *testing.T) {
	service := newExchangeService(t.TempDir())
	service.newSeed = func() (uint64, error) { return 99, nil }
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	entry, _, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRealTimeSequencer(t, entry)
	if _, err := entry.startRealTime(t.Context(), "start", fixed.Price(995_000), fixed.Price(1_005_000)); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Duration(entry.scenario.RealTime.CountdownMilliseconds) * time.Millisecond)
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecycleRunning })
	next, ok, err := entry.generator.Next()
	if err != nil || !ok {
		t.Fatalf("next=%+v ok=%v err=%v", next, ok, err)
	}
	clock.Advance(next.Due)
	pause, err := entry.pauseRealTime(t.Context(), "pause")
	if err != nil || pause.Elapsed != next.Due {
		t.Fatalf("pause=%+v err=%v", pause, err)
	}
	entry.mu.Lock()
	generated := entry.commands[next.Action.ID]
	pauseRecord := entry.commands["pause"]
	recordsWhilePaused := len(entry.commands)
	entry.mu.Unlock()
	if generated.Version == 0 || generated.Version >= pauseRecord.Version || pauseRecord.ElapsedNanoseconds != int64(next.Due) {
		t.Fatalf("generated=%+v pause=%+v", generated, pauseRecord)
	}
	clock.Advance(time.Hour)
	time.Sleep(time.Millisecond)
	entry.mu.Lock()
	if len(entry.commands) != recordsWhilePaused {
		entry.mu.Unlock()
		t.Fatal("paused sequencer executed market activity")
	}
	entry.mu.Unlock()
	resume, err := entry.resumeRealTime(t.Context(), "resume")
	if err != nil || resume.Elapsed != next.Due {
		t.Fatalf("resume=%+v err=%v", resume, err)
	}
	if _, err := entry.systemPauseRealTime(t.Context(), "system/pause_after_resume", realtime.PauseReasonShutdown); err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	finalPause := entry.commands["system/pause_after_resume"]
	entry.mu.Unlock()
	if finalPause.ElapsedNanoseconds != int64(next.Due) {
		t.Fatalf("logical time advanced during pause: %+v", finalPause)
	}
}

func TestRealTimeColdLoadNormalizesActiveLifecycle(t *testing.T) {
	for _, lifecycle := range []game.LifecycleState{game.LifecycleCountdown, game.LifecycleRunning} {
		t.Run(string(lifecycle), func(t *testing.T) {
			root := t.TempDir()
			creator := newExchangeService(root)
			creator.newSeed = func() (uint64, error) { return 99, nil }
			entry, _, err := creator.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
			if err != nil {
				t.Fatal(err)
			}
			start := realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)}}
			if outcome := entry.executeRealTimeAction(start, 0); outcome.Disposition != realtime.DispositionContinue {
				t.Fatalf("start=%+v", outcome)
			}
			wantElapsed := time.Duration(0)
			if lifecycle == game.LifecycleRunning {
				countdown := realtime.Action{ID: "system/countdown_complete/start", Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem, Payload: realtime.CountdownCompletePayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)}}
				if outcome := entry.executeRealTimeAction(countdown, 0); outcome.Disposition != realtime.DispositionContinue {
					t.Fatalf("countdown=%+v", outcome)
				}
				wantElapsed = time.Millisecond
				revision := uint64(1)
				quote := realtime.Action{ID: "quote", Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: fixed.Price(994_000), Ask: fixed.Price(1_006_000), ExpectedRevision: &revision}}
				if outcome := entry.executeRealTimeAction(quote, wantElapsed); outcome.Disposition != realtime.DispositionContinue {
					t.Fatalf("quote=%+v", outcome)
				}
			}
			loader := newExchangeService(root)
			calls := 0
			installManualSequencer(loader, newServerManualClock(), &calls, nil)
			loaded, err := loader.load(testGameID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.realTime.Lifecycle != game.LifecyclePaused || loaded.sequencer != nil || calls != 0 {
				t.Fatalf("loaded lifecycle=%s sequencer=%v calls=%d", loaded.realTime.Lifecycle, loaded.sequencer, calls)
			}
			_, records, err := eventlog.Open(root, testGameID)
			if err != nil {
				t.Fatal(err)
			}
			last := records[len(records)-1]
			if last.Action.Kind != realtime.ActionPauseSession || last.Action.Source != realtime.SourceSystem || last.Lifecycle != game.LifecyclePaused || last.ElapsedNanoseconds != int64(wantElapsed) || last.Action.ID != fmt.Sprintf("system/recovery_pause/%d", len(records)-1) {
				t.Fatalf("recovery record=%+v", last)
			}
		})
	}
}

func TestRealTimeRestartPausedRestoresExactGeneratorCheckpoint(t *testing.T) {
	root := t.TempDir()
	creator := newExchangeService(root)
	creator.newSeed = func() (uint64, error) { return 99, nil }
	clock := newServerManualClock()
	creatorCalls := 0
	installManualSequencer(creator, clock, &creatorCalls, nil)
	entry, _, err := creator.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.startRealTime(t.Context(), "start", fixed.Price(995_000), fixed.Price(1_005_000)); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Duration(entry.scenario.RealTime.CountdownMilliseconds) * time.Millisecond)
	waitForRealTime(t, entry, func(entry *exchangeEntry) bool { return entry.realTime.Lifecycle == game.LifecycleRunning })
	first, ok, err := entry.generator.Next()
	if err != nil || !ok {
		t.Fatalf("first=%+v ok=%v err=%v", first, ok, err)
	}
	clock.Advance(first.Due)
	pauseExecution, err := entry.pauseRealTime(t.Context(), "pause")
	if err != nil {
		t.Fatal(err)
	}
	wantNext, ok, err := entry.generator.Next()
	if err != nil || !ok {
		t.Fatalf("next=%+v ok=%v err=%v", wantNext, ok, err)
	}
	closeRealTimeSequencer(t, entry)

	restarted := newExchangeService(root)
	restartClock := newServerManualClock()
	restartCalls := 0
	var checkpoint realtime.Checkpoint
	var restoredNext realtime.ScheduledAction
	restarted.newSequencer = func(config realtime.Config) (*realtime.Sequencer, error) {
		restartCalls++
		checkpoint = config.Checkpoint
		restoredNext, _, err = config.Schedule.Next()
		if err != nil {
			return nil, err
		}
		config.Clock = restartClock
		return realtime.NewConfigured(config)
	}
	loaded, err := restarted.load(testGameID)
	if err != nil || restartCalls != 0 || loaded.realTime.Lifecycle != game.LifecyclePaused {
		t.Fatalf("load lifecycle=%s calls=%d err=%v", loaded.realTime.Lifecycle, restartCalls, err)
	}
	defer closeRealTimeSequencer(t, loaded)
	replayedPause, err := loaded.pauseRealTime(t.Context(), "pause")
	if err != nil || !replayedPause.Replayed || !executionsEqualIgnoringReplay(replayedPause, pauseExecution) || restartCalls != 1 {
		t.Fatalf("pause replay=%+v calls=%d err=%v", replayedPause, restartCalls, err)
	}
	if !reflect.DeepEqual(restoredNext, wantNext) || checkpoint.Status != realtime.StatusPaused || checkpoint.Elapsed != first.Due || checkpoint.NextScheduled != 1 || checkpoint.Sequence != uint64(len(loaded.commands)) {
		t.Fatalf("next=%+v want=%+v checkpoint=%+v", restoredNext, wantNext, checkpoint)
	}
	resume, err := loaded.resumeRealTime(t.Context(), "resume")
	if err != nil || resume.Elapsed != first.Due {
		t.Fatalf("resume=%+v err=%v", resume, err)
	}
}

func TestRealTimeCompletedReplaySurvivesRestart(t *testing.T) {
	root := t.TempDir()
	service := newExchangeService(root)
	service.newSeed = func() (uint64, error) { return 99, nil }
	clock := newServerManualClock()
	calls := 0
	installManualSequencer(service, clock, &calls, nil)
	entry, _, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil {
		t.Fatal(err)
	}
	start, err := entry.startRealTime(t.Context(), "start", fixed.Price(995_000), fixed.Price(1_005_000))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Duration(entry.scenario.RealTime.CountdownMilliseconds) * time.Millisecond)
	sequencer, err := entry.ensureRealTimeSequencer()
	if err != nil {
		t.Fatal(err)
	}
	view, err := sequencer.View(t.Context(), func() any { return nil })
	if err != nil || view.Snapshot.Status != realtime.StatusRunning {
		t.Fatalf("countdown view=%+v err=%v", view, err)
	}
	clock.Advance(time.Duration(entry.scenario.RealTime.DurationMilliseconds) * time.Millisecond)
	view, err = sequencer.View(t.Context(), func() any { return nil })
	if err != nil || view.Snapshot.Status != realtime.StatusCompleted {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	closeRealTimeSequencer(t, entry)

	restarted := newExchangeService(root)
	restartCalls := 0
	var observed realtime.Config
	installManualSequencer(restarted, newServerManualClock(), &restartCalls, &observed)
	loaded, err := restarted.load(testGameID)
	if err != nil || loaded.realTime.Lifecycle != game.LifecycleCompleted || restartCalls != 0 {
		t.Fatalf("load lifecycle=%s calls=%d err=%v", loaded.realTime.Lifecycle, restartCalls, err)
	}
	defer closeRealTimeSequencer(t, loaded)
	replayed, err := loaded.startRealTime(t.Context(), "start", fixed.Price(995_000), fixed.Price(1_005_000))
	if err != nil || !replayed.Replayed || !executionsEqualIgnoringReplay(replayed, start) || restartCalls != 1 || observed.Checkpoint.Status != realtime.StatusCompleted || observed.Checkpoint.NextScheduled != loaded.replay.GeneratorCommitted {
		t.Fatalf("replay=%+v start=%+v calls=%d checkpoint=%+v summary=%+v err=%v", replayed, start, restartCalls, observed.Checkpoint, loaded.replay, err)
	}
	var expiry eventlog.Record
	for _, record := range loaded.commands {
		if record.Action != nil && record.Action.Kind == realtime.ActionTimeExpired {
			expiry = record
			break
		}
	}
	if expiry.Action == nil {
		t.Fatal("completed game has no durable expiry action")
	}
	action, err := expiry.Action.Decode()
	if err != nil {
		t.Fatal(err)
	}
	replayedExpiry, found := loaded.replayRealTimeAction(action)
	if !found || replayedExpiry.Disposition != realtime.DispositionComplete || replayedExpiry.Sequence != expiry.Version || replayedExpiry.Elapsed != time.Duration(expiry.ElapsedNanoseconds) {
		t.Fatalf("expiry retry=%+v found=%v record=%+v", replayedExpiry, found, expiry)
	}
}

func TestV2ValidatesAndScopesCreateMode(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		scenarioID string
		code       string
	}{
		{name: "null", mode: "null", scenarioID: "first-spread-v1", code: "invalid_mode"},
		{name: "empty", mode: `""`, scenarioID: "first-spread-v1", code: "invalid_mode"},
		{name: "unknown", mode: `"continuous"`, scenarioID: "first-spread-v1", code: "invalid_mode"},
		{name: "non-string", mode: "1", scenarioID: "first-spread-v1", code: "invalid_mode"},
		{name: "unsupported", mode: `"real_time"`, scenarioID: "inventory-pressure-v1", code: "unsupported_mode"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := v2Server(t.TempDir())
			defer server.Close()
			body := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"` + test.scenarioID + `","mode":` + test.mode + `}`
			resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			var response apiError
			if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
				resp.Body.Close()
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || response.Error.Code != test.code {
				t.Fatalf("status=%d response=%+v", resp.StatusCode, response)
			}
		})
	}

	root := t.TempDir()
	server := v2Server(root)
	defer server.Close()
	defaultBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(defaultBody))
	if err != nil {
		t.Fatal(err)
	}
	var created exchangeCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.Mode != game.PlayModeTurnBased || created.RealTime != nil {
		t.Fatalf("default mode response=%+v", created)
	}
	explicitBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"turn_based"}`
	resp, err = http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(explicitBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("explicit turn-based retry status=%d", resp.StatusCode)
	}
	realTimeBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1","mode":"real_time"}`
	resp, err = http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(realTimeBody))
	if err != nil {
		t.Fatal(err)
	}
	var conflict apiError
	if err := json.NewDecoder(resp.Body).Decode(&conflict); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || conflict.Error.Code != "idempotency_key_reused" {
		t.Fatalf("mode conflict status=%d error=%+v", resp.StatusCode, conflict)
	}
}

func TestRealTimeCreateRetryUsesPersistedModeAndScenario(t *testing.T) {
	root := t.TempDir()
	service := newExchangeService(root)
	seedCalls := 0
	service.newSeed = func() (uint64, error) {
		seedCalls++
		return 99, nil
	}
	entry, created, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil || !created || entry.mode != game.PlayModeRealTime || entry.log.Meta().RealTime.Seed != 99 || seedCalls != 1 {
		t.Fatalf("create entry=%+v created=%v err=%v", entry, created, err)
	}
	service.lookupScenario = func(string) (scenario.Definition, bool) { return scenario.Definition{}, false }
	entry, created, err = service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil || created || entry.mode != game.PlayModeRealTime || seedCalls != 1 {
		t.Fatalf("in-memory retry entry=%+v created=%v err=%v", entry, created, err)
	}

	reloaded := newExchangeService(root)
	reloaded.lookupScenario = func(string) (scenario.Definition, bool) { return scenario.Definition{}, false }
	entry, created, err = reloaded.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil || created || entry.mode != game.PlayModeRealTime || entry.realTime == nil || entry.realTime.Lifecycle != game.LifecyclePreparing {
		t.Fatalf("persisted retry entry=%+v created=%v err=%v", entry, created, err)
	}
}

func TestRealTimeLogRejectsTurnBasedCommandRecords(t *testing.T) {
	root := t.TempDir()
	service := newExchangeService(root)
	entry, created, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil || !created {
		t.Fatalf("create created=%v err=%v", created, err)
	}
	if _, err := entry.log.Append(eventlog.Record{Schema: eventlog.SchemaVersion, Version: 1, Command: exchange.Command{ID: testQuoteID, Type: exchange.CommandQuit}}); err == nil {
		t.Fatal("preparing log accepted command record")
	}
	_, records, err := eventlog.Open(root, testGameID)
	if err != nil || len(records) != 0 {
		t.Fatalf("rejected append changed records=%+v err=%v", records, err)
	}
}

func TestRealTimeActionsAndRejectionsReplayExactly(t *testing.T) {
	root := t.TempDir()
	service := newExchangeService(root)
	service.newSeed = func() (uint64, error) { return 99, nil }
	entry, created, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil || !created {
		t.Fatalf("create created=%v err=%v", created, err)
	}
	start := realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)}}
	if outcome := entry.executeRealTimeAction(start, 0); outcome.Disposition != realtime.DispositionContinue {
		t.Fatalf("start=%+v", outcome)
	}
	countdown := realtime.Action{ID: "system/countdown_complete/start", Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem, Payload: realtime.CountdownCompletePayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)}}
	if outcome := entry.executeRealTimeAction(countdown, 0); outcome.Disposition != realtime.DispositionContinue {
		t.Fatalf("countdown=%+v", outcome)
	}
	revision := uint64(1)
	quote := realtime.Action{ID: testQuoteID, Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000), ExpectedRevision: &revision}}
	if outcome := entry.executeRealTimeAction(quote, 0); outcome.Disposition != realtime.DispositionContinue {
		t.Fatalf("quote=%+v", outcome)
	}
	rejectedRevision := uint64(2)
	rejected := realtime.Action{ID: testQuitID, Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: fixed.Price(1_010_000), Ask: fixed.Price(990_000), ExpectedRevision: &rejectedRevision}}
	rejectedOutcome := entry.executeRealTimeAction(rejected, 0)
	if rejectedOutcome.Disposition != realtime.DispositionReject {
		t.Fatalf("rejected outcome=%+v", rejectedOutcome)
	}
	system, ok, err := entry.generator.Next()
	if err != nil || !ok {
		t.Fatalf("system=%+v ok=%v err=%v", system, ok, err)
	}
	systemOutcome := entry.executeRealTimeAction(system.Action, system.Due)
	if systemOutcome.Disposition == realtime.DispositionFail || systemOutcome.Disposition == realtime.DispositionReject {
		t.Fatalf("system outcome=%+v", systemOutcome)
	}
	if err := entry.generator.Commit(system); err != nil {
		t.Fatal(err)
	}
	wantNext, _, err := entry.generator.Next()
	if err != nil {
		t.Fatal(err)
	}

	log, records, err := eventlog.Open(root, testGameID)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := rebuildExchangeEntry(log, records)
	if err != nil {
		t.Fatal(err)
	}
	gotNext, _, err := rebuilt.generator.Next()
	if err != nil || !reflect.DeepEqual(rebuilt.engine.State(), entry.engine.State()) || !reflect.DeepEqual(gotNext, wantNext) || len(rebuilt.commands) != 5 || rebuilt.replay.Lifecycle != game.LifecycleRunning || rebuilt.replay.GeneratorCommitted != 1 {
		t.Fatalf("rebuilt state=%+v next=%+v commands=%d err=%v", rebuilt.engine.State(), gotNext, len(rebuilt.commands), err)
	}
}

func TestRealTimeDurableExecutorRemembersAcceptedAndRejectedActions(t *testing.T) {
	root := t.TempDir()
	service := newExchangeService(root)
	service.newSeed = func() (uint64, error) { return 99, nil }
	entry, _, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil {
		t.Fatal(err)
	}
	initialState := entry.engine.State()
	for _, id := range []string{testCreateID, "system/customer_arrival/1"} {
		revision := uint64(0)
		reserved := realtime.Action{ID: id, Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000), ExpectedRevision: &revision}}
		outcome := entry.executeRealTimeAction(reserved, 0)
		if outcome.Disposition != realtime.DispositionReject || outcome.Err == nil || !reflect.DeepEqual(entry.engine.State(), initialState) {
			t.Fatalf("reserved action id %q outcome=%+v state=%+v", id, outcome, entry.engine.State())
		}
	}
	start := realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)}}
	if outcome := entry.executeRealTimeAction(start, 0); outcome.Disposition != realtime.DispositionContinue {
		t.Fatalf("start=%+v", outcome)
	}
	countdown := realtime.Action{ID: "system/countdown_complete/start", Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem, Payload: realtime.CountdownCompletePayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)}}
	if outcome := entry.executeRealTimeAction(countdown, 0); outcome.Disposition != realtime.DispositionContinue {
		t.Fatalf("countdown=%+v", outcome)
	}
	revision := uint64(1)
	quote := realtime.Action{ID: testQuoteID, Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000), ExpectedRevision: &revision}}
	accepted := entry.executeRealTimeAction(quote, 0)
	if accepted.Disposition != realtime.DispositionContinue || accepted.Err != nil {
		t.Fatalf("accepted=%+v", accepted)
	}
	rejectedRevision := uint64(2)
	rejectedAction := realtime.Action{ID: testQuitID, Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: fixed.Price(1_010_000), Ask: fixed.Price(990_000), ExpectedRevision: &rejectedRevision}}
	rejected := entry.executeRealTimeAction(rejectedAction, time.Millisecond)
	if rejected.Disposition != realtime.DispositionReject || rejected.Err == nil {
		t.Fatalf("rejected=%+v", rejected)
	}
	if replayed := entry.executeRealTimeAction(rejectedAction, 2*time.Millisecond); replayed.Disposition != realtime.DispositionReject || replayed.Err == nil || replayed.Err.Error() != rejected.Err.Error() {
		t.Fatalf("replayed rejection=%+v", replayed)
	}
	conflict := rejectedAction
	conflict.Payload = realtime.UpdateQuotePayload{Bid: fixed.Price(990_000), Ask: fixed.Price(1_010_000), ExpectedRevision: &rejectedRevision}
	if outcome := entry.executeRealTimeAction(conflict, 2*time.Millisecond); outcome.Disposition != realtime.DispositionReject || !strings.Contains(outcome.Err.Error(), "different payload") {
		t.Fatalf("conflict=%+v", outcome)
	}
	_, records, err := eventlog.Open(root, testGameID)
	if err != nil || len(records) != 4 || records[3].Rejection == nil {
		t.Fatalf("records=%+v err=%v", records, err)
	}

	reloaded := newExchangeService(root)
	rebuilt, err := reloaded.load(testGameID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome := rebuilt.executeRealTimeAction(quote, time.Second); outcome.Disposition != realtime.DispositionContinue || !durableResultsEqual(outcome.Result.(exchange.Result), accepted.Result.(exchange.Result)) {
		t.Fatalf("replayed acceptance=%+v", outcome)
	}
	if outcome := rebuilt.executeRealTimeAction(rejectedAction, time.Second); outcome.Disposition != realtime.DispositionReject || outcome.Err.Error() != rejected.Err.Error() {
		t.Fatalf("replayed rejection after restart=%+v", outcome)
	}
}

func TestRealTimeRecoveryRetainsSystemActionAndTruncatesPartialSuffix(t *testing.T) {
	root := t.TempDir()
	service := newExchangeService(root)
	service.newSeed = func() (uint64, error) { return 99, nil }
	entry, _, err := service.createOrLoadMode(testGameID, testCreateID, "first-spread-v1", game.PlayModeRealTime)
	if err != nil {
		t.Fatal(err)
	}
	start := realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)}}
	if outcome := entry.executeRealTimeAction(start, 0); outcome.Disposition != realtime.DispositionContinue {
		t.Fatalf("start=%+v", outcome)
	}
	countdown := realtime.Action{ID: "system/countdown_complete/start", Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem, Payload: realtime.CountdownCompletePayload{Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)}}
	if outcome := entry.executeRealTimeAction(countdown, 0); outcome.Disposition != realtime.DispositionContinue {
		t.Fatalf("countdown=%+v", outcome)
	}
	scheduled, ok, err := entry.generator.Next()
	if err != nil || !ok {
		t.Fatalf("next scheduled action=%+v ok=%v err=%v", scheduled, ok, err)
	}
	outcome := entry.executeRealTimeAction(scheduled.Action, scheduled.Due)
	if outcome.Disposition == realtime.DispositionFail || outcome.Disposition == realtime.DispositionReject {
		t.Fatalf("system outcome=%+v", outcome)
	}
	if err := entry.generator.Commit(scheduled); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(entry.log.Path(), "events.jsonl")
	acknowledged, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"schema":5`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := entry.recover(); err != nil {
		t.Fatal(err)
	}
	next, ok, err := entry.generator.Next()
	if err != nil || !ok || next.Action.ID == scheduled.Action.ID || entry.generator.Snapshot().Committed != 1 || len(entry.commands) != 3 {
		t.Fatalf("recovered next=%+v ok=%v snapshot=%+v commands=%d err=%v", next, ok, entry.generator.Snapshot(), len(entry.commands), err)
	}
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, acknowledged) {
		t.Fatal("partial suffix was not truncated exactly")
	}
}

func TestV2EndpointSemantics(t *testing.T) {
	ts := v2Server(t.TempDir())
	defer ts.Close()

	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1"}`
	resp, err := http.Post(ts.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/api/v2/games/"+testGameID || resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		resp.Body.Close()
		t.Fatalf("create status=%d location=%q content-type=%q", resp.StatusCode, resp.Header.Get("Location"), resp.Header.Get("Content-Type"))
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, field := range []string{"summary", "events", "command"} {
		if _, exists := state[field]; exists {
			t.Fatalf("state response contains command-only field %q", field)
		}
	}
	if _, exists := state["latest_turn"]; exists {
		t.Fatal("state response contains latest_turn before the first turn")
	}
	var initialState exchange.State
	var startingEquity fixed.Money
	var eventsThrough uint64
	if err := json.Unmarshal(state["state"], &initialState); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(state["starting_equity"], &startingEquity); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(state["events_through"], &eventsThrough); err != nil {
		t.Fatal(err)
	}
	if startingEquity != initialState.Equity || eventsThrough != 0 || initialState.Turn != 0 {
		t.Fatalf("turn-zero snapshot state=%+v starting_equity=%s events_through=%d", initialState, startingEquity, eventsThrough)
	}

	methodTests := []struct {
		path   string
		method string
		allow  string
	}{
		{"/api/v2/scenarios", http.MethodPost, http.MethodGet},
		{"/api/v2/games", http.MethodGet, http.MethodPost},
		{"/api/v2/games/" + testGameID, http.MethodPost, http.MethodGet},
		{"/api/v2/games/" + testGameID + "/commands", http.MethodGet, http.MethodPost},
		{"/api/v2/games/" + testGameID + "/events", http.MethodPost, http.MethodGet},
	}
	for _, test := range methodTests {
		t.Run(test.path, func(t *testing.T) {
			req, err := http.NewRequest(test.method, ts.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != test.allow {
				t.Fatalf("status=%d allow=%q, want status=%d allow=%q", resp.StatusCode, resp.Header.Get("Allow"), http.StatusMethodNotAllowed, test.allow)
			}
		})
	}

	for _, path := range []string{
		"/api/v2/games/" + testGameID + "/unknown",
		"/api/v2/games/" + testGameID + "/events/extra",
	} {
		resp, err := http.Post(ts.URL+path, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown route %q status=%d, want %d", path, resp.StatusCode, http.StatusNotFound)
		}
	}
}

func TestV2EventsRequiresCanonicalAfterCursor(t *testing.T) {
	ts := v2Server(t.TempDir())
	defer ts.Close()
	createV2Game(t, ts.URL)

	for _, value := range []string{"0", "18446744073709551615"} {
		resp, err := http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?after=" + url.QueryEscape(value))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("after=%q status=%d, want %d", value, resp.StatusCode, http.StatusOK)
		}
	}
	for _, value := range []string{"", "00", "01", "+1", "-1", "1.0", "1x", "18446744073709551616"} {
		resp, err := http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?after=" + url.QueryEscape(value))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("after=%q status=%d, want %d", value, resp.StatusCode, http.StatusBadRequest)
		}
	}
	resp, err := http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?after=1&after=2")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("repeated after status=%d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestV2EventsRequiresCanonicalLimit(t *testing.T) {
	ts := v2Server(t.TempDir())
	defer ts.Close()
	createV2Game(t, ts.URL)

	for _, value := range []string{"1", "200"} {
		resp, err := http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?limit=" + url.QueryEscape(value))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("limit=%q status=%d, want %d", value, resp.StatusCode, http.StatusOK)
		}
	}
	for _, value := range []string{"", "0", "00", "01", "+1", "-1", "1.0", "1x", "201", "18446744073709551616"} {
		resp, err := http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?limit=" + url.QueryEscape(value))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("limit=%q status=%d, want %d", value, resp.StatusCode, http.StatusBadRequest)
		}
	}
	resp, err := http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?limit=1&limit=2")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("repeated limit status=%d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestV2EventsRequiresCanonicalDurableThrough(t *testing.T) {
	ts := v2Server(t.TempDir())
	defer ts.Close()
	createV2Game(t, ts.URL)

	resp, err := http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?through=0")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("through=0 status=%d, want %d", resp.StatusCode, http.StatusOK)
	}
	for _, value := range []string{"", "00", "01", "+1", "-1", "1.0", "1x", "18446744073709551616"} {
		resp, err := http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?through=" + url.QueryEscape(value))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("through=%q status=%d, want %d", value, resp.StatusCode, http.StatusBadRequest)
		}
	}
	for _, query := range []string{"through=0&through=0", "through=1"} {
		resp, err := http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?" + query)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("query=%q status=%d, want %d", query, resp.StatusCode, http.StatusBadRequest)
		}
	}
}

func TestV2EventsThroughBoundsPagination(t *testing.T) {
	ts := v2Server(t.TempDir())
	defer ts.Close()
	createV2Game(t, ts.URL)
	command := `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99.50","ask":"100.50"}`
	resp, err := http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", strings.NewReader(command))
	if err != nil {
		t.Fatal(err)
	}
	var result exchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(result.Events) < 2 {
		t.Fatalf("command emitted %d events, want at least two", len(result.Events))
	}
	through := result.Events[0].Sequence

	resp, err = http.Get(fmt.Sprintf("%s/api/v2/games/%s/events?after=0&through=%d&limit=1", ts.URL, testGameID, through))
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Events    []exchange.Event `json:"events"`
		NextAfter uint64           `json:"next_after"`
		HasMore   bool             `json:"has_more"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(page.Events) != 1 || page.Events[0].Sequence != through || page.NextAfter != through || page.HasMore {
		t.Fatalf("bounded page=%+v through=%d", page, through)
	}

	resp, err = http.Get(fmt.Sprintf("%s/api/v2/games/%s/events?after=%d&through=%d&limit=200", ts.URL, testGameID, through, result.Events[len(result.Events)-1].Sequence))
	if err != nil {
		t.Fatal(err)
	}
	var remainder struct {
		Events []exchange.Event `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&remainder); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(remainder.Events) != len(result.Events)-1 || remainder.Events[0].Sequence <= through {
		t.Fatalf("remainder=%+v", remainder.Events)
	}

	resp, err = http.Get(ts.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot exchangeStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	wantThrough := result.Events[len(result.Events)-1].Sequence
	if snapshot.Version != result.Version || snapshot.State != result.State || snapshot.EventsThrough != wantThrough {
		t.Fatalf("state snapshot=%+v result version=%d through=%d", snapshot, result.Version, wantThrough)
	}
}

func TestV2EventsRejectsMalformedRawQuery(t *testing.T) {
	svc := newExchangeService(t.TempDir())
	entry, created, err := svc.createOrLoad(testGameID, testCreateID, "first-spread-v1")
	if err != nil || !created {
		t.Fatalf("create: created=%t err=%v", created, err)
	}
	for _, rawQuery := range []string{"after=0;limit=1", "after=%zz"} {
		request := httptest.NewRequest(http.MethodGet, "/api/v2/games/"+testGameID+"/events", nil)
		request.URL.RawQuery = rawQuery
		response := httptest.NewRecorder()
		svc.handleExchangeEvents(response, request, entry)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("query=%q status=%d body=%s", rawQuery, response.Code, response.Body.String())
		}
	}
}

func TestV2RequiresJSONContentType(t *testing.T) {
	body := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1"}`
	tests := []struct {
		name        string
		contentType string
		wantStatus  int
	}{
		{name: "missing", wantStatus: http.StatusBadRequest},
		{name: "jsonp", contentType: "application/jsonp", wantStatus: http.StatusBadRequest},
		{name: "suffix", contentType: "application/problem+json", wantStatus: http.StatusBadRequest},
		{name: "wrong type", contentType: "text/json", wantStatus: http.StatusBadRequest},
		{name: "malformed parameter", contentType: "application/json; charset", wantStatus: http.StatusBadRequest},
		{name: "charset", contentType: "application/json; charset=utf-8", wantStatus: http.StatusCreated},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc := newExchangeService(t.TempDir())
			request := httptest.NewRequest(http.MethodPost, "/api/v2/games", strings.NewReader(body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			svc.handleGames(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
		})
	}

	t.Run("multiple header fields", func(t *testing.T) {
		svc := newExchangeService(t.TempDir())
		request := httptest.NewRequest(http.MethodPost, "/api/v2/games", strings.NewReader(body))
		request.Header.Add("Content-Type", "application/json")
		request.Header.Add("Content-Type", "application/json")
		response := httptest.NewRecorder()
		svc.handleGames(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestV2RejectsDuplicateJSONKeys(t *testing.T) {
	t.Run("top-level", func(t *testing.T) {
		svc := newExchangeService(t.TempDir())
		body := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1"}`
		request := httptest.NewRequest(http.MethodPost, "/api/v2/games", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		svc.handleGames(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "duplicate JSON object key") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("nested", func(t *testing.T) {
		svc := newExchangeService(t.TempDir())
		entry, created, err := svc.createOrLoad(testGameID, testCreateID, "first-spread-v1")
		if err != nil || !created {
			t.Fatalf("create: created=%t err=%v", created, err)
		}
		body := `{"id":"` + testQuoteID + `","type":"open_account","payload":{"account_id":"local","account_id":"local"}}`
		request := httptest.NewRequest(http.MethodPost, "/api/v2/games/"+testGameID+"/commands", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		svc.handleExchangeCommand(response, request, testGameID, entry)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "duplicate JSON object key") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestV2ValidatesMutatingRequestOrigins(t *testing.T) {
	body := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1"}`
	tests := []struct {
		name       string
		host       string
		origin     string
		wantStatus int
	}{
		{name: "attacker host and origin", host: "attacker.example", origin: "https://attacker.example", wantStatus: http.StatusBadRequest},
		{name: "attacker origin", host: "localhost:8080", origin: "https://attacker.example", wantStatus: http.StatusBadRequest},
		{name: "origin path", host: "localhost:8080", origin: "http://localhost:8080/path", wantStatus: http.StatusBadRequest},
		{name: "origin query", host: "localhost:8080", origin: "http://localhost:8080?query", wantStatus: http.StatusBadRequest},
		{name: "origin userinfo", host: "localhost:8080", origin: "http://user@localhost:8080", wantStatus: http.StatusBadRequest},
		{name: "no origin", host: "attacker.example", wantStatus: http.StatusCreated},
		{name: "localhost", host: "localhost:8080", origin: "http://localhost:8080", wantStatus: http.StatusCreated},
		{name: "different loopback authority", host: "127.20.30.40:8080", origin: "https://127.0.0.1:8443", wantStatus: http.StatusBadRequest},
		{name: "https loopback", host: "127.20.30.40:8080", origin: "https://127.20.30.40:8080", wantStatus: http.StatusBadRequest},
		{name: "ipv4 loopback range", host: "127.20.30.40:8080", origin: "http://127.20.30.40:8080", wantStatus: http.StatusCreated},
		{name: "ipv6 loopback", host: "[::1]:8080", origin: "http://[::1]:8080", wantStatus: http.StatusCreated},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc := newExchangeService(t.TempDir())
			request := httptest.NewRequest(http.MethodPost, "/api/v2/games", strings.NewReader(body))
			request.Host = test.host
			request.Header.Set("Content-Type", "application/json")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			svc.handleGames(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
		})
	}
}

func createV2Game(t *testing.T, url string) {
	t.Helper()
	body := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1"}`
	resp, err := http.Post(url+"/api/v2/games", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d", resp.StatusCode)
	}
}

func TestV2CommandIsIdempotentAndRecoversAfterReload(t *testing.T) {
	root := t.TempDir()
	ts := v2Server(root)
	createV2Game(t, ts.URL)
	command := `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99.50","ask":"100.50"}`
	resp, err := http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", bytes.NewBufferString(command))
	if err != nil {
		t.Fatal(err)
	}
	var first exchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if first.Version != 1 || first.Command.Replayed || first.Scenario == nil || len(first.Scenario.Tutorial) != 4 || first.Coaching == nil {
		t.Fatalf("first=%+v", first)
	}
	flowOrders := 0
	for _, event := range first.Events {
		if event.Type == "flow_order" && event.Order != nil {
			flowOrders++
		}
	}
	if flowOrders == 0 || flowOrders != first.Summary.OrdersReceived {
		t.Fatalf("flow orders=%d summary=%+v", flowOrders, first.Summary)
	}
	eventsResp, err := http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?after=0&limit=200")
	if err != nil {
		t.Fatal(err)
	}
	var eventPage struct {
		Events []struct {
			Type      string `json:"type"`
			CommandID string `json:"command_id"`
			Sequence  uint64 `json:"sequence"`
		} `json:"events"`
	}
	if err := json.NewDecoder(eventsResp.Body).Decode(&eventPage); err != nil {
		eventsResp.Body.Close()
		t.Fatal(err)
	}
	eventsResp.Body.Close()
	persistedFlowOrders := 0
	lastSequence := uint64(0)
	for _, event := range eventPage.Events {
		if event.Sequence <= lastSequence {
			t.Fatalf("event sequence=%d after=%d", event.Sequence, lastSequence)
		}
		lastSequence = event.Sequence
		if event.Type == "flow_order" {
			if event.CommandID != testQuoteID {
				t.Fatalf("flow command id=%q", event.CommandID)
			}
			persistedFlowOrders++
		}
	}
	if persistedFlowOrders != flowOrders {
		t.Fatalf("persisted flow orders=%d response=%d", persistedFlowOrders, flowOrders)
	}

	resp, err = http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", bytes.NewBufferString(command))
	if err != nil {
		t.Fatal(err)
	}
	var replayed exchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&replayed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if replayed.Version != 1 || !replayed.Command.Replayed || replayed.State != first.State || !reflect.DeepEqual(replayed.Coaching, first.Coaching) {
		t.Fatalf("replayed=%+v", replayed)
	}
	ts.Close()

	// A new service reads exactly the committed command and retains its outcome.
	reloaded := v2Server(root)
	defer reloaded.Close()
	stateResp, err := http.Get(reloaded.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	var state exchangeResponse
	if err := json.NewDecoder(stateResp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	stateResp.Body.Close()
	if state.Version != 1 || state.State != first.State || !reflect.DeepEqual(state.Scenario, first.Scenario) || !reflect.DeepEqual(state.Coaching, first.Coaching) {
		t.Fatalf("reloaded=%+v", state)
	}
}

func TestV2CreateRetryReturnsOriginalCreateResult(t *testing.T) {
	root := t.TempDir()
	server := v2Server(root)
	createBody := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1"}`
	resp, err := http.Post(server.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	var original exchangeCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&original); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || original.Version != 0 || original.Coaching != nil || original.Recap != nil {
		t.Fatalf("original status=%d response=%+v", resp.StatusCode, original)
	}

	quote := `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99","ask":"101"}`
	resp, err = http.Post(server.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", strings.NewReader(quote))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("quote status=%d", resp.StatusCode)
	}
	server.Close()

	reloaded := v2Server(root)
	defer reloaded.Close()
	resp, err = http.Post(reloaded.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	var retried exchangeCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&retried); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !retried.Command.Replayed || retried.Version != 0 || retried.State != original.State || !reflect.DeepEqual(retried.Scenario, original.Scenario) || retried.Coaching != nil || retried.Recap != nil {
		t.Fatalf("retry status=%d original=%+v retry=%+v", resp.StatusCode, original, retried)
	}
}

func TestV2HistoricalCreateIDCommandCanBeRetried(t *testing.T) {
	svc := newExchangeService(t.TempDir())
	entry, created, err := svc.createOrLoad(testGameID, testCreateID, "first-spread-v1")
	if err != nil || !created {
		t.Fatalf("create: created=%t err=%v", created, err)
	}
	command := exchange.Command{ID: testCreateID, Type: exchange.CommandQuit, ExpectedVersion: 0}
	replayEngine, err := exchange.New(entry.log.Meta().Config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := replayEngine.Execute(command)
	if err != nil {
		t.Fatal(err)
	}
	for i := range result.Events {
		result.Events[i].CommandID = command.ID
	}
	entry.commands[command.ID] = eventlog.Record{Schema: eventlog.SchemaVersion, Version: result.State.Version, Command: command, Result: result}

	body := `{"id":"` + testCreateID + `","type":"quit","expected_version":0}`
	request := httptest.NewRequest(http.MethodPost, "/api/v2/games/"+testGameID+"/commands", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	svc.handleExchangeCommand(response, request, testGameID, entry)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var retried exchangeResponse
	if err := json.NewDecoder(response.Body).Decode(&retried); err != nil {
		t.Fatal(err)
	}
	if !retried.Command.Replayed || retried.Version != result.State.Version || retried.State != result.State {
		t.Fatalf("retry=%+v", retried)
	}
}

func TestV2AdverseSelectionLatestTurnPersistsAcrossReload(t *testing.T) {
	root := t.TempDir()
	base, ok := scenario.Get("first-spread-v1")
	if !ok {
		t.Fatal("missing base scenario")
	}
	definition := scenario.Definition{
		ID:            "test-adverse-selection-v2",
		Revision:      "1",
		Title:         "Test adverse selection",
		Briefing:      "Test briefing",
		Objective:     "Test objective",
		Reflection:    "Test reflection",
		ScorecardKind: "adverse_selection_turns",
		Config:        base.Config,
	}
	definition.Config.NumTurns = 3
	definition.Config.Seed = 909
	definition.Config.SimulationVersion = exchange.SimulationVersionAdverseSelection
	definition.Config.InformedFlowBps = 10_000
	definition.Config.MaxOrdersPerTurn = 1
	definition.Config.MaxOrderQty = fixed.Qty(10_000)
	definition.Config.MaxFlowSlippageBps = 0
	definition.Config.MinMoveBps = 100
	definition.Config.MaxMoveBps = 100

	svc := newExchangeService(root)
	svc.lookupScenario = func(id string) (scenario.Definition, bool) {
		return definition, id == definition.ID
	}
	ts := v2ServerForService(svc)
	createBody := fmt.Sprintf(`{"game_id":"%s","command_id":"%s","scenario_id":"%s"}`, testGameID, testCreateID, definition.ID)
	resp, err := http.Post(ts.URL+"/api/v2/games", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	var initial exchangeStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&initial); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if initial.LatestTurn != nil {
		t.Fatalf("new game latest turn=%+v", initial.LatestTurn)
	}

	quote := fmt.Sprintf(`{"id":"%s","type":"submit_quote","expected_version":0,"bid":"99","ask":"100"}`, testQuoteID)
	resp, err = http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", strings.NewReader(quote))
	if err != nil {
		t.Fatal(err)
	}
	var first exchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if first.Summary.PnLAttribution == nil || len(first.Events) == 0 {
		t.Fatalf("quote response omitted v2 attribution/events: %+v", first)
	}

	resp, err = http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", strings.NewReader(quote))
	if err != nil {
		t.Fatal(err)
	}
	var replayed exchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&replayed); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if !replayed.Command.Replayed || !reflect.DeepEqual(replayed.Summary, first.Summary) || !reflect.DeepEqual(replayed.Events, first.Events) {
		t.Fatalf("idempotent response diverged: first=%+v replayed=%+v", first, replayed)
	}

	resp, err = http.Get(ts.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	var afterQuote exchangeStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&afterQuote); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	wantLatest := &latestTurn{Turn: first.State.Turn, Summary: first.Summary, Coaching: first.Coaching}
	if !reflect.DeepEqual(afterQuote.LatestTurn, wantLatest) {
		t.Fatalf("latest turn=%+v want=%+v", afterQuote.LatestTurn, wantLatest)
	}

	quit := fmt.Sprintf(`{"id":"%s","type":"quit","expected_version":1}`, testQuitID)
	resp, err = http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", strings.NewReader(quit))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("quit status=%d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	var afterQuit exchangeStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&afterQuit); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if !reflect.DeepEqual(afterQuit.LatestTurn, wantLatest) {
		t.Fatalf("quit changed latest turn: %+v", afterQuit.LatestTurn)
	}

	type eventPage struct {
		Events []exchange.Event `json:"events"`
	}
	resp, err = http.Get(ts.URL + "/api/v2/games/" + testGameID + "/events?limit=200")
	if err != nil {
		t.Fatal(err)
	}
	var beforeReloadEvents eventPage
	if err := json.NewDecoder(resp.Body).Decode(&beforeReloadEvents); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	ts.Close()

	metaLog, records, err := eventlog.Open(root, testGameID)
	if err != nil {
		t.Fatal(err)
	}
	if metaLog.Meta().Schema != eventlog.SchemaVersion || eventlog.SchemaVersion != 4 || len(records) != 2 || metaLog.Meta().Config.SimulationVersion != exchange.SimulationVersionAdverseSelection {
		t.Fatalf("persisted schema/config/records changed: meta=%+v records=%d", metaLog.Meta(), len(records))
	}

	reloaded := v2Server(root)
	defer reloaded.Close()
	resp, err = http.Get(reloaded.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	var recovered exchangeStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&recovered); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if !reflect.DeepEqual(recovered.LatestTurn, wantLatest) || recovered.LatestTurn.Summary.PnLAttribution == nil {
		t.Fatalf("recovered latest turn=%+v", recovered.LatestTurn)
	}

	resp, err = http.Get(reloaded.URL + "/api/v2/games/" + testGameID + "/events?limit=200")
	if err != nil {
		t.Fatal(err)
	}
	var afterReloadEvents eventPage
	if err := json.NewDecoder(resp.Body).Decode(&afterReloadEvents); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if !reflect.DeepEqual(afterReloadEvents.Events, beforeReloadEvents.Events) {
		t.Fatalf("events changed after reload: before=%+v after=%+v", beforeReloadEvents.Events, afterReloadEvents.Events)
	}
}

func TestReplayTreatsOmittedEmptyLedgerAsEquivalent(t *testing.T) {
	definition, ok := scenario.Get("first-spread-v1")
	if !ok {
		t.Fatal("missing scenario")
	}
	cfg := definition.Config
	cfg.NumTurns = 0
	cfg.MaxOrdersPerTurn = 0
	cfg.StoragePerUnit = 0
	engine, err := exchange.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	commands := []exchange.Command{
		{ID: testQuoteID, Type: exchange.CommandSubmitQuote, ExpectedVersion: 0, Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)},
		{ID: "55555555-5555-4555-8555-555555555555", Type: exchange.CommandSubmitQuote, ExpectedVersion: 1, Bid: fixed.Price(995_000), Ask: fixed.Price(1_005_000)},
	}
	records := make([]eventlog.Record, 0, len(commands))
	for _, command := range commands {
		result, err := engine.Execute(command)
		if err != nil {
			t.Fatal(err)
		}
		for i := range result.Events {
			result.Events[i].CommandID = command.ID
		}
		records = append(records, eventlog.Record{Schema: eventlog.SchemaVersion, Version: result.State.Version, Command: command, Result: result})
	}
	if records[1].Result.Ledger == nil || len(records[1].Result.Ledger) != 0 {
		t.Fatalf("second quote ledger=%+v, want non-nil empty slice", records[1].Result.Ledger)
	}
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []eventlog.Record
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded[1].Result.Ledger != nil {
		t.Fatalf("omitted ledger decoded as %+v", decoded[1].Result.Ledger)
	}
	replayed, _, err := replayRecords(cfg, decoded)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.State() != engine.State() {
		t.Fatalf("replayed=%+v want=%+v", replayed.State(), engine.State())
	}
}

func TestRecoveryRetainsActiveLoggerBeforeNextAppend(t *testing.T) {
	root := t.TempDir()
	svc := newExchangeService(root)
	entry, created, err := svc.createOrLoad(testGameID, testCreateID, "first-spread-v1")
	if err != nil || !created {
		t.Fatalf("create: created=%t err=%v", created, err)
	}
	staleLog := entry.log

	separateLog, records, err := eventlog.Open(root, testGameID)
	if err != nil || len(records) != 0 {
		t.Fatalf("open separate log: records=%d err=%v", len(records), err)
	}
	engine, err := exchange.New(separateLog.Meta().Config)
	if err != nil {
		t.Fatal(err)
	}
	bid, err := fixed.ParsePrice("99.50")
	if err != nil {
		t.Fatal(err)
	}
	ask, err := fixed.ParsePrice("100.50")
	if err != nil {
		t.Fatal(err)
	}
	firstCommand := exchange.Command{ID: testQuoteID, Type: exchange.CommandSubmitQuote, ExpectedVersion: 0, Bid: bid, Ask: ask}
	before := engine.State()
	firstResult, err := engine.Execute(firstCommand)
	if err != nil {
		t.Fatal(err)
	}
	for i := range firstResult.Events {
		firstResult.Events[i].CommandID = firstCommand.ID
	}
	firstRecord := eventlog.Record{Schema: eventlog.SchemaVersion, Version: firstResult.State.Version, Command: firstCommand, Result: firstResult, Coaching: scenario.Coach(*entry.scenario, before, firstResult)}
	if _, err := separateLog.Append(firstRecord); err != nil {
		t.Fatal(err)
	}

	_, persistedRecords, err := eventlog.Open(root, testGameID)
	if err != nil {
		t.Fatal(err)
	}
	// The engine has observed the command while the active Log still considers
	// the externally appended bytes an uncertain suffix.
	entry.engine, entry.commands, err = replayRecords(separateLog.Meta().Config, persistedRecords)
	if err != nil {
		t.Fatal(err)
	}
	entry.coaching = firstRecord.Coaching

	secondCommand := `{"id":"55555555-5555-4555-8555-555555555555","type":"submit_quote","expected_version":1,"bid":"99.50","ask":"100.50"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v2/games/"+testGameID+"/commands", strings.NewReader(secondCommand))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	svc.handleExchangeCommand(response, req, testGameID, entry)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("stale append status=%d body=%s", response.Code, response.Body.String())
	}
	if entry.log != staleLog || entry.engine.State().Version != 1 || entry.commands[firstCommand.ID].Version != 1 || !reflect.DeepEqual(entry.coaching, firstRecord.Coaching) || entry.latestTurn == nil || !reflect.DeepEqual(entry.latestTurn.Summary, firstRecord.Result.Summary) {
		t.Fatalf("recovered entry did not preserve the active log and install all persisted state: %+v", entry)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v2/games/"+testGameID+"/commands", strings.NewReader(secondCommand))
	req.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	svc.handleExchangeCommand(response, req, testGameID, entry)
	if response.Code != http.StatusOK {
		t.Fatalf("next append status=%d body=%s", response.Code, response.Body.String())
	}

	_, records, err = eventlog.Open(root, testGameID)
	if err != nil || len(records) != 2 || records[1].Version != 2 || records[1].Command.ID != "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("persisted records=%+v err=%v", records, err)
	}
}

func TestRecoveryFencesChangedActiveLog(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, []byte)
	}{
		{
			name: "truncate",
			mutate: func(t *testing.T, path string, data []byte) {
				t.Helper()
				if err := os.Truncate(path, int64(len(data)-1)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "same-size replacement",
			mutate: func(t *testing.T, path string, data []byte) {
				t.Helper()
				replacement := path + ".replacement"
				if err := os.WriteFile(replacement, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			svc := newExchangeService(root)
			entry, created, err := svc.createOrLoad(testGameID, testCreateID, "first-spread-v1")
			if err != nil || !created {
				t.Fatalf("create: created=%t err=%v", created, err)
			}
			post := func(body string) *httptest.ResponseRecorder {
				request := httptest.NewRequest(http.MethodPost, "/api/v2/games/"+testGameID+"/commands", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				svc.handleExchangeCommand(response, request, testGameID, entry)
				return response
			}
			first := `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99.50","ask":"100.50"}`
			if response := post(first); response.Code != http.StatusOK {
				t.Fatalf("first command status=%d body=%s", response.Code, response.Body.String())
			}
			activeLog := entry.log
			path := filepath.Join(activeLog.Path(), "events.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, path, data)

			second := `{"id":"55555555-5555-4555-8555-555555555555","type":"submit_quote","expected_version":1,"bid":"99.50","ask":"100.50"}`
			response := post(second)
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"storage_failure"`) {
				t.Fatalf("changed log status=%d body=%s", response.Code, response.Body.String())
			}
			if !entry.storageFailed || entry.log != activeLog {
				t.Fatalf("changed active log was not fenced: storageFailed=%t sameLog=%t", entry.storageFailed, entry.log == activeLog)
			}
		})
	}
}

func TestRecoveryRequiresFreshDurabilitySync(t *testing.T) {
	root := t.TempDir()
	svc := newExchangeService(root)
	entry, created, err := svc.createOrLoad(testGameID, testCreateID, "first-spread-v1")
	if err != nil || !created {
		t.Fatalf("create: created=%t err=%v", created, err)
	}

	replayEngine, err := exchange.New(entry.log.Meta().Config)
	if err != nil {
		t.Fatal(err)
	}
	command := exchange.Command{ID: testQuitID, Type: exchange.CommandQuit}
	result, err := replayEngine.Execute(command)
	if err != nil {
		t.Fatal(err)
	}
	for i := range result.Events {
		result.Events[i].CommandID = command.ID
	}
	if _, err := entry.log.Append(eventlog.Record{Schema: eventlog.SchemaVersion, Version: result.State.Version, Command: command, Result: result}); err != nil {
		t.Fatal(err)
	}

	syncCalls := 0
	entry.syncLog = func(*eventlog.Log) error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("injected sync failure")
		}
		return nil
	}
	if err := entry.recover(); err == nil {
		t.Fatal("recovery succeeded without a fresh durability barrier")
	}
	if syncCalls != 1 || entry.engine.State().Version != 0 || len(entry.commands) != 0 {
		t.Fatalf("failed recovery installed records: calls=%d version=%d commands=%d", syncCalls, entry.engine.State().Version, len(entry.commands))
	}
	if err := entry.recover(); err != nil {
		t.Fatal(err)
	}
	if syncCalls != 2 || entry.engine.State().Version != 1 || entry.commands[testQuitID].Version != 1 {
		t.Fatalf("successful recovery did not install records: calls=%d version=%d commands=%+v", syncCalls, entry.engine.State().Version, entry.commands)
	}
}

func TestColdLoadRequiresDurabilitySync(t *testing.T) {
	root := t.TempDir()
	creator := newExchangeService(root)
	if _, created, err := creator.createOrLoad(testGameID, testCreateID, "first-spread-v1"); err != nil || !created {
		t.Fatalf("create: created=%t err=%v", created, err)
	}

	reloaded := newExchangeService(root)
	syncCalls := 0
	reloaded.syncLog = func(*eventlog.Log) error {
		syncCalls++
		return errors.New("injected sync failure")
	}
	if _, err := reloaded.load(testGameID); err == nil {
		t.Fatal("cold load succeeded without a durability barrier")
	}
	if syncCalls != 1 || reloaded.entries[testGameID] != nil {
		t.Fatalf("cold load installed entry: calls=%d entry=%v", syncCalls, reloaded.entries[testGameID])
	}
}

func TestRecapRecoveryFailureFencesMutatedEngine(t *testing.T) {
	root := t.TempDir()
	svc := newExchangeService(root)
	entry, created, err := svc.createOrLoad(testGameID, testCreateID, "first-spread-v1")
	if err != nil || !created {
		t.Fatalf("create: created=%t err=%v", created, err)
	}
	entry.commands["poison-recap"] = eventlog.Record{Result: exchange.Result{Summary: exchange.Summary{InformedOrders: -1}}}
	syncCalls := 0
	entry.syncLog = func(*eventlog.Log) error {
		syncCalls++
		return errors.New("injected sync failure")
	}

	command := fmt.Sprintf(`{"id":"%s","type":"quit","expected_version":0}`, testQuitID)
	request := httptest.NewRequest(http.MethodPost, "/api/v2/games/"+testGameID+"/commands", strings.NewReader(command))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	svc.handleExchangeCommand(response, request, testGameID, entry)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"recap_failure"`) {
		t.Fatalf("recap failure status=%d body=%s", response.Code, response.Body.String())
	}
	if syncCalls != 1 || !entry.storageFailed || entry.engine.State().Version != 1 {
		t.Fatalf("recap failure was not fenced: calls=%d storageFailed=%t version=%d", syncCalls, entry.storageFailed, entry.engine.State().Version)
	}
	_, records, err := eventlog.Open(root, testGameID)
	if err != nil || len(records) != 0 {
		t.Fatalf("uncommitted command reached storage: records=%+v err=%v", records, err)
	}

	stateResponse := httptest.NewRecorder()
	svc.handleExchangeState(stateResponse, testGameID, entry)
	if stateResponse.Code != http.StatusInternalServerError || !strings.Contains(stateResponse.Body.String(), `"code":"storage_failure"`) {
		t.Fatalf("state exposed fenced engine: status=%d body=%s", stateResponse.Code, stateResponse.Body.String())
	}

	nextCommand := fmt.Sprintf(`{"id":"%s","type":"quit","expected_version":1}`, testQuoteID)
	nextRequest := httptest.NewRequest(http.MethodPost, "/api/v2/games/"+testGameID+"/commands", strings.NewReader(nextCommand))
	nextRequest.Header.Set("Content-Type", "application/json")
	nextResponse := httptest.NewRecorder()
	svc.handleExchangeCommand(nextResponse, nextRequest, testGameID, entry)
	if nextResponse.Code != http.StatusInternalServerError || entry.engine.State().Version != 1 {
		t.Fatalf("command bypassed storage fence: status=%d version=%d body=%s", nextResponse.Code, entry.engine.State().Version, nextResponse.Body.String())
	}
}

func TestStorageFailureBlocksFurtherGameAccess(t *testing.T) {
	root := t.TempDir()
	svc := newExchangeService(root)
	entry, created, err := svc.createOrLoad(testGameID, testCreateID, "first-spread-v1")
	if err != nil || !created {
		t.Fatalf("create: created=%t err=%v", created, err)
	}
	entry.storageFailed = true

	command := `{"id":"55555555-5555-4555-8555-555555555555","type":"submit_quote","expected_version":0,"bid":"99.50","ask":"100.50"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v2/games/"+testGameID+"/commands", strings.NewReader(command))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	svc.handleExchangeCommand(response, request, testGameID, entry)
	if response.Code != http.StatusInternalServerError || entry.engine.State().Version != 0 {
		t.Fatalf("command status=%d version=%d body=%s", response.Code, entry.engine.State().Version, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v2/games/"+testGameID, nil)
	response = httptest.NewRecorder()
	svc.handleExchangeState(response, testGameID, entry)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("state status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestV2RejectsUnknownScenario(t *testing.T) {
	ts := v2Server(t.TempDir())
	defer ts.Close()
	body := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"not-a-scenario"}`
	resp, err := http.Post(ts.URL+"/api/v2/games", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestV2PersistsTerminalRecap(t *testing.T) {
	root := t.TempDir()
	ts := v2Server(root)
	createV2Game(t, ts.URL)
	var terminal exchangeResponse
	for turn := 0; turn < 8; turn++ {
		commandID := fmt.Sprintf("44444444-4444-4444-8444-%012d", turn+1)
		quote := fmt.Sprintf(`{"id":"%s","type":"submit_quote","expected_version":%d,"bid":"99.50","ask":"100.50"}`, commandID, turn)
		resp, err := http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", bytes.NewBufferString(quote))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(resp.Body).Decode(&terminal); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if terminal.Recap == nil || terminal.Recap.Scorecard == nil || terminal.Coaching == nil || !terminal.State.IsOver {
		t.Fatalf("terminal=%+v", terminal)
	}
	if terminal.Recap.UnitsTraded < terminal.Summary.UnitsTraded || terminal.Recap.StoragePaid < terminal.Summary.StorageCost || terminal.Recap.MaxAbsInventory < fixed.AbsQty(terminal.State.Position) {
		t.Fatalf("recap=%+v terminal summary=%+v", terminal.Recap, terminal.Summary)
	}
	ts.Close()

	reloaded := v2Server(root)
	defer reloaded.Close()
	resp, err := http.Get(reloaded.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	var state exchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !reflect.DeepEqual(state.Recap, terminal.Recap) {
		t.Fatalf("recap after reload=%+v", state.Recap)
	}
}

func TestSchema1TerminalResponseUsesDurableProjectedRecord(t *testing.T) {
	root := t.TempDir()
	definition, ok := scenario.Get("first-spread-v1")
	if !ok {
		t.Fatal("missing legacy scenario")
	}
	cfg := definition.Config
	cfg.NumTurns = 1
	cfg.MaxOrdersPerTurn = 0
	cfg.StoragePerUnit = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	snapshot := definition.Snapshot()
	snapshot.Turns = 1
	snapshot.Modes = nil
	snapshot.RealTime = nil
	dir := filepath.Join(root, testGameID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := eventlog.Meta{Schema: 1, GameID: testGameID, OwnerID: localPrincipal, CreateCommandID: testCreateID, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Config: cfg, Scenario: &snapshot}
	metaData, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), append(metaData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	command := fmt.Sprintf(`{"id":"%s","type":"submit_quote","expected_version":0,"bid":"99","ask":"101"}`, testQuoteID)
	postCommand := func(t *testing.T, serverURL string) exchangeResponse {
		t.Helper()
		resp, err := http.Post(serverURL+"/api/v2/games/"+testGameID+"/commands", "application/json", strings.NewReader(command))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("command status=%d", resp.StatusCode)
		}
		var response exchangeResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	normalizeReplay := func(response exchangeResponse) exchangeResponse {
		response.Command.Replayed = false
		return response
	}

	server := v2Server(root)
	immediate := postCommand(t, server.URL)
	if !immediate.State.IsOver || immediate.State.Reason != exchange.TurnsComplete {
		t.Fatalf("terminal command did not complete legacy lesson: %+v", immediate)
	}
	unprojected, err := scenario.BuildRecap(snapshot, cfg, nil, exchange.Result{State: immediate.State, Summary: immediate.Summary, Events: immediate.Events})
	if err != nil {
		t.Fatal(err)
	}
	if unprojected.Scorecard == nil {
		t.Fatalf("source recap did not contain a scorecard: %+v", unprojected)
	}
	if immediate.Recap == nil || immediate.Recap.Scorecard != nil {
		t.Fatalf("immediate recap was not schema-1 projected: %+v", immediate.Recap)
	}
	inMemoryReplay := postCommand(t, server.URL)
	if !inMemoryReplay.Command.Replayed || !reflect.DeepEqual(normalizeReplay(inMemoryReplay), normalizeReplay(immediate)) {
		t.Fatalf("in-memory replay differs: immediate=%+v replay=%+v", immediate, inMemoryReplay)
	}

	_, records, err := eventlog.Open(root, testGameID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !reflect.DeepEqual(records[0].Recap, immediate.Recap) || records[0].Result.State != immediate.State || !reflect.DeepEqual(records[0].Result.Summary, immediate.Summary) || !reflect.DeepEqual(records[0].Result.Events, immediate.Events) {
		t.Fatalf("durable record differs from immediate response: response=%+v records=%+v", immediate, records)
	}
	server.Close()

	reloaded := v2Server(root)
	defer reloaded.Close()
	stateResp, err := http.Get(reloaded.URL + "/api/v2/games/" + testGameID)
	if err != nil {
		t.Fatal(err)
	}
	var state exchangeStateResponse
	if err := json.NewDecoder(stateResp.Body).Decode(&state); err != nil {
		stateResp.Body.Close()
		t.Fatal(err)
	}
	stateResp.Body.Close()
	if !reflect.DeepEqual(state.Recap, immediate.Recap) || state.LatestTurn == nil || !reflect.DeepEqual(state.LatestTurn.Summary, immediate.Summary) || !reflect.DeepEqual(state.Coaching, immediate.Coaching) {
		t.Fatalf("reloaded state differs from durable response: %+v", state)
	}
	reloadedReplay := postCommand(t, reloaded.URL)
	if !reloadedReplay.Command.Replayed || !reflect.DeepEqual(normalizeReplay(reloadedReplay), normalizeReplay(immediate)) {
		t.Fatalf("reloaded replay differs: immediate=%+v replay=%+v", immediate, reloadedReplay)
	}
}

func TestV2CreateRetrySurvivesCatalogRemoval(t *testing.T) {
	root := t.TempDir()
	ts := v2Server(root)
	createV2Game(t, ts.URL)
	ts.Close()

	reloadedService := newExchangeService(root)
	reloadedService.lookupScenario = func(string) (scenario.Definition, bool) { return scenario.Definition{}, false }
	reloaded := v2ServerForService(reloadedService)
	defer reloaded.Close()
	body := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1"}`
	resp, err := http.Post(reloaded.URL+"/api/v2/games", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var retried exchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&retried); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !retried.Command.Replayed || retried.Scenario == nil || retried.Scenario.ID != "first-spread-v1" {
		t.Fatalf("retry status=%d response=%+v", resp.StatusCode, retried)
	}
}

func TestV2ValidatesScenarioCommandPayloads(t *testing.T) {
	ts := v2Server(t.TempDir())
	defer ts.Close()
	createV2Game(t, ts.URL)
	commandURL := ts.URL + "/api/v2/games/" + testGameID + "/commands"

	invalid := []struct {
		name string
		body string
	}{
		{name: "omitted expected version", body: `{"id":"` + testQuoteID + `","type":"submit_quote","bid":"99","ask":"101"}`},
		{name: "null expected version", body: `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":null,"bid":"99","ask":"101"}`},
		{name: "quit omitted expected version", body: `{"id":"` + testQuoteID + `","type":"quit"}`},
		{name: "quit null expected version", body: `{"id":"` + testQuoteID + `","type":"quit","expected_version":null}`},
		{name: "missing bid", body: `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"ask":"101"}`},
		{name: "missing ask", body: `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99"}`},
		{name: "null bid", body: `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":null,"ask":"101"}`},
		{name: "submit venue field", body: `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99","ask":"101","account_id":"player"}`},
		{name: "submit irrelevant field", body: `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99","ask":"101","quantity":"1"}`},
		{name: "quit bid", body: `{"id":"` + testQuoteID + `","type":"quit","expected_version":0,"bid":"99"}`},
		{name: "quit null ask", body: `{"id":"` + testQuoteID + `","type":"quit","expected_version":0,"ask":null}`},
		{name: "quit command payload", body: `{"id":"` + testQuoteID + `","type":"quit","expected_version":0,"order_id":1}`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			resp, err := http.Post(commandURL, "application/json", strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
		})
	}

	unsupported := []string{
		`{"id":"` + testQuoteID + `","type":"open_account","expected_version":0}`,
		`{"id":"` + testQuoteID + `","type":"place_order","account_id":"local","quantity":"1","limit_price":"100"}`,
	}
	for _, body := range unsupported {
		resp, err := http.Post(commandURL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("unsupported body=%s status=%d, want %d", body, resp.StatusCode, http.StatusForbidden)
		}
	}

	createIDReuse := `{"id":"` + testCreateID + `","type":"quit","expected_version":0}`
	resp, err := http.Post(commandURL, "application/json", strings.NewReader(createIDReuse))
	if err != nil {
		t.Fatal(err)
	}
	var reuseError apiError
	if err := json.NewDecoder(resp.Body).Decode(&reuseError); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || reuseError.Error.Code != "idempotency_key_reused" {
		t.Fatalf("create id reuse status=%d error=%+v", resp.StatusCode, reuseError.Error)
	}

	command := `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99","ask":"101"}`
	for attempt := 0; attempt < 2; attempt++ {
		resp, err = http.Post(commandURL, "application/json; charset=utf-8", strings.NewReader(command))
		if err != nil {
			t.Fatal(err)
		}
		var response exchangeResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || response.Version != 1 || response.Command.Replayed != (attempt == 1) {
			t.Fatalf("attempt=%d status=%d response=%+v", attempt, resp.StatusCode, response)
		}
	}
}

func TestV2RejectsStaleAndMalformedCommands(t *testing.T) {
	ts := v2Server(t.TempDir())
	defer ts.Close()
	createV2Game(t, ts.URL)
	current := `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99","ask":"101"}`
	resp, err := http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", bytes.NewBufferString(current))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("current status=%d", resp.StatusCode)
	}
	stale := `{"id":"` + testQuitID + `","type":"submit_quote","expected_version":0,"bid":"99","ask":"101"}`
	resp, err = http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", bytes.NewBufferString(stale))
	if err != nil {
		t.Fatal(err)
	}
	var staleError apiError
	if err := json.NewDecoder(resp.Body).Decode(&staleError); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || staleError.Error.Code != "version_conflict" {
		t.Fatalf("stale status=%d error=%+v", resp.StatusCode, staleError.Error)
	}
	bad := `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":1,"bid":"99","ask":"101","unexpected":true}`
	resp, err = http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", bytes.NewBufferString(bad))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestV2CreateIsIdempotentOnlyForMatchingRequest(t *testing.T) {
	ts := v2Server(t.TempDir())
	defer ts.Close()
	createV2Game(t, ts.URL)
	body := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","scenario_id":"first-spread-v1"}`
	resp, err := http.Post(ts.URL+"/api/v2/games", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	conflict := strings.Replace(body, testCreateID, "44444444-4444-4444-8444-444444444444", 1)
	resp, err = http.Post(ts.URL+"/api/v2/games", "application/json", bytes.NewBufferString(conflict))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status=%d", resp.StatusCode)
	}
}

func TestV2CreateCommandIDIsScopedAcrossGamesAndRestart(t *testing.T) {
	root := t.TempDir()
	service := newExchangeService(root)
	if _, created, err := service.createOrLoad(testGameID, testCreateID, "first-spread-v1"); err != nil || !created {
		t.Fatalf("first create: created=%t err=%v", created, err)
	}
	if _, _, err := service.createOrLoad(testOtherGameID, testCreateID, "first-spread-v1"); !errors.Is(err, errCreateConflict) {
		t.Fatalf("same-process cross-game create error=%v, want conflict", err)
	}

	restarted := newExchangeService(root)
	if len(restarted.entries) != 0 {
		t.Fatalf("new service unexpectedly loaded entries: %+v", restarted.entries)
	}
	if _, _, err := restarted.createOrLoad(testOtherGameID, testCreateID, "inventory-pressure-v1"); !errors.Is(err, errCreateConflict) {
		t.Fatalf("restart cross-game create error=%v, want conflict", err)
	}
	if restarted.entries[testGameID] != nil {
		t.Fatal("metadata scan cold-loaded the existing game")
	}
}

func TestV2CreateMetadataScanIsStrictAndIgnoresStaging(t *testing.T) {
	t.Run("unreadable published metadata", func(t *testing.T) {
		root := t.TempDir()
		published := filepath.Join(root, testGameID)
		if err := os.Mkdir(published, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(published, "meta.json"), []byte("invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
		service := newExchangeService(root)
		body := `{"game_id":"` + testOtherGameID + `","command_id":"` + testQuitID + `","scenario_id":"first-spread-v1"}`
		request := httptest.NewRequest(http.MethodPost, "/api/v2/games", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		service.handleGames(response, request)
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"storage_failure"`) {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("abandoned staging", func(t *testing.T) {
		root := t.TempDir()
		staging, err := os.MkdirTemp(root, ".eventlog-staging-")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staging, "meta.json"), []byte("invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
		service := newExchangeService(root)
		if _, created, err := service.createOrLoad(testGameID, testCreateID, "first-spread-v1"); err != nil || !created {
			t.Fatalf("create with staging entry: created=%t err=%v", created, err)
		}
	})
}

func TestCreateRetryUsesPersistedScenarioIdentity(t *testing.T) {
	root := t.TempDir()
	svc := newExchangeService(root)
	first, ok := scenario.Get("first-spread-v1")
	if !ok {
		t.Fatal("missing first scenario")
	}
	entry, created, err := svc.createOrLoad(testGameID, testCreateID, first.ID)
	if err != nil || !created {
		t.Fatalf("create: entry=%v created=%t err=%v", entry != nil, created, err)
	}

	retry, created, err := svc.createOrLoad(testGameID, testCreateID, first.ID)
	if err != nil || created || retry != entry {
		t.Fatalf("retry: entry=%v created=%t err=%v", retry == entry, created, err)
	}
	reloaded := newExchangeService(root)
	reloaded.lookupScenario = func(string) (scenario.Definition, bool) { return scenario.Definition{}, false }
	retry, created, err = reloaded.createOrLoad(testGameID, testCreateID, first.ID)
	if err != nil || created || retry.log.Meta().Scenario.ID != first.ID {
		t.Fatalf("persisted retry: created=%t err=%v", created, err)
	}

	second, ok := scenario.Get("inventory-pressure-v1")
	if !ok {
		t.Fatal("missing second scenario")
	}
	if _, _, err := svc.createOrLoad(testGameID, testCreateID, second.ID); !errors.Is(err, errCreateConflict) {
		t.Fatalf("different scenario error=%v", err)
	}
}
