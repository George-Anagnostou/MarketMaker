package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIndexServesGuidedQuoteUI(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test path")
	}
	s := newServer()
	s.indexPath = filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "index.html")
	response := httptest.NewRecorder()
	s.handleIndex(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{"Make a market.", "Post a two-sided quote", "Start a practice session", "Choose a lesson", "How to make a market well", "Spread is opportunity, not guaranteed profit.", "Move with inventory", "First Spread tutorial", "Guided practice", "lesson-check", "Lesson scorecard", "score-pnl", "Turn audit", "Customer orders", "Session tape: earlier customer flow", "immediate-or-cancel", "loadAuditHistory", "auditGeneration", "BigInt", "parseFixed", `role="status"`, `aria-live="polite"`, `step="0.01"`, `onclick="startDefault()"`, "function startDefault()", "sessionEpoch", "mmg.pending_create", "function initialize()", "turn-attribution", "attribution-execution-edge", "attribution-inventory-mark", "attribution-storage", "attribution-total", "informed-flow", "data.latest_turn?.summary", "latestTurn.coaching", "Informed customer buy", "Informed customer sell", "markEvent.previous_mark", "previousMark(markEvent.message)", "function resetLatestTurn()"} {
		if !strings.Contains(body, expected) {
			t.Errorf("missing %q", expected)
		}
	}
}
