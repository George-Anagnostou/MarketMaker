// Command server runs the local durable market-making exchange and web UI.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type server struct {
	indexPath    string
	gameRoot     string
	v2           *exchangeService
	logger       *jsonLogger
	metrics      *metrics
	requestID    func() string
	storageReady func(string) error
}

type jsonLogger struct {
	mu     sync.Mutex
	writer io.Writer
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

type jsonLogWriter struct{ logger *jsonLogger }

var fallbackRequestID uint64

const gracefulShutdownTimeout = 10 * time.Second

func newServer() (*server, error) {
	indexPath := os.Getenv("MMG_STATIC_INDEX")
	if indexPath == "" {
		indexPath = "web/static/index.html"
	}
	gameRoot := os.Getenv("MMG_GAME_ROOT")
	if gameRoot == "" {
		gameRoot = "data/games"
	}
	return newServerWithConfig(indexPath, gameRoot, newJSONLogger(os.Stderr))
}

func newServerWithConfig(indexPath, gameRoot string, logger *jsonLogger) (*server, error) {
	if err := initializeGameRoot(gameRoot); err != nil {
		return nil, fmt.Errorf("initialize game storage: %w", err)
	}
	metrics := newMetrics()
	v2 := newExchangeService(gameRoot)
	v2.metrics = metrics
	return &server{
		indexPath: indexPath, gameRoot: gameRoot, v2: v2, logger: logger, metrics: metrics,
		requestID: newRequestID, storageReady: gameStorageReady,
	}, nil
}

func newJSONLogger(writer io.Writer) *jsonLogger { return &jsonLogger{writer: writer} }

func (l *jsonLogger) log(level, event string, fields map[string]any) {
	entry := make(map[string]any, len(fields)+3)
	entry["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	entry["level"] = level
	entry["event"] = event
	for key, value := range fields {
		entry[key] = value
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.writer.Write(append(data, '\n'))
}

func initializeGameRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return validateGameStorage(root)
}

func validateGameStorage(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	return writeStorageProbe(root)
}

func writeStorageProbe(root string) error {
	probe, err := os.CreateTemp(root, ".mmg-ready-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	if n, err := probe.Write([]byte("ready\n")); err != nil || n != len("ready\n") {
		_ = probe.Close()
		_ = os.Remove(name)
		if err != nil {
			return err
		}
		return io.ErrShortWrite
	}
	if err := probe.Sync(); err != nil {
		_ = probe.Close()
		_ = os.Remove(name)
		return err
	}
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Remove(name); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func gameStorageReady(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	return writeStorageProbe(root)
}

func newRequestID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err == nil {
		return hex.EncodeToString(data[:])
	}
	return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), atomic.AddUint64(&fallbackRequestID, 1))
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, s.indexPath)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	if err := s.storageReady(s.gameRoot); err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "not_ready", "game storage is unavailable")
		return
	}
	if !s.v2.storageHealthy() {
		writeAPIError(w, http.StatusServiceUnavailable, "not_ready", "durable game storage is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/metrics", s.metrics.handleMetrics)
	mux.HandleFunc("/api/v2/scenarios", s.v2.handleScenarios)
	mux.HandleFunc("/api/v2/games", s.v2.handleGames)
	mux.HandleFunc("/api/v2/games/", s.v2.handleGame)
	return s.withRequestLog(mux)
}

func (s *server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := s.requestID()
		w.Header().Set("X-Request-ID", requestID)
		recorded := &responseRecorder{ResponseWriter: w}
		s.metrics.requestStarted()
		defer func() {
			if recovered := recover(); recovered != nil {
				responseStarted := recorded.status != 0
				s.metrics.observeHTTPPanic()
				if !responseStarted {
					writeAPIError(recorded, http.StatusInternalServerError, "internal_error", "internal server error")
				}
				s.logger.log("error", "http_panic", map[string]any{"request_id": requestID, "route": routeTemplate(r.URL.Path)})
				if responseStarted {
					// The wire response is incomplete; record it as a server error.
					recorded.status = http.StatusInternalServerError
					defer func() { panic(recovered) }()
				}
			}
			if recorded.status == 0 {
				recorded.status = http.StatusOK
			}
			duration := time.Since(started)
			s.logger.log("info", "http_request", map[string]any{
				"request_id": requestID, "method": r.Method, "route": routeTemplate(r.URL.Path),
				"status": recorded.status, "duration_ms": float64(duration) / float64(time.Millisecond), "response_bytes": recorded.bytes,
			})
			s.metrics.observeRequest(r.Method, routeTemplate(r.URL.Path), recorded.status, duration)
		}()
		next.ServeHTTP(recorded, r)
	})
}

func (w jsonLogWriter) Write(data []byte) (int, error) {
	if len(data) > 0 {
		w.logger.log("error", "http_server_error", nil)
	}
	return len(data), nil
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func routeTemplate(path string) string {
	switch path {
	case "/", "/index.html":
		return "/"
	case "/healthz", "/readyz", "/metrics", "/api/v2/scenarios", "/api/v2/games":
		return path
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 4 && strings.Join(parts[:3], "/") == "api/v2/games" {
		switch {
		case len(parts) == 4:
			return "/api/v2/games/{game_id}"
		case len(parts) == 5 && (parts[4] == "commands" || parts[4] == "events" || parts[4] == "stream"):
			return "/api/v2/games/{game_id}/" + parts[4]
		}
	}
	return "unmatched"
}

func serve(ctx context.Context, httpServer *http.Server, listener net.Listener, logger *jsonLogger, metrics *metrics, shutdownTimeout time.Duration, shutdownRuntime func(context.Context) error) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.log("info", "server_shutdown_started", map[string]any{"timeout_ms": shutdownTimeout.Milliseconds()})
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		err := httpServer.Shutdown(shutdownCtx)
		if err != nil {
			cancel()
			metrics.observeShutdown(errors.Is(err, context.DeadlineExceeded))
			logger.log("error", "server_shutdown_failed", map[string]any{"reason": "drain_failure"})
			return err
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.log("error", "server_shutdown_failed", map[string]any{"reason": "serve_failure"})
			cancel()
			metrics.observeShutdown(false)
			return err
		}
		if shutdownRuntime != nil {
			if err := shutdownRuntime(shutdownCtx); err != nil {
				cancel()
				metrics.observeShutdown(errors.Is(err, context.DeadlineExceeded))
				logger.log("error", "server_shutdown_failed", map[string]any{"reason": "runtime_failure"})
				return err
			}
		}
		cancel()
		metrics.observeShutdown(false)
		logger.log("info", "server_stopped", map[string]any{"reason": "shutdown"})
		return nil
	}
}

func main() {
	srv, err := newServer()
	if err != nil {
		newJSONLogger(os.Stderr).log("error", "server_start_failed", map[string]any{"reason": "startup_failure"})
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              "127.0.0.1:8080",
		Handler:           srv.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// SSE controller streams are intentionally long-lived; handlers still
		// bound request parsing with ReadTimeout and emit heartbeats themselves.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
		ErrorLog:     log.New(jsonLogWriter{logger: srv.logger}, "", 0),
	}
	listener, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		srv.logger.log("error", "server_start_failed", map[string]any{"reason": "listen_failure"})
		os.Exit(1)
	}
	srv.logger.log("info", "server_started", map[string]any{"address": httpServer.Addr})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, httpServer, listener, srv.logger, srv.metrics, gracefulShutdownTimeout, srv.v2.Shutdown); err != nil {
		srv.logger.log("error", "server_stopped", map[string]any{"reason": "shutdown_failure"})
		os.Exit(1)
	}
}
