package utils

import (
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LogLevel represents the logging level
type LogLevel int

const (
	TRACE LogLevel = iota
	DEBUG
	INFO
	WARN
	ERROR
	FATAL
)

// String returns the string representation of the log level
func (l LogLevel) String() string {
	switch l {
	case TRACE:
		return "TRACE"
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO"
	case WARN:
		return "WARN"
	case ERROR:
		return "ERROR"
	case FATAL:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// ParseLogLevel parses a string log level
func ParseLogLevel(level string) (LogLevel, error) {
	switch strings.ToUpper(level) {
	case "TRACE":
		return TRACE, nil
	case "DEBUG":
		return DEBUG, nil
	case "INFO":
		return INFO, nil
	case "WARN", "WARNING":
		return WARN, nil
	case "ERROR":
		return ERROR, nil
	case "FATAL":
		return FATAL, nil
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
func (l *Logger) Debug(format string, args ...any) {
	if l.level <= DEBUG {
		l.log("DEBUG", format, args...)
	}
}

// Info logs an info message
func (l *Logger) Info(format string, args ...any) {
	if l.level <= INFO {
		l.log("INFO", format, args...)
	}
}

// Warn logs a warning message
func (l *Logger) Warn(format string, args ...any) {
	if l.level <= WARN {
		l.log("WARN", format, args...)
	}
}

// Error logs an error message
func (l *Logger) Error(format string, args ...any) {
	if l.level <= ERROR {
		l.log("ERROR", format, args...)
	}
}

// log writes a log message
func (l *Logger) log(level, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.output, "[%s] %s\n", level, message)
}

// SetupLogging configures the global logger
func SetupLogging(levelStr, logFile string) error {
	// Parse log level
	_, err := ParseLogLevel(levelStr)
	if err != nil {
		return fmt.Errorf("invalid log level: %w", err)
	}

	// Determine output destination
	var output io.Writer = os.Stdout

	if logFile != "" {
		// Validate log file path
		if err := ValidatePath(logFile, true); err != nil {
			return fmt.Errorf("invalid log file path: %w", err)
		}

		cleanPath := filepath.Clean(logFile)
		file, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
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

// ParseBytes parses a human-readable byte size — "512", "4KB", "1.5G", " 2 GB " — into a byte count.
//
// The accepted form is a decimal number, optional whitespace, an optional K/M/G/T/P multiplier, and
// an optional trailing B. Multipliers are binary: 1 K is 1024 bytes, as everywhere else in ObjectFS.
// Case and surrounding whitespace are ignored. A bare number is a count of bytes.
//
// This is the only size parser in the repository, and consolidating on it was the point (#159). There
// were four: this one, `internal/adapter.parseSize`, one in internal/compression, and a fourth copy in
// tests/unit_test.go. They disagreed about exactly the cases that matter, in every direction:
//
//   - the adapter's returned 1 GiB — silently, with no error — for *any* input it could not parse, so
//     `cache_size: 2G` (no B), `cache_size: 64MiB` and `cache_size: tpyo` all configured the same 1 GiB
//     cache and none of them said so;
//   - internal/compression's unit table stopped at GB, so it *rejected* "1TB" while accepting "-1MB" as
//     a negative floor and "99999999999GB" as math.MaxInt64 — a compression floor below every object
//     and a floor above every object, neither reported;
//   - the copy in tests fell through to strconv.ParseFloat on the number, which accepts Go float
//     syntax: it read "InfMB" as math.MaxInt64 and "1e3MB" as 1000 MB, and it rejected "1TB" for the
//     same missing-unit reason;
//   - and they disagreed about the empty string, which is the most common value in a config file:
//     (0, nil), 1 GiB, and an error respectively.
//
// A configuration value that means one thing to the layer that validates it and another to the layer
// that acts on it is audit finding C1's mechanism, and four parsers is four chances to reintroduce it.
// See [ParseOptionalBytes] for the empty-string half.
//
// It is strict for the same reason [internal/config.Configuration.LoadFromFile] is strict: this
// function's callers are reading operator configuration, and a size it accepts becomes a cache
// capacity, a multipart threshold, or a read chunk size. Rejecting "12abc" costs a person a minute;
// accepting it as 12 bytes gives them a filesystem quietly configured to something they did not ask
// for. Specifically it rejects
//
//   - trailing garbage: the old implementation used fmt.Sscanf, which stops at the first character
//     it cannot consume and reports success, so "12abc" parsed as 12 and "64MiB" — the spelling a
//     person who knows the units writes — parsed as 64 bytes;
//   - a negative size, which no caller has a meaning for and which reached callers as a negative
//     capacity;
//   - "Inf" and "NaN", which strconv.ParseFloat accepts and which became math.MinInt64 on conversion;
//   - hex float and exponent notation, which is valid Go and is never what a config file means;
//   - a value that overflows int64 once multiplied.
func ParseBytes(s string) (int64, error) {
	original := s

	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0, fmt.Errorf("size is empty; write a byte count, optionally with a K, M, G, T or P " +
			"unit (for example \"512\", \"4KB\" or \"1.5G\")")
	}

	// A trailing B is decoration: "4KB" and "4K" are the same size, and a bare "512B" is bytes.
	s = strings.TrimSuffix(s, "B")

	var multiplier int64 = 1
	if s != "" {
		switch s[len(s)-1] {
		case 'K':
			multiplier = 1 << 10
		case 'M':
			multiplier = 1 << 20
		case 'G':
			multiplier = 1 << 30
		case 'T':
			multiplier = 1 << 40
		case 'P':
			multiplier = 1 << 50
		}
		if multiplier != 1 {
			s = s[:len(s)-1]
		}
	}

	numStr := strings.TrimSpace(s)
	if err := checkDecimal(numStr); err != nil {
		return 0, fmt.Errorf("size %q is not valid: %w", original, err)
	}

	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q is not valid: %q is not a number", original, numStr)
	}

	// The multiply is done in float64 and range-checked before the conversion, because converting an
	// out-of-range float to int64 is implementation-defined in Go and yields a large negative number
	// in practice — a 16 EiB configured cache arriving downstream as a negative capacity.
	bytes := num * float64(multiplier)
	if bytes > math.MaxInt64 {
		return 0, fmt.Errorf("size %q is larger than the maximum representable size (%d bytes)",
			original, int64(math.MaxInt64))
	}

	return int64(bytes), nil
}

// ParseOptionalBytes is [ParseBytes] for a size an operator may leave unset, where unset means zero.
//
// Zero is the caller's signal to fall back to a built-in default, and it is deliberately not distinct
// from a literal "0": no caller in this repository distinguishes them, because a zero-byte threshold
// and an absent one both mean "use the default". A caller that needs the distinction should read the
// string itself rather than teach this function a third answer.
//
// It exists so that "" is handled identically everywhere, which was the second half of the four-parser
// problem described on [ParseBytes]. The parsers disagreed about the empty string as much as about
// malformed input: internal/compression's returned (0, nil), internal/adapter's returned 1 GiB and no
// error, and the copy in tests returned an error. An unset size is the most common thing in a
// configuration file, so that is the disagreement most likely to be reached.
func ParseOptionalBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}

	return ParseBytes(s)
}

// checkDecimal reports whether s is a plain non-negative decimal number.
//
// strconv.ParseFloat is deliberately not the gate: it accepts "+1", "-1", "1e9", "0x1p10", "Inf" and
// "NaN", none of which a size in a configuration file ever means, and two of which convert to
// nonsense rather than to an error.
func checkDecimal(s string) error {
	if s == "" {
		return fmt.Errorf("no number before the unit")
	}

	digits, dots := 0, 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '.':
			dots++
		default:
			return fmt.Errorf("%q contains %q, which is not a digit; a size is a plain decimal "+
				"number and an optional K, M, G, T or P unit", s, string(r))
		}
	}

	if digits == 0 {
		return fmt.Errorf("%q has no digits", s)
	}
	if dots > 1 {
		return fmt.Errorf("%q has more than one decimal point", s)
	}

	return nil
}
