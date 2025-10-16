package utils

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// LogFormat defines the output format for logs
type LogFormat int

const (
	FormatText LogFormat = iota
	FormatJSON
)

// Field represents a structured logging field
type Field struct {
	Key   string
	Value interface{}
}

// LogEntry represents a complete log entry
type LogEntry struct {
	Timestamp time.Time              `json:"timestamp"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
	Caller    string                 `json:"caller,omitempty"`
	Stack     string                 `json:"stack,omitempty"`
}

// StructuredLogger provides structured logging with levels and fields
type StructuredLogger struct {
	mu              sync.RWMutex
	level           LogLevel
	output          io.Writer
	format          LogFormat
	contextFields   map[string]interface{}
	includeCaller   bool
	includeStack    bool // Only for ERROR and FATAL
	componentLevels map[string]LogLevel
	rotator         *LogRotator
}

// StructuredLoggerConfig holds configuration for the logger
type StructuredLoggerConfig struct {
	Level         LogLevel
	Output        io.Writer
	Format        LogFormat
	IncludeCaller bool
	IncludeStack  bool
	Rotation      *RotationConfig
}

// DefaultStructuredLoggerConfig returns default configuration
func DefaultStructuredLoggerConfig() *StructuredLoggerConfig {
	return &StructuredLoggerConfig{
		Level:         INFO,
		Output:        os.Stdout,
		Format:        FormatText,
		IncludeCaller: true,
		IncludeStack:  false,
	}
}

// NewStructuredLogger creates a new structured logger
func NewStructuredLogger(config *StructuredLoggerConfig) (*StructuredLogger, error) {
	if config == nil {
		config = DefaultStructuredLoggerConfig()
	}

	logger := &StructuredLogger{
		level:           config.Level,
		output:          config.Output,
		format:          config.Format,
		contextFields:   make(map[string]interface{}),
		includeCaller:   config.IncludeCaller,
		includeStack:    config.IncludeStack,
		componentLevels: make(map[string]LogLevel),
	}

	// Setup log rotation if configured
	if config.Rotation != nil {
		rotator, err := NewLogRotator(config.Rotation)
		if err != nil {
			return nil, fmt.Errorf("failed to create log rotator: %w", err)
		}
		logger.rotator = rotator
		logger.output = rotator
	}

	return logger, nil
}

// WithField returns a new logger with an additional context field
func (sl *StructuredLogger) WithField(key string, value interface{}) *StructuredLogger {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	newFields := make(map[string]interface{}, len(sl.contextFields)+1)
	for k, v := range sl.contextFields {
		newFields[k] = v
	}
	newFields[key] = value

	return &StructuredLogger{
		level:           sl.level,
		output:          sl.output,
		format:          sl.format,
		contextFields:   newFields,
		includeCaller:   sl.includeCaller,
		includeStack:    sl.includeStack,
		componentLevels: sl.componentLevels,
		rotator:         sl.rotator,
	}
}

// WithFields returns a new logger with multiple context fields
func (sl *StructuredLogger) WithFields(fields map[string]interface{}) *StructuredLogger {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	newFields := make(map[string]interface{}, len(sl.contextFields)+len(fields))
	for k, v := range sl.contextFields {
		newFields[k] = v
	}
	for k, v := range fields {
		newFields[k] = v
	}

	return &StructuredLogger{
		level:           sl.level,
		output:          sl.output,
		format:          sl.format,
		contextFields:   newFields,
		includeCaller:   sl.includeCaller,
		includeStack:    sl.includeStack,
		componentLevels: sl.componentLevels,
		rotator:         sl.rotator,
	}
}

// WithComponent returns a logger with a component field
func (sl *StructuredLogger) WithComponent(component string) *StructuredLogger {
	return sl.WithField("component", component)
}

// SetComponentLevel sets the log level for a specific component
func (sl *StructuredLogger) SetComponentLevel(component string, level LogLevel) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	sl.componentLevels[component] = level
}

// SetLevel sets the global log level
func (sl *StructuredLogger) SetLevel(level LogLevel) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	sl.level = level
}

// GetLevel returns the current log level
func (sl *StructuredLogger) GetLevel() LogLevel {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return sl.level
}

// isEnabled checks if a log level is enabled for the current component
func (sl *StructuredLogger) isEnabled(level LogLevel) bool {
	sl.mu.RLock()
	defer sl.mu.RUnlock()

	// Check component-specific level if component field exists
	if component, ok := sl.contextFields["component"]; ok {
		if compStr, ok := component.(string); ok {
			if compLevel, exists := sl.componentLevels[compStr]; exists {
				return level >= compLevel
			}
		}
	}

	// Fall back to global level
	return level >= sl.level
}

// log writes a log entry
func (sl *StructuredLogger) log(level LogLevel, message string, fields map[string]interface{}) {
	if !sl.isEnabled(level) {
		return
	}

	entry := LogEntry{
		Timestamp: time.Now(),
		Level:     level.String(),
		Message:   message,
		Fields:    make(map[string]interface{}),
	}

	// Add context fields
	sl.mu.RLock()
	for k, v := range sl.contextFields {
		entry.Fields[k] = v
	}
	sl.mu.RUnlock()

	// Add provided fields
	for k, v := range fields {
		entry.Fields[k] = v
	}

	// Add caller information if enabled
	if sl.includeCaller {
		if _, file, line, ok := runtime.Caller(2); ok {
			// Get just the filename, not the full path
			parts := strings.Split(file, "/")
			entry.Caller = fmt.Sprintf("%s:%d", parts[len(parts)-1], line)
		}
	}

	// Add stack trace for errors if enabled
	if sl.includeStack && (level == ERROR || level == FATAL) {
		buf := make([]byte, 4096)
		n := runtime.Stack(buf, false)
		entry.Stack = string(buf[:n])
	}

	// Format and write the entry
	var output string
	if sl.format == FormatJSON {
		jsonBytes, err := json.Marshal(entry)
		if err != nil {
			// Fall back to text format on JSON error
			output = sl.formatText(entry)
		} else {
			output = string(jsonBytes) + "\n"
		}
	} else {
		output = sl.formatText(entry)
	}

	sl.mu.Lock()
	defer sl.mu.Unlock()
	_, _ = sl.output.Write([]byte(output))
}

// formatText formats a log entry as human-readable text
func (sl *StructuredLogger) formatText(entry LogEntry) string {
	var sb strings.Builder

	// Timestamp and level
	sb.WriteString(entry.Timestamp.Format("2006-01-02 15:04:05.000"))
	sb.WriteString(" [")
	sb.WriteString(entry.Level)
	sb.WriteString("] ")

	// Caller
	if entry.Caller != "" {
		sb.WriteString("[")
		sb.WriteString(entry.Caller)
		sb.WriteString("] ")
	}

	// Message
	sb.WriteString(entry.Message)

	// Fields
	if len(entry.Fields) > 0 {
		sb.WriteString(" {")
		first := true
		for k, v := range entry.Fields {
			if !first {
				sb.WriteString(", ")
			}
			first = false
			sb.WriteString(k)
			sb.WriteString("=")
			sb.WriteString(fmt.Sprintf("%v", v))
		}
		sb.WriteString("}")
	}

	sb.WriteString("\n")

	// Stack trace if present
	if entry.Stack != "" {
		sb.WriteString("Stack trace:\n")
		sb.WriteString(entry.Stack)
		sb.WriteString("\n")
	}

	return sb.String()
}

// Trace logs a trace message
func (sl *StructuredLogger) Trace(message string, fields ...map[string]interface{}) {
	sl.logWithFields(TRACE, message, fields...)
}

// Debug logs a debug message
func (sl *StructuredLogger) Debug(message string, fields ...map[string]interface{}) {
	sl.logWithFields(DEBUG, message, fields...)
}

// Info logs an info message
func (sl *StructuredLogger) Info(message string, fields ...map[string]interface{}) {
	sl.logWithFields(INFO, message, fields...)
}

// Warn logs a warning message
func (sl *StructuredLogger) Warn(message string, fields ...map[string]interface{}) {
	sl.logWithFields(WARN, message, fields...)
}

// Error logs an error message
func (sl *StructuredLogger) Error(message string, fields ...map[string]interface{}) {
	sl.logWithFields(ERROR, message, fields...)
}

// Fatal logs a fatal message and exits
func (sl *StructuredLogger) Fatal(message string, fields ...map[string]interface{}) {
	sl.logWithFields(FATAL, message, fields...)
	os.Exit(1)
}

// logWithFields is a helper to log with optional field maps
func (sl *StructuredLogger) logWithFields(level LogLevel, message string, fieldMaps ...map[string]interface{}) {
	var fields map[string]interface{}
	if len(fieldMaps) > 0 && fieldMaps[0] != nil {
		fields = fieldMaps[0]
	}
	sl.log(level, message, fields)
}

// Tracef logs a formatted trace message
func (sl *StructuredLogger) Tracef(format string, args ...interface{}) {
	sl.log(TRACE, fmt.Sprintf(format, args...), nil)
}

// Debugf logs a formatted debug message
func (sl *StructuredLogger) Debugf(format string, args ...interface{}) {
	sl.log(DEBUG, fmt.Sprintf(format, args...), nil)
}

// Infof logs a formatted info message
func (sl *StructuredLogger) Infof(format string, args ...interface{}) {
	sl.log(INFO, fmt.Sprintf(format, args...), nil)
}

// Warnf logs a formatted warning message
func (sl *StructuredLogger) Warnf(format string, args ...interface{}) {
	sl.log(WARN, fmt.Sprintf(format, args...), nil)
}

// Errorf logs a formatted error message
func (sl *StructuredLogger) Errorf(format string, args ...interface{}) {
	sl.log(ERROR, fmt.Sprintf(format, args...), nil)
}

// Fatalf logs a formatted fatal message and exits
func (sl *StructuredLogger) Fatalf(format string, args ...interface{}) {
	sl.log(FATAL, fmt.Sprintf(format, args...), nil)
	os.Exit(1)
}

// Close closes the logger and any associated resources
func (sl *StructuredLogger) Close() error {
	if sl.rotator != nil {
		return sl.rotator.Close()
	}
	return nil
}

// Sync flushes any buffered log entries
func (sl *StructuredLogger) Sync() error {
	if sl.rotator != nil {
		return sl.rotator.Sync()
	}
	return nil
}
