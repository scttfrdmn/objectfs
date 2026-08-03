package config

import (
	"strings"
	"testing"
	"time"
)

// TestReadAheadDefaultsAreTheManagersOwn asserts the mount and the manager agree on the defaults.
//
// These two default sets were unequal for four releases and it did not show, because the mount path
// constructed the manager with nil and the config block reached nothing (#176). config said a 64 MB
// read-ahead with a "predictive" strategy; the manager ran a 64 KiB window with a sequential detector;
// mounts ran the manager's. Now that the block is wired, a divergence would mean a mount and a
// directly-constructed manager behave differently on the same defaults, so it is asserted rather than
// maintained by eye.
//
// Deliberately not comparing against fuse.DefaultReadAheadConfig by import: internal/fuse is
// linux||darwin-tagged and importing it here would tie this package's tests to a build tag. The values
// are duplicated with the source named instead, and TestBuildReadAheadConfigMapsEveryConfiguredValue in
// internal/adapter closes the loop by asserting the mapped output against the manager's own type.
func TestReadAheadDefaultsAreTheManagersOwn(t *testing.T) {
	t.Parallel()

	ra := NewDefault().Performance.ReadAhead

	// fuse.DefaultReadAheadConfig, internal/fuse/optimizations.go.
	want := ReadAheadConfig{
		Enabled:         true,
		WindowSize:      "64KB",
		MinSequential:   3,
		ConcurrentReads: 4,
		TTL:             5 * time.Minute,
	}

	if ra != want {
		t.Errorf("the mount default and the read-ahead manager's own default disagree, so a mount and a "+
			"directly-constructed manager would prefetch differently with no configuration at all:\n"+
			" config.NewDefault: %+v\n fuse.DefaultReadAheadConfig: %+v", ra, want)
	}
}

// TestValidateRejectsEachWayReadAheadCanBeWrong covers the values the manager cannot run with.
//
// Every case here is a value that reaches code. That is the change: validation used to range-check
// sequential_threshold, learning_rate, pattern_depth, prefetch_bandwidth_mbs and five more, none of
// which any code read — so a mount could be refused over a setting that would have done nothing had it
// been accepted, and a user whose file was rejected for an out-of-range learning_rate reasonably
// concluded the accepted values did something.
func TestValidateRejectsEachWayReadAheadCanBeWrong(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*ReadAheadConfig)
		mustSay string
	}{
		{
			name:    "no prefetch workers",
			mutate:  func(ra *ReadAheadConfig) { ra.ConcurrentReads = 0 },
			mustSay: "concurrent_reads",
		},
		{
			name:    "negative prefetch workers",
			mutate:  func(ra *ReadAheadConfig) { ra.ConcurrentReads = -1 },
			mustSay: "concurrent_reads",
		},
		{
			name:    "prefetching before any sequential read",
			mutate:  func(ra *ReadAheadConfig) { ra.MinSequential = 0 },
			mustSay: "min_sequential",
		},
		{
			name:    "patterns expire immediately",
			mutate:  func(ra *ReadAheadConfig) { ra.TTL = 0 },
			mustSay: "ttl",
		},
		{
			name:    "no prefetch floor",
			mutate:  func(ra *ReadAheadConfig) { ra.WindowSize = "" },
			mustSay: "window_size",
		},
		{
			name:    "unparseable prefetch floor",
			mutate:  func(ra *ReadAheadConfig) { ra.WindowSize = "64 kilobytes" },
			mustSay: "window_size",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()
			tc.mutate(&cfg.Performance.ReadAhead)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("this configuration validated, so a mount would come up with read-ahead in a "+
					"state the manager cannot run: %+v", cfg.Performance.ReadAhead)
			}

			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("the error does not name the key to edit; want it to mention %q, got: %v",
					tc.mustSay, err)
			}
		})
	}
}

// TestValidateIgnoresADisabledReadAheadBlock asserts the requirements lift when read-ahead is off.
//
// Same reasoning as a disabled listener's address: refusing to start over a value nothing will read is
// the defect this change fixed, pointing the other way. An operator who turned read-ahead off should not
// have to also supply a worker count and a TTL for a prefetcher that will not run.
//
// window_size is left syntactically valid and empty-tested separately below, because the two checks over
// it are different questions and only one of them is conditional. validateSizes asks "is this a size",
// which is a typo either way and is reported whether the feature is on or off; validateReadAheadConfig
// asks "is a floor required here", and that is what enabled governs.
func TestValidateIgnoresADisabledReadAheadBlock(t *testing.T) {
	t.Parallel()

	for _, windowSize := range []string{"", "64KB"} {
		t.Run("window_size="+windowSize, func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()
			cfg.Performance.ReadAhead = ReadAheadConfig{
				Enabled:         false,
				WindowSize:      windowSize,
				MinSequential:   -5,
				ConcurrentReads: 0,
				TTL:             0,
			}

			if err := cfg.Validate(); err != nil {
				t.Errorf("a disabled read-ahead block failed validation, so a mount is refused over "+
					"settings that will not be read: %v", err)
			}
		})
	}
}

// TestValidateReportsAnUnparseableWindowSizeEvenWhenDisabled pins the unconditional half.
//
// A value that is not a size at all is a typo, and reporting it only when the feature happens to be
// enabled means an operator fixes it once, turns read-ahead on months later, and is met with a
// validation failure over a line they have not touched. This is the one read-ahead check that does not
// respect `enabled`, and it is deliberate rather than an oversight in the test above.
func TestValidateReportsAnUnparseableWindowSizeEvenWhenDisabled(t *testing.T) {
	t.Parallel()

	cfg := NewDefault()
	cfg.Performance.ReadAhead.Enabled = false
	cfg.Performance.ReadAhead.WindowSize = "not a size"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unparseable window_size was accepted because read-ahead was disabled, so the typo " +
			"is only reported when the operator later turns the feature on")
	}

	if !strings.Contains(err.Error(), "window_size") {
		t.Errorf("the error does not name the key to fix; got: %v", err)
	}
}

// TestValidateReportsAUnitlessReadAheadTTLEvenWhenDisabled pins the other unconditional check.
//
// `ttl: 5` is 5 nanoseconds, because yaml.v2 reads a bare integer into a time.Duration as a raw
// nanosecond count and reports nothing. That is the same trap as an unparseable window_size — a typo,
// not a setting — so it is caught whether the block is enabled or not, and by validateDurations rather
// than validateReadAheadConfig, which returns early on a disabled block.
//
// TestEveryDurationInTheSchemaIsValidated already fails if this key leaves the list, by walking the
// schema with reflection; that is how the omission was found when #176 gave read-ahead a duration.
// This test is here because the walk sets the field on a default (enabled) config and so proves only
// the enabled half.
func TestValidateReportsAUnitlessReadAheadTTLEvenWhenDisabled(t *testing.T) {
	t.Parallel()

	cfg := NewDefault()
	cfg.Performance.ReadAhead.Enabled = false
	cfg.Performance.ReadAhead.TTL = 5 // "ttl: 5" in YAML: five nanoseconds.

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a 5ns read-ahead ttl was accepted because read-ahead was disabled, so the missing unit " +
			"is only reported once the operator turns the feature on — in a release that changed nothing " +
			"about the line")
	}

	if !strings.Contains(err.Error(), "read_ahead.ttl") {
		t.Errorf("the error does not name the key to fix; got: %v", err)
	}
}

// TestLoadFromEnvGovernsReadAhead asserts each read-ahead variable reaches its field.
//
// Not parallel: t.Setenv forbids it.
//
// The three variables this replaces — OBJECTFS_READAHEAD_STRATEGY, _PATTERN_DETECTION and
// _ML_PREDICTION — are gone with the fields they assigned to. So is OBJECTFS_READ_AHEAD_SIZE, whose
// target performance.read_ahead_size was a second, separately-defaulted name for the same quantity as
// performance.read_ahead.size, and both were read by nothing.
func TestLoadFromEnvGovernsReadAhead(t *testing.T) {
	t.Setenv("OBJECTFS_READAHEAD_ENABLED", "false")
	t.Setenv("OBJECTFS_READAHEAD_WINDOW_SIZE", "256KB")
	t.Setenv("OBJECTFS_READAHEAD_MIN_SEQUENTIAL", "9")
	t.Setenv("OBJECTFS_READAHEAD_CONCURRENT_READS", "2")

	cfg := NewDefault()

	// Asserting false against a default of true only means something if the default is true.
	if !cfg.Performance.ReadAhead.Enabled {
		t.Fatal("read-ahead must default to enabled for the assertion below to distinguish " +
			"'the variable was applied' from 'the field was already false'")
	}

	if err := cfg.LoadFromEnv(); err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}

	ra := cfg.Performance.ReadAhead

	if ra.Enabled {
		t.Error("OBJECTFS_READAHEAD_ENABLED=false did not turn read-ahead off, so an operator who " +
			"exported it to stop prefetch traffic and its egress is still paying for it")
	}

	if ra.WindowSize != "256KB" {
		t.Errorf("OBJECTFS_READAHEAD_WINDOW_SIZE did not reach the prefetch floor: got %q", ra.WindowSize)
	}

	if ra.MinSequential != 9 {
		t.Errorf("OBJECTFS_READAHEAD_MIN_SEQUENTIAL did not reach the field: got %d", ra.MinSequential)
	}

	if ra.ConcurrentReads != 2 {
		t.Errorf("OBJECTFS_READAHEAD_CONCURRENT_READS did not reach the worker count: got %d",
			ra.ConcurrentReads)
	}
}

// TestLoadFromEnvRefusesANonNumericReadAheadCount asserts the integer variables report a bad value.
//
// Not parallel: t.Setenv forbids it.
//
// These two return their error rather than ignoring it, unlike OBJECTFS_MAX_CONCURRENCY beside them,
// which discards a parse failure and leaves the default in place. Both of these are counts whose
// silent default is a different behavior an operator did not ask for — a worker count especially,
// where the value governs how much prefetch traffic a mount generates.
func TestLoadFromEnvRefusesANonNumericReadAheadCount(t *testing.T) {
	for _, envVar := range []string{
		"OBJECTFS_READAHEAD_MIN_SEQUENTIAL",
		"OBJECTFS_READAHEAD_CONCURRENT_READS",
	} {
		t.Run(envVar, func(t *testing.T) {
			t.Setenv(envVar, "several")

			err := NewDefault().LoadFromEnv()
			if err == nil {
				t.Fatalf("%s=several was accepted, so the value was silently discarded and the mount "+
					"runs a count the operator did not choose", envVar)
			}

			if !strings.Contains(err.Error(), envVar) {
				t.Errorf("the error does not name the variable to fix; got: %v", err)
			}
		})
	}
}
