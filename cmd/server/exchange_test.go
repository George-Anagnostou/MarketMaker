package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
	if len(body.Scenarios) != 3 || body.Scenarios[0].ID == "" || body.Scenarios[0].Turns == 0 || len(body.Scenarios[0].Tutorial) != 4 || body.Scenarios[0].Reflection == "" || body.Scenarios[1].ID != "inventory-pressure-v1" || len(body.Scenarios[1].Tutorial) != 5 || body.Scenarios[1].Reflection == "" || body.Scenarios[2].ID != "volatility-shock-v1" || len(body.Scenarios[2].Tutorial) != 5 || body.Scenarios[2].Reflection == "" {
		t.Fatalf("scenarios=%+v", body.Scenarios)
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
	if terminal.Recap == nil || terminal.Coaching == nil || !terminal.State.IsOver {
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
	bad := `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":1,"bid":"99","ask":"101","unexpected":true}`
	resp, err := http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", bytes.NewBufferString(bad))
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
