package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"market-maker/internal/game"
	"market-maker/internal/types"
)

// server holds in-memory games for the single-player web experience.
// This is Phase 3/4 style: same engine, driven over HTTP.
// NOTE: Each gameEntry holds its own mutex so that concurrent requests for
// different games do not block each other, and access to a single Game is
// serialized to protect the non-thread-safe engine state.
type gameEntry struct {
	g  *game.Game
	mu sync.Mutex
}

type server struct {
	mu    sync.Mutex
	games map[string]*gameEntry
	next  int // simple ID counter

	// indexPath is the filesystem path to the static UI.
	// Defaults to "web/static/index.html" (run the binary from repo root).
	// Tests override this with an absolute path computed from the test source.
	indexPath string

	// maxGames if > 0 is a hard cap on the number of *active* (not-yet-over) games.
	// Ended games (bankrupt, turns, or explicit quit via /quit) are removed immediately.
	// 0 means no enforced cap (legacy/unlimited).
	maxGames int
}

func newServer() *server {
	return &server{
		games:     make(map[string]*gameEntry),
		next:      1,
		indexPath: "web/static/index.html",
		maxGames:  1000, // reasonable default to bound memory; override in tests if needed
	}
}

type createReq struct {
	NumTurns  *int     `json:"num_turns"`
	Seed      *int64   `json:"seed"`
	Cash      *float64 `json:"starting_cash"`
	Inventory *float64 `json:"starting_inventory"`
	Price     *float64 `json:"starting_price"`
	Storage   *float64 `json:"storage_cost_per_unit"`
	VolMin    *float64 `json:"vol_min_pct"`
	VolMax    *float64 `json:"vol_max_pct"`
}

type createResp struct {
	ID             string          `json:"game_id"`
	State          types.GameState `json:"state"`
	StartingEquity float64         `json:"starting_equity"`
}

type turnReq struct {
	Bid float64 `json:"bid"`
	Ask float64 `json:"ask"`
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB safety
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	cfg := game.DefaultConfig()
	if req.NumTurns != nil {
		cfg.NumTurns = *req.NumTurns
	}
	if req.Seed != nil {
		cfg.Seed = *req.Seed
	}
	if req.Cash != nil {
		cfg.StartingCash = *req.Cash
	}
	if req.Inventory != nil {
		cfg.StartingInventory = *req.Inventory
	}
	if req.Price != nil {
		cfg.StartingPrice = *req.Price
	}
	if req.Storage != nil {
		cfg.StorageCostPerUnit = *req.Storage
	}
	if req.VolMin != nil {
		cfg.MinPriceMovePct = *req.VolMin / 100
	}
	if req.VolMax != nil {
		cfg.MaxPriceMovePct = *req.VolMax / 100
	}

	g := game.NewGame(cfg)
	startingEquity := types.StartingEquity(cfg)

	s.mu.Lock()
	if s.maxGames > 0 && len(s.games) >= s.maxGames {
		s.mu.Unlock()
		http.Error(w, "too many games", http.StatusTooManyRequests)
		return
	}
	id := fmt.Sprintf("g-%d", s.next)
	s.next++
	s.games[id] = &gameEntry{g: g}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(createResp{ID: id, State: g.State(), StartingEquity: startingEquity}); err != nil {
		// best effort; client will see partial response or connection error
		_ = err
	}
}

func (s *server) handleTurn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	id := extractID(r.URL.Path, "/api/game/", "/turn")
	if id == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	var req turnReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	entry, ok := s.games[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "game not found", http.StatusNotFound)
		return
	}

	entry.mu.Lock()
	res, err := entry.g.SubmitTurn(req.Bid, req.Ask)
	entry.mu.Unlock()

	// Clean up ended games immediately on resolution so they are removed from
	// the in-memory map. This reclaims memory promptly instead of waiting for
	// a future create to evict them. See considerations in design: final state
	// is delivered in the terminating TurnResult; subsequent operations on the
	// id will return 404.
	if res.State.IsOver {
		s.mu.Lock()
		delete(s.games, id)
		s.mu.Unlock()
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		_ = err
	}
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	id := extractID(r.URL.Path, "/api/game/", "")
	if id == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	entry, ok := s.games[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "game not found", http.StatusNotFound)
		return
	}

	entry.mu.Lock()
	st := entry.g.State()
	entry.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"state": st}); err != nil {
		_ = err
	}
}

func (s *server) handleQuit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	id := extractID(r.URL.Path, "/api/game/", "/quit")
	if id == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	entry, ok := s.games[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "game not found", http.StatusNotFound)
		return
	}

	entry.mu.Lock()
	res, _ := entry.g.Quit() // ignore err; already over is ok
	entry.mu.Unlock()

	// Always clean up on explicit quit (or if it was already over)
	s.mu.Lock()
	delete(s.games, id)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		_ = err
	}
}

func extractID(path, prefix, suffix string) string {
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if suffix != "" {
		rest = strings.TrimSuffix(rest, suffix)
	}
	return strings.Trim(rest, "/")
}

// --- Static UI ---
// The polished trading-terminal style UI lives in web/static/index.html (or s.indexPath).
// We serve it directly with no build step and no embedded giant string.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	path := s.indexPath
	if path == "" {
		path = "web/static/index.html"
	}
	http.ServeFile(w, r, path)
}

func main() {
	srv := newServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/game", srv.handleCreate)
	mux.HandleFunc("/api/game/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/turn") {
			srv.handleTurn(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/quit") {
			srv.handleQuit(w, r)
			return
		}
		srv.handleState(w, r)
	})

	addr := ":8080"
	log.Printf("market-maker web listening on %s", addr)
	srvHTTP := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srvHTTP.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
