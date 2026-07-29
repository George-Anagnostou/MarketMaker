package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"market-maker/internal/eventlog"
	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
	"market-maker/internal/scenario"
)

const testGameID = "11111111-1111-4111-8111-111111111111"
const testCreateID = "22222222-2222-4222-8222-222222222222"
const testQuoteID = "33333333-3333-4333-8333-333333333333"
const testQuitID = "44444444-4444-4444-8444-444444444444"

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
			ID         string                  `json:"id"`
			Turns      int                     `json:"turns"`
			Tutorial   []scenario.TutorialStep `json:"tutorial"`
			Reflection string                  `json:"reflection"`
		} `json:"scenarios"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Scenarios) != 4 || body.Scenarios[0].ID == "" || body.Scenarios[0].Turns == 0 || len(body.Scenarios[0].Tutorial) != 4 || body.Scenarios[0].Reflection == "" || body.Scenarios[1].ID != "inventory-pressure-v1" || len(body.Scenarios[1].Tutorial) != 5 || body.Scenarios[1].Reflection == "" || body.Scenarios[2].ID != "volatility-shock-v1" || len(body.Scenarios[2].Tutorial) != 5 || body.Scenarios[2].Reflection == "" || body.Scenarios[3].ID != "volatility-shock-v2" || body.Scenarios[3].Turns != 8 || len(body.Scenarios[3].Tutorial) != 5 || body.Scenarios[3].Reflection == "" {
		t.Fatalf("scenarios=%+v", body.Scenarios)
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
	if metaLog.Meta().Schema != eventlog.SchemaVersion || eventlog.SchemaVersion != 3 || len(records) != 2 || metaLog.Meta().Config.SimulationVersion != exchange.SimulationVersionAdverseSelection {
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

func TestRecoveryReplacesStaleLoggerBeforeNextAppend(t *testing.T) {
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
	if err := separateLog.Append(firstRecord); err != nil {
		t.Fatal(err)
	}

	_, persistedRecords, err := eventlog.Open(root, testGameID)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the old recovery path, which rebuilt state but retained the stale log.
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
	if entry.log == staleLog || entry.engine.State().Version != 1 || entry.commands[firstCommand.ID].Version != 1 || !reflect.DeepEqual(entry.coaching, firstRecord.Coaching) || entry.latestTurn == nil || !reflect.DeepEqual(entry.latestTurn.Summary, firstRecord.Result.Summary) {
		t.Fatalf("recovered entry did not replace all persisted state: %+v", entry)
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
