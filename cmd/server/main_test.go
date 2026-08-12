package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*server, *bytes.Buffer) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test path")
	}
	logs := &bytes.Buffer{}
	s, err := newServerWithConfig(filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "index.html"), filepath.Join(t.TempDir(), "games"), newJSONLogger(logs))
	if err != nil {
		t.Fatalf("new test server: %v", err)
	}
	return s, logs
}

func TestIndexServesGuidedQuoteUI(t *testing.T) {
	s, _ := newTestServer(t)
	response := httptest.NewRecorder()
	s.handleIndex(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		"Make a market.",
		"Post a two-sided quote",
		"Choose a lesson",
		"Turn audit",
		"Lesson scorecard",
		"function hydrateGame(id)",
		"starting_equity",
		"events_through",
		"through=${through}",
		"validateCommandEnvelope",
		"BigInt",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("missing %q", expected)
		}
	}
	if count := strings.Count(body, `aria-live="polite"`); count != 2 {
		t.Errorf("aria-live polite regions=%d, want 2", count)
	}
	if count := strings.Count(body, `role="status"`); count != 1 {
		t.Errorf("status roles=%d, want 1", count)
	}
}

func TestServerInitializesGameRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "games")
	_, err := newServerWithConfig("index.html", root, newJSONLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("game root info=%v, err=%v", info, err)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	s, _ := newTestServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		response := httptest.NewRecorder()
		s.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Body.String() != "{\"status\":\"ok\"}\n" {
			t.Errorf("%s status=%d body=%q", path, response.Code, response.Body.String())
		}
	}
	entries, err := os.ReadDir(s.gameRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("readiness changed game root: entries=%v err=%v", entries, err)
	}
}

func TestReadinessReportsUnavailableStorage(t *testing.T) {
	s, _ := newTestServer(t)
	s.storageReady = func(string) error { return os.ErrPermission }
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestReadinessReportsFencedGameStorage(t *testing.T) {
	s, _ := newTestServer(t)
	s.v2.entries["fenced"] = &exchangeEntry{storageFailed: true}
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestRequestLogUsesServerGeneratedIDAndRedactedRoute(t *testing.T) {
	s, logs := newTestServer(t)
	s.requestID = func() string { return "server-request-id" }
	request := httptest.NewRequest(http.MethodGet, "/api/v2/games/11111111-1111-4111-8111-111111111111/events?after=42", nil)
	request.Header.Set("X-Request-ID", "untrusted-client-id")
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, request)
	if got := response.Header().Get("X-Request-ID"); got != "server-request-id" {
		t.Fatalf("request id=%q", got)
	}
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("decode request log: %v; log=%q", err, logs.String())
	}
	if entry["event"] != "http_request" || entry["route"] != "/api/v2/games/{game_id}/events" || entry["request_id"] != "server-request-id" || entry["status"] != float64(http.StatusNotFound) {
		t.Fatalf("unexpected request log: %#v", entry)
	}
	if strings.Contains(logs.String(), "11111111-1111-4111-8111-111111111111") || strings.Contains(logs.String(), "after=42") || strings.Contains(logs.String(), "untrusted-client-id") {
		t.Fatalf("request log contains sensitive request data: %q", logs.String())
	}
}

func TestMetricsEndpointReportsCompletedRequests(t *testing.T) {
	s, _ := newTestServer(t)
	s.handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content type=%q", contentType)
	}
	if !strings.Contains(response.Body.String(), `mmg_http_requests_total{method="GET",route="/healthz",status="200"} 1`) {
		t.Fatalf("health request missing from metrics:\n%s", response.Body.String())
	}
}

func TestRequestMiddlewareRecoversPanicsWithoutLoggingPanicValue(t *testing.T) {
	s, logs := newTestServer(t)
	s.requestID = func() string { return "server-request-id" }
	handler := s.withRequestLog(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("secret panic value") }))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unexpected?secret=value", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", response.Code)
	}
	if strings.Contains(logs.String(), "secret panic value") || strings.Contains(logs.String(), "secret=value") {
		t.Fatalf("panic log leaked request or panic data: %q", logs.String())
	}
	metrics := string(s.metrics.prometheus())
	if !strings.Contains(metrics, "mmg_http_panics_total 1") || !strings.Contains(metrics, `mmg_http_requests_total{method="GET",route="unmatched",status="500"} 1`) {
		t.Fatalf("panic metrics missing:\n%s", metrics)
	}
}

func TestRequestMiddlewareAbortsStartedResponseAfterPanic(t *testing.T) {
	s, logs := newTestServer(t)
	handler := s.withRequestLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial response"))
		panic("secret panic value")
	}))
	ts := httptest.NewUnstartedServer(handler)
	ts.Config.ErrorLog = log.New(jsonLogWriter{logger: newJSONLogger(logs)}, "", 0)
	ts.Start()
	response, err := ts.Client().Get(ts.URL)
	if err == nil {
		_, err = io.ReadAll(response.Body)
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("started response completed after panic")
	}
	ts.Close()
	if strings.Contains(logs.String(), "secret panic value") || !strings.Contains(logs.String(), `"event":"http_panic"`) {
		t.Fatalf("unexpected panic logs: %q", logs.String())
	}
	if !strings.Contains(string(s.metrics.prometheus()), `mmg_http_requests_total{method="GET",route="/",status="500"} 1`) {
		t.Fatalf("aborted response was not recorded as a 500:\n%s", s.metrics.prometheus())
	}
}

func TestHTTPServerErrorWriterRedactsUnderlyingMessage(t *testing.T) {
	logs := &bytes.Buffer{}
	writer := jsonLogWriter{logger: newJSONLogger(logs)}
	if n, err := writer.Write([]byte("http: secret remote address 127.0.0.1")); err != nil || n == 0 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "127.0.0.1") || !strings.Contains(logs.String(), `"event":"http_server_error"`) {
		t.Fatalf("unexpected server error log: %q", logs.String())
	}
}

func TestServeDrainsInFlightRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &bytes.Buffer{}
	serveDone := make(chan error, 1)
	metrics := newMetrics()
	runtimeShutdown := make(chan struct{})
	go func() {
		serveDone <- serve(ctx, httpServer, listener, newJSONLogger(logs), metrics, time.Second, func(context.Context) error {
			close(runtimeShutdown)
			return nil
		})
	}()
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-serveDone:
		t.Fatalf("server stopped before request drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-runtimeShutdown:
		t.Fatal("runtime shut down before HTTP request drained")
	default:
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatalf("in-flight request: %v", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("serve: %v", err)
	}
	select {
	case <-runtimeShutdown:
	default:
		t.Fatal("runtime shutdown was not called")
	}
	if !strings.Contains(logs.String(), `"event":"server_shutdown_started"`) || !strings.Contains(logs.String(), `"event":"server_stopped"`) {
		t.Fatalf("missing shutdown lifecycle logs: %q", logs.String())
	}
	if !strings.Contains(string(metrics.prometheus()), "mmg_server_shutdowns_total 1") {
		t.Fatalf("shutdown metric missing:\n%s", metrics.prometheus())
	}
}

func TestServeLogsTimeoutWithoutForceClosingRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &bytes.Buffer{}
	serveDone := make(chan error, 1)
	metrics := newMetrics()
	go func() {
		serveDone <- serve(ctx, httpServer, listener, newJSONLogger(logs), metrics, 10*time.Millisecond, nil)
	}()
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	if err := <-serveDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("serve error=%v, want deadline exceeded", err)
	}
	select {
	case err := <-requestDone:
		t.Fatalf("request was force-closed: %v", err)
	default:
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatalf("in-flight request after timeout: %v", err)
	}
	if !strings.Contains(logs.String(), `"event":"server_shutdown_failed"`) {
		t.Fatalf("missing shutdown timeout log: %q", logs.String())
	}
	if !strings.Contains(string(metrics.prometheus()), "mmg_server_shutdown_timeouts_total 1") {
		t.Fatalf("shutdown timeout metric missing:\n%s", metrics.prometheus())
	}
}
