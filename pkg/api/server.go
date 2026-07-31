// Package api provides HTTP API endpoints for health and status monitoring
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/objectfs/objectfs/pkg/health"
	"github.com/objectfs/objectfs/pkg/status"
)

// MountManager handles mount/unmount operations.
type MountManager interface {
	Mount(mountPoint string, opts MountOptions) error
	Unmount(mountPoint string) error
	IsMounted(mountPoint string) bool
	ListMounts() []MountInfo
}

// MountOptions contains options for mounting a filesystem.
type MountOptions struct {
	StorageURI string `json:"storage_uri"`
	ReadOnly   bool   `json:"read_only,omitempty"`
}

// MountInfo contains information about a mounted filesystem.
type MountInfo struct {
	MountPoint string `json:"mount_point"`
	StorageURI string `json:"storage_uri"`
	ReadOnly   bool   `json:"read_only"`
	MountedAt  string `json:"mounted_at,omitempty"`
}

// Server provides HTTP API endpoints for monitoring
type Server struct {
	httpServer    *http.Server
	statusTracker *status.Tracker
	healthTracker *health.Tracker
	gatherer      prometheus.Gatherer
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

	// Version is the application version reported by the /info endpoint.
	Version string `yaml:"version" json:"version"`

	// MountManager handles mount/unmount operations via the REST API.
	// When nil, mount-related endpoints return 501 Not Implemented.
	MountManager MountManager `yaml:"-" json:"-"`
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

// NewServer creates a new API server. Pass a non-nil prometheus.Gatherer to
// enable real Prometheus text-format output at /metrics; pass nil to keep the
// endpoint returning an empty metric set.
func NewServer(config ServerConfig, statusTracker *status.Tracker, healthTracker *health.Tracker, gatherer prometheus.Gatherer) *Server {
	s := &Server{
		statusTracker: statusTracker,
		healthTracker: healthTracker,
		gatherer:      gatherer,
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

	// Mount endpoints
	mux.HandleFunc("/api/v1/mounts", s.handleMounts)
	mux.HandleFunc("/api/v1/mounts/", s.handleMount)

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
	slog.Info("starting API server", "address", s.config.Address)
	return s.httpServer.ListenAndServe()
}

// StartBackground starts the server in a background goroutine
func (s *Server) StartBackground() {
	go func() {
		if err := s.Start(); err != nil && err != http.ErrServerClosed {
			slog.Error("API server error", "error", err)
		}
	}()
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	slog.Info("shutting down API server")
	return s.httpServer.Shutdown(ctx)
}

// Health endpoint handlers

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if s.healthTracker == nil {
		s.respondJSON(w, http.StatusOK, map[string]any{
			"status": "healthy",
			"note":   "Health tracking not configured",
		})
		return
	}

	overallHealth := s.healthTracker.GetOverallHealth()
	components := s.healthTracker.GetAllComponents()

	response := map[string]any{
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
	s.respondJSON(w, http.StatusOK, map[string]any{
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
		s.respondJSON(w, http.StatusOK, map[string]any{
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

	s.respondJSON(w, statusCode, map[string]any{
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
	s.respondJSON(w, http.StatusOK, map[string]any{
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
	s.respondJSON(w, http.StatusOK, map[string]any{
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

	version := s.config.Version
	if version == "" {
		version = "unknown"
	}

	info := map[string]any{
		"service":   "ObjectFS API",
		"version":   version,
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
			"/api/v1/mounts",
			"/api/v1/mounts/{point}",
			"/info",
		},
	}

	if s.config.EnableMetrics {
		info["endpoints"] = append(info["endpoints"].([]string), "/metrics")
	}

	s.respondJSON(w, http.StatusOK, info)
}

// Metrics endpoint

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if s.gatherer == nil {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}

	promhttp.HandlerFor(s.gatherer, promhttp.HandlerOpts{
		ErrorLog:      log.Default(),
		ErrorHandling: promhttp.HTTPErrorOnError,
	}).ServeHTTP(w, r)
}

// Mount endpoint handlers

// handleMounts handles GET /api/v1/mounts (list) and POST /api/v1/mounts (mount).
func (s *Server) handleMounts(w http.ResponseWriter, r *http.Request) {
	if s.config.MountManager == nil {
		s.respondError(w, http.StatusNotImplemented, "Mount management not configured")
		return
	}

	switch r.Method {
	case http.MethodGet:
		mounts := s.config.MountManager.ListMounts()
		s.respondJSON(w, http.StatusOK, map[string]any{
			"mounts":    mounts,
			"count":     len(mounts),
			"timestamp": time.Now(),
		})

	case http.MethodPost:
		var req struct {
			MountPoint string       `json:"mount_point"`
			Options    MountOptions `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.respondError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err))
			return
		}
		if req.MountPoint == "" {
			s.respondError(w, http.StatusBadRequest, "mount_point is required")
			return
		}
		if err := s.config.MountManager.Mount(req.MountPoint, req.Options); err != nil {
			s.respondError(w, http.StatusInternalServerError, fmt.Sprintf("Mount failed: %v", err))
			return
		}
		s.respondJSON(w, http.StatusCreated, map[string]any{
			"mount_point": req.MountPoint,
			"mounted":     true,
			"timestamp":   time.Now(),
		})

	default:
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// handleMount handles GET/DELETE /api/v1/mounts/{point}.
func (s *Server) handleMount(w http.ResponseWriter, r *http.Request) {
	if s.config.MountManager == nil {
		s.respondError(w, http.StatusNotImplemented, "Mount management not configured")
		return
	}

	// Extract and URL-decode the mount point from the path.
	rawPoint := strings.TrimPrefix(r.URL.Path, "/api/v1/mounts/")
	if rawPoint == "" {
		s.respondError(w, http.StatusBadRequest, "Mount point required")
		return
	}
	mountPoint, err := url.PathUnescape(rawPoint)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, "Invalid mount point encoding")
		return
	}

	switch r.Method {
	case http.MethodGet:
		mounted := s.config.MountManager.IsMounted(mountPoint)
		s.respondJSON(w, http.StatusOK, map[string]any{
			"mount_point": mountPoint,
			"mounted":     mounted,
			"timestamp":   time.Now(),
		})

	case http.MethodDelete:
		if err := s.config.MountManager.Unmount(mountPoint); err != nil {
			s.respondError(w, http.StatusInternalServerError, fmt.Sprintf("Unmount failed: %v", err))
			return
		}
		s.respondJSON(w, http.StatusOK, map[string]any{
			"mount_point": mountPoint,
			"mounted":     false,
			"timestamp":   time.Now(),
		})

	default:
		s.respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// Middleware

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		slog.Info("API request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
		slog.Info("API request completed", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Helper methods

func (s *Server) respondJSON(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("error encoding JSON response", "error", err)
	}
}

func (s *Server) respondError(w http.ResponseWriter, statusCode int, message string) {
	s.respondJSON(w, statusCode, map[string]any{
		"error":     message,
		"timestamp": time.Now(),
	})
}
