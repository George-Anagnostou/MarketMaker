package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsPrometheusContract(t *testing.T) {
	m := newMetrics()
	m.observeRequest(http.MethodGet, "/healthz", http.StatusOK, 5*time.Millisecond)
	m.observeRequest("BREW", "unmatched", http.StatusTeapot, 2*time.Second)
	m.observeCommand("create_game", "accepted")
	m.observeCommand("unexpected", "unexpected")
	m.observeRecoveryFailure()
	m.requestStarted()
	m.observeEventLogAppend("success", 2*time.Millisecond)
	m.observeEventLogAppend("failure", 2*time.Millisecond)
	m.observeShutdown(true)
	output := string(m.prometheus())
	for _, expected := range []string{
		"# TYPE mmg_http_requests_total counter",
		`mmg_http_requests_total{method="GET",route="/healthz",status="200"} 1`,
		`mmg_http_requests_total{method="other",route="unmatched",status="418"} 1`,
		`mmg_http_request_duration_seconds_bucket{method="GET",route="/healthz",status="200",le="0.01"} 1`,
		`mmg_game_commands_total{command="create_game",outcome="accepted"} 1`,
		`mmg_game_commands_total{command="unknown",outcome="rejected"} 1`,
		"mmg_game_recovery_failures_total 1",
		`mmg_event_log_append_duration_seconds_count{outcome="success"} 1`,
		`mmg_event_log_append_duration_seconds_count{outcome="failure"} 1`,
		"mmg_http_requests_in_flight 1",
		"mmg_server_shutdowns_total 1",
		"mmg_server_shutdown_timeouts_total 1",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("metrics output missing %q:\n%s", expected, output)
		}
	}
}

func TestMetricsHandlerOnlyAllowsGet(t *testing.T) {
	m := newMetrics()
	response := httptest.NewRecorder()
	m.handleMetrics(response, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("status=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}
}
