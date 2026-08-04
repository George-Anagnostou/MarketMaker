package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	if _, err := separateLog.Append(firstRecord); err != nil {
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
