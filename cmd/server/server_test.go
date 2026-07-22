package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// resolveIndexPath returns an absolute path to web/static/index.html
// computed from the location of this test source file.
func resolveIndexPath() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "web/static/index.html"
	}
	// this file is cmd/server/server_test.go -> repo root is ../../
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(repoRoot, "web", "static", "index.html")
}

func newTestServer() *httptest.Server {
	s := newServer()
	s.indexPath = resolveIndexPath()
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/game", s.handleCreate)
	mux.HandleFunc("/api/game/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/turn") {
			s.handleTurn(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/quit") {
			s.handleQuit(w, r)
			return
		}
		s.handleState(w, r)
	})
	return httptest.NewServer(mux)
}

func TestCreateGame(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var cr struct {
		ID    string          `json:"game_id"`
		State map[string]any  `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	if cr.ID == "" || cr.State == nil {
		t.Fatalf("bad create resp: %+v", cr)
	}
}

func TestCreateUnlimitedNumTurnsZero(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	body := `{"num_turns":0,"seed":424242}`
	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create status %d", resp.StatusCode)
	}
	var cr struct {
		ID string `json:"game_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	if cr.ID == "" {
		t.Fatal("no id")
	}

	// Submit more turns than default (10). With seed it is deterministic and safe.
	for i := 0; i < 15; i++ {
		turn := `{"bid":99.5,"ask":100.5}`
		r2, err := http.Post(ts.URL+"/api/game/"+cr.ID+"/turn", "application/json", bytes.NewBufferString(turn))
		if err != nil {
			t.Fatal(err)
		}
		if r2.StatusCode != 200 {
			b, readErr := io.ReadAll(r2.Body)
			r2.Body.Close()
			if readErr != nil {
				t.Fatalf("turn %d failed and could not read body: %v", i, readErr)
			}
			t.Fatalf("turn %d failed: %s", i, string(b))
		}
		r2.Body.Close()
	}

	// Verify still not over from turn limit
	r3, err := http.Get(ts.URL + "/api/game/" + cr.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Body.Close()
	var st struct {
		State struct {
			Turn   int  `json:"turn"`
			IsOver bool `json:"is_over"`
			Reason string `json:"reason"`
		} `json:"state"`
	}
	if err := json.NewDecoder(r3.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.State.IsOver {
		t.Fatalf("game over too early (reason=%s, turn=%d) — num_turns:0 was not respected", st.State.Reason, st.State.Turn)
	}
	if st.State.Turn < 15 {
		t.Fatalf("expected at least 15 turns, got %d", st.State.Turn)
	}
}

func TestTurnAndGetState(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	// create
	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(`{"seed":123}`))
	if err != nil {
		t.Fatal(err)
	}
	var cr struct{ ID string `json:"game_id"` }
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// turn
	turnBody := `{"bid":99,"ask":101}`
	r2, err := http.Post(ts.URL+"/api/game/"+cr.ID+"/turn", "application/json", bytes.NewBufferString(turnBody))
	if err != nil {
		t.Fatal(err)
	}
	if r2.StatusCode != 200 {
		t.Fatalf("turn status %d", r2.StatusCode)
	}
	var tr struct {
		State   map[string]any `json:"state"`
		Events  []any          `json:"events"`
		Summary map[string]any `json:"summary"`
	}
	if err := json.NewDecoder(r2.Body).Decode(&tr); err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if tr.State == nil || tr.Summary == nil {
		t.Fatalf("bad TurnResult: %+v", tr)
	}

	// get state
	r3, err := http.Get(ts.URL + "/api/game/" + cr.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Body.Close()
	if r3.StatusCode != 200 {
		t.Fatalf("state status %d", r3.StatusCode)
	}
	var gs struct {
		State map[string]any `json:"state"`
	}
	if err := json.NewDecoder(r3.Body).Decode(&gs); err != nil {
		t.Fatal(err)
	}
	if gs.State == nil {
		t.Fatal("no state from GET")
	}
}

func TestIndexServesStatic(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	r, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("index status %d", r.StatusCode)
	}
	b, _ := io.ReadAll(r.Body)
	s := string(b)
	if !strings.Contains(s, "market-maker") {
		t.Fatalf("index.html did not contain expected UI string; got prefix: %.200s", s)
	}
}

func TestCreateWithOverridesAndStartingEquity(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	body := `{
		"starting_cash": 12345.67,
		"starting_inventory": 5.0,
		"starting_price": 200.0,
		"storage_cost_per_unit": 0.5,
		"vol_min_pct": -1.0,
		"vol_max_pct": 2.5,
		"num_turns": 7,
		"seed": 999
	}`
	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var cr struct {
		ID             string          `json:"game_id"`
		State          map[string]any  `json:"state"`
		StartingEquity float64         `json:"starting_equity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	if cr.ID == "" {
		t.Fatal("no game_id")
	}
	if cr.StartingEquity != 12345.67+5.0*200.0 {
		t.Fatalf("starting_equity mismatch: got %v want %v", cr.StartingEquity, 12345.67+5.0*200.0)
	}
	cash := cr.State["cash"].(float64)
	inv := cr.State["inventory"].(float64)
	price := cr.State["last_price"].(float64)
	if cash != 12345.67 || inv != 5.0 || price != 200.0 {
		t.Fatalf("state mismatch: cash=%v inv=%v price=%v", cash, inv, price)
	}
}
func TestConcurrentTurnsSameGame(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	body := `{"num_turns":0,"seed":424242}`
	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	var cr struct{ ID string `json:"game_id"` }
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()

	// Fire many turns concurrently on same game
	const N = 20
	done := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, err := http.Post(ts.URL+"/api/game/"+cr.ID+"/turn", "application/json", bytes.NewBufferString(`{"bid":99.5,"ask":100.5}`))
			done <- err
		}()
	}
	for i := 0; i < N; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent turn err: %v", err)
		}
	}
	// Just ensure it didn't panic and game has advanced
	r, err := http.Get(ts.URL + "/api/game/" + cr.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var st struct{ State struct{ Turn int `json:"turn"` } `json:"state"` }
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.State.Turn == 0 {
		t.Error("expected turns to have advanced under concurrency")
	}
}

// --- Bad path / not found tests ---

// TestPathExtractionForBadIDs verifies that when extractID produces an empty ID
// (malformed route with no game ID segment), the handlers return 400 "bad path".
// We invoke the handlers directly with paths that cause empty ID.
func TestPathExtractionForBadIDs(t *testing.T) {
	s := newServer()
	s.indexPath = resolveIndexPath()

	// Turn paths that after prefix + suffix stripping leave no ID
	turnCases := []string{
		"/api/game//turn", // rest after prefix="/turn" -> after suffix trim = "" -> id=""
	}

	for _, p := range turnCases {
		req := httptest.NewRequest("POST", p, bytes.NewBufferString(`{"bid":99,"ask":101}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.handleTurn(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("handleTurn %s got %d, want 400", p, rr.Code)
		}
	}

	// State paths with no ID segment
	stateCases := []string{
		"/api/game/",
		"/api/game//",
	}

	for _, p := range stateCases {
		req := httptest.NewRequest("GET", p, nil)
		rr := httptest.NewRecorder()
		s.handleState(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("handleState %s got %d, want 400", p, rr.Code)
		}
	}
}

// TestWellFormedPathButMissingGameIs404 confirms that a path with a plausible ID
// that simply doesn't exist returns 404, not 400. This is the "game not found" case.
func TestWellFormedPathButMissingGameIs404(t *testing.T) {
	s := newServer()
	s.indexPath = resolveIndexPath()

	// Direct handler call with a well-formed ID that isn't registered
	req := httptest.NewRequest("POST", "/api/game/g-missing/turn", bytes.NewBufferString(`{"bid":99,"ask":101}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.handleTurn(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("missing game turn got %d, want 404", rr.Code)
	}

	req2 := httptest.NewRequest("GET", "/api/game/g-missing", nil)
	rr2 := httptest.NewRecorder()
	s.handleState(rr2, req2)
	if rr2.Code != http.StatusNotFound {
		t.Errorf("missing game state got %d, want 404", rr2.Code)
	}
}

func TestGameNotFound(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	// state on missing
	r, err := http.Get(ts.URL + "/api/game/g-nope")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("state notfound got %d, want 404", r.StatusCode)
	}

	// turn on missing
	req, err := http.NewRequest("POST", ts.URL+"/api/game/g-nope/turn", bytes.NewBufferString(`{"bid":99,"ask":101}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	r, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("turn notfound got %d, want 404", r.StatusCode)
	}
}

// --- Invalid JSON / bad request body on create ---

func TestCreateBadJsonReturns400(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(`{not json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json create got %d, want 400", resp.StatusCode)
	}
}

// --- Game over behavior ---

func TestTurnAfterGameOverReturns404(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	// Start a 1-turn game
	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(`{"num_turns":1,"seed":42}`))
	if err != nil {
		t.Fatal(err)
	}
	var cr struct{ ID string `json:"game_id"` }
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// First turn should complete the game and cause immediate removal from map
	r2, err := http.Post(ts.URL+"/api/game/"+cr.ID+"/turn", "application/json", bytes.NewBufferString(`{"bid":99,"ask":101}`))
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("first turn status %d", r2.StatusCode)
	}

	// Subsequent turn on ended game: 404 not found (game was cleaned up on resolution)
	r3, err := http.Post(ts.URL+"/api/game/"+cr.ID+"/turn", "application/json", bytes.NewBufferString(`{"bid":99,"ask":101}`))
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != http.StatusNotFound {
		t.Errorf("turn after over got %d, want 404", r3.StatusCode)
	}

	// GET state after end also 404s (final state was returned in the last TurnResult)
	gr, err := http.Get(ts.URL + "/api/game/" + cr.ID)
	if err != nil {
		t.Fatal(err)
	}
	gr.Body.Close()
	if gr.StatusCode != http.StatusNotFound {
		t.Errorf("state after over got %d, want 404", gr.StatusCode)
	}
}

func TestQuitEndsGameAndRemovesIt(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	// create unlimited game
	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(`{"num_turns":0}`))
	if err != nil {
		t.Fatal(err)
	}
	var cr struct{ ID string `json:"game_id"` }
	json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()

	// do one turn
	http.Post(ts.URL+"/api/game/"+cr.ID+"/turn", "application/json", bytes.NewBufferString(`{"bid":99,"ask":101}`))

	// quit
	qr, err := http.Post(ts.URL+"/api/game/"+cr.ID+"/quit", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if qr.StatusCode != 200 {
		t.Fatalf("quit status %d", qr.StatusCode)
	}
	qr.Body.Close()

	// further action should 404
	tr, _ := http.Post(ts.URL+"/api/game/"+cr.ID+"/turn", "application/json", bytes.NewBufferString(`{"bid":99,"ask":101}`))
	tr.Body.Close()
	if tr.StatusCode != http.StatusNotFound {
		t.Errorf("turn after quit got %d, want 404", tr.StatusCode)
	}
}

// --- Create response contract: always includes starting_equity ---

func TestCreateAlwaysReturnsStartingEquity(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	cases := []struct {
		body string
		want float64
	}{
		{`{}`, 100000},                            // defaults
		{`{"starting_cash":50000}`, 50000},        // cash only
		{`{"starting_inventory":10,"starting_price":50}`, 100000 + 10*50},
		{`{"num_turns":0}`, 100000},
	}

	for _, c := range cases {
		resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(c.body))
		if err != nil {
			t.Fatal(err)
		}
		var cr struct {
			ID             string  `json:"game_id"`
			StartingEquity float64 `json:"starting_equity"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()

		if cr.ID == "" {
			t.Error("missing game_id")
		}
		if cr.StartingEquity != c.want {
			t.Errorf("body %s: starting_equity=%v want %v", c.body, cr.StartingEquity, c.want)
		}
	}
}

// --- Edge config values: document current behavior (negative cash, zero vol, etc) ---
// These tests capture the *current* server/engine behavior without asserting it is "correct".
// They exist so future changes are deliberate.

func TestCreateWithNegativeCashIsAccepted(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	body := `{"starting_cash":-1000,"seed":1}`
	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Currently the server accepts it (engine will likely bankrupt quickly).
	// We only assert that we get a 200 + a starting_equity that reflects the negative.
	var cr struct {
		ID             string  `json:"game_id"`
		StartingEquity float64 `json:"starting_equity"`
		State          struct {
			Cash float64 `json:"cash"`
		} `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	if cr.ID == "" {
		t.Fatal("no game_id")
	}
	if cr.StartingEquity != -1000 {
		t.Errorf("starting_equity = %v, want -1000", cr.StartingEquity)
	}
	if cr.State.Cash != -1000 {
		t.Errorf("state.cash = %v, want -1000", cr.State.Cash)
	}
}

func TestCreateWithZeroVolRangeIsAccepted(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	body := `{"vol_min_pct":0,"vol_max_pct":0,"seed":123}`
	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("zero vol create got %d", resp.StatusCode)
	}

	var cr struct {
		ID             string `json:"game_id"`
		StartingEquity float64 `json:"starting_equity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	if cr.ID == "" {
		t.Fatal("no id")
	}
	// With zero vol and defaults, starting equity should be 100000
	if cr.StartingEquity != 100000 {
		t.Errorf("starting_equity=%v, want 100000", cr.StartingEquity)
	}
}

// --- Static serving for /index.html explicitly ---

func TestIndexHtmlPathServesStatic(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	r, err := http.Get(ts.URL + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("index.html status %d", r.StatusCode)
	}
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), "market-maker") {
		t.Error("expected market-maker content from /index.html")
	}
}

// --- MaxBytesReader on create (large body) ---

func TestCreateRejectsLargeBody(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	// Create a body larger than 1MB (the limit in handleCreate)
	large := make([]byte, 1<<20+100)
	for i := range large {
		large[i] = 'a'
	}
	// Wrap as fake JSON object to pass content-type, but oversized
	body := append([]byte(`{"junk":"`), large...)
	body = append(body, '"', '}')

	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// MaxBytesReader + decode failure should result in 400 or 413.
	// Go's http.MaxBytesReader writes 413 to the ResponseWriter on exceed when used this way.
	// We accept either as the signal that the limit was enforced.
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body create got %d, want 400 or 413", resp.StatusCode)
	}
}

// --- Bad JSON body on turn ---

func TestTurnBadJsonReturns400(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(`{"seed":11}`))
	if err != nil {
		t.Fatal(err)
	}
	var cr struct{ ID string `json:"game_id"` }
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()

	r2, err := http.Post(ts.URL+"/api/game/"+cr.ID+"/turn", "application/json", bytes.NewBufferString(`{bad json`))
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json turn got %d, want 400", r2.StatusCode)
	}
}

// --- Invalid bid/ask on turn returns 400 ---

func TestTurnInvalidBidAskReturns400(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(`{"seed":7}`))
	if err != nil {
		t.Fatal(err)
	}
	var cr struct{ ID string `json:"game_id"` }
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()

	cases := []string{
		`{"bid":0,"ask":101}`,
		`{"bid":99,"ask":0}`,
		`{"bid":101,"ask":99}`,
		`{"bid":-1,"ask":100}`,
	}
	for _, b := range cases {
		r2, err := http.Post(ts.URL+"/api/game/"+cr.ID+"/turn", "application/json", bytes.NewBufferString(b))
		if err != nil {
			t.Fatal(err)
		}
		r2.Body.Close()
		if r2.StatusCode != http.StatusBadRequest {
			t.Errorf("turn %s got %d, want 400", b, r2.StatusCode)
		}
	}
}

// --- Unbounded games storage / leak prevention (high priority) ---

// TestFinishedGamesAreEvictedUnderCap demonstrates that the server does not
// leak memory by keeping every finished game forever. Games are cleaned up
// immediately on resolution (in handleTurn), so the cap limits only active
// (not-yet-over) games.
func TestFinishedGamesAreEvictedUnderCap(t *testing.T) {
	s := newServer()
	origMax := s.maxGames
	s.maxGames = 3 // small cap for this test
	defer func() { s.maxGames = origMax }()

	const total = 10
	for i := 0; i < total; i++ {
		// Create a 1-turn game so it finishes on first submit.
		createBody := fmt.Sprintf(`{"num_turns":1,"seed":%d}`, 10000+i)
		req := httptest.NewRequest("POST", "/api/game", bytes.NewBufferString(createBody))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.handleCreate(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("create %d failed: %d %s", i, rr.Code, rr.Body.String())
		}
		var cr struct {
			ID string `json:"game_id"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&cr); err != nil {
			t.Fatal(err)
		}

		// Finish the game immediately.
		turnReq := httptest.NewRequest("POST", "/api/game/"+cr.ID+"/turn", bytes.NewBufferString(`{"bid":99,"ask":101}`))
		turnReq.Header.Set("Content-Type", "application/json")
		trr := httptest.NewRecorder()
		s.handleTurn(trr, turnReq)
		if trr.Code != http.StatusOK {
			t.Fatalf("turn %d failed: %d", i, trr.Code)
		}
	}

	if len(s.games) > s.maxGames {
		t.Errorf("after creating and finishing %d games with cap=%d, map still holds %d entries (leak)", total, s.maxGames, len(s.games))
	}
}

// --- Method enforcement tests (write first, then ensure impl) ---

func TestCreateRejectsNonPost(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	for _, method := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		req, err := http.NewRequest(method, ts.URL+"/api/game", bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/game got %d, want 405", method, resp.StatusCode)
		}
	}
}

func TestTurnRejectsNonPost(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	// create a game first
	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(`{"seed":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var cr struct{ ID string `json:"game_id"` }
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()

	url := ts.URL + "/api/game/" + cr.ID + "/turn"
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		req, err := http.NewRequest(method, url, bytes.NewBufferString(`{"bid":99,"ask":101}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s turn got %d, want 405", method, r.StatusCode)
		}
	}
}

func TestStateRejectsNonGet(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/game", "application/json", bytes.NewBufferString(`{"seed":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var cr struct{ ID string `json:"game_id"` }
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()

	url := ts.URL + "/api/game/" + cr.ID
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s state got %d, want 405", method, r.StatusCode)
		}
	}
}
