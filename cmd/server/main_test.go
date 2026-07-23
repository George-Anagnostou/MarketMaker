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
	for _, expected := range []string{"Make a market.", "Post a two-sided quote", "Start a practice session", "Choose a lesson", `onclick="startDefault()"`, "function startDefault()", "sessionEpoch", "mmg.pending_create", "function initialize()"} {
		if !strings.Contains(body, expected) {
			t.Errorf("missing %q", expected)
		}
	}
}
