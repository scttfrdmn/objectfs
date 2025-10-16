package utils

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewStructuredLogger(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         DEBUG,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: true,
		IncludeStack:  false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	if logger.GetLevel() != DEBUG {
		t.Errorf("Expected DEBUG level, got %v", logger.GetLevel())
	}
}

func TestLogLevels(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         INFO,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	// Debug should not be logged (below INFO)
	logger.Debug("debug message")
	if buf.Len() > 0 {
		t.Error("Debug message was logged when level is INFO")
	}

	// Info should be logged
	buf.Reset()
	logger.Info("info message")
	if buf.Len() == 0 {
		t.Error("Info message was not logged")
	}
	if !strings.Contains(buf.String(), "info message") {
		t.Error("Info message content not found in output")
	}

	// Warn should be logged
	buf.Reset()
	logger.Warn("warn message")
	if buf.Len() == 0 {
		t.Error("Warn message was not logged")
	}
	if !strings.Contains(buf.String(), "warn message") {
		t.Error("Warn message content not found in output")
	}

	// Error should be logged
	buf.Reset()
	logger.Error("error message")
	if buf.Len() == 0 {
		t.Error("Error message was not logged")
	}
	if !strings.Contains(buf.String(), "error message") {
		t.Error("Error message content not found in output")
	}
}

func TestStructuredFields(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         INFO,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	fields := map[string]interface{}{
		"user_id": 123,
		"action":  "login",
		"ip":      "192.168.1.1",
	}

	logger.Info("User logged in", fields)

	output := buf.String()
	if !strings.Contains(output, "user_id=123") {
		t.Error("user_id field not found in output")
	}
	if !strings.Contains(output, "action=login") {
		t.Error("action field not found in output")
	}
	if !strings.Contains(output, "ip=192.168.1.1") {
		t.Error("ip field not found in output")
	}
}

func TestWithField(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         INFO,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	// Create logger with context field
	contextLogger := logger.WithField("request_id", "abc-123")

	// Log message - should include context field
	contextLogger.Info("Processing request")

	output := buf.String()
	if !strings.Contains(output, "request_id=abc-123") {
		t.Error("request_id context field not found in output")
	}
	if !strings.Contains(output, "Processing request") {
		t.Error("Message not found in output")
	}
}

func TestWithFields(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         INFO,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	contextFields := map[string]interface{}{
		"user_id":    456,
		"session_id": "xyz-789",
	}

	contextLogger := logger.WithFields(contextFields)
	contextLogger.Info("Session started")

	output := buf.String()
	if !strings.Contains(output, "user_id=456") {
		t.Error("user_id context field not found in output")
	}
	if !strings.Contains(output, "session_id=xyz-789") {
		t.Error("session_id context field not found in output")
	}
}

func TestWithComponent(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         INFO,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	componentLogger := logger.WithComponent("storage")
	componentLogger.Info("Storage initialized")

	output := buf.String()
	if !strings.Contains(output, "component=storage") {
		t.Error("component field not found in output")
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         INFO,
		Output:        &buf,
		Format:        FormatJSON,
		IncludeCaller: false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	fields := map[string]interface{}{
		"count": 42,
		"name":  "test",
	}

	logger.Info("Test message", fields)

	// Parse JSON output
	var entry LogEntry
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("Failed to parse JSON output: %v", err)
	}

	if entry.Level != "INFO" {
		t.Errorf("Expected level INFO, got %s", entry.Level)
	}

	if entry.Message != "Test message" {
		t.Errorf("Expected message 'Test message', got %s", entry.Message)
	}

	if entry.Fields["count"] != float64(42) {
		t.Errorf("Expected count 42, got %v", entry.Fields["count"])
	}

	if entry.Fields["name"] != "test" {
		t.Errorf("Expected name 'test', got %v", entry.Fields["name"])
	}
}

func TestComponentLevels(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         INFO,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	// Set component-specific level
	logger.SetComponentLevel("storage", DEBUG)

	// Create component loggers
	storageLogger := logger.WithComponent("storage")
	cacheLogger := logger.WithComponent("cache")

	// Debug should be logged for storage (component level is DEBUG)
	buf.Reset()
	storageLogger.Debug("storage debug message")
	if buf.Len() == 0 {
		t.Error("Storage debug message was not logged despite component level being DEBUG")
	}

	// Debug should NOT be logged for cache (global level is INFO)
	buf.Reset()
	cacheLogger.Debug("cache debug message")
	if buf.Len() > 0 {
		t.Error("Cache debug message was logged when global level is INFO")
	}

	// Info should be logged for both
	buf.Reset()
	storageLogger.Info("storage info")
	cacheLogger.Info("cache info")
	output := buf.String()
	if !strings.Contains(output, "storage info") {
		t.Error("Storage info message not found")
	}
	if !strings.Contains(output, "cache info") {
		t.Error("Cache info message not found")
	}
}

func TestFormatfMethods(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         DEBUG,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	// Test Debugf
	buf.Reset()
	logger.Debugf("Debug %s %d", "test", 123)
	if !strings.Contains(buf.String(), "Debug test 123") {
		t.Error("Debugf output incorrect")
	}

	// Test Infof
	buf.Reset()
	logger.Infof("Info %s %d", "test", 456)
	if !strings.Contains(buf.String(), "Info test 456") {
		t.Error("Infof output incorrect")
	}

	// Test Warnf
	buf.Reset()
	logger.Warnf("Warn %s %d", "test", 789)
	if !strings.Contains(buf.String(), "Warn test 789") {
		t.Error("Warnf output incorrect")
	}

	// Test Errorf
	buf.Reset()
	logger.Errorf("Error %s %d", "test", 999)
	if !strings.Contains(buf.String(), "Error test 999") {
		t.Error("Errorf output incorrect")
	}
}

func TestCaller(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         INFO,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: true,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	logger.Info("Test caller")

	output := buf.String()
	// Should contain filename and line number (check for .go: pattern)
	if !strings.Contains(output, ".go:") || !strings.Contains(output, "[") {
		t.Errorf("Caller information not found in output: %s", output)
	}
}

func TestStructuredParseLogLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected LogLevel
	}{
		{"trace", TRACE},
		{"TRACE", TRACE},
		{"debug", DEBUG},
		{"DEBUG", DEBUG},
		{"info", INFO},
		{"INFO", INFO},
		{"warn", WARN},
		{"WARN", WARN},
		{"warning", WARN},
		{"error", ERROR},
		{"ERROR", ERROR},
		{"fatal", FATAL},
		{"FATAL", FATAL},
	}

	for _, tt := range tests {
		result, _ := ParseLogLevel(tt.input)
		if result != tt.expected {
			t.Errorf("ParseLogLevel(%s) = %v, want %v", tt.input, result, tt.expected)
		}
	}
}

func TestStructuredLogLevelString(t *testing.T) {
	tests := []struct {
		level    LogLevel
		expected string
	}{
		{TRACE, "TRACE"},
		{DEBUG, "DEBUG"},
		{INFO, "INFO"},
		{WARN, "WARN"},
		{ERROR, "ERROR"},
		{FATAL, "FATAL"},
	}

	for _, tt := range tests {
		result := tt.level.String()
		if result != tt.expected {
			t.Errorf("LogLevel(%d).String() = %s, want %s", tt.level, result, tt.expected)
		}
	}
}

func TestSetLevel(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         INFO,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	// Initially INFO
	if logger.GetLevel() != INFO {
		t.Errorf("Expected INFO level, got %v", logger.GetLevel())
	}

	// Debug should not log
	logger.Debug("debug message")
	if buf.Len() > 0 {
		t.Error("Debug message logged at INFO level")
	}

	// Change to DEBUG
	logger.SetLevel(DEBUG)
	if logger.GetLevel() != DEBUG {
		t.Errorf("Expected DEBUG level, got %v", logger.GetLevel())
	}

	// Debug should now log
	buf.Reset()
	logger.Debug("debug message")
	if buf.Len() == 0 {
		t.Error("Debug message not logged at DEBUG level")
	}
}

func TestTrace(t *testing.T) {
	var buf bytes.Buffer

	config := &StructuredLoggerConfig{
		Level:         TRACE,
		Output:        &buf,
		Format:        FormatText,
		IncludeCaller: false,
	}

	logger, err := NewStructuredLogger(config)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	logger.Trace("trace message")
	output := buf.String()

	if !strings.Contains(output, "TRACE") {
		t.Error("TRACE level not found in output")
	}
	if !strings.Contains(output, "trace message") {
		t.Error("Trace message not found in output")
	}
}

func TestDefaultConfig(t *testing.T) {
	config := DefaultStructuredLoggerConfig()

	if config.Level != INFO {
		t.Errorf("Expected default level INFO, got %v", config.Level)
	}
	if config.Format != FormatText {
		t.Errorf("Expected default format FormatText, got %v", config.Format)
	}
	if !config.IncludeCaller {
		t.Error("Expected IncludeCaller to be true")
	}
	if config.IncludeStack {
		t.Error("Expected IncludeStack to be false")
	}
}
