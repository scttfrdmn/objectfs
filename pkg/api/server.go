// Package api provides HTTP API endpoints for health and status monitoring
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/objectfs/objectfs/pkg/health"
	"github.com/objectfs/objectfs/pkg/status"
)

// Server provides HTTP API endpoints for monitoring
type Server struct {
	httpServer    *http.Server
	statusTracker *status.Tracker
	healthTracker *health.Tracker
	config        ServerConfig
}

// ServerConfig configures the API server
type ServerConfig struct {
	// Address to bind the server to (e.g., "localhost:8080")
	Address string `yaml:"address" json:"address"`

	// ReadTimeout is the maximum duration for reading the entire request
	ReadTimeout time.Duration `yaml:"read_timeout" json:"read_timeout"`

	// WriteTimeout is the maximum duration for writing the response
	WriteTimeout time.Duration `yaml:"write_timeout" json:"write_timeout"`

	// IdleTimeout is the maximum duration to wait for the next request
	IdleTimeout time.Duration `yaml:"idle_timeout" json:"idle_timeout"`

	// EnableCORS enables Cross-Origin Resource Sharing
	EnableCORS bool `yaml:"enable_cors" json:"enable_cors"`

	// EnableMetrics enables Prometheus-style metrics endpoint
	EnableMetrics bool `yaml:"enable_metrics" json:"enable_metrics"`
}

// DefaultServerConfig returns default server configuration
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Address:       "localhost:8080",
		ReadTimeout:   10 * time.Second,
		WriteTimeout:  10 * time.Second,
		IdleTimeout:   60 * time.Second,
		EnableCORS:    true,
		EnableMetrics: false,
	}
}

// NewServer creates a new API server
func NewServer(config ServerConfig, statusTracker *status.Tracker, healthTracker *health.Tracker) *Server {
	s := &Server{
		statusTracker: statusTracker,
		healthTracker: healthTracker,
		config:        config,
	}

	mux := http.NewServeMux()

	// Health endpoints
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/health/components", s.handleHealthComponents)
	mux.HandleFunc("/health/live", s.handleLiveness)
	mux.HandleFunc("/health/ready", s.handleReadiness)

	// Status endpoints
	mux.HandleFunc("/status", s.handleSystemStatus)
	mux.HandleFunc("/status/operations", s.handleOperations)
	mux.HandleFunc("/status/operations/", s.handleOperation)
	mux.HandleFunc("/status/history", s.handleHistory)

	// Metrics endpoint (if enabled)
	if config.EnableMetrics {
		mux.HandleFunc("/metrics", s.handleMetrics)
	}

	// Info endpoint
	mux.HandleFunc("/info", s.handleInfo)

	// Apply middleware
	handler := s.loggingMiddleware(mux)
	if config.EnableCORS {
		handler = s.corsMiddleware(handler)
	}

	s.httpServer = &http.Server{
		Addr:         config.Address,
		Handler:      handler,
		ReadTimeout:  config.ReadTimeout,
		WriteTimeout: config.WriteTimeout,
		IdleTimeout:  config.IdleTimeout,
	}

	return s
}

// Start starts the HTTP server
func (s *Server) Start() error {
	log.Printf("Starting API server on %s", s.config.Address)
	return s.httpServer.ListenAndServe()
}

// StartBackground starts the server in a background goroutine
func (s *Server) StartBackground() {
	go func() {
		if err := s.Start(); err != nil && err != http.ErrServerClosed {
			log.Printf("API server error: %v", err)
		}
	}()
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	log.Printf("Shutting down API server...")
	return s.httpServer.Shutdown(ctx)
}

// Health endpoint handlers

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if s.healthTracker == nil {
		s.respondJSON(w, http.StatusOK, map[string]interface{}{
			"status": "healthy",
			"note":   "Health tracking not configured",
		})
		return
	}

	overallHealth := s.healthTracker.GetOverallHealth()
	components := s.healthTracker.GetAllComponents()

	response := map[string]interface{}{
		"status":     overallHealth.String(),
		"timestamp":  time.Now(),
		"components": len(components),
	}

	statusCode := http.StatusOK
	switch overallHealth {
	case health.StateUnavailable:
		statusCode = http.StatusServiceUnavailable
	case health.StateDegraded, health.StateReadOnly:
		statusCode = http.StatusPartialContent
	}

	s.respondJSON(w, statusCode, response)
}

func (s *Server) handleHealthComponents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if s.healthTracker == nil {
		s.respondError(w, http.StatusServiceUnavailable, "Health tracking not configured")
		return
	}

	components := s.healthTracker.GetAllComponents()
	s.respondJSON(w, http.StatusOK, components)
}

func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Liveness probe - is the service running?
	s.respondJSON(w, http.StatusOK, map[string]interface{}{
		"alive":     true,
		"timestamp": time.Now(),
	})
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Readiness probe - can the service accept traffic?
	if s.healthTracker == nil {
		s.respondJSON(w, http.StatusOK, map[string]interface{}{
			"ready":     true,
			"timestamp": time.Now(),
			"note":      "Health tracking not configured",
		})
		return
	}

	overallHealth := s.healthTracker.GetOverallHealth()
	ready := overallHealth != health.StateUnavailable

	statusCode := http.StatusOK
	if !ready {
		statusCode = http.StatusServiceUnavailable
	}

	s.respondJSON(w, statusCode, map[string]interface{}{
		"ready":     ready,
		"status":    overallHealth.String(),
		"timestamp": time.Now(),
	})
}

// Status endpoint handlers

func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if s.statusTracker == nil {
		s.respondError(w, http.StatusServiceUnavailable, "Status tracking not configured")
		return
	}

	systemStatus := s.statusTracker.GetSystemStatus()
	s.respondJSON(w, http.StatusOK, systemStatus)
}

func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if s.statusTracker == nil {
		s.respondError(w, http.StatusServiceUnavailable, "Status tracking not configured")
		return
	}

	operations := s.statusTracker.GetAllOperations()
	s.respondJSON(w, http.StatusOK, map[string]interface{}{
		"operations": operations,
		"count":      len(operations),
		"timestamp":  time.Now(),
	})
}

func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if s.statusTracker == nil {
		s.respondError(w, http.StatusServiceUnavailable, "Status tracking not configured")
		return
	}

	// Extract operation ID from path
	opID := r.URL.Path[len("/status/operations/"):]
	if opID == "" {
		s.respondError(w, http.StatusBadRequest, "Operation ID required")
		return
	}

	operation, err := s.statusTracker.GetOperation(opID)
	if err != nil {
		s.respondError(w, http.StatusNotFound, fmt.Sprintf("Operation not found: %s", opID))
		return
	}

	s.respondJSON(w, http.StatusOK, operation)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if s.statusTracker == nil {
		s.respondError(w, http.StatusServiceUnavailable, "Status tracking not configured")
		return
	}

	// Get limit from query parameter (default 10)
	limitStr := r.URL.Query().Get("limit")
	limit := 10
	if limitStr != "" {
		if _, err := fmt.Sscanf(limitStr, "%d", &limit); err != nil {
			// If parsing fails, use default limit
			limit = 10
		}
	}

	history := s.statusTracker.GetHistory(limit)
	s.respondJSON(w, http.StatusOK, map[string]interface{}{
		"history":   history,
		"count":     len(history),
		"limit":     limit,
		"timestamp": time.Now(),
	})
}

// Info endpoint

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	info := map[string]interface{}{
		"service":   "ObjectFS API",
		"version":   "0.4.0",
		"timestamp": time.Now(),
		"endpoints": []string{
			"/health",
			"/health/components",
			"/health/live",
			"/health/ready",
			"/status",
			"/status/operations",
			"/status/operations/{id}",
			"/status/history",
			"/info",
		},
	}

	if s.config.EnableMetrics {
		info["endpoints"] = append(info["endpoints"].([]string), "/metrics")
	}

	s.respondJSON(w, http.StatusOK, info)
}

// Metrics endpoint (placeholder)

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Placeholder for Prometheus-style metrics
	// In production, this would integrate with a metrics library
	w.Header().Set("Content-Type", "text/plain")
	if _, err := fmt.Fprintf(w, "# ObjectFS Metrics\n"); err != nil {
		log.Printf("Failed to write metrics header: %v", err)
	}
	if _, err := fmt.Fprintf(w, "# Coming soon\n"); err != nil {
		log.Printf("Failed to write metrics body: %v", err)
	}
}

// Middleware

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("API: %s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
		log.Printf("API: %s %s completed in %v", r.Method, r.URL.Path, time.Since(start))
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Helper methods

func (s *Server) respondJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("Error encoding JSON response: %v", err)
	}
}

func (s *Server) respondError(w http.ResponseWriter, statusCode int, message string) {
	s.respondJSON(w, statusCode, map[string]interface{}{
		"error":     message,
		"timestamp": time.Now(),
	})
}
