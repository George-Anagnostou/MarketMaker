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
	for _, expected := range []string{"Make a market.", "Post a two-sided quote", "Start a practice session", "Choose a lesson", "How to make a market well", "Spread is opportunity, not guaranteed profit.", "Move with inventory", "First Spread tutorial", "Guided practice", "lesson-check", "Lesson scorecard", "score-pnl", "Turn audit", "Customer orders", "Session tape: earlier customer flow", "immediate-or-cancel", "loadAuditHistory", "auditGeneration", "BigInt", "parseFixed", "function currency", "startingEquityFixed", "new AbortController()", "setTimeout(() => controller.abort(), 5000)", "controller.signal", `id="turn-status" class="turn-status" role="status" aria-live="polite"`, `step="0.01"`, `onclick="startDefault()"`, "function startDefault()", "sessionEpoch", "restorationState", "await loadAuditHistory(id, epoch, data.state, controller.signal)", "finalState?.position", "state.equity", "mmg.pending_create", "const catalog = loadScenarios(), request = pendingCreate()", "result?.status === 'rejected'", "function initialize()", `id="play-again"`, `id="review-history"`, `id="quit-session"`, "turn-attribution", "attribution-execution-edge", "attribution-inventory-mark", "attribution-storage", "attribution-total", "informed-flow", "signedMoney(totalPnl)", "scorecard.focus_label === 'Informed-flow P&L' ? signedMoney(scorecard.focus_value)", "data.latest_turn?.summary", "latestTurn.coaching", "Informed customer buy", "Informed customer sell", "markEvent.previous_mark", "previousMark(markEvent.message)", "function resetLatestTurn()", "['bid', 'ask'].forEach(id => $(id).addEventListener('input', updateQuotePreview))", "unresolved = Boolean(retryCommand || retryQuitCommand)", "$('submitbtn').disabled = disabled || Boolean(retryQuitCommand)", "$('bid').disabled = disabled || unresolved", "$('quit-session').disabled = disabled || Boolean(retryCommand)", "Retry previous quote", "Retry ending session", "if (response.status >= 500)", "The same quote will be retried.", "The same end-session command will be retried.", "pendingCommand || retryCommand || retryQuitCommand"} {
		if !strings.Contains(body, expected) {
			t.Errorf("missing %q", expected)
		}
	}
	if count := strings.Count(body, `aria-live="polite"`); count != 1 {
		t.Errorf("aria-live polite regions=%d, want 1", count)
	}
	if count := strings.Count(body, `role="status"`); count != 1 {
		t.Errorf("status roles=%d, want 1", count)
	}
	if count := strings.Count(body, "if (response.status >= 500)"); count != 2 {
		t.Errorf("5xx retry handlers=%d, want 2", count)
	}
	if strings.Contains(body, "retryCommand = null; updateQuotePreview()") {
		t.Error("bid/ask input clears unresolved quote retry")
	}
}
