package utils

import (
	"context"
	"testing"
	"time"
)

func TestGetDebugManager(t *testing.T) {
	dm1 := GetDebugManager()
	dm2 := GetDebugManager()

	if dm1 != dm2 {
		t.Error("GetDebugManager should return the same instance")
	}
}

func TestStartStopSession(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-1"
	components := []string{"storage", "cache"}

	// Start session
	session := dm.StartSession(sessionID, components, 1000)
	if session == nil {
		t.Fatal("Failed to start session")
	}

	if session.id != sessionID {
		t.Errorf("Expected session ID %s, got %s", sessionID, session.id)
	}

	if !session.enabled {
		t.Error("Session should be enabled")
	}

	// Retrieve session
	retrieved := dm.GetSession(sessionID)
	if retrieved != session {
		t.Error("Retrieved session doesn't match started session")
	}

	// Stop session
	stopped := dm.StopSession(sessionID)
	if stopped != session {
		t.Error("Stopped session doesn't match started session")
	}

	if stopped.enabled {
		t.Error("Session should be disabled after stopping")
	}

	if stopped.endTime.IsZero() {
		t.Error("End time should be set after stopping")
	}
}

func TestRecordEvent(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-2"
	components := []string{"storage"}

	session := dm.StartSession(sessionID, components, 1000)
	defer dm.StopSession(sessionID)

	// Record event
	fields := map[string]interface{}{
		"file": "/test/file.txt",
		"size": 1024,
	}

	dm.RecordEvent("storage", "read", "Reading file", fields)

	// Check event was recorded
	events := session.GetEvents()
	if len(events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(events))
	}

	event := events[0]
	if event.Component != "storage" {
		t.Errorf("Expected component 'storage', got %s", event.Component)
	}
	if event.Operation != "read" {
		t.Errorf("Expected operation 'read', got %s", event.Operation)
	}
	if event.Message != "Reading file" {
		t.Errorf("Expected message 'Reading file', got %s", event.Message)
	}
	if event.Fields["file"] != "/test/file.txt" {
		t.Error("Field 'file' not found or incorrect")
	}
	if event.Fields["size"] != 1024 {
		t.Error("Field 'size' not found or incorrect")
	}
}

func TestComponentFiltering(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-3"
	components := []string{"storage"} // Only track storage

	session := dm.StartSession(sessionID, components, 1000)
	defer dm.StopSession(sessionID)

	// Record storage event (should be tracked)
	dm.RecordEvent("storage", "read", "Storage event", nil)

	// Record cache event (should NOT be tracked)
	dm.RecordEvent("cache", "get", "Cache event", nil)

	// Check only storage event was recorded
	events := session.GetEvents()
	if len(events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(events))
	}

	if events[0].Component != "storage" {
		t.Error("Only storage component should be tracked")
	}
}

func TestRecordEventWithDuration(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-4"
	session := dm.StartSession(sessionID, nil, 1000)
	defer dm.StopSession(sessionID)

	duration := 100 * time.Millisecond
	session.RecordEventWithDuration("storage", "write", "Write completed", nil, duration)

	events := session.GetEvents()
	if len(events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(events))
	}

	if events[0].Duration != duration {
		t.Errorf("Expected duration %v, got %v", duration, events[0].Duration)
	}
}

func TestGetEventsByComponent(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-5"
	session := dm.StartSession(sessionID, nil, 1000)
	defer dm.StopSession(sessionID)

	// Record events for different components
	dm.RecordEvent("storage", "read", "Storage read", nil)
	dm.RecordEvent("storage", "write", "Storage write", nil)
	dm.RecordEvent("cache", "get", "Cache get", nil)
	dm.RecordEvent("cache", "set", "Cache set", nil)

	// Get storage events
	storageEvents := session.GetEventsByComponent("storage")
	if len(storageEvents) != 2 {
		t.Errorf("Expected 2 storage events, got %d", len(storageEvents))
	}

	// Get cache events
	cacheEvents := session.GetEventsByComponent("cache")
	if len(cacheEvents) != 2 {
		t.Errorf("Expected 2 cache events, got %d", len(cacheEvents))
	}
}

func TestMaxEvents(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-6"
	maxEvents := 5
	session := dm.StartSession(sessionID, nil, maxEvents)
	defer dm.StopSession(sessionID)

	// Record more events than max
	for i := 0; i < maxEvents+3; i++ {
		dm.RecordEvent("test", "operation", "Test event", nil)
	}

	events := session.GetEvents()
	if len(events) > maxEvents {
		t.Errorf("Expected at most %d events, got %d", maxEvents, len(events))
	}
}

func TestListSessions(t *testing.T) {
	dm := GetDebugManager()

	// Start multiple sessions
	session1ID := "session-list-1"
	session2ID := "session-list-2"

	dm.StartSession(session1ID, nil, 1000)
	dm.StartSession(session2ID, nil, 1000)

	sessions := dm.ListSessions()

	// Check both sessions are listed
	foundSession1 := false
	foundSession2 := false

	for _, id := range sessions {
		if id == session1ID {
			foundSession1 = true
		}
		if id == session2ID {
			foundSession2 = true
		}
	}

	if !foundSession1 || !foundSession2 {
		t.Error("Not all sessions were listed")
	}

	// Clean up
	dm.StopSession(session1ID)
	dm.StopSession(session2ID)
}

func TestGetStats(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-7"
	components := []string{"storage", "cache"}
	session := dm.StartSession(sessionID, components, 1000)

	// Record some events
	dm.RecordEvent("storage", "read", "Read event", nil)
	dm.RecordEvent("storage", "write", "Write event", nil)
	dm.RecordEvent("cache", "get", "Get event", nil)

	stats := session.GetStats()

	if stats["id"] != sessionID {
		t.Error("Stats should contain session ID")
	}

	if stats["event_count"] != 3 {
		t.Errorf("Expected 3 events in stats, got %v", stats["event_count"])
	}

	if stats["enabled"] != true {
		t.Error("Session should be enabled in stats")
	}

	if stats["components"] != len(components) {
		t.Errorf("Expected %d components in stats, got %v", len(components), stats["components"])
	}

	// Check events by component
	if eventsByComp, ok := stats["events_by_component"].(map[string]int); ok {
		if eventsByComp["storage"] != 2 {
			t.Errorf("Expected 2 storage events in stats, got %d", eventsByComp["storage"])
		}
		if eventsByComp["cache"] != 1 {
			t.Errorf("Expected 1 cache event in stats, got %d", eventsByComp["cache"])
		}
	} else {
		t.Error("events_by_component not found or wrong type in stats")
	}

	dm.StopSession(sessionID)
}

func TestCaptureProfile(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-8"
	session := dm.StartSession(sessionID, nil, 1000)
	defer dm.StopSession(sessionID)

	// Capture goroutine profile
	err := session.CaptureProfile("goroutine")
	if err != nil {
		t.Fatalf("Failed to capture goroutine profile: %v", err)
	}

	// Get the profile
	profile := session.GetProfile("goroutine")
	if profile == nil {
		t.Error("Goroutine profile should not be nil")
	}

	if len(profile) == 0 {
		t.Error("Goroutine profile should not be empty")
	}
}

func TestCaptureProfile_InvalidType(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-9"
	session := dm.StartSession(sessionID, nil, 1000)
	defer dm.StopSession(sessionID)

	// Try to capture invalid profile type
	err := session.CaptureProfile("invalid-profile-type")
	if err == nil {
		t.Error("Expected error for invalid profile type")
	}
}

func TestDebugTrace(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-10"
	session := dm.StartSession(sessionID, nil, 1000)
	defer dm.StopSession(sessionID)

	// Start trace
	fields := map[string]interface{}{
		"operation_id": "op-123",
	}
	trace := StartTrace(sessionID, "storage", "write", fields)

	if trace == nil {
		t.Fatal("Failed to start trace")
	}

	// Simulate some work
	time.Sleep(10 * time.Millisecond)

	// End trace
	trace.End("Write completed successfully")

	// Check event was recorded with duration
	events := session.GetEvents()
	if len(events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(events))
	}

	event := events[0]
	if event.Duration == 0 {
		t.Error("Event duration should not be zero")
	}
	if event.Duration < 10*time.Millisecond {
		t.Error("Event duration seems too short")
	}
}

func TestDebugTrace_WithError(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-11"
	session := dm.StartSession(sessionID, nil, 1000)
	defer dm.StopSession(sessionID)

	// Start trace
	trace := StartTrace(sessionID, "storage", "read", nil)

	// Simulate work with error
	time.Sleep(5 * time.Millisecond)

	// End trace with error
	testErr := context.DeadlineExceeded
	trace.EndWithError(testErr)

	// Check event was recorded with error
	events := session.GetEvents()
	if len(events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(events))
	}

	event := events[0]
	if event.Fields["error"] == nil {
		t.Error("Event should have error field")
	}
	if event.Message != "operation failed" {
		t.Error("Event message should indicate failure")
	}
}

func TestDebugTrace_NoSession(t *testing.T) {
	// Try to start trace with non-existent session
	trace := StartTrace("non-existent-session", "test", "op", nil)

	if trace != nil {
		t.Error("Trace should be nil for non-existent session")
	}
}

func TestContextIntegration(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-12"
	dm.StartSession(sessionID, nil, 1000)
	defer dm.StopSession(sessionID)

	// Add session to context
	ctx := context.Background()
	ctx = WithContext(ctx, sessionID)

	// Extract session from context
	extractedID := FromContext(ctx)

	if extractedID != sessionID {
		t.Errorf("Expected session ID %s, got %s", sessionID, extractedID)
	}
}

func TestContextIntegration_NoSession(t *testing.T) {
	ctx := context.Background()

	// Try to extract from context without session
	extractedID := FromContext(ctx)

	if extractedID != "" {
		t.Errorf("Expected empty session ID, got %s", extractedID)
	}
}

func TestSetLogger(t *testing.T) {
	dm := GetDebugManager()

	// Create a test logger
	config := DefaultStructuredLoggerConfig()
	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	dm.SetLogger(logger)

	// Start a session (should log with the logger)
	sessionID := "test-session-13"
	dm.StartSession(sessionID, nil, 1000)
	dm.StopSession(sessionID)

	// No errors should occur
}

func TestRecordEvent_AfterStop(t *testing.T) {
	dm := GetDebugManager()

	sessionID := "test-session-14"
	session := dm.StartSession(sessionID, nil, 1000)

	// Record event while active
	dm.RecordEvent("test", "op1", "Event 1", nil)

	// Stop session
	dm.StopSession(sessionID)

	// Try to record event after stop (should not be recorded)
	dm.RecordEvent("test", "op2", "Event 2", nil)

	events := session.GetEvents()
	if len(events) != 1 {
		t.Errorf("Expected 1 event (before stop), got %d", len(events))
	}
}

func TestGoroutineID(t *testing.T) {
	id := getGoroutineID()

	if id <= 0 {
		t.Errorf("Expected positive goroutine ID, got %d", id)
	}
}
