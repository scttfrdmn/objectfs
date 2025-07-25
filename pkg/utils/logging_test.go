package utils

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected LogLevel
		wantErr  bool
	}{
		{
			name:     "debug level",
			input:    "DEBUG",
			expected: DEBUG,
			wantErr:  false,
		},
		{
			name:     "info level",
			input:    "INFO",
			expected: INFO,
			wantErr:  false,
		},
		{
			name:     "warn level",
			input:    "WARN",
			expected: WARN,
			wantErr:  false,
		},
		{
			name:     "warning level",
			input:    "WARNING",
			expected: WARN,
			wantErr:  false,
		},
		{
			name:     "error level",
			input:    "ERROR",
			expected: ERROR,
			wantErr:  false,
		},
		{
			name:     "case insensitive",
			input:    "debug",
			expected: DEBUG,
			wantErr:  false,
		},
		{
			name:     "invalid level",
			input:    "INVALID",
			expected: INFO,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseLogLevel(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseLogLevel() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if result != tt.expected {
				t.Errorf("ParseLogLevel() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestLogLevelString(t *testing.T) {
	tests := []struct {
		level    LogLevel
		expected string
	}{
		{DEBUG, "DEBUG"},
		{INFO, "INFO"},
		{WARN, "WARN"},
		{ERROR, "ERROR"},
		{LogLevel(999), "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := tt.level.String()
			if result != tt.expected {
				t.Errorf("LogLevel.String() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(DEBUG, &buf)

	// Test all log levels
	logger.Debug("debug message %s", "arg")
	logger.Info("info message %s", "arg")
	logger.Warn("warn message %s", "arg")
	logger.Error("error message %s", "arg")

	output := buf.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")

	if len(lines) != 4 {
		t.Errorf("Expected 4 log lines, got %d", len(lines))
	}

	expectedContains := []string{
		"[DEBUG] debug message arg",
		"[INFO] info message arg",
		"[WARN] warn message arg",
		"[ERROR] error message arg",
	}

	for i, expected := range expectedContains {
		if i < len(lines) && !strings.Contains(lines[i], expected) {
			t.Errorf("Line %d does not contain expected text. Got: %s, Expected: %s", i, lines[i], expected)
		}
	}
}

func TestLoggerLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(WARN, &buf)

	// Test that lower level messages are filtered out
	logger.Debug("debug message")
	logger.Info("info message")
	logger.Warn("warn message")
	logger.Error("error message")

	output := buf.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")

	// Should only have WARN and ERROR messages
	expectedLines := 2
	if len(lines) != expectedLines {
		t.Errorf("Expected %d log lines, got %d", expectedLines, len(lines))
	}

	if !strings.Contains(output, "[WARN]") {
		t.Error("Expected WARN message in output")
	}
	if !strings.Contains(output, "[ERROR]") {
		t.Error("Expected ERROR message in output")
	}
	if strings.Contains(output, "[DEBUG]") {
		t.Error("DEBUG message should be filtered out")
	}
	if strings.Contains(output, "[INFO]") {
		t.Error("INFO message should be filtered out")
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		name     string
		bytes    int64
		expected string
	}{
		{
			name:     "zero bytes",
			bytes:    0,
			expected: "0 B",
		},
		{
			name:     "bytes",
			bytes:    512,
			expected: "512 B",
		},
		{
			name:     "kilobytes",
			bytes:    1024,
			expected: "1.0 KB",
		},
		{
			name:     "megabytes",
			bytes:    1024 * 1024,
			expected: "1.0 MB",
		},
		{
			name:     "gigabytes",
			bytes:    1024 * 1024 * 1024,
			expected: "1.0 GB",
		},
		{
			name:     "terabytes",
			bytes:    1024 * 1024 * 1024 * 1024,
			expected: "1.0 TB",
		},
		{
			name:     "fractional",
			bytes:    1536, // 1.5 KB
			expected: "1.5 KB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatBytes(tt.bytes)
			if result != tt.expected {
				t.Errorf("FormatBytes() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestParseBytes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
		wantErr  bool
	}{
		{
			name:     "bytes",
			input:    "512",
			expected: 512,
			wantErr:  false,
		},
		{
			name:     "bytes with B suffix",
			input:    "512B",
			expected: 512,
			wantErr:  false,
		},
		{
			name:     "kilobytes",
			input:    "1K",
			expected: 1024,
			wantErr:  false,
		},
		{
			name:     "kilobytes with B suffix",
			input:    "2KB",
			expected: 2048,
			wantErr:  false,
		},
		{
			name:     "megabytes",
			input:    "1M",
			expected: 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "megabytes with B suffix",
			input:    "5MB",
			expected: 5 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "gigabytes",
			input:    "2G",
			expected: 2 * 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "gigabytes with B suffix",
			input:    "1GB",
			expected: 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "terabytes",
			input:    "1T",
			expected: 1024 * 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "terabytes with B suffix",
			input:    "2TB",
			expected: 2 * 1024 * 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "petabytes",
			input:    "1P",
			expected: 1024 * 1024 * 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "fractional",
			input:    "1.5G",
			expected: int64(1.5 * 1024 * 1024 * 1024),
			wantErr:  false,
		},
		{
			name:     "case insensitive",
			input:    "1gb",
			expected: 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "with spaces",
			input:    " 2 GB ",
			expected: 2 * 1024 * 1024 * 1024,
			wantErr:  false,
		},
		{
			name:     "empty string",
			input:    "",
			expected: 0,
			wantErr:  true,
		},
		{
			name:     "invalid format",
			input:    "invalid",
			expected: 0,
			wantErr:  true,
		},
		{
			name:     "invalid number",
			input:    "XGB",
			expected: 0,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseBytes(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseBytes() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if result != tt.expected {
				t.Errorf("ParseBytes() = %v, want %v", result, tt.expected)
			}
		})
	}
}