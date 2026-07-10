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
type server struct {
	mu    sync.Mutex
	games map[string]*game.Game
	next  int // simple ID counter
}

func newServer() *server {
	return &server{
		games: make(map[string]*game.Game),
		next:  1,
	}
}

type createReq struct {
	NumTurns  int     `json:"num_turns"`
	Seed      int64   `json:"seed"`
	Cash      float64 `json:"starting_cash"`
	Inventory float64 `json:"starting_inventory"`
	Price     float64 `json:"starting_price"`
	Storage   float64 `json:"storage_cost_per_unit"`
	VolMin    float64 `json:"vol_min_pct"`
	VolMax    float64 `json:"vol_max_pct"`
}

type createResp struct {
	ID    string          `json:"game_id"`
	State types.GameState `json:"state"`
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

	var req createReq
	_ = json.NewDecoder(r.Body).Decode(&req) // best effort; use defaults below

	cfg := game.DefaultConfig()
	if req.NumTurns != 0 {
		cfg.NumTurns = req.NumTurns
	}
	if req.Seed != 0 {
		cfg.Seed = req.Seed
	}
	if req.Cash != 0 {
		cfg.StartingCash = req.Cash
	}
	if req.Inventory != 0 {
		cfg.StartingInventory = req.Inventory
	}
	if req.Price != 0 {
		cfg.StartingPrice = req.Price
	}
	if req.Storage != 0 {
		cfg.StorageCostPerUnit = req.Storage
	}
	if req.VolMin != 0 || req.VolMax != 0 {
		cfg.MinPriceMovePct = req.VolMin / 100
		cfg.MaxPriceMovePct = req.VolMax / 100
	}

	g := game.NewGame(cfg)

	s.mu.Lock()
	id := fmt.Sprintf("g-%d", s.next)
	s.next++
	s.games[id] = g
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(createResp{ID: id, State: g.State()})
}

func (s *server) handleTurn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	id := extractID(r.URL.Path, "/api/game/", "/turn")
	if id == "" {
		http.Error(w, "bad path", 400)
		return
	}

	var req turnReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}

	s.mu.Lock()
	g, ok := s.games[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "game not found", 404)
		return
	}

	res, err := g.SubmitTurn(req.Bid, req.Ask)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	id := extractID(r.URL.Path, "/api/game/", "")
	if id == "" {
		http.Error(w, "bad path", 400)
		return
	}

	s.mu.Lock()
	g, ok := s.games[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "game not found", 404)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"state": g.State(),
	})
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

// --- Static / minimal UI serving ---

const indexHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>market-maker</title>
<style>
body { font-family: system-ui, sans-serif; margin: 20px; background: #0b0d0f; color: #ddd; }
h1 { color: #fff; }
.card { background:#14171a; padding:16px; border-radius:8px; margin-bottom:12px; }
.row { display:flex; gap:12px; align-items:center; flex-wrap:wrap; }
input[type=number] { width: 120px; padding:4px; background:#1f2428; color:#fff; border:1px solid #333; }
button { padding:8px 14px; background:#2a6; color:#fff; border:none; border-radius:4px; cursor:pointer; }
button.secondary { background:#444; }
.log { font-family: ui-monospace, monospace; background:#0a0c0e; padding:10px; height:220px; overflow:auto; white-space:pre-wrap; }
.kv { display:flex; gap:16px; }
.kv div { min-width: 140px; }
.positive { color:#6c6; }
.negative { color:#c66; }
.muted { color:#888; }
</style>
</head>
<body>
<h1>market-maker <span class="muted">(web v0)</span></h1>

<div class="card">
  <div class="row">
    <label>Turns <input id="turns" type="number" value="10"></label>
    <label>Seed <input id="seed" type="number" value="42"></label>
    <label>Start Cash <input id="cash" type="number" value="100000"></label>
    <button onclick="newGame()">New Game</button>
    <button class="secondary" onclick="newGameDefault()">Default (seed 42, 10 turns)</button>
  </div>
</div>

<div class="card" id="game" style="display:none">
  <div class="kv">
    <div><strong>Game</strong> <span id="gid"></span></div>
    <div><strong>Turn</strong> <span id="turn"></span></div>
    <div><strong>Mid</strong> <span id="mid"></span></div>
    <div><strong>Cash</strong> <span id="cashv"></span></div>
    <div><strong>Inv</strong> <span id="inv"></span></div>
    <div><strong>Equity</strong> <span id="equity"></span></div>
    <div><strong>P&amp;L</strong> <span id="pnl"></span></div>
  </div>

  <div class="row" style="margin-top:12px">
    <label>Bid <input id="bid" type="number" step="0.01" value="99.50"></label>
    <label>Ask <input id="ask" type="number" step="0.01" value="100.50"></label>
    <button onclick="submitTurn()">Submit Turn</button>
    <button class="secondary" onclick="quitGame()">Quit</button>
  </div>

  <div style="margin-top:8px" class="muted" id="posnote"></div>
  <div style="margin-top:4px" class="muted" id="flowtone"></div>

  <div class="log" id="log"></div>

  <div class="card" style="margin-top:8px; font-size: 0.9em;">
    <div><strong>Last turn summary</strong></div>
    <div id="lastsummary" class="muted">—</div>
  </div>
</div>

<script>
let currentId = null;

function fmtMoney(n) {
  return '$' + Number(n).toLocaleString('en-US', {minimumFractionDigits:2, maximumFractionDigits:2});
}
function fmtNum(n, d=2) { return Number(n).toFixed(d); }

function setText(id, v) { document.getElementById(id).textContent = v; }

function updateUI(state) {
  const gidEl = document.getElementById('gid');
  gidEl.textContent = currentId;

  setText('turn', state.turn + (state.is_over ? ' (over)' : ''));
  setText('mid', fmtMoney(state.last_price));
  setText('cashv', fmtMoney(state.cash));
  setText('inv', fmtNum(state.inventory));
  setText('equity', fmtMoney(state.cash + state.inventory * state.last_price));

  const startEquity = 100000; // we don't track it client-side easily; use delta display only
  // For P&L we show vs implicit 0 for simplicity in this minimal UI.
  // Better: the server could return starting equity, but for v0 we just show raw equity.
  setText('pnl', '');

  const pos = state.inventory;
  let note = '';
  if (pos > 0.01) note = 'LONG ' + fmtNum(pos) + ' — price down hurts MTM';
  else if (pos < -0.01) note = 'SHORT ' + fmtNum(pos) + ' — price up hurts MTM';
  else note = 'FLAT — no inventory risk';
  document.getElementById('posnote').textContent = note;

  if (state.is_over) {
    document.getElementById('log').textContent += '\n[GAME OVER] ' + (state.reason || '');
  }
}

function log(msg) {
  const el = document.getElementById('log');
  el.textContent += msg + '\n';
  el.scrollTop = el.scrollHeight;
}

async function newGame() {
  const body = {
    num_turns: parseInt(document.getElementById('turns').value) || 0,
    seed: parseInt(document.getElementById('seed').value) || 0,
    starting_cash: parseFloat(document.getElementById('cash').value) || 100000
  };
  const res = await fetch('/api/game', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body)});
  const data = await res.json();
  currentId = data.game_id;
  document.getElementById('game').style.display = 'block';
  document.getElementById('log').textContent = '';
  log('New game ' + currentId);
  updateUI(data.state);
}

async function newGameDefault() {
  document.getElementById('turns').value = 10;
  document.getElementById('seed').value = 42;
  document.getElementById('cash').value = 100000;
  await newGame();
}

async function submitTurn() {
  if (!currentId) return;
  const bid = parseFloat(document.getElementById('bid').value);
  const ask = parseFloat(document.getElementById('ask').value);
  const res = await fetch('/api/game/' + currentId + '/turn', {
    method:'POST',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify({bid, ask})
  });
  if (!res.ok) {
    const txt = await res.text();
    log('Error: ' + txt);
    return;
  }
  const data = await res.json();
  for (const ev of data.events) {
    log(ev.message);
  }
  log('NetFill ' + fmtMoney(data.summary.net_fill_cash) +
      ' | Storage ' + fmtMoney(data.summary.storage_cost) +
      ' | TurnPnL ' + fmtMoney(data.summary.turn_pnl));

  // Show clearer flow breakdown in the UI (mirrors CLI polish)
  const bs = data.summary;
  let flowLine = 'No flow hit.';
  if ((bs.buy_volume || 0) + (bs.sell_volume || 0) > 0) {
    flowLine = 'Flow: ' +
      (bs.buy_volume ? (fmtNum(bs.buy_volume) + ' BUY @ ASK ') : '') +
      (bs.sell_volume ? (fmtNum(bs.sell_volume) + ' SELL @ BID') : '');
  }
  document.getElementById('lastsummary').textContent =
    flowLine + ' | NetInv ' + fmtNum((bs.sell_volume||0) - (bs.buy_volume||0)) + ' | PnL ' + fmtMoney(bs.turn_pnl);

  // Simple tone hint
  const total = (bs.buy_volume||0) + (bs.sell_volume||0);
  let tone = '';
  if (total > 0) {
    const sellPct = ((bs.sell_volume||0) / total) * 100;
    if (sellPct > 70) tone = 'Heavy sell pressure (sellers hitting your bid).';
    else if (sellPct < 30) tone = 'Heavy buy pressure (buyers hitting your ask).';
    else tone = 'Two-way flow.';
  }
  document.getElementById('flowtone').textContent = tone;

  updateUI(data.state);
}

async function quitGame() {
  if (!currentId) return;
  const res = await fetch('/api/game/' + currentId);
  const data = await res.json();
  log('Quit. Final equity ~ ' + fmtMoney(data.state.cash + data.state.inventory * data.state.last_price));
  currentId = null;
  document.getElementById('game').style.display = 'none';
}
</script>
</body>
</html>
`

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
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
