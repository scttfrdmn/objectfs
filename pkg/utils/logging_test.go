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

		// Everything below rejects an input the previous fmt.Sscanf implementation accepted, each
		// silently and each producing a size the operator did not write. These are the reason
		// ParseBytes became the repository's only size parser: it is now what every configured
		// capacity, threshold and chunk size is read through, so what it accepts loosely it accepts
		// loosely everywhere.
		{
			name:    "trailing garbage",
			input:   "12abc",
			wantErr: true,
		},
		{
			name: "the units a person who knows the units writes",
			// "64MiB" parsed as 64 *bytes*: Sscanf consumed the 64, stopped at the M, and reported
			// success — so a 64 MiB read chunk was configured as a 64-byte one.
			input:   "64MiB",
			wantErr: true,
		},
		{
			name:    "negative",
			input:   "-5M",
			wantErr: true,
		},
		{
			name:  "explicit plus",
			input: "+5M",
			// Not a size anyone writes, and accepting it means the sign character is parsed at all,
			// which is how the negative case got in.
			wantErr: true,
		},
		{
			name:    "infinity",
			input:   "Inf",
			wantErr: true,
		},
		{
			name:    "not a number",
			input:   "NaN",
			wantErr: true,
		},
		{
			name:  "exponent notation",
			input: "1e9",
			// Valid Go, never a config file's intent, and ambiguous next to the K/M/G units.
			wantErr: true,
		},
		{
			name:    "hex float",
			input:   "0x1p10",
			wantErr: true,
		},
		{
			name:    "two decimal points",
			input:   "1.2.3G",
			wantErr: true,
		},
		{
			name:  "unit with no number",
			input: "GB",
			// Sscanf on the empty remainder returned num unset and no error on some paths.
			wantErr: true,
		},
		{
			name:    "whitespace only",
			input:   "   ",
			wantErr: true,
		},
		{
			name:  "overflows int64",
			input: "16384P",
			// 16384 PiB is 2^64 bytes. Converting the out-of-range float64 to int64 is
			// implementation-defined and yielded a large negative number, so a nonsense size arrived
			// downstream as a negative capacity rather than as an error.
			wantErr: true,
		},
		{
			name:     "the largest representable size",
			input:    "8191P",
			expected: 8191 * (1 << 50),
		},
		{
			name:     "zero",
			input:    "0",
			expected: 0,
			// Zero is a legitimate size and means "no limit" or "disabled" to several callers, so it
			// must not be an error.
		},
		{
			name:     "zero with a unit",
			input:    "0MB",
			expected: 0,
		},
		{
			name:     "fractional bytes truncate",
			input:    "1.5",
			expected: 1,
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

// TestParseBytesErrorsQuoteTheInput asserts the error names the value that was rejected.
//
// The operator's next action is to edit a line in a YAML file, and they can only do that if the
// message says which value was wrong. It matters most for the cases where the problem is invisible:
// a trailing space, a non-breaking space pasted from a web page, a Cyrillic 'М' where an ASCII 'M'
// belongs. %q escapes all three; the value printed bare does not.
func TestParseBytesErrorsQuoteTheInput(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"64MiB", "-5M", "12abc", "1.2.3G", "16384P", "МB"} {
		_, err := ParseBytes(input)
		if err == nil {
			t.Errorf("ParseBytes(%q) was accepted; it must not be", input)
			continue
		}

		if !strings.Contains(err.Error(), `"`+input+`"`) &&
			!strings.Contains(err.Error(), strings.ToUpper(input)) {
			t.Errorf("ParseBytes(%q) error does not quote the offending value, so an invisible "+
				"character in it cannot be seen: %v", input, err)
		}
	}
}

// TestParseOptionalBytesDiffersOnlyOnTheEmptyString is the whole contract, and it is stated as a
// difference rather than as a table of sizes on purpose.
//
// A table would restate TestParseBytes and pass just as well if ParseOptionalBytes grew a second
// implementation — which is the defect this function exists to prevent, four times over. The parsers
// it replaced disagreed about "" (compression's returned zero, the adapter's returned 1 GiB, the copy
// in tests returned an error) *and* about "1TB", "-1MB" and "InfMB", because each was a separate
// implementation rather than a wrapper. So the property asserted is delegation: for every input except
// "", the two functions must return the same value and the same error-ness.
func TestParseOptionalBytesDiffersOnlyOnTheEmptyString(t *testing.T) {
	t.Parallel()

	// Every shape a config file produces, plus the four the deleted parsers got wrong.
	inputs := []string{
		"0", "1", "512", "4KB", "1.5G", " 2 GB ", "1TB", "1PB", "128K",
		"-1MB", "99999999999GB", "InfMB", "1e3MB", "64MiB", "4MG", "12abc", "not-a-size", " ",
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			want, wantErr := ParseBytes(in)
			got, gotErr := ParseOptionalBytes(in)

			if (gotErr != nil) != (wantErr != nil) {
				t.Fatalf("ParseOptionalBytes(%q) err=%v but ParseBytes err=%v: the two disagree about "+
					"whether this value is usable, which is the four-parser defect returning", in, gotErr, wantErr)
			}
			if got != want {
				t.Errorf("ParseOptionalBytes(%q) = %d, ParseBytes = %d: a size means one thing to the "+
					"layer that validates it and another to the layer that acts on it", in, got, want)
			}
		})
	}

	// The one deliberate divergence. Unset is zero and not an error, because zero is the caller's
	// signal to use its own default and an absent size is the most common thing in a config file.
	n, err := ParseOptionalBytes("")
	if err != nil {
		t.Errorf("ParseOptionalBytes(\"\") = %v; an omitted size must mean \"use the default\", not "+
			"\"refuse to start\" — every partial config file omits most sizes", err)
	}
	if n != 0 {
		t.Errorf("ParseOptionalBytes(\"\") = %d, want 0: a non-zero substitute is the silent 1 GiB "+
			"the adapter's parser returned", n)
	}

	// And ParseBytes itself must still reject it, since its callers require a size.
	if _, err := ParseBytes(""); err == nil {
		t.Error("ParseBytes(\"\") was accepted, so performance.cache_size could be omitted and " +
			"become a zero-byte read cache rather than a config-load error")
	}
}

// FuzzParseBytes asserts the parser is total and that what it accepts is non-negative.
//
// Two properties, both about what the callers do with the result. It reads operator configuration, so
// it must not panic on any string — a panic here is a mount that dies during startup with a stack
// trace instead of a message naming the setting. And every caller treats the result as a capacity, a
// threshold or a chunk size; a negative one is at best a disabled feature reported as enabled, and at
// worst a slice length. The old implementation returned negatives for "-5M" and math.MinInt64 for
// "-Inf".
func FuzzParseBytes(f *testing.F) {
	for _, seed := range []string{
		"", "0", "512", "4KB", "1.5G", " 2 GB ", "-5M", "Inf", "NaN", "1e9", "0x1p10", "16384P",
		"64MiB", "12abc", "GB", "...", "9999999999999999999999P", "\x00", "١٢٣",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseBytes(s)
		if err != nil {
			if n != 0 {
				t.Errorf("ParseBytes(%q) returned %d alongside an error; a caller that logs the "+
					"error and carries on would use it", s, n)
			}
			return
		}

		if n < 0 {
			t.Errorf("ParseBytes(%q) = %d, a negative size — it becomes a cache capacity or a "+
				"chunk length", s, n)
		}
	})
}
