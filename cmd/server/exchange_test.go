package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testGameID = "11111111-1111-4111-8111-111111111111"
const testCreateID = "22222222-2222-4222-8222-222222222222"
const testQuoteID = "33333333-3333-4333-8333-333333333333"

func v2Server(root string) *httptest.Server {
	svc := newExchangeService(root)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/games", svc.handleGames)
	mux.HandleFunc("/api/v2/games/", svc.handleGame)
	return httptest.NewServer(mux)
}

func createV2Game(t *testing.T, url string) {
	t.Helper()
	body := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","config":{"num_turns":3,"seed":42,"starting_cash":"100000","starting_position":"0","starting_mark":"100","storage_per_unit":"1","initial_margin_bps":5000,"maintenance_margin_bps":2500,"max_position":"1000","max_orders_per_turn":3,"max_order_qty":"10","max_flow_slippage_bps":200,"min_move_bps":-50,"max_move_bps":300}}`
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
	if first.Version != 1 || first.Command.Replayed {
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
	if replayed.Version != 1 || !replayed.Command.Replayed || replayed.State != first.State {
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
	if state.Version != 1 || state.State != first.State {
		t.Fatalf("reloaded=%+v", state)
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
	body := `{"game_id":"` + testGameID + `","command_id":"` + testCreateID + `","config":{"num_turns":3,"seed":42,"starting_cash":"100000","starting_position":"0","starting_mark":"100","storage_per_unit":"1","initial_margin_bps":5000,"maintenance_margin_bps":2500,"max_position":"1000","max_orders_per_turn":3,"max_order_qty":"10","max_flow_slippage_bps":200,"min_move_bps":-50,"max_move_bps":300}}`
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
