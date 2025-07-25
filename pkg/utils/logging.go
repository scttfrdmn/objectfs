package utils

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// LogLevel represents the logging level
type LogLevel int

const (
	DEBUG LogLevel = iota
	INFO
	WARN
	ERROR
)

// String returns the string representation of the log level
func (l LogLevel) String() string {
	switch l {
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO"
	case WARN:
		return "WARN"
	case ERROR:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// ParseLogLevel parses a string log level
func ParseLogLevel(level string) (LogLevel, error) {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return DEBUG, nil
	case "INFO":
		return INFO, nil
	case "WARN", "WARNING":
		return WARN, nil
	case "ERROR":
		return ERROR, nil
	default:
		return INFO, fmt.Errorf("invalid log level: %s", level)
	}
}

// Logger represents a configurable logger
type Logger struct {
	level  LogLevel
	output io.Writer
}

// NewLogger creates a new logger with the specified level and output
func NewLogger(level LogLevel, output io.Writer) *Logger {
	return &Logger{
		level:  level,
		output: output,
	}
}

// Debug logs a debug message
func (l *Logger) Debug(format string, args ...interface{}) {
	if l.level <= DEBUG {
		l.log("DEBUG", format, args...)
	}
}

// Info logs an info message
func (l *Logger) Info(format string, args ...interface{}) {
	if l.level <= INFO {
		l.log("INFO", format, args...)
	}
}

// Warn logs a warning message
func (l *Logger) Warn(format string, args ...interface{}) {
	if l.level <= WARN {
		l.log("WARN", format, args...)
	}
}

// Error logs an error message
func (l *Logger) Error(format string, args ...interface{}) {
	if l.level <= ERROR {
		l.log("ERROR", format, args...)
	}
}

// log writes a log message
func (l *Logger) log(level, format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.output, "[%s] %s\n", level, message)
}

// SetupLogging configures the global logger
func SetupLogging(levelStr, logFile string) error {
	// Parse log level
	level, err := ParseLogLevel(levelStr)
	if err != nil {
		return fmt.Errorf("invalid log level: %w", err)
	}

	// Determine output destination
	var output io.Writer = os.Stdout
	
	if logFile != "" {
		file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed to open log file: %w", err)
		}
		output = file
	}

	// Configure the global logger
	log.SetOutput(output)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	return nil
}

// FormatBytes formats bytes as human-readable string
func FormatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// ParseBytes parses a human-readable byte string
func ParseBytes(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty string")
	}

	s = strings.ToUpper(strings.TrimSpace(s))
	
	// Handle plain numbers
	if strings.HasSuffix(s, "B") {
		s = s[:len(s)-1]
	}
	
	var multiplier int64 = 1
	var numStr string
	
	if len(s) > 0 {
		lastChar := s[len(s)-1]
		switch lastChar {
		case 'K':
			multiplier = 1024
			numStr = s[:len(s)-1]
		case 'M':
			multiplier = 1024 * 1024
			numStr = s[:len(s)-1]
		case 'G':
			multiplier = 1024 * 1024 * 1024
			numStr = s[:len(s)-1]
		case 'T':
			multiplier = 1024 * 1024 * 1024 * 1024
			numStr = s[:len(s)-1]
		case 'P':
			multiplier = 1024 * 1024 * 1024 * 1024 * 1024
			numStr = s[:len(s)-1]
		default:
			numStr = s
		}
	}
	
	var num float64
	if _, err := fmt.Sscanf(numStr, "%f", &num); err != nil {
		return 0, fmt.Errorf("invalid number format: %s", s)
	}
	
	return int64(num * float64(multiplier)), nil
}