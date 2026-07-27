package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"market-maker/internal/eventlog"
	"market-maker/internal/exchange"
	"market-maker/internal/scenario"
)

const localPrincipal = "local"

var uuidPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[1-5][a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)

type exchangeService struct {
	mu             sync.Mutex
	root           string
	entries        map[string]*exchangeEntry
	lookupScenario func(string) (scenario.Definition, bool)
}

type exchangeEntry struct {
	mu       sync.Mutex
	engine   *exchange.Engine
	log      *eventlog.Log
	commands map[string]eventlog.Record
	scenario *scenario.Snapshot
	coaching *scenario.Coaching
	recap    *scenario.Recap
}

func newExchangeService(root string) *exchangeService {
	return &exchangeService{root: root, entries: make(map[string]*exchangeEntry), lookupScenario: scenario.Get}
}

type createExchangeRequest struct {
	GameID     string `json:"game_id"`
	CommandID  string `json:"command_id"`
	ScenarioID string `json:"scenario_id"`
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
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *exchangeService) handleGames(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
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
	req.GameID, req.CommandID = strings.ToLower(req.GameID), strings.ToLower(req.CommandID)
	entry, created, err := s.createOrLoad(req.GameID, req.CommandID, req.ScenarioID)
	if errors.Is(err, errUnknownScenario) {
		writeAPIError(w, http.StatusBadRequest, "unknown_scenario", "scenario_id is not in the server catalog")
		return
	}
	if errors.Is(err, errCreateConflict) {
		writeAPIError(w, http.StatusConflict, "idempotency_key_reused", "game id already exists with a different create command or scenario")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "storage_failure", "could not create game")
		return
	}
	entry.mu.Lock()
	state := entry.engine.State()
	entry.mu.Unlock()
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, exchangeResponse{GameID: req.GameID, Version: state.Version, State: state, Command: commandResponse{ID: req.CommandID, Type: "create_game", Replayed: !created}, Scenario: entry.scenario, Coaching: entry.coaching, Recap: entry.recap})
}

func (s *exchangeService) handleScenarios(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
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
	if len(parts) == 1 && r.Method == http.MethodGet {
		s.handleExchangeState(w, parts[0], entry)
		return
	}
	if len(parts) == 2 && parts[1] == "commands" && r.Method == http.MethodPost {
		s.handleExchangeCommand(w, r, parts[0], entry)
		return
	}
	if len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet {
		s.handleExchangeEvents(w, r, entry)
		return
	}
	writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "unsupported game endpoint")
}

func (s *exchangeService) handleExchangeState(w http.ResponseWriter, id string, entry *exchangeEntry) {
	entry.mu.Lock()
	state := entry.engine.State()
	entry.mu.Unlock()
	writeJSON(w, http.StatusOK, exchangeResponse{GameID: id, Version: state.Version, State: state, Scenario: entry.scenario, Coaching: entry.coaching, Recap: entry.recap})
}

func (s *exchangeService) handleExchangeCommand(w http.ResponseWriter, r *http.Request, id string, entry *exchangeEntry) {
	var command exchange.Command
	if err := decodeJSON(w, r, &command); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !validUUID(command.ID) {
		writeAPIError(w, http.StatusBadRequest, "invalid_id", "command id must be a UUID")
		return
	}
	command.ID = strings.ToLower(command.ID)
	if command.Type != exchange.CommandSubmitQuote && command.Type != exchange.CommandQuit {
		writeAPIError(w, http.StatusForbidden, "venue_command_unavailable", "account commands require authenticated venue access")
		return
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if prior, ok := entry.commands[command.ID]; ok {
		if prior.Command != command {
			writeAPIError(w, http.StatusConflict, "idempotency_key_reused", "command id has a different payload")
			return
		}
		response := exchangeResult(id, prior.Result, command, true, entry)
		response.Coaching, response.Recap = prior.Coaching, prior.Recap
		writeJSON(w, http.StatusOK, response)
		return
	}
	if command.ExpectedVersion != entry.engine.State().Version {
		writeAPIError(w, http.StatusConflict, "version_conflict", "expected_version does not match current game version")
		return
	}
	before := entry.engine.State()
	result, err := entry.engine.Execute(command)
	if err != nil {
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
			if rebuilt, commands, rebuildErr := replay(entry.log); rebuildErr == nil {
				entry.engine, entry.commands = rebuilt, commands
			}
			writeAPIError(w, http.StatusInternalServerError, "recap_failure", "could not build session recap")
			return
		}
		record.Recap = recap
	}
	if err := entry.log.Append(record); err != nil {
		// Restore authoritative state from the last committed log before exposing an error.
		if rebuilt, commands, rebuildErr := replay(entry.log); rebuildErr == nil {
			entry.engine, entry.commands = rebuilt, commands
		}
		writeAPIError(w, http.StatusInternalServerError, "storage_failure", "command was not committed")
		return
	}
	entry.commands[command.ID] = record
	entry.coaching, entry.recap = record.Coaching, record.Recap
	writeJSON(w, http.StatusOK, exchangeResult(id, result, command, false, entry))
}

func (s *exchangeService) handleExchangeEvents(w http.ResponseWriter, r *http.Request, entry *exchangeEntry) {
	after := uint64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		_, err = fmt.Sscan(raw, &after)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request", "after must be an integer")
			return
		}
	}
	entry.mu.Lock()
	events := make([]exchange.Event, 0)
	for _, record := range entry.commands {
		for _, event := range record.Result.Events {
			if event.Sequence > after {
				events = append(events, event)
			}
		}
	}
	entry.mu.Unlock()
	// Command records are in a map; restore the global sequence before returning.
	sort.Slice(events, func(i, j int) bool { return events[i].Sequence < events[j].Sequence })
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeAPIError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	nextAfter := after
	if len(events) > 0 {
		nextAfter = events[len(events)-1].Sequence
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_after": nextAfter, "has_more": hasMore})
}

func exchangeResult(id string, result exchange.Result, command exchange.Command, replayed bool, entry *exchangeEntry) exchangeResponse {
	return exchangeResponse{GameID: id, Version: result.State.Version, State: result.State, Summary: result.Summary, Events: result.Events, Command: commandResponse{ID: command.ID, Type: command.Type, Replayed: replayed}, Scenario: entry.scenario, Coaching: entry.coaching, Recap: entry.recap}
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
)

func (s *exchangeService) createOrLoad(id, createCommandID, scenarioID string) (*exchangeEntry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.entries[id]; entry != nil {
		if !matchesCreate(entry.log.Meta(), createCommandID, scenarioID) {
			return nil, false, errCreateConflict
		}
		return entry, false, nil
	}
	if entry, err := s.loadLocked(id); err == nil {
		if !matchesCreate(entry.log.Meta(), createCommandID, scenarioID) {
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
	cfg := definition.Config
	engine, err := exchange.New(cfg)
	if err != nil {
		return nil, false, err
	}
	snapshot := definition.Snapshot()
	log, err := eventlog.Create(s.root, id, localPrincipal, createCommandID, cfg, &snapshot)
	if errors.Is(err, os.ErrExist) {
		entry, loadErr := s.loadLocked(id)
		if loadErr == nil && !matchesCreate(entry.log.Meta(), createCommandID, scenarioID) {
			return nil, false, errCreateConflict
		}
		return entry, false, loadErr
	}
	if err != nil {
		return nil, false, err
	}
	entry := &exchangeEntry{engine: engine, log: log, commands: make(map[string]eventlog.Record), scenario: &snapshot}
	s.entries[id] = entry
	return entry, true, nil
}

func matchesCreate(meta eventlog.Meta, createCommandID, scenarioID string) bool {
	return meta.CreateCommandID == createCommandID && meta.Scenario != nil && meta.Scenario.ID == scenarioID
}

func (s *exchangeService) load(id string) (*exchangeEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}
func (s *exchangeService) loadLocked(id string) (*exchangeEntry, error) {
	if entry := s.entries[id]; entry != nil {
		return entry, nil
	}
	log, records, err := eventlog.Open(s.root, id)
	if err != nil {
		return nil, err
	}
	engine, commands, err := replayRecords(log.Meta().Config, records)
	if err != nil {
		return nil, err
	}
	entry := &exchangeEntry{engine: engine, log: log, commands: commands, scenario: log.Meta().Scenario}
	for _, record := range records {
		if record.Coaching != nil {
			entry.coaching = record.Coaching
		}
		if record.Recap != nil {
			entry.recap = record.Recap
		}
	}
	s.entries[id] = entry
	return entry, nil
}

func replay(log *eventlog.Log) (*exchange.Engine, map[string]eventlog.Record, error) {
	_, records, err := eventlog.Open(filepath.Dir(log.Path()), filepath.Base(log.Path()))
	if err != nil {
		return nil, nil, err
	}
	return replayRecords(log.Meta().Config, records)
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
		if result.State != record.Result.State || result.Summary != record.Result.Summary || !reflect.DeepEqual(result.Events, record.Result.Events) || !reflect.DeepEqual(result.Ledger, record.Result.Ledger) {
			return nil, nil, fmt.Errorf("replay result mismatch at version %d", record.Version)
		}
		commands[record.Command.ID] = record
	}
	return engine, commands, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host != r.Host {
			return errors.New("cross-origin requests are not allowed")
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	var response apiError
	response.Error.Code, response.Error.Message = code, message
	writeJSON(w, status, response)
}
func validUUID(value string) bool { return uuidPattern.MatchString(strings.ToLower(value)) }
