package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const testGameID = "11111111-1111-4111-8111-111111111111"
const testCreateID = "22222222-2222-4222-8222-222222222222"
const testQuoteID = "33333333-3333-4333-8333-333333333333"
const testQuitID = "44444444-4444-4444-8444-444444444444"

func v2Server(root string) *httptest.Server {
	svc := newExchangeService(root)
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
			ID    string `json:"id"`
			Turns int    `json:"turns"`
		} `json:"scenarios"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Scenarios) != 3 || body.Scenarios[0].ID == "" || body.Scenarios[0].Turns == 0 {
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
	if first.Version != 1 || first.Command.Replayed || first.Scenario == nil || first.Coaching == nil {
		t.Fatalf("first=%+v", first)
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
	quote := `{"id":"` + testQuoteID + `","type":"submit_quote","expected_version":0,"bid":"99.50","ask":"100.50"}`
	resp, err := http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", bytes.NewBufferString(quote))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	quit := `{"id":"` + testQuitID + `","type":"quit","expected_version":1}`
	resp, err = http.Post(ts.URL+"/api/v2/games/"+testGameID+"/commands", "application/json", bytes.NewBufferString(quit))
	if err != nil {
		t.Fatal(err)
	}
	var terminal exchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&terminal); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if terminal.Recap == nil || terminal.Coaching == nil || !terminal.State.IsOver {
		t.Fatalf("terminal=%+v", terminal)
	}
	ts.Close()

	reloaded := v2Server(root)
	defer reloaded.Close()
	resp, err = http.Get(reloaded.URL + "/api/v2/games/" + testGameID)
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
