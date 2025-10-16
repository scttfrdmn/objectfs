package utils

import (
	"context"
	"fmt"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"time"
)

// DebugSession represents an active debug session
type DebugSession struct {
	mu         sync.RWMutex
	id         string
	startTime  time.Time
	endTime    time.Time
	events     []DebugEvent
	profiles   map[string][]byte
	enabled    bool
	maxEvents  int
	components map[string]bool // Track which components to debug
}

// DebugEvent represents a single debug event
type DebugEvent struct {
	Timestamp  time.Time              `json:"timestamp"`
	Component  string                 `json:"component"`
	Operation  string                 `json:"operation"`
	Message    string                 `json:"message"`
	Fields     map[string]interface{} `json:"fields,omitempty"`
	Duration   time.Duration          `json:"duration,omitempty"`
	Goroutine  int                    `json:"goroutine"`
	StackTrace string                 `json:"stack_trace,omitempty"`
}

// DebugManager manages debug sessions
type DebugManager struct {
	mu       sync.RWMutex
	sessions map[string]*DebugSession
	logger   *StructuredLogger
}

var (
	globalDebugManager *DebugManager
	debugManagerOnce   sync.Once
)

// GetDebugManager returns the global debug manager
func GetDebugManager() *DebugManager {
	debugManagerOnce.Do(func() {
		globalDebugManager = &DebugManager{
			sessions: make(map[string]*DebugSession),
		}
	})
	return globalDebugManager
}

// SetLogger sets the logger for the debug manager
func (dm *DebugManager) SetLogger(logger *StructuredLogger) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.logger = logger
}

// StartSession starts a new debug session
func (dm *DebugManager) StartSession(id string, components []string, maxEvents int) *DebugSession {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	if maxEvents <= 0 {
		maxEvents = 10000 // Default max events
	}

	session := &DebugSession{
		id:         id,
		startTime:  time.Now(),
		events:     make([]DebugEvent, 0, 100),
		profiles:   make(map[string][]byte),
		enabled:    true,
		maxEvents:  maxEvents,
		components: make(map[string]bool),
	}

	// Add components to track
	for _, comp := range components {
		session.components[comp] = true
	}

	dm.sessions[id] = session

	if dm.logger != nil {
		dm.logger.Info("Debug session started", map[string]interface{}{
			"session_id": id,
			"components": components,
		})
	}

	return session
}

// StopSession stops a debug session
func (dm *DebugManager) StopSession(id string) *DebugSession {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	session, exists := dm.sessions[id]
	if !exists {
		return nil
	}

	session.mu.Lock()
	session.enabled = false
	session.endTime = time.Now()
	session.mu.Unlock()

	if dm.logger != nil {
		dm.logger.Info("Debug session stopped", map[string]interface{}{
			"session_id":    id,
			"duration":      session.endTime.Sub(session.startTime),
			"event_count":   len(session.events),
			"profile_count": len(session.profiles),
		})
	}

	return session
}

// GetSession returns a debug session by ID
func (dm *DebugManager) GetSession(id string) *DebugSession {
	dm.mu.RLock()
	defer dm.mu.RUnlock()
	return dm.sessions[id]
}

// ListSessions returns all active sessions
func (dm *DebugManager) ListSessions() []string {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	sessions := make([]string, 0, len(dm.sessions))
	for id := range dm.sessions {
		sessions = append(sessions, id)
	}
	return sessions
}

// RecordEvent records a debug event in active sessions
func (dm *DebugManager) RecordEvent(component, operation, message string, fields map[string]interface{}) {
	dm.mu.RLock()
	sessions := make([]*DebugSession, 0, len(dm.sessions))
	for _, session := range dm.sessions {
		sessions = append(sessions, session)
	}
	dm.mu.RUnlock()

	// Get goroutine ID
	goroutineID := getGoroutineID()

	for _, session := range sessions {
		session.RecordEvent(component, operation, message, fields, goroutineID)
	}
}

// RecordEvent records an event in this session
func (ds *DebugSession) RecordEvent(component, operation, message string, fields map[string]interface{}, goroutineID int) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if !ds.enabled {
		return
	}

	// Check if we're tracking this component
	if len(ds.components) > 0 {
		if !ds.components[component] {
			return
		}
	}

	// Check if we've reached max events
	if len(ds.events) >= ds.maxEvents {
		return
	}

	event := DebugEvent{
		Timestamp: time.Now(),
		Component: component,
		Operation: operation,
		Message:   message,
		Fields:    fields,
		Goroutine: goroutineID,
	}

	ds.events = append(ds.events, event)
}

// RecordEventWithDuration records an event with duration
func (ds *DebugSession) RecordEventWithDuration(component, operation, message string, fields map[string]interface{}, duration time.Duration) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if !ds.enabled {
		return
	}

	// Check if we're tracking this component
	if len(ds.components) > 0 {
		if !ds.components[component] {
			return
		}
	}

	// Check if we've reached max events
	if len(ds.events) >= ds.maxEvents {
		return
	}

	goroutineID := getGoroutineID()

	event := DebugEvent{
		Timestamp: time.Now(),
		Component: component,
		Operation: operation,
		Message:   message,
		Fields:    fields,
		Duration:  duration,
		Goroutine: goroutineID,
	}

	ds.events = append(ds.events, event)
}

// GetEvents returns all recorded events
func (ds *DebugSession) GetEvents() []DebugEvent {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	// Return a copy
	events := make([]DebugEvent, len(ds.events))
	copy(events, ds.events)
	return events
}

// GetEventsByComponent returns events for a specific component
func (ds *DebugSession) GetEventsByComponent(component string) []DebugEvent {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	var events []DebugEvent
	for _, event := range ds.events {
		if event.Component == component {
			events = append(events, event)
		}
	}
	return events
}

// CaptureProfile captures a runtime profile
func (ds *DebugSession) CaptureProfile(profileType string) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if !ds.enabled {
		return fmt.Errorf("session is not active")
	}

	// Get the profile
	var profile *pprof.Profile
	switch profileType {
	case "goroutine":
		profile = pprof.Lookup("goroutine")
	case "heap":
		profile = pprof.Lookup("heap")
	case "threadcreate":
		profile = pprof.Lookup("threadcreate")
	case "block":
		profile = pprof.Lookup("block")
	case "mutex":
		profile = pprof.Lookup("mutex")
	default:
		return fmt.Errorf("unknown profile type: %s", profileType)
	}

	if profile == nil {
		return fmt.Errorf("profile not available: %s", profileType)
	}

	// Write profile to buffer
	var buf strings.Builder
	if err := profile.WriteTo(&buf, 1); err != nil {
		return fmt.Errorf("failed to write profile: %w", err)
	}

	ds.profiles[profileType] = []byte(buf.String())

	return nil
}

// GetProfile returns a captured profile
func (ds *DebugSession) GetProfile(profileType string) []byte {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.profiles[profileType]
}

// GetStats returns statistics about the debug session
func (ds *DebugSession) GetStats() map[string]interface{} {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	stats := map[string]interface{}{
		"id":            ds.id,
		"start_time":    ds.startTime,
		"event_count":   len(ds.events),
		"profile_count": len(ds.profiles),
		"enabled":       ds.enabled,
		"max_events":    ds.maxEvents,
		"components":    len(ds.components),
	}

	if !ds.endTime.IsZero() {
		stats["end_time"] = ds.endTime
		stats["duration"] = ds.endTime.Sub(ds.startTime)
	} else {
		stats["duration"] = time.Since(ds.startTime)
	}

	// Count events by component
	componentCounts := make(map[string]int)
	for _, event := range ds.events {
		componentCounts[event.Component]++
	}
	stats["events_by_component"] = componentCounts

	return stats
}

// DebugTrace is a helper for tracing operations
type DebugTrace struct {
	session   *DebugSession
	component string
	operation string
	startTime time.Time
	fields    map[string]interface{}
}

// StartTrace starts a debug trace
func StartTrace(sessionID, component, operation string, fields map[string]interface{}) *DebugTrace {
	dm := GetDebugManager()
	session := dm.GetSession(sessionID)

	if session == nil || !session.enabled {
		return nil
	}

	return &DebugTrace{
		session:   session,
		component: component,
		operation: operation,
		startTime: time.Now(),
		fields:    fields,
	}
}

// End ends the debug trace and records the event
func (dt *DebugTrace) End(message string) {
	if dt == nil || dt.session == nil {
		return
	}

	duration := time.Since(dt.startTime)
	dt.session.RecordEventWithDuration(dt.component, dt.operation, message, dt.fields, duration)
}

// EndWithError ends the trace with an error
func (dt *DebugTrace) EndWithError(err error) {
	if dt == nil || dt.session == nil {
		return
	}

	duration := time.Since(dt.startTime)
	if dt.fields == nil {
		dt.fields = make(map[string]interface{})
	}
	dt.fields["error"] = err.Error()

	dt.session.RecordEventWithDuration(dt.component, dt.operation, "operation failed", dt.fields, duration)
}

// WithContext adds a debug session ID to a context
func WithContext(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, debugSessionKey, sessionID)
}

// FromContext extracts the debug session ID from a context
func FromContext(ctx context.Context) string {
	if sessionID, ok := ctx.Value(debugSessionKey).(string); ok {
		return sessionID
	}
	return ""
}

type contextKey string

const debugSessionKey contextKey = "debug_session"

// getGoroutineID returns the current goroutine ID
func getGoroutineID() int {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	idField := strings.Fields(strings.TrimPrefix(string(buf[:n]), "goroutine "))[0]

	var id int
	_, _ = fmt.Sscanf(idField, "%d", &id)
	return id
}

// EnableRuntimeProfiling enables runtime profiling for debugging
func EnableRuntimeProfiling() {
	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)
}

// DisableRuntimeProfiling disables runtime profiling
func DisableRuntimeProfiling() {
	runtime.SetBlockProfileRate(0)
	runtime.SetMutexProfileFraction(0)
}
