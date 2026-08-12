package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"market-maker/internal/eventlog"
	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
	"market-maker/internal/game"
	"market-maker/internal/realtime"
	"market-maker/internal/scenario"
)

const localPrincipal = "local"

var uuidPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[1-5][a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)

type exchangeService struct {
	mu             sync.Mutex
	shutdownMu     sync.Mutex
	shutdownDone   bool
	shutdownErr    error
	root           string
	entries        map[string]*exchangeEntry
	lookupScenario func(string) (scenario.Definition, bool)
	newSeed        func() (uint64, error)
	syncLog        func(*eventlog.Log) error
	metrics        *metrics
	newSequencer   func(realtime.Config) (*realtime.Sequencer, error)
}

type exchangeEntry struct {
	mu             sync.Mutex
	sequencerMu    sync.Mutex
	engine         *exchange.Engine
	log            *eventlog.Log
	commands       map[string]eventlog.Record
	startingEquity fixed.Money
	eventsThrough  uint64
	latestTurn     *latestTurn
	scenario       *scenario.Snapshot
	coaching       *scenario.Coaching
	recap          *scenario.Recap
	mode           game.PlayMode
	realTime       *realTimeState
	generator      *realtime.Generator
	realtimeExec   *realtime.ExchangeExecutor
	replay         realTimeReplaySummary
	sequencer      *realtime.Sequencer
	newSequencer   func(realtime.Config) (*realtime.Sequencer, error)
	storageFailed  bool
	syncLog        func(*eventlog.Log) error
	metrics        *metrics
}

func newExchangeService(root string) *exchangeService {
	return &exchangeService{root: root, entries: make(map[string]*exchangeEntry), lookupScenario: scenario.Get, newSeed: randomSeed, syncLog: syncEventLog, newSequencer: realtime.NewConfigured}
}

func syncEventLog(log *eventlog.Log) error { return log.Sync() }

func randomSeed() (uint64, error) {
	var bytes [8]byte
	for {
		if _, err := rand.Read(bytes[:]); err != nil {
			return 0, err
		}
		if seed := binary.LittleEndian.Uint64(bytes[:]); seed != 0 {
			return seed, nil
		}
	}
}

type createExchangeRequest struct {
	GameID     string          `json:"game_id"`
	CommandID  string          `json:"command_id"`
	ScenarioID string          `json:"scenario_id"`
	Mode       json.RawMessage `json:"mode"`
}

func (req createExchangeRequest) playMode() (game.PlayMode, error) {
	if len(req.Mode) == 0 {
		return game.PlayModeTurnBased, nil
	}
	var mode game.PlayMode
	if bytes.Equal(bytes.TrimSpace(req.Mode), []byte("null")) || json.Unmarshal(req.Mode, &mode) != nil {
		return "", errors.New("mode must be a play-mode string")
	}
	if err := mode.Validate(); err != nil {
		return "", err
	}
	return mode, nil
}

type exchangeCommandRequest struct {
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	ExpectedVersion *uint64         `json:"expected_version"`
	Bid             json.RawMessage `json:"bid"`
	Ask             json.RawMessage `json:"ask"`
}

func (req exchangeCommandRequest) command() (exchange.Command, error) {
	if req.ExpectedVersion == nil {
		return exchange.Command{}, errors.New("expected_version is required")
	}
	switch req.Type {
	case exchange.CommandSubmitQuote:
		if len(req.Bid) == 0 || len(req.Ask) == 0 {
			return exchange.Command{}, errors.New("submit_quote requires bid and ask")
		}
		var bid, ask fixed.Price
		if err := json.Unmarshal(req.Bid, &bid); err != nil {
			return exchange.Command{}, fmt.Errorf("invalid bid: %w", err)
		}
		if err := json.Unmarshal(req.Ask, &ask); err != nil {
			return exchange.Command{}, fmt.Errorf("invalid ask: %w", err)
		}
		return exchange.Command{ID: req.ID, Type: req.Type, ExpectedVersion: *req.ExpectedVersion, Bid: bid, Ask: ask}, nil
	case exchange.CommandQuit:
		if len(req.Bid) != 0 || len(req.Ask) != 0 {
			return exchange.Command{}, errors.New("quit does not accept bid or ask")
		}
		return exchange.Command{ID: req.ID, Type: req.Type, ExpectedVersion: *req.ExpectedVersion}, nil
	default:
		return exchange.Command{}, errors.New("unsupported command type")
	}
}

type commandResponse struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Replayed bool   `json:"replayed"`
}

type exchangeResponse struct {
	GameID   string             `json:"game_id"`
	Version  uint64             `json:"version"`
	State    exchange.State     `json:"state"`
	Summary  exchange.Summary   `json:"summary"`
	Events   []exchange.Event   `json:"events"`
	Command  commandResponse    `json:"command"`
	Scenario *scenario.Snapshot `json:"scenario,omitempty"`
	Coaching *scenario.Coaching `json:"coaching,omitempty"`
	Recap    *scenario.Recap    `json:"recap,omitempty"`
	Mode     game.PlayMode      `json:"mode"`
	RealTime *realTimeState     `json:"real_time,omitempty"`
}

type exchangeStateResponse struct {
	GameID         string             `json:"game_id"`
	Version        uint64             `json:"version"`
	StartingEquity fixed.Money        `json:"starting_equity"`
	EventsThrough  uint64             `json:"events_through"`
	State          exchange.State     `json:"state"`
	LatestTurn     *latestTurn        `json:"latest_turn,omitempty"`
	Scenario       *scenario.Snapshot `json:"scenario,omitempty"`
	Coaching       *scenario.Coaching `json:"coaching,omitempty"`
	Recap          *scenario.Recap    `json:"recap,omitempty"`
	Mode           game.PlayMode      `json:"mode"`
	RealTime       *realTimeState     `json:"real_time,omitempty"`
}

type realTimeState struct {
	Lifecycle game.LifecycleState `json:"lifecycle"`
}

type latestTurn struct {
	Turn     int                `json:"turn"`
	Summary  exchange.Summary   `json:"summary"`
	Coaching *scenario.Coaching `json:"coaching,omitempty"`
}

type exchangeCreateResponse struct {
	GameID   string             `json:"game_id"`
	Version  uint64             `json:"version"`
	State    exchange.State     `json:"state"`
	Command  commandResponse    `json:"command"`
	Scenario *scenario.Snapshot `json:"scenario,omitempty"`
	Coaching *scenario.Coaching `json:"coaching,omitempty"`
	Recap    *scenario.Recap    `json:"recap,omitempty"`
	Mode     game.PlayMode      `json:"mode"`
	RealTime *realTimeState     `json:"real_time,omitempty"`
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *exchangeService) handleGames(w http.ResponseWriter, r *http.Request) {
	outcome := "rejected"
	defer func() { s.metrics.observeCommand("create_game", outcome) }()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	var req createExchangeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !validUUID(req.GameID) || !validUUID(req.CommandID) {
		writeAPIError(w, http.StatusBadRequest, "invalid_id", "game_id and command_id must be UUIDs")
		return
	}
	mode, err := req.playMode()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_mode", err.Error())
		return
	}
	req.GameID, req.CommandID = strings.ToLower(req.GameID), strings.ToLower(req.CommandID)
	entry, created, err := s.createOrLoadMode(req.GameID, req.CommandID, req.ScenarioID, mode)
	if errors.Is(err, errUnknownScenario) {
		writeAPIError(w, http.StatusBadRequest, "unknown_scenario", "scenario_id is not in the server catalog")
		return
	}
	if errors.Is(err, errCreateConflict) {
		writeAPIError(w, http.StatusConflict, "idempotency_key_reused", "game id or create command id is already associated with a different create request")
		return
	}
	if errors.Is(err, errUnsupportedMode) {
		writeAPIError(w, http.StatusBadRequest, "unsupported_mode", "scenario does not support the requested play mode")
		return
	}
	if err != nil {
		outcome = "storage_failure"
		writeAPIError(w, http.StatusInternalServerError, "storage_failure", "could not create game")
		return
	}
	entry.mu.Lock()
	if entry.storageFailed {
		entry.mu.Unlock()
		outcome = "storage_failure"
		writeAPIError(w, http.StatusInternalServerError, "storage_failure", "game storage is unavailable")
		return
	}
	state := entry.engine.State()
	snapshot, coaching, recap, mode, realTime := entry.scenario, entry.coaching, entry.recap, entry.mode, entry.realTime
	if !created {
		initial, err := exchange.New(entry.log.Meta().Config)
		if err != nil {
			entry.mu.Unlock()
			outcome = "storage_failure"
			writeAPIError(w, http.StatusInternalServerError, "storage_failure", "could not restore create result")
			return
		}
		state, coaching, recap = initial.State(), nil, nil
	}
	entry.mu.Unlock()
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	} else {
		w.Header().Set("Location", "/api/v2/games/"+req.GameID)
	}
	writeJSON(w, status, exchangeCreateResponse{GameID: req.GameID, Version: state.Version, State: state, Command: commandResponse{ID: req.CommandID, Type: "create_game", Replayed: !created}, Scenario: snapshot, Coaching: coaching, Recap: recap, Mode: mode, RealTime: realTime})
	if created {
		outcome = "accepted"
	} else {
		outcome = "replayed"
	}
}

func (s *exchangeService) handleScenarios(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scenarios": scenario.List()})
}

func (s *exchangeService) handleGame(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v2/games/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || !validUUID(parts[0]) {
		writeAPIError(w, http.StatusBadRequest, "invalid_id", "game_id must be a UUID")
		return
	}
	if len(parts) > 2 || (len(parts) == 2 && parts[1] != "commands" && parts[1] != "events") {
		writeAPIError(w, http.StatusNotFound, "not_found", "game endpoint not found")
		return
	}
	parts[0] = strings.ToLower(parts[0])
	entry, err := s.load(parts[0])
	if errors.Is(err, os.ErrNotExist) {
		writeAPIError(w, http.StatusNotFound, "not_found", "game not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "storage_failure", "could not load game")
		return
	}
	if len(parts) == 1 {
		if r.Method == http.MethodGet {
			s.handleExchangeState(w, parts[0], entry)
			return
		}
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	if parts[1] == "commands" {
		if r.Method == http.MethodPost {
			s.handleExchangeCommand(w, r, parts[0], entry)
			return
		}
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	if r.Method == http.MethodGet {
		s.handleExchangeEvents(w, r, entry)
		return
	}
	w.Header().Set("Allow", http.MethodGet)
	writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
}

func (s *exchangeService) handleExchangeState(w http.ResponseWriter, id string, entry *exchangeEntry) {
	entry.mu.Lock()
	if entry.storageFailed {
		entry.mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "storage_failure", "game storage is unavailable")
		return
	}
	state := entry.engine.State()
	startingEquity, eventsThrough := entry.startingEquity, entry.eventsThrough
	latest, snapshot, coaching, recap, mode, realTime := entry.latestTurn, entry.scenario, entry.coaching, entry.recap, entry.mode, entry.realTime
	entry.mu.Unlock()
	writeJSON(w, http.StatusOK, exchangeStateResponse{GameID: id, Version: state.Version, StartingEquity: startingEquity, EventsThrough: eventsThrough, State: state, LatestTurn: latest, Scenario: snapshot, Coaching: coaching, Recap: recap, Mode: mode, RealTime: realTime})
}

func (s *exchangeService) handleExchangeCommand(w http.ResponseWriter, r *http.Request, id string, entry *exchangeEntry) {
	commandType, outcome := "unknown", "rejected"
	defer func() { s.metrics.observeCommand(commandType, outcome) }()
	entry.mu.Lock()
	mode, storageFailed := entry.mode, entry.storageFailed
	entry.mu.Unlock()
	if storageFailed {
		outcome = "storage_failure"
		writeAPIError(w, http.StatusInternalServerError, "storage_failure", "game storage is unavailable")
		return
	}
	if mode == game.PlayModeRealTime {
		writeAPIError(w, http.StatusConflict, "command_unavailable_for_mode", "real-time commands are not available yet")
		return
	}
	data, err := readJSONRequest(w, r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &kind); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	commandType = kind.Type
	if kind.Type != exchange.CommandSubmitQuote && kind.Type != exchange.CommandQuit {
		writeAPIError(w, http.StatusForbidden, "venue_command_unavailable", "account commands require authenticated venue access")
		return
	}
	var req exchangeCommandRequest
	if err := decodeStrictJSON(data, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !validUUID(req.ID) {
		writeAPIError(w, http.StatusBadRequest, "invalid_id", "command id must be a UUID")
		return
	}
	req.ID = strings.ToLower(req.ID)
	command, err := req.command()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	entry.mu.Lock()
	if entry.storageFailed {
		entry.mu.Unlock()
		outcome = "storage_failure"
		writeAPIError(w, http.StatusInternalServerError, "storage_failure", "game storage is unavailable")
		return
	}
	if prior, ok := entry.commands[command.ID]; ok {
		if prior.Command != command {
			entry.mu.Unlock()
			writeAPIError(w, http.StatusConflict, "idempotency_key_reused", "command id has a different payload")
			return
		}
		response := exchangeResult(id, prior.Result, command, true, entry.scenario, prior.Coaching, prior.Recap, entry.mode, entry.realTime)
		entry.mu.Unlock()
		writeJSON(w, http.StatusOK, response)
		outcome = "replayed"
		return
	}
	if command.ID == entry.log.Meta().CreateCommandID {
		entry.mu.Unlock()
		writeAPIError(w, http.StatusConflict, "idempotency_key_reused", "command id was already used to create this game")
		return
	}
	if command.ExpectedVersion != entry.engine.State().Version {
		entry.mu.Unlock()
		writeAPIError(w, http.StatusConflict, "version_conflict", "expected_version does not match current game version")
		return
	}
	before := entry.engine.State()
	result, err := entry.engine.Execute(command)
	if err != nil {
		entry.mu.Unlock()
		writeAPIError(w, http.StatusConflict, "command_rejected", err.Error())
		return
	}
	for i := range result.Events {
		result.Events[i].CommandID = command.ID
	}
	record := eventlog.Record{Schema: eventlog.SchemaVersion, Version: result.State.Version, Command: command, Result: result}
	if entry.scenario != nil {
		record.Coaching = scenario.Coach(*entry.scenario, before, result)
	}
	if entry.scenario != nil && result.State.IsOver {
		recap, err := scenario.BuildRecap(*entry.scenario, entry.log.Meta().Config, priorResults(entry.commands), result)
		if err != nil {
			if recoverErr := entry.recover(); recoverErr != nil {
				entry.storageFailed = true
			}
			entry.mu.Unlock()
			outcome = "recap_failure"
			writeAPIError(w, http.StatusInternalServerError, "recap_failure", "could not build session recap")
			return
		}
		record.Recap = recap
	}
	appendStarted := time.Now()
	record, err = entry.log.Append(record)
	appendOutcome := "success"
	if err != nil {
		appendOutcome = "failure"
	}
	s.metrics.observeEventLogAppend(appendOutcome, time.Since(appendStarted))
	if err != nil {
		// A write or sync error leaves commit status uncertain. Rebuild so a
		// retry can be answered idempotently, but never acknowledge the command
		// here because durability was not confirmed.
		if recoverErr := entry.recover(); recoverErr != nil {
			entry.storageFailed = true
		}
		entry.mu.Unlock()
		outcome = "storage_failure"
		writeAPIError(w, http.StatusInternalServerError, "storage_failure", "command was not committed")
		return
	}
	entry.commands[command.ID] = record
	entry.eventsThrough = resultEventsThrough(record.Result, entry.eventsThrough)
	if command.Type == exchange.CommandSubmitQuote {
		entry.latestTurn = &latestTurn{Turn: record.Result.State.Turn, Summary: record.Result.Summary, Coaching: record.Coaching}
	}
	entry.coaching, entry.recap = record.Coaching, record.Recap
	response := exchangeResult(id, record.Result, command, false, entry.scenario, entry.coaching, entry.recap, entry.mode, entry.realTime)
	entry.mu.Unlock()
	writeJSON(w, http.StatusOK, response)
	outcome = "accepted"
}

func (s *exchangeService) handleExchangeEvents(w http.ResponseWriter, r *http.Request, entry *exchangeEntry) {
	after := uint64(0)
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "malformed event query")
		return
	}
	if parsed, present, err := canonicalUintQuery(query, "after"); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "after must be a canonical unsigned integer")
		return
	} else if present {
		after = parsed
	}
	limit := uint64(100)
	if parsed, present, err := canonicalUintQuery(query, "limit"); err != nil || (present && (parsed < 1 || parsed > 200)) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "limit must be a canonical unsigned integer between 1 and 200")
		return
	} else if present {
		limit = parsed
	}
	through, hasThrough, err := canonicalUintQuery(query, "through")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "through must be a canonical unsigned integer")
		return
	}
	entry.mu.Lock()
	if entry.storageFailed {
		entry.mu.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "storage_failure", "game storage is unavailable")
		return
	}
	if !hasThrough {
		through = entry.eventsThrough
	} else if through > entry.eventsThrough {
		entry.mu.Unlock()
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "through cannot exceed the current durable event sequence")
		return
	}
	events := make([]exchange.Event, 0)
	for _, record := range entry.commands {
		for _, event := range record.Result.Events {
			if event.Sequence > after && event.Sequence <= through {
				events = append(events, event)
			}
		}
	}
	entry.mu.Unlock()
	// Command records are in a map; restore the global sequence before returning.
	sort.Slice(events, func(i, j int) bool { return events[i].Sequence < events[j].Sequence })
	hasMore := uint64(len(events)) > limit
	if hasMore {
		events = events[:limit]
	}
	nextAfter := after
	if len(events) > 0 {
		nextAfter = events[len(events)-1].Sequence
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_after": nextAfter, "has_more": hasMore})
}

func (entry *exchangeEntry) executeRealTimeAction(action realtime.Action, elapsed time.Duration) realtime.Outcome {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.storageFailed {
		return realtime.Outcome{Disposition: realtime.DispositionFail, Err: errors.New("game storage is unavailable")}
	}
	if action.Source == realtime.SourceParticipant {
		if action.ID == entry.log.Meta().CreateCommandID {
			return realtime.Outcome{Disposition: realtime.DispositionReject, Err: errors.New("action id is reserved by game creation")}
		}
		if strings.HasPrefix(action.ID, realtime.SystemActionIDPrefix) {
			return realtime.Outcome{Disposition: realtime.DispositionReject, Err: errors.New("action id uses the reserved system namespace")}
		}
	}
	durable, err := realtime.EncodeAction(action)
	if err != nil {
		return realtime.Outcome{Disposition: realtime.DispositionFail, Err: err}
	}
	if prior, ok := entry.commands[action.ID]; ok {
		execution := replayRealTimeRecord(action, durable, prior)
		return realtime.Outcome{Result: execution.Result, Disposition: execution.Disposition, Err: execution.Err}
	}
	previousLifecycle := entry.realTime.Lifecycle
	outcome := realtime.Outcome{Result: exchange.Result{State: entry.engine.State()}, Disposition: realtime.DispositionContinue}
	switch action.Kind {
	case realtime.ActionStartSession:
		payload, ok := action.Payload.(realtime.StartSessionPayload)
		if !ok {
			return realtime.Outcome{Disposition: realtime.DispositionFail, Err: errors.New("start action has an invalid payload")}
		}
		outcome = entry.realtimeExec.Execute(realtime.Action{ID: action.ID, Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: payload.Bid, Ask: payload.Ask}}, elapsed)
	case realtime.ActionCountdownComplete, realtime.ActionPauseSession, realtime.ActionResumeSession:
	default:
		outcome = entry.realtimeExec.Execute(action, elapsed)
	}
	if outcome.Disposition == realtime.DispositionFail {
		return outcome
	}
	lifecycle := previousLifecycle
	switch action.Kind {
	case realtime.ActionStartSession:
		if outcome.Disposition != realtime.DispositionReject {
			lifecycle = game.LifecycleCountdown
		}
	case realtime.ActionCountdownComplete:
		lifecycle = game.LifecycleRunning
	case realtime.ActionPauseSession:
		lifecycle = game.LifecyclePaused
	case realtime.ActionResumeSession:
		lifecycle = game.LifecycleRunning
	default:
		if outcome.Disposition != realtime.DispositionReject {
			if result, ok := outcome.Result.(exchange.Result); ok && result.State.IsOver {
				lifecycle = game.LifecycleCompleted
			}
		}
	}
	record := eventlog.Record{Schema: eventlog.RealTimeSchemaVersion, Version: uint64(len(entry.commands) + 1), Action: &durable, ElapsedNanoseconds: int64(elapsed), Lifecycle: lifecycle}
	if outcome.Disposition == realtime.DispositionReject {
		record.Result = exchange.Result{State: entry.engine.State()}
		record.Rejection = &eventlog.ActionRejection{Code: eventlog.RejectionCodeCommandRejected, Message: outcome.Err.Error()}
	} else {
		result, ok := outcome.Result.(exchange.Result)
		if !ok {
			return realtime.Outcome{Disposition: realtime.DispositionFail, Err: errors.New("exchange executor returned an invalid result")}
		}
		for index := range result.Events {
			result.Events[index].CommandID = action.ID
		}
		record.Result = result
		outcome.Result = result
	}
	appendStarted := time.Now()
	record, err = entry.log.Append(record)
	appendOutcome := "success"
	if err != nil {
		appendOutcome = "failure"
	}
	entry.metrics.observeEventLogAppend(appendOutcome, time.Since(appendStarted))
	if err != nil {
		_ = entry.recover()
		// Recovery may replace the generator while the current sequencer still
		// owns the old one. Fence this process and require a clean reload.
		entry.storageFailed = true
		return realtime.Outcome{Disposition: realtime.DispositionFail, Err: errors.New("real-time action was not committed")}
	}
	entry.commands[action.ID] = record
	entry.eventsThrough = resultEventsThrough(record.Result, entry.eventsThrough)
	entry.realTime.Lifecycle = lifecycle
	entry.replay = realTimeReplaySummary{LastElapsed: elapsed, Lifecycle: lifecycle, DurableRecords: uint64(len(entry.commands)), GeneratorCommitted: entry.generator.Snapshot().Committed}
	return outcome
}

func replayRealTimeRecord(action realtime.Action, durable realtime.DurableAction, record eventlog.Record) realtime.Execution {
	canonical := action
	if record.Action != nil {
		if decoded, err := record.Action.Decode(); err == nil {
			canonical = decoded
		}
	}
	execution := realtime.Execution{Sequence: record.Version, Elapsed: time.Duration(record.ElapsedNanoseconds), Action: canonical, Result: record.Result, Disposition: realtime.DispositionContinue}
	if record.Action == nil || !reflect.DeepEqual(*record.Action, durable) {
		execution.Result = nil
		execution.Disposition = realtime.DispositionReject
		execution.Err = errors.New("action id has a different payload")
	} else if record.Rejection != nil {
		execution.Disposition = realtime.DispositionReject
		execution.Err = errors.New(record.Rejection.Message)
	} else if record.Lifecycle == game.LifecycleCompleted && action.Kind != realtime.ActionStartSession && action.Kind != realtime.ActionCountdownComplete && action.Kind != realtime.ActionPauseSession && action.Kind != realtime.ActionResumeSession {
		execution.Disposition = realtime.DispositionComplete
	}
	return execution
}

func (entry *exchangeEntry) replayRealTimeAction(action realtime.Action) (realtime.Execution, bool) {
	durable, err := realtime.EncodeAction(action)
	if err != nil {
		return realtime.Execution{}, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	record, ok := entry.commands[action.ID]
	if !ok {
		return realtime.Execution{}, false
	}
	return replayRealTimeRecord(action, durable, record), true
}

func (entry *exchangeEntry) ensureRealTimeSequencer() (*realtime.Sequencer, error) {
	entry.sequencerMu.Lock()
	defer entry.sequencerMu.Unlock()

	entry.mu.Lock()
	if entry.mode != game.PlayModeRealTime || entry.realTime == nil || entry.generator == nil || entry.realtimeExec == nil {
		entry.mu.Unlock()
		return nil, errors.New("game is not configured for real-time sequencing")
	}
	if entry.sequencer != nil {
		sequencer := entry.sequencer
		entry.mu.Unlock()
		return sequencer, nil
	}
	checkpoint := realtime.Checkpoint{
		Elapsed:       entry.replay.LastElapsed,
		NextScheduled: entry.replay.GeneratorCommitted,
		Sequence:      entry.replay.DurableRecords,
	}
	switch entry.realTime.Lifecycle {
	case game.LifecyclePreparing:
		checkpoint.Status = realtime.StatusPreparing
	case game.LifecyclePaused:
		checkpoint.Status = realtime.StatusPaused
	case game.LifecycleCompleted:
		checkpoint.Status = realtime.StatusCompleted
	default:
		err := fmt.Errorf("cannot restore active lifecycle %q", entry.realTime.Lifecycle)
		entry.mu.Unlock()
		return nil, err
	}
	factory := entry.newSequencer
	if factory == nil {
		factory = realtime.NewConfigured
	}
	countdown := time.Duration(entry.scenario.RealTime.CountdownMilliseconds) * time.Millisecond
	generator := entry.generator
	entry.mu.Unlock()

	sequencer, err := factory(realtime.Config{
		Schedule:     generator,
		Executor:     entry.executeRealTimeAction,
		ReplayLookup: entry.replayRealTimeAction,
		Countdown:    countdown,
		CountdownAction: func(start realtime.Action) realtime.Action {
			return realtime.Action{ID: realtime.SystemActionIDPrefix + "countdown_complete/" + start.ID, Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem}
		},
		Checkpoint:       checkpoint,
		DurableLifecycle: true,
	})
	if err != nil {
		return nil, err
	}
	entry.mu.Lock()
	entry.sequencer = sequencer
	entry.mu.Unlock()
	return sequencer, nil
}

func (entry *exchangeEntry) startRealTime(ctx context.Context, id string, bid, ask fixed.Price) (realtime.Execution, error) {
	sequencer, err := entry.ensureRealTimeSequencer()
	if err != nil {
		return realtime.Execution{}, err
	}
	return sequencer.StartAction(ctx, realtime.Action{ID: id, Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: bid, Ask: ask}})
}

func (entry *exchangeEntry) updateRealTimeQuote(ctx context.Context, id string, bid, ask fixed.Price) (realtime.Execution, error) {
	sequencer, err := entry.ensureRealTimeSequencer()
	if err != nil {
		return realtime.Execution{}, err
	}
	return sequencer.Submit(ctx, realtime.Action{ID: id, Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: bid, Ask: ask}})
}

func (entry *exchangeEntry) pauseRealTime(ctx context.Context, id string) (realtime.Execution, error) {
	return entry.pauseRealTimeWithSource(ctx, id, realtime.SourceParticipant, realtime.PauseReasonPlayer)
}

func (entry *exchangeEntry) systemPauseRealTime(ctx context.Context, id string, reason realtime.PauseReason) (realtime.Execution, error) {
	return entry.pauseRealTimeWithSource(ctx, id, realtime.SourceSystem, reason)
}

func (entry *exchangeEntry) pauseRealTimeWithSource(ctx context.Context, id string, source realtime.Source, reason realtime.PauseReason) (realtime.Execution, error) {
	sequencer, err := entry.ensureRealTimeSequencer()
	if err != nil {
		return realtime.Execution{}, err
	}
	return sequencer.PauseAction(ctx, realtime.Action{ID: id, Kind: realtime.ActionPauseSession, Source: source, Payload: realtime.PauseSessionPayload{Reason: reason}})
}

func (entry *exchangeEntry) resumeRealTime(ctx context.Context, id string) (realtime.Execution, error) {
	sequencer, err := entry.ensureRealTimeSequencer()
	if err != nil {
		return realtime.Execution{}, err
	}
	return sequencer.ResumeAction(ctx, realtime.Action{ID: id, Kind: realtime.ActionResumeSession, Source: realtime.SourceParticipant})
}

func (s *exchangeService) Shutdown(ctx context.Context) error {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	if s.shutdownDone {
		return s.shutdownErr
	}

	s.mu.Lock()
	entries := make([]*exchangeEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		entries = append(entries, entry)
	}
	s.mu.Unlock()

	var shutdownErr error
	for _, entry := range entries {
		entry.mu.Lock()
		sequencer := entry.sequencer
		entry.mu.Unlock()
		if sequencer == nil {
			continue
		}
		snapshot, err := sequencer.Snapshot(ctx)
		if err != nil {
			if errors.Is(err, realtime.ErrClosed) {
				if _, closeErr := sequencer.Shutdown(ctx); closeErr != nil {
					shutdownErr = errors.Join(shutdownErr, closeErr)
				}
				continue
			}
			shutdownErr = errors.Join(shutdownErr, err)
			continue
		}
		if snapshot.Status == realtime.StatusRunning || snapshot.Status == realtime.StatusCountdown {
			id := fmt.Sprintf("%sshutdown_pause/%d", realtime.SystemActionIDPrefix, snapshot.Sequence+1)
			if _, err := entry.systemPauseRealTime(ctx, id, realtime.PauseReasonShutdown); err != nil {
				latest, snapshotErr := sequencer.Snapshot(ctx)
				if snapshotErr != nil || latest.Status != realtime.StatusCompleted {
					shutdownErr = errors.Join(shutdownErr, err, snapshotErr)
					continue
				}
			}
		}
		if _, err := sequencer.Shutdown(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	if shutdownErr == nil {
		s.shutdownDone = true
	}
	s.shutdownErr = shutdownErr
	return shutdownErr
}

func canonicalUintQuery(query url.Values, name string) (uint64, bool, error) {
	values, present := query[name]
	if !present {
		return 0, false, nil
	}
	if len(values) != 1 {
		return 0, true, errors.New("query parameter must have one value")
	}
	parsed, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil || strconv.FormatUint(parsed, 10) != values[0] {
		return 0, true, errors.New("query parameter must be a canonical unsigned integer")
	}
	return parsed, true, nil
}

func exchangeResult(id string, result exchange.Result, command exchange.Command, replayed bool, snapshot *scenario.Snapshot, coaching *scenario.Coaching, recap *scenario.Recap, mode game.PlayMode, realTime *realTimeState) exchangeResponse {
	return exchangeResponse{GameID: id, Version: result.State.Version, State: result.State, Summary: result.Summary, Events: result.Events, Command: commandResponse{ID: command.ID, Type: command.Type, Replayed: replayed}, Scenario: snapshot, Coaching: coaching, Recap: recap, Mode: mode, RealTime: realTime}
}

func priorResults(commands map[string]eventlog.Record) []exchange.Result {
	records := make([]eventlog.Record, 0, len(commands))
	for _, record := range commands {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Version < records[j].Version })
	results := make([]exchange.Result, len(records))
	for i, record := range records {
		results[i] = record.Result
	}
	return results
}

var (
	errCreateConflict  = errors.New("create command conflicts with existing game")
	errUnknownScenario = errors.New("scenario is not in the server catalog")
	errUnsupportedMode = errors.New("scenario does not support play mode")
)

func (s *exchangeService) createOrLoad(id, createCommandID, scenarioID string) (*exchangeEntry, bool, error) {
	return s.createOrLoadMode(id, createCommandID, scenarioID, game.PlayModeTurnBased)
}

func (s *exchangeService) createOrLoadMode(id, createCommandID, scenarioID string, mode game.PlayMode) (*exchangeEntry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkCreateCommandScope(id, createCommandID, scenarioID, mode); err != nil {
		return nil, false, err
	}
	if entry := s.entries[id]; entry != nil {
		if !matchesCreate(entry.log.Meta(), createCommandID, scenarioID, mode) {
			return nil, false, errCreateConflict
		}
		return entry, false, nil
	}
	if entry, err := s.loadLocked(id); err == nil {
		if !matchesCreate(entry.log.Meta(), createCommandID, scenarioID, mode) {
			return nil, false, errCreateConflict
		}
		return entry, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	definition, ok := s.lookupScenario(scenarioID)
	if !ok {
		return nil, false, errUnknownScenario
	}
	if mode == game.PlayModeRealTime && definition.RealTime == nil {
		return nil, false, errUnsupportedMode
	}
	cfg := definition.Config
	engine, err := exchange.New(cfg)
	if err != nil {
		return nil, false, err
	}
	snapshot := definition.Snapshot()
	var log *eventlog.Log
	if mode == game.PlayModeRealTime {
		seed, seedErr := s.newSeed()
		if seedErr != nil {
			return nil, false, seedErr
		}
		log, err = eventlog.CreateRealTime(s.root, id, localPrincipal, createCommandID, cfg, &snapshot, seed)
	} else {
		log, err = eventlog.Create(s.root, id, localPrincipal, createCommandID, cfg, &snapshot)
	}
	if errors.Is(err, os.ErrExist) {
		entry, loadErr := s.loadLocked(id)
		if loadErr == nil && !matchesCreate(entry.log.Meta(), createCommandID, scenarioID, mode) {
			return nil, false, errCreateConflict
		}
		return entry, false, loadErr
	}
	if err != nil {
		return nil, false, err
	}
	entry := &exchangeEntry{engine: engine, log: log, commands: make(map[string]eventlog.Record), startingEquity: engine.State().Equity, scenario: &snapshot, mode: mode, realTime: realTimeStateFromMeta(log.Meta()), syncLog: s.syncLog, metrics: s.metrics, newSequencer: s.newSequencer}
	if mode == game.PlayModeRealTime {
		entry.generator, entry.realtimeExec, err = newRealtimeRuntime(log.Meta(), engine)
		if err != nil {
			return nil, false, err
		}
	}
	s.entries[id] = entry
	return entry, true, nil
}

func (s *exchangeService) checkCreateCommandScope(id, createCommandID, scenarioID string, mode game.PlayMode) error {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, item := range entries {
		if !item.IsDir() || !validUUID(item.Name()) {
			continue
		}
		meta, err := eventlog.ReadMeta(s.root, item.Name())
		if err != nil {
			return fmt.Errorf("read game metadata %s: %w", item.Name(), err)
		}
		if meta.CreateCommandID != createCommandID {
			continue
		}
		if meta.GameID != id || meta.Scenario == nil || meta.Scenario.ID != scenarioID || meta.EffectiveMode() != mode {
			return errCreateConflict
		}
	}
	return nil
}

func matchesCreate(meta eventlog.Meta, createCommandID, scenarioID string, mode game.PlayMode) bool {
	return meta.CreateCommandID == createCommandID && meta.Scenario != nil && meta.Scenario.ID == scenarioID && meta.EffectiveMode() == mode
}

func (s *exchangeService) load(id string) (*exchangeEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}

func (s *exchangeService) storageHealthy() bool {
	s.mu.Lock()
	entries := make([]*exchangeEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		entries = append(entries, entry)
	}
	s.mu.Unlock()
	for _, entry := range entries {
		entry.mu.Lock()
		failed := entry.storageFailed
		entry.mu.Unlock()
		if failed {
			return false
		}
	}
	return true
}
func (s *exchangeService) loadLocked(id string) (*exchangeEntry, error) {
	if entry := s.entries[id]; entry != nil {
		return entry, nil
	}
	log, records, err := eventlog.Open(s.root, id)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.metrics.observeRecoveryFailure()
		}
		return nil, err
	}
	if err := s.syncLog(log); err != nil {
		s.metrics.observeRecoveryFailure()
		return nil, err
	}
	entry, err := rebuildExchangeEntry(log, records)
	if err != nil {
		s.metrics.observeRecoveryFailure()
		return nil, err
	}
	entry.syncLog = s.syncLog
	entry.metrics = s.metrics
	entry.newSequencer = s.newSequencer
	if entry.mode == game.PlayModeRealTime && (entry.realTime.Lifecycle == game.LifecycleRunning || entry.realTime.Lifecycle == game.LifecycleCountdown) {
		recoveryID := fmt.Sprintf("%srecovery_pause/%d", realtime.SystemActionIDPrefix, entry.replay.DurableRecords)
		outcome := entry.executeRealTimeAction(realtime.Action{ID: recoveryID, Kind: realtime.ActionPauseSession, Source: realtime.SourceSystem, Payload: realtime.PauseSessionPayload{Reason: realtime.PauseReasonRecovery}}, entry.replay.LastElapsed)
		if outcome.Disposition != realtime.DispositionContinue {
			if outcome.Err != nil {
				return nil, fmt.Errorf("persist real-time recovery pause: %w", outcome.Err)
			}
			return nil, errors.New("persist real-time recovery pause: action did not continue")
		}
	}
	s.entries[id] = entry
	return entry, nil
}

func (entry *exchangeEntry) recover() (err error) {
	defer func() {
		if err != nil {
			entry.metrics.observeRecoveryFailure()
		}
	}()
	log := entry.log
	records, err := log.Recover()
	if err != nil {
		return err
	}
	if err := entry.syncLog(log); err != nil {
		return err
	}
	rebuilt, err := rebuildExchangeEntry(log, records)
	if err != nil {
		return err
	}
	entry.engine = rebuilt.engine
	entry.log = rebuilt.log
	entry.commands = rebuilt.commands
	entry.startingEquity = rebuilt.startingEquity
	entry.eventsThrough = rebuilt.eventsThrough
	entry.latestTurn = rebuilt.latestTurn
	entry.scenario = rebuilt.scenario
	entry.coaching = rebuilt.coaching
	entry.recap = rebuilt.recap
	entry.mode = rebuilt.mode
	entry.realTime = rebuilt.realTime
	entry.generator = rebuilt.generator
	entry.realtimeExec = rebuilt.realtimeExec
	entry.replay = rebuilt.replay
	entry.storageFailed = false
	return nil
}

func rebuildExchangeEntry(log *eventlog.Log, records []eventlog.Record) (*exchangeEntry, error) {
	meta := log.Meta()
	mode := meta.EffectiveMode()
	var engine *exchange.Engine
	var commands map[string]eventlog.Record
	var generator *realtime.Generator
	var realtimeExec *realtime.ExchangeExecutor
	var replay realTimeReplaySummary
	var err error
	if mode == game.PlayModeRealTime {
		engine, generator, realtimeExec, commands, replay, err = replayRealTimeRecords(meta, records)
	} else {
		engine, commands, err = replayRecords(meta.Config, records)
	}
	if err != nil {
		return nil, err
	}
	startingEquity, err := checkedStartingEquity(meta.Config)
	if err != nil {
		return nil, err
	}
	entry := &exchangeEntry{engine: engine, log: log, commands: commands, startingEquity: startingEquity, scenario: meta.Scenario, mode: mode, realTime: realTimeStateFromMeta(meta), generator: generator, realtimeExec: realtimeExec, replay: replay}
	if mode == game.PlayModeRealTime {
		entry.realTime.Lifecycle = replay.Lifecycle
	}
	for _, record := range records {
		entry.eventsThrough = resultEventsThrough(record.Result, entry.eventsThrough)
		if record.Command.Type == exchange.CommandSubmitQuote {
			entry.latestTurn = &latestTurn{Turn: record.Result.State.Turn, Summary: record.Result.Summary, Coaching: record.Coaching}
		}
		if record.Coaching != nil {
			entry.coaching = record.Coaching
		}
		if record.Recap != nil {
			entry.recap = record.Recap
		}
	}
	return entry, nil
}

func realTimeStateFromMeta(meta eventlog.Meta) *realTimeState {
	if meta.RealTime == nil {
		return nil
	}
	return &realTimeState{Lifecycle: meta.RealTime.Lifecycle}
}

func checkedStartingEquity(cfg exchange.Config) (fixed.Money, error) {
	startingNotional, err := fixed.Notional(cfg.StartingMark, cfg.StartingPosition)
	if err != nil {
		return 0, err
	}
	return fixed.AddMoney(cfg.StartingCash, startingNotional)
}

func resultEventsThrough(result exchange.Result, through uint64) uint64 {
	for _, event := range result.Events {
		if event.Sequence > through {
			through = event.Sequence
		}
	}
	return through
}

func replayRecords(cfg exchange.Config, records []eventlog.Record) (*exchange.Engine, map[string]eventlog.Record, error) {
	engine, err := exchange.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	commands := make(map[string]eventlog.Record, len(records))
	for _, record := range records {
		result, err := engine.Execute(record.Command)
		if err != nil {
			return nil, nil, fmt.Errorf("replay command %s: %w", record.Command.ID, err)
		}
		for i := range result.Events {
			result.Events[i].CommandID = record.Command.ID
		}
		if !durableResultsEqual(result, record.Result) {
			return nil, nil, fmt.Errorf("replay result mismatch at version %d", record.Version)
		}
		commands[record.Command.ID] = record
	}
	return engine, commands, nil
}

func newRealtimeRuntime(meta eventlog.Meta, engine *exchange.Engine) (*realtime.Generator, *realtime.ExchangeExecutor, error) {
	if meta.RealTime == nil || meta.Scenario == nil || meta.Scenario.RealTime == nil {
		return nil, nil, errors.New("real-time metadata is incomplete")
	}
	config := meta.Scenario.RealTime
	generator, err := realtime.NewGenerator(realtime.GeneratorConfig{
		Version:            config.GeneratorVersion,
		Seed:               meta.RealTime.Seed,
		Duration:           time.Duration(config.DurationMilliseconds) * time.Millisecond,
		CustomerCadence:    realtime.Interval{Minimum: time.Duration(config.CustomerCadence.MinMilliseconds) * time.Millisecond, Maximum: time.Duration(config.CustomerCadence.MaxMilliseconds) * time.Millisecond},
		MarkCadence:        realtime.Interval{Minimum: time.Duration(config.MarkCadence.MinMilliseconds) * time.Millisecond, Maximum: time.Duration(config.MarkCadence.MaxMilliseconds) * time.Millisecond},
		CarryCadence:       time.Duration(config.CarryCadenceMilliseconds) * time.Millisecond,
		MaxOrderQuantity:   meta.Config.MaxOrderQty,
		MaxFlowSlippageBps: meta.Config.MaxFlowSlippageBps,
		MinMoveBps:         meta.Config.MinMoveBps,
		MaxMoveBps:         meta.Config.MaxMoveBps,
		CustomerDomain:     config.CustomerSeedDomain,
		MarkDomain:         config.MarkSeedDomain,
		InformedDomain:     config.InformedSeedDomain,
	})
	if err != nil {
		return nil, nil, err
	}
	executor, err := realtime.NewExchangeExecutor(engine, realtime.ExchangeExecutorConfig{QuoteQuantity: config.QuoteQuantity, CarryPerUnit: config.CarryPerUnit, InformedFlowBps: meta.Config.InformedFlowBps})
	if err != nil {
		return nil, nil, err
	}
	return generator, executor, nil
}

type realTimeReplaySummary struct {
	LastElapsed        time.Duration
	Lifecycle          game.LifecycleState
	DurableRecords     uint64
	GeneratorCommitted uint64
}

func replayRealTimeRecords(meta eventlog.Meta, records []eventlog.Record) (*exchange.Engine, *realtime.Generator, *realtime.ExchangeExecutor, map[string]eventlog.Record, realTimeReplaySummary, error) {
	engine, err := exchange.New(meta.Config)
	if err != nil {
		return nil, nil, nil, nil, realTimeReplaySummary{}, err
	}
	generator, executor, err := newRealtimeRuntime(meta, engine)
	if err != nil {
		return nil, nil, nil, nil, realTimeReplaySummary{}, err
	}
	actions := make(map[string]eventlog.Record, len(records))
	lastElapsed := time.Duration(0)
	lifecycle := game.LifecyclePreparing
	if meta.RealTime != nil {
		lifecycle = meta.RealTime.Lifecycle
	}
	for _, record := range records {
		action, err := record.Action.Decode()
		if err != nil {
			return nil, nil, nil, nil, realTimeReplaySummary{}, fmt.Errorf("decode real-time action at record %d: %w", record.Version, err)
		}
		elapsed := time.Duration(record.ElapsedNanoseconds)
		if elapsed < lastElapsed {
			return nil, nil, nil, nil, realTimeReplaySummary{}, fmt.Errorf("real-time action moved backward at record %d", record.Version)
		}
		var scheduled realtime.ScheduledAction
		generated := action.Kind == realtime.ActionCustomerArrival || action.Kind == realtime.ActionMarkMove || action.Kind == realtime.ActionCarryCharge || action.Kind == realtime.ActionTimeExpired
		if generated {
			var ok bool
			scheduled, ok, err = generator.Next()
			if err != nil || !ok || scheduled.Due != elapsed || !reflect.DeepEqual(scheduled.Action, action) {
				return nil, nil, nil, nil, realTimeReplaySummary{}, fmt.Errorf("system action does not match generator at record %d", record.Version)
			}
		} else if action.Kind == realtime.ActionUpdateQuote || action.Kind == realtime.ActionPauseSession || action.Kind == realtime.ActionResumeSession {
			if next, ok, nextErr := generator.Next(); nextErr != nil || ok && next.Due <= elapsed {
				return nil, nil, nil, nil, realTimeReplaySummary{}, fmt.Errorf("external action bypassed due system work at record %d", record.Version)
			}
		}
		outcome := realtime.Outcome{Result: exchange.Result{State: engine.State()}, Disposition: realtime.DispositionContinue}
		switch action.Kind {
		case realtime.ActionStartSession:
			payload, ok := action.Payload.(realtime.StartSessionPayload)
			if !ok {
				return nil, nil, nil, nil, realTimeReplaySummary{}, fmt.Errorf("start action has invalid payload at record %d", record.Version)
			}
			outcome = executor.Execute(realtime.Action{ID: action.ID, Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: payload.Bid, Ask: payload.Ask}}, elapsed)
		case realtime.ActionCountdownComplete, realtime.ActionPauseSession, realtime.ActionResumeSession:
		default:
			outcome = executor.Execute(action, elapsed)
		}
		if record.Rejection != nil {
			if record.Rejection.Code != eventlog.RejectionCodeCommandRejected || outcome.Disposition != realtime.DispositionReject || outcome.Err == nil || outcome.Err.Error() != record.Rejection.Message || !durableResultsEqual(record.Result, exchange.Result{State: engine.State()}) {
				return nil, nil, nil, nil, realTimeReplaySummary{}, fmt.Errorf("rejected action mismatch at record %d", record.Version)
			}
		} else {
			result, ok := outcome.Result.(exchange.Result)
			if ok {
				for index := range result.Events {
					result.Events[index].CommandID = action.ID
				}
			}
			if !ok || outcome.Err != nil || outcome.Disposition != realtime.DispositionContinue && outcome.Disposition != realtime.DispositionComplete || !durableResultsEqual(result, record.Result) {
				return nil, nil, nil, nil, realTimeReplaySummary{}, fmt.Errorf("accepted action mismatch at record %d", record.Version)
			}
			if generated {
				if err := generator.Commit(scheduled); err != nil {
					return nil, nil, nil, nil, realTimeReplaySummary{}, fmt.Errorf("advance generator at record %d: %w", record.Version, err)
				}
			}
		}
		actions[action.ID] = record
		lastElapsed = elapsed
		lifecycle = record.Lifecycle
	}
	summary := realTimeReplaySummary{LastElapsed: lastElapsed, Lifecycle: lifecycle, DurableRecords: uint64(len(records)), GeneratorCommitted: generator.Snapshot().Committed}
	return engine, generator, executor, actions, summary, nil
}

func durableResultsEqual(left, right exchange.Result) bool {
	// An empty ledger is omitted on the wire and decodes as nil. Both represent
	// a successful command with no journal entries.
	if len(left.Ledger) == 0 {
		left.Ledger = nil
	}
	if len(right.Ledger) == 0 {
		right.Ledger = nil
	}
	return reflect.DeepEqual(left, right)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	data, err := readJSONRequest(w, r)
	if err != nil {
		return err
	}
	return decodeStrictJSON(data, target)
}

func readJSONRequest(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return nil, errors.New("exactly one Content-Type header is required")
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" {
		return nil, errors.New("Content-Type must be application/json")
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 {
		return nil, errors.New("cross-origin requests are not allowed")
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || !validLocalOrigin(origin) || !validLocalHost(r.Host) || !strings.EqualFold(parsed.Host, r.Host) {
			return nil, errors.New("cross-origin requests are not allowed")
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if err := validateUniqueJSONKeys(data); err != nil {
		return nil, err
	}
	return data, nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func validateUniqueJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("request must contain one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("JSON object key must be a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

func validLocalOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	return err == nil &&
		parsed.Scheme == "http" &&
		parsed.User == nil && parsed.Opaque == "" && validLocalAuthority(parsed.Host, parsed.Hostname()) &&
		parsed.Path == "" && parsed.RawPath == "" && parsed.RawQuery == "" && parsed.Fragment == "" &&
		!parsed.ForceQuery && origin == parsed.Scheme+"://"+parsed.Host
}

func validLocalHost(host string) bool {
	parsed, err := url.Parse("http://" + host)
	return err == nil && parsed.User == nil && parsed.Host == host && parsed.Path == "" && validLocalAuthority(parsed.Host, parsed.Hostname())
}

func validLocalAuthority(authority, hostname string) bool {
	if authority == "" || strings.HasSuffix(authority, ":") || !isLoopbackHostname(hostname) {
		return false
	}
	port := (&url.URL{Host: authority}).Port()
	if port == "" {
		return true
	}
	_, err := strconv.ParseUint(port, 10, 16)
	return err == nil
}

func isLoopbackHostname(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	var response apiError
	response.Error.Code, response.Error.Message = code, message
	writeJSON(w, status, response)
}
func validUUID(value string) bool { return uuidPattern.MatchString(strings.ToLower(value)) }
