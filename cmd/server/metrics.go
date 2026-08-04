package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var durationBuckets = []float64{0.001, 0.01, 0.1, 1, 5, 10}

type requestMetricKey struct {
	method string
	route  string
	status int
}

type commandMetricKey struct {
	command string
	outcome string
}

type histogram struct {
	count   uint64
	sum     float64
	buckets []uint64
}

type metrics struct {
	mu               sync.Mutex
	requests         map[requestMetricKey]*histogram
	commands         map[commandMetricKey]uint64
	recoveryFailures uint64
	eventLogAppend   map[string]*histogram
	startedAt        time.Time
	activeRequests   uint64
	httpPanics       uint64
	shutdowns        uint64
	shutdownTimeouts uint64
}

func newMetrics() *metrics {
	return &metrics{
		requests:       make(map[requestMetricKey]*histogram),
		commands:       make(map[commandMetricKey]uint64),
		eventLogAppend: make(map[string]*histogram),
		startedAt:      time.Now().UTC(),
	}
}

func (m *metrics) observeRequest(method, route string, status int, duration time.Duration) {
	if m == nil {
		return
	}
	key := requestMetricKey{method: metricMethod(method), route: route, status: status}
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.requests[key]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(durationBuckets))}
		m.requests[key] = h
	}
	h.observe(duration.Seconds())
	if m.activeRequests > 0 {
		m.activeRequests--
	}
}

func (m *metrics) requestStarted() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeRequests++
}

func (m *metrics) observeHTTPPanic() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.httpPanics++
}

func (m *metrics) observeShutdown(timeout bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdowns++
	if timeout {
		m.shutdownTimeouts++
	}
}

func (m *metrics) observeCommand(command, outcome string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands[commandMetricKey{command: metricCommand(command), outcome: metricOutcome(outcome)}]++
}

func (m *metrics) observeRecoveryFailure() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recoveryFailures++
}

func (m *metrics) observeEventLogAppend(outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.eventLogAppend[eventLogOutcome(outcome)]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(durationBuckets))}
		m.eventLogAppend[eventLogOutcome(outcome)] = h
	}
	h.observe(duration.Seconds())
}

func (h *histogram) observe(seconds float64) {
	h.count++
	h.sum += seconds
	for i, bucket := range durationBuckets {
		if seconds <= bucket {
			h.buckets[i]++
		}
	}
}

func (m *metrics) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(m.prometheus())
}

func (m *metrics) prometheus() []byte {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var output strings.Builder
	output.WriteString("# HELP mmg_http_requests_total Total HTTP requests completed.\n# TYPE mmg_http_requests_total counter\n")
	keys := make([]requestMetricKey, 0, len(m.requests))
	for key := range m.requests {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		if keys[i].route != keys[j].route {
			return keys[i].route < keys[j].route
		}
		return keys[i].status < keys[j].status
	})
	for _, key := range keys {
		labels := fmt.Sprintf(`method=%q,route=%q,status=%q`, key.method, key.route, strconv.Itoa(key.status))
		output.WriteString("mmg_http_requests_total{" + labels + "} " + strconv.FormatUint(m.requests[key].count, 10) + "\n")
	}
	writeHistogram(&output, "mmg_http_request_duration_seconds", "HTTP request duration in seconds.", keys, func(key requestMetricKey) string {
		return fmt.Sprintf(`method=%q,route=%q,status=%q`, key.method, key.route, strconv.Itoa(key.status))
	}, func(key requestMetricKey) histogram { return *m.requests[key] })

	output.WriteString("# HELP mmg_game_commands_total Durable game command outcomes.\n# TYPE mmg_game_commands_total counter\n")
	commandKeys := make([]commandMetricKey, 0, len(m.commands))
	for key := range m.commands {
		commandKeys = append(commandKeys, key)
	}
	sort.Slice(commandKeys, func(i, j int) bool {
		if commandKeys[i].command != commandKeys[j].command {
			return commandKeys[i].command < commandKeys[j].command
		}
		return commandKeys[i].outcome < commandKeys[j].outcome
	})
	for _, key := range commandKeys {
		output.WriteString(fmt.Sprintf("mmg_game_commands_total{command=%q,outcome=%q} %d\n", key.command, key.outcome, m.commands[key]))
	}
	output.WriteString("# HELP mmg_game_recovery_failures_total Failed durable game recovery attempts.\n# TYPE mmg_game_recovery_failures_total counter\n")
	output.WriteString("mmg_game_recovery_failures_total " + strconv.FormatUint(m.recoveryFailures, 10) + "\n")
	appendOutcomes := make([]string, 0, len(m.eventLogAppend))
	for outcome := range m.eventLogAppend {
		appendOutcomes = append(appendOutcomes, outcome)
	}
	sort.Strings(appendOutcomes)
	writeHistogram(&output, "mmg_event_log_append_duration_seconds", "Durable event-log append duration in seconds.", appendOutcomes, func(outcome string) string { return fmt.Sprintf(`outcome=%q`, outcome) }, func(outcome string) histogram { return *m.eventLogAppend[outcome] })
	output.WriteString("# HELP mmg_process_start_time_seconds Unix time when this server process started.\n# TYPE mmg_process_start_time_seconds gauge\n")
	output.WriteString("mmg_process_start_time_seconds " + strconv.FormatFloat(float64(m.startedAt.UnixNano())/float64(time.Second), 'f', -1, 64) + "\n")
	output.WriteString("# HELP mmg_http_requests_in_flight Current HTTP requests in progress.\n# TYPE mmg_http_requests_in_flight gauge\n")
	output.WriteString("mmg_http_requests_in_flight " + strconv.FormatUint(m.activeRequests, 10) + "\n")
	output.WriteString("# HELP mmg_http_panics_total HTTP handler panics observed.\n# TYPE mmg_http_panics_total counter\n")
	output.WriteString("mmg_http_panics_total " + strconv.FormatUint(m.httpPanics, 10) + "\n")
	output.WriteString("# HELP mmg_server_shutdowns_total Graceful shutdown attempts.\n# TYPE mmg_server_shutdowns_total counter\n")
	output.WriteString("mmg_server_shutdowns_total " + strconv.FormatUint(m.shutdowns, 10) + "\n")
	output.WriteString("# HELP mmg_server_shutdown_timeouts_total Graceful shutdown attempts that reached their deadline.\n# TYPE mmg_server_shutdown_timeouts_total counter\n")
	output.WriteString("mmg_server_shutdown_timeouts_total " + strconv.FormatUint(m.shutdownTimeouts, 10) + "\n")
	return []byte(output.String())
}

func writeHistogram[T any](output *strings.Builder, name, help string, keys []T, labels func(T) string, value func(T) histogram) {
	output.WriteString("# HELP " + name + " " + help + "\n# TYPE " + name + " histogram\n")
	for _, key := range keys {
		h := value(key)
		base := labels(key)
		for index, bucket := range durationBuckets {
			output.WriteString(name + "_bucket{" + joinLabels(base, fmt.Sprintf(`le=%q`, strconv.FormatFloat(bucket, 'f', -1, 64))) + "} " + strconv.FormatUint(h.buckets[index], 10) + "\n")
		}
		output.WriteString(name + "_bucket{" + joinLabels(base, `le="+Inf"`) + "} " + strconv.FormatUint(h.count, 10) + "\n")
		if base == "" {
			output.WriteString(name + "_sum " + strconv.FormatFloat(h.sum, 'f', -1, 64) + "\n")
			output.WriteString(name + "_count " + strconv.FormatUint(h.count, 10) + "\n")
		} else {
			output.WriteString(name + "_sum{" + base + "} " + strconv.FormatFloat(h.sum, 'f', -1, 64) + "\n")
			output.WriteString(name + "_count{" + base + "} " + strconv.FormatUint(h.count, 10) + "\n")
		}
	}
}

func joinLabels(left, right string) string {
	if left == "" {
		return right
	}
	return left + "," + right
}

func metricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
		return method
	default:
		return "other"
	}
}

func metricCommand(command string) string {
	switch command {
	case "create_game", "submit_quote", "quit":
		return command
	default:
		return "unknown"
	}
}

func metricOutcome(outcome string) string {
	switch outcome {
	case "accepted", "replayed", "rejected", "recap_failure", "storage_failure":
		return outcome
	default:
		return "rejected"
	}
}

func eventLogOutcome(outcome string) string {
	if outcome == "success" {
		return outcome
	}
	return "failure"
}
