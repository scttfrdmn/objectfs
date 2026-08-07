package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/scttfrdmn/objectfs/pkg/errors"
	"github.com/scttfrdmn/objectfs/pkg/health"
	"github.com/scttfrdmn/objectfs/pkg/status"
)

// jsonInt reads an integer field out of a decoded JSON object, failing the test if it is absent or not
// a number.
//
// The four call sites were each `int(response["count"].(float64))`, a single-value type assertion that
// panics on a response shaped differently than expected — and a panic in a test is a crashed binary that
// takes every other test in the package with it, reported as a stack trace rather than as this test
// failing on this field. Both wrong shapes are worth distinguishing and neither is hypothetical: a
// handler that stops emitting the field gives the missing case, and one that emits a string gives the
// type case.
//
// float64 because that is what encoding/json produces for every JSON number decoded into an `any`;
// there is no integer case to handle.
func jsonInt(t *testing.T, obj map[string]any, field string) int {
	t.Helper()

	raw, ok := obj[field]
	if !ok {
		t.Fatalf("response has no %q field; got fields %v", field, slices.Sorted(maps.Keys(obj)))
	}

	f, ok := raw.(float64)
	if !ok {
		t.Fatalf("response[%q] = %#v (%T), want a JSON number", field, raw, raw)
	}

	return int(f)
}

func TestNewServer(t *testing.T) {
	t.Parallel()

	config := DefaultServerConfig()
	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	healthTracker := health.NewTracker(health.DefaultConfig())

	server := NewServer(config, statusTracker, healthTracker, nil)

	if server == nil {
		t.Fatal("NewServer returned nil")
	}

	if server.statusTracker != statusTracker {
		t.Error("Status tracker not set correctly")
	}

	if server.healthTracker != healthTracker {
		t.Error("Health tracker not set correctly")
	}

	if server.httpServer == nil {
		t.Error("HTTP server not initialized")
	}
}

func TestHandleHealth(t *testing.T) {
	t.Parallel()

	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("test-service")

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response["status"] != "healthy" {
		t.Errorf("Expected status=healthy, got %v", response["status"])
	}
}

func TestHandleHealthDegraded(t *testing.T) {
	t.Parallel()

	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("test-service")

	// Make service degraded
	for range 3 {
		healthTracker.RecordError("test-service", fmt.Errorf("test error"))
	}

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusPartialContent {
		t.Errorf("Expected status 206, got %d", w.Code)
	}

	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response["status"] != "degraded" {
		t.Errorf("Expected status=degraded, got %v", response["status"])
	}
}

func TestHandleHealthComponents(t *testing.T) {
	t.Parallel()

	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("service-1")
	healthTracker.RegisterComponent("service-2")

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health/components", nil)
	w := httptest.NewRecorder()

	server.handleHealthComponents(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]*health.ComponentHealth
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if len(response) != 2 {
		t.Errorf("Expected 2 components, got %d", len(response))
	}

	if _, exists := response["service-1"]; !exists {
		t.Error("service-1 not found in response")
	}

	if _, exists := response["service-2"]; !exists {
		t.Error("service-2 not found in response")
	}
}

func TestHandleLiveness(t *testing.T) {
	t.Parallel()

	server := &Server{
		config: DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health/live", nil)
	w := httptest.NewRecorder()

	server.handleLiveness(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if alive, ok := response["alive"].(bool); !ok || !alive {
		t.Error("Expected alive=true")
	}
}

func TestHandleReadiness(t *testing.T) {
	t.Parallel()

	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("test-service")

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health/ready", nil)
	w := httptest.NewRecorder()

	server.handleReadiness(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if ready, ok := response["ready"].(bool); !ok || !ready {
		t.Error("Expected ready=true")
	}
}

func TestHandleReadinessUnavailable(t *testing.T) {
	t.Parallel()

	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("test-service")

	// Make service unavailable
	for range 10 {
		healthTracker.RecordError("test-service", fmt.Errorf("test error"))
	}

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health/ready", nil)
	w := httptest.NewRecorder()

	server.handleReadiness(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", w.Code)
	}

	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if ready, ok := response["ready"].(bool); !ok || ready {
		t.Error("Expected ready=false")
	}
}

func TestHandleSystemStatus(t *testing.T) {
	t.Parallel()

	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	ctx := context.Background()

	// Start some operations
	statusTracker.StartOperation(ctx, "read", nil)
	statusTracker.StartOperation(ctx, "write", nil)

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()

	server.handleSystemStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response status.SystemStatus
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.ActiveOps != 2 {
		t.Errorf("Expected 2 active operations, got %d", response.ActiveOps)
	}
}

func TestHandleOperations(t *testing.T) {
	t.Parallel()

	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	ctx := context.Background()

	// Start operations
	op1, _ := statusTracker.StartOperation(ctx, "read", nil)
	op2, _ := statusTracker.StartOperation(ctx, "write", nil)

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/status/operations", nil)
	w := httptest.NewRecorder()

	server.handleOperations(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	count := jsonInt(t, response, "count")
	if count != 2 {
		t.Errorf("Expected 2 operations, got %d", count)
	}

	// Verify we can access the operations
	_, _ = op1, op2
}

func TestHandleOperation(t *testing.T) {
	t.Parallel()

	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	ctx := context.Background()

	op, _ := statusTracker.StartOperation(ctx, "test", nil)

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/status/operations/"+op.ID, nil)
	w := httptest.NewRecorder()

	server.handleOperation(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response status.Operation
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.ID != op.ID {
		t.Errorf("Expected operation ID=%s, got %s", op.ID, response.ID)
	}
}

func TestHandleOperationNotFound(t *testing.T) {
	t.Parallel()

	statusTracker := status.NewTracker(status.DefaultTrackerConfig())

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/status/operations/non-existent", nil)
	w := httptest.NewRecorder()

	server.handleOperation(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}
}

func TestHandleHistory(t *testing.T) {
	t.Parallel()

	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	ctx := context.Background()

	// Complete some operations
	for i := range 3 {
		op, _ := statusTracker.StartOperation(ctx, fmt.Sprintf("op-%d", i), nil)
		if err := statusTracker.CompleteOperation(op.ID); err != nil {
			t.Fatalf("Failed to complete operation: %v", err)
		}
	}

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/status/history?limit=2", nil)
	w := httptest.NewRecorder()

	server.handleHistory(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	count := jsonInt(t, response, "count")
	if count != 2 {
		t.Errorf("Expected 2 history entries, got %d", count)
	}

	limit := jsonInt(t, response, "limit")
	if limit != 2 {
		t.Errorf("Expected limit=2, got %d", limit)
	}
}

func TestHandleInfo(t *testing.T) {
	t.Parallel()

	t.Run("version from config", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultServerConfig()
		cfg.Version = "0.9.0"
		server := &Server{config: cfg}

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/info", nil)
		w := httptest.NewRecorder()
		server.handleInfo(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", w.Code)
		}

		var response map[string]any
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}
		if response["service"] != "ObjectFS API" {
			t.Errorf("Expected service='ObjectFS API', got %v", response["service"])
		}
		if response["version"] != "0.9.0" {
			t.Errorf("Expected version='0.9.0', got %v", response["version"])
		}
	})

	t.Run("version defaults to unknown when not set", func(t *testing.T) {
		t.Parallel()
		server := &Server{config: DefaultServerConfig()} // Version is ""

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/info", nil)
		w := httptest.NewRecorder()
		server.handleInfo(w, req)

		var response map[string]any
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("Failed to decode response: %v", err)
		}
		if response["version"] != "unknown" {
			t.Errorf("Expected version='unknown', got %v", response["version"])
		}
	})
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()

	server := &Server{
		config: DefaultServerConfig(),
	}

	// Test POST on GET-only endpoint
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestCORSMiddleware(t *testing.T) {
	t.Parallel()

	config := DefaultServerConfig()
	config.EnableCORS = true

	server := NewServer(config, nil, nil, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/health", nil)
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200 for OPTIONS, got %d", w.Code)
	}

	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS header not set correctly")
	}
}

func TestServerShutdown(t *testing.T) {
	t.Parallel()

	config := DefaultServerConfig()
	config.Address = "localhost:0" // Use random available port

	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	server := NewServer(config, statusTracker, nil, nil)

	// Start server in background
	server.StartBackground()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		t.Errorf("Server shutdown failed: %v", err)
	}
}

func TestNilTrackers(t *testing.T) {
	t.Parallel()

	server := &Server{
		config: DefaultServerConfig(),
	}

	tests := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		path    string
		wantErr bool
	}{
		{"Health without tracker", server.handleHealth, "/health", false},
		{"Status without tracker", server.handleSystemStatus, "/status", true},
		{"Operations without tracker", server.handleOperations, "/status/operations", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()

			tt.handler(w, req)

			if tt.wantErr {
				if w.Code != http.StatusServiceUnavailable {
					t.Errorf("Expected status 503, got %d", w.Code)
				}
			}
		})
	}
}

// Benchmark tests

func BenchmarkHandleHealth(b *testing.B) {
	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("test-service")

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/health", nil)

	b.ResetTimer()
	for range b.N {
		w := httptest.NewRecorder()
		server.handleHealth(w, req)
	}
}

func BenchmarkHandleOperations(b *testing.B) {
	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	ctx := context.Background()

	// Create some operations
	for range 10 {
		statusTracker.StartOperation(ctx, "test", nil)
	}

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/status/operations", nil)

	b.ResetTimer()
	for range b.N {
		w := httptest.NewRecorder()
		server.handleOperations(w, req)
	}
}

// Test with actual errors integration

func TestHealthWithActualErrors(t *testing.T) {
	t.Parallel()

	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("storage")

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	// Record write errors to trigger read-only mode
	writeErr := errors.NewError(errors.ErrCodeStorageWrite, "write failed")
	for range 3 {
		healthTracker.RecordError("storage", writeErr)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusPartialContent {
		t.Errorf("Expected status 206, got %d", w.Code)
	}

	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response["status"] != "read-only" {
		t.Errorf("Expected status=read-only, got %v", response["status"])
	}
}

// Metrics endpoint tests

func TestHandleMetrics_NilGatherer(t *testing.T) {
	t.Parallel()
	server := &Server{
		config:   DefaultServerConfig(),
		gatherer: nil,
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	server.handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Expected text/plain Content-Type, got %q", ct)
	}
}

func TestHandleMetrics_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	server := &Server{
		config:   DefaultServerConfig(),
		gatherer: nil,
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/metrics", nil)
	w := httptest.NewRecorder()

	server.handleMetrics(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestHandleMetrics_WithGatherer(t *testing.T) {
	t.Parallel()

	// Build a test registry with known metrics.
	reg := prometheus.NewRegistry()

	opsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "objectfs",
		Name:      "operations_total",
		Help:      "Total operations",
	}, []string{"operation", "status"})
	// Labels here mirror internal/metrics.Collector exactly. A fixture that invents its own label set
	// tests the handler against a metric shape nothing emits — this one carried a "source" label after
	// the collector stopped exporting it, and passed either way.
	cacheTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "objectfs",
		Name:      "cache_requests_total",
		Help:      "Total cache requests",
	}, []string{"type"})
	errorsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "objectfs",
		Name:      "errors_total",
		Help:      "Total errors",
	}, []string{"operation", "type"})

	reg.MustRegister(opsTotal, cacheTotal, errorsTotal)

	// Record some observations so the families appear in the output.
	opsTotal.With(prometheus.Labels{"operation": "read", "status": "success"}).Inc()
	cacheTotal.With(prometheus.Labels{"type": "hit"}).Inc()
	errorsTotal.With(prometheus.Labels{"operation": "write", "type": "timeout"}).Inc()

	server := &Server{
		config:   DefaultServerConfig(),
		gatherer: reg,
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	server.handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	body := w.Body.String()

	// Verify Content-Type header contains text/plain
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Expected text/plain Content-Type, got %q", ct)
	}

	// Verify the three core metric families are present.
	for _, want := range []string{
		"objectfs_operations_total",
		"objectfs_cache_requests_total",
		"objectfs_errors_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Expected metric %q in /metrics output; body:\n%s", want, body)
		}
	}

	// Verify Prometheus text format: lines should contain HELP and TYPE comments.
	if !strings.Contains(body, "# HELP") {
		t.Error("Expected '# HELP' comment lines in Prometheus text output")
	}
	if !strings.Contains(body, "# TYPE") {
		t.Error("Expected '# TYPE' comment lines in Prometheus text output")
	}
}

// ─── Mount endpoint tests ─────────────────────────────────────────────────────

// mockMountManager is a test double for MountManager.
type mockMountManager struct {
	mounts map[string]MountInfo
}

func newMockMountManager() *mockMountManager {
	return &mockMountManager{mounts: make(map[string]MountInfo)}
}

func (m *mockMountManager) Mount(mountPoint string, opts MountOptions) error {
	m.mounts[mountPoint] = MountInfo{
		MountPoint: mountPoint,
		StorageURI: opts.StorageURI,
		ReadOnly:   opts.ReadOnly,
	}
	return nil
}

func (m *mockMountManager) Unmount(mountPoint string) error {
	delete(m.mounts, mountPoint)
	return nil
}

func (m *mockMountManager) IsMounted(mountPoint string) bool {
	_, ok := m.mounts[mountPoint]
	return ok
}

func (m *mockMountManager) ListMounts() []MountInfo {
	list := make([]MountInfo, 0, len(m.mounts))
	for _, mi := range m.mounts {
		list = append(list, mi)
	}
	return list
}

func TestHandleMounts_NoMountManager(t *testing.T) {
	t.Parallel()
	server := &Server{config: DefaultServerConfig()} // MountManager is nil

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), method, "/api/v1/mounts", strings.NewReader("{}"))
			w := httptest.NewRecorder()
			server.handleMounts(w, req)
			if w.Code != http.StatusNotImplemented {
				t.Errorf("%s: expected 501, got %d", method, w.Code)
			}
		})
	}
}

func TestHandleMounts_List(t *testing.T) {
	t.Parallel()
	mm := newMockMountManager()
	_ = mm.Mount("/mnt/bucket1", MountOptions{StorageURI: "s3://bucket1"})

	cfg := DefaultServerConfig()
	cfg.MountManager = mm
	server := &Server{config: cfg}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/mounts", nil)
	w := httptest.NewRecorder()
	server.handleMounts(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if count := jsonInt(t, response, "count"); count != 1 {
		t.Errorf("Expected count=1, got %d", count)
	}
}

func TestHandleMounts_Post(t *testing.T) {
	t.Parallel()
	mm := newMockMountManager()
	cfg := DefaultServerConfig()
	cfg.MountManager = mm
	server := &Server{config: cfg}

	body := `{"mount_point":"/mnt/test","options":{"storage_uri":"s3://my-bucket"}}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/mounts", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.handleMounts(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Expected 201, got %d; body: %s", w.Code, w.Body.String())
	}
	if !mm.IsMounted("/mnt/test") {
		t.Error("Expected /mnt/test to be mounted")
	}
}

func TestHandleMounts_Post_MissingMountPoint(t *testing.T) {
	t.Parallel()
	mm := newMockMountManager()
	cfg := DefaultServerConfig()
	cfg.MountManager = mm
	server := &Server{config: cfg}

	body := `{"options":{"storage_uri":"s3://bucket"}}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/mounts", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.handleMounts(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d", w.Code)
	}
}

func TestHandleMount_GetStatus(t *testing.T) {
	t.Parallel()
	mm := newMockMountManager()
	_ = mm.Mount("/mnt/bucket", MountOptions{StorageURI: "s3://bucket"})

	cfg := DefaultServerConfig()
	cfg.MountManager = mm
	server := &Server{config: cfg}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/mounts//mnt/bucket", nil)
	req.URL.Path = "/api/v1/mounts//mnt/bucket"
	w := httptest.NewRecorder()
	server.handleMount(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if mounted, _ := response["mounted"].(bool); !mounted {
		t.Error("Expected mounted=true")
	}
}

func TestHandleMount_Delete(t *testing.T) {
	t.Parallel()
	mm := newMockMountManager()
	_ = mm.Mount("/mnt/bucket", MountOptions{StorageURI: "s3://bucket"})

	cfg := DefaultServerConfig()
	cfg.MountManager = mm
	server := &Server{config: cfg}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/v1/mounts//mnt/bucket", nil)
	req.URL.Path = "/api/v1/mounts//mnt/bucket"
	w := httptest.NewRecorder()
	server.handleMount(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	if mm.IsMounted("/mnt/bucket") {
		t.Error("Expected /mnt/bucket to be unmounted")
	}
}

func TestHandleMount_NoMountManager(t *testing.T) {
	t.Parallel()
	server := &Server{config: DefaultServerConfig()} // MountManager is nil

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/mounts//mnt/foo", nil)
	req.URL.Path = "/api/v1/mounts//mnt/foo"
	w := httptest.NewRecorder()
	server.handleMount(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("Expected 501, got %d", w.Code)
	}
}

func TestHandleMount_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	mm := newMockMountManager()
	cfg := DefaultServerConfig()
	cfg.MountManager = mm
	server := &Server{config: cfg}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/v1/mounts//mnt/foo", nil)
	req.URL.Path = "/api/v1/mounts//mnt/foo"
	w := httptest.NewRecorder()
	server.handleMount(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected 405, got %d", w.Code)
	}
}

// TestRequestContextReachesTheHandler is why the httptest.NewRequestWithContext migration is worth more
// than a quiet linter.
//
// httptest.NewRequest attaches context.Background(), so every handler test above ran with a context
// that no test could cancel and that carried nothing. This asserts the replacement actually plumbs
// t.Context through: a handler reading r.Context() sees a live context whose Done channel is the
// test's, not a background one that is never done. Without it, "noctx: 0" would be indistinguishable
// from a mechanical rewrite that compiled and changed nothing observable.
func TestRequestContextReachesTheHandler(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil)

	if got := req.Context(); got != t.Context() {
		t.Fatalf("request context = %v, want the test's own context", got)
	}

	// context.Background()'s Done is nil forever; a test context's is a real channel that closes when
	// the test ends. That difference is the whole point of the migration.
	if req.Context().Done() == nil {
		t.Error("request context has a nil Done channel, so nothing can cancel this request")
	}
}
