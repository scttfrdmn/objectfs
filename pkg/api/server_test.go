package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/objectfs/objectfs/pkg/errors"
	"github.com/objectfs/objectfs/pkg/health"
	"github.com/objectfs/objectfs/pkg/status"
)

func TestNewServer(t *testing.T) {
	config := DefaultServerConfig()
	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	healthTracker := health.NewTracker(health.DefaultConfig())

	server := NewServer(config, statusTracker, healthTracker)

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
	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("test-service")

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response["status"] != "healthy" {
		t.Errorf("Expected status=healthy, got %v", response["status"])
	}
}

func TestHandleHealthDegraded(t *testing.T) {
	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("test-service")

	// Make service degraded
	for i := 0; i < 3; i++ {
		healthTracker.RecordError("test-service", fmt.Errorf("test error"))
	}

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusPartialContent {
		t.Errorf("Expected status 206, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response["status"] != "degraded" {
		t.Errorf("Expected status=degraded, got %v", response["status"])
	}
}

func TestHandleHealthComponents(t *testing.T) {
	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("service-1")
	healthTracker.RegisterComponent("service-2")

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/health/components", nil)
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
	server := &Server{
		config: DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	w := httptest.NewRecorder()

	server.handleLiveness(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if alive, ok := response["alive"].(bool); !ok || !alive {
		t.Error("Expected alive=true")
	}
}

func TestHandleReadiness(t *testing.T) {
	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("test-service")

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	w := httptest.NewRecorder()

	server.handleReadiness(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if ready, ok := response["ready"].(bool); !ok || !ready {
		t.Error("Expected ready=true")
	}
}

func TestHandleReadinessUnavailable(t *testing.T) {
	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("test-service")

	// Make service unavailable
	for i := 0; i < 10; i++ {
		healthTracker.RecordError("test-service", fmt.Errorf("test error"))
	}

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	w := httptest.NewRecorder()

	server.handleReadiness(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if ready, ok := response["ready"].(bool); !ok || ready {
		t.Error("Expected ready=false")
	}
}

func TestHandleSystemStatus(t *testing.T) {
	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	ctx := context.Background()

	// Start some operations
	statusTracker.StartOperation(ctx, "read", nil)
	statusTracker.StartOperation(ctx, "write", nil)

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
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
	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	ctx := context.Background()

	// Start operations
	op1, _ := statusTracker.StartOperation(ctx, "read", nil)
	op2, _ := statusTracker.StartOperation(ctx, "write", nil)

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/status/operations", nil)
	w := httptest.NewRecorder()

	server.handleOperations(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	count := int(response["count"].(float64))
	if count != 2 {
		t.Errorf("Expected 2 operations, got %d", count)
	}

	// Verify we can access the operations
	_, _ = op1, op2
}

func TestHandleOperation(t *testing.T) {
	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	ctx := context.Background()

	op, _ := statusTracker.StartOperation(ctx, "test", nil)

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/status/operations/"+op.ID, nil)
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
	statusTracker := status.NewTracker(status.DefaultTrackerConfig())

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/status/operations/non-existent", nil)
	w := httptest.NewRecorder()

	server.handleOperation(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}
}

func TestHandleHistory(t *testing.T) {
	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	ctx := context.Background()

	// Complete some operations
	for i := 0; i < 3; i++ {
		op, _ := statusTracker.StartOperation(ctx, fmt.Sprintf("op-%d", i), nil)
		if err := statusTracker.CompleteOperation(op.ID); err != nil {
			t.Fatalf("Failed to complete operation: %v", err)
		}
	}

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/status/history?limit=2", nil)
	w := httptest.NewRecorder()

	server.handleHistory(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	count := int(response["count"].(float64))
	if count != 2 {
		t.Errorf("Expected 2 history entries, got %d", count)
	}

	limit := int(response["limit"].(float64))
	if limit != 2 {
		t.Errorf("Expected limit=2, got %d", limit)
	}
}

func TestHandleInfo(t *testing.T) {
	server := &Server{
		config: DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/info", nil)
	w := httptest.NewRecorder()

	server.handleInfo(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response["service"] != "ObjectFS API" {
		t.Errorf("Expected service='ObjectFS API', got %v", response["service"])
	}

	if response["version"] != "0.4.0" {
		t.Errorf("Expected version='0.4.0', got %v", response["version"])
	}
}

func TestMethodNotAllowed(t *testing.T) {
	server := &Server{
		config: DefaultServerConfig(),
	}

	// Test POST on GET-only endpoint
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", w.Code)
	}
}

func TestCORSMiddleware(t *testing.T) {
	config := DefaultServerConfig()
	config.EnableCORS = true

	server := NewServer(config, nil, nil)

	req := httptest.NewRequest(http.MethodOptions, "/health", nil)
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
	config := DefaultServerConfig()
	config.Address = "localhost:0" // Use random available port

	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	server := NewServer(config, statusTracker, nil)

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
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
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

	req := httptest.NewRequest(http.MethodGet, "/health", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		server.handleHealth(w, req)
	}
}

func BenchmarkHandleOperations(b *testing.B) {
	statusTracker := status.NewTracker(status.DefaultTrackerConfig())
	ctx := context.Background()

	// Create some operations
	for i := 0; i < 10; i++ {
		statusTracker.StartOperation(ctx, "test", nil)
	}

	server := &Server{
		statusTracker: statusTracker,
		config:        DefaultServerConfig(),
	}

	req := httptest.NewRequest(http.MethodGet, "/status/operations", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		server.handleOperations(w, req)
	}
}

// Test with actual errors integration

func TestHealthWithActualErrors(t *testing.T) {
	healthTracker := health.NewTracker(health.DefaultConfig())
	healthTracker.RegisterComponent("storage")

	server := &Server{
		healthTracker: healthTracker,
		config:        DefaultServerConfig(),
	}

	// Record write errors to trigger read-only mode
	writeErr := errors.NewError(errors.ErrCodeStorageWrite, "write failed")
	for i := 0; i < 3; i++ {
		healthTracker.RecordError("storage", writeErr)
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusPartialContent {
		t.Errorf("Expected status 206, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response["status"] != "read-only" {
		t.Errorf("Expected status=read-only, got %v", response["status"])
	}
}
