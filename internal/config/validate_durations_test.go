package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestValidateRejectsUnitlessDurations covers the trap that gopkg.in/yaml.v2 sets for every duration
// in this schema: a bare integer decodes as a raw nanosecond count, silently.
//
// `read: 30` is what someone writing a 30-second timeout tries first. It configures 30 nanoseconds,
// and every layer below accepts it — internal/circuit.NewBreaker, Checker.checkLoop, and
// Monitor.monitorLoop all substitute a default at <= 0, and a small positive satisfies each of those
// guards. What the operator gets is a mount that dies inside NewBackend's health check with
// "exceeded maximum number of attempts ... timeout awaiting response headers", which reads as a
// network problem and names no setting.
//
// This is why the check refuses rather than clamps. A clamp would substitute some duration the
// operator did not choose, and the number they wrote is not a number they meant — there is no
// interpretation of 30 nanoseconds that is what they were asking for, so the only useful response is
// to say the unit is missing.
func TestValidateRejectsUnitlessDurations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		mutate   func(*Configuration)
		wantText string
		why      string
	}{
		{
			name: "a read timeout in bare nanoseconds",
			mutate: func(c *Configuration) {
				c.Network.Timeouts.Read = 2
			},
			wantText: "network.timeouts.read",
			why: "this is the exact input FuzzConfigConstructsBackend found: it becomes the " +
				"transport's ResponseHeaderTimeout, so every request fails before S3 can answer",
		},
		{
			name: "a connect timeout in bare nanoseconds",
			mutate: func(c *Configuration) {
				c.Network.Timeouts.Connect = 10
			},
			wantText: "network.timeouts.connect",
			why:      "it becomes the dialer's timeout, so no connection can be established",
		},
		{
			name: "a write timeout in bare nanoseconds",
			mutate: func(c *Configuration) {
				c.Network.Timeouts.Write = 300
			},
			wantText: "network.timeouts.write",
			why: "not yet wired, and checked anyway: a value that is wrong now stays wrong when it " +
				"is wired, and it would then be wrong in a release that changed nothing about it",
		},
		{
			name: "a retry base delay in bare nanoseconds",
			mutate: func(c *Configuration) {
				c.Network.Retry.BaseDelay = 1
			},
			wantText: "network.retry.base_delay",
			why: "a 1ns backoff retries three times inside a microsecond, which is a burst against " +
				"a throttling S3 rather than a retry",
		},
		{
			name: "a health-check interval in bare nanoseconds",
			mutate: func(c *Configuration) {
				c.Monitoring.HealthChecks.Interval = 30
			},
			wantText: "monitoring.health_checks.interval",
			why: "it becomes a time.NewTicker period; 30ns is a busy loop issuing HeadBucket as " +
				"fast as the scheduler allows",
		},
		{
			name: "a circuit breaker timeout in bare nanoseconds",
			mutate: func(c *Configuration) {
				c.Network.CircuitBreaker.Timeout = 60
			},
			wantText: "network.circuit_breaker.timeout",
			why: "it is how long the breaker stays open, so 60ns makes an open breaker close again " +
				"immediately and the protection never applies",
		},
		{
			name: "a negative duration",
			mutate: func(c *Configuration) {
				c.Network.Timeouts.Read = -5
			},
			wantText: "must not be negative",
			why:      "yaml.v2 accepts a negative bare integer as a negative duration without complaint",
		},
		{
			name: "a negative retry attempt count",
			mutate: func(c *Configuration) {
				c.Network.Retry.MaxAttempts = -1
			},
			wantText: "network.retry.max_attempts",
			why:      "pkg/retry would treat it as no attempts at all, so nothing is ever retried",
		},
		{
			name: "a negative circuit breaker threshold",
			mutate: func(c *Configuration) {
				c.Network.CircuitBreaker.FailureThreshold = -3
			},
			wantText: "network.circuit_breaker.failure_threshold",
			why: "it becomes a ReadyToTrip predicate's bound; a negative one trips on the first " +
				"success as readily as the first failure",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()
			tc.mutate(cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted this configuration, so the mount fails later with an "+
					"error naming the network rather than the setting. %s", tc.why)
			}

			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("Validate rejected it but the message does not mention %q, so the operator "+
					"is not told which line to edit:\n%v", tc.wantText, err)
			}
		})
	}
}

// TestUnitlessDurationErrorExplainsTheUnitRule asserts the message says what to write, not merely
// that the value is wrong.
//
// The operator is looking at a line that reads `read: 30` and an error saying 30ns is too small. The
// non-obvious part is that adding a suffix is the fix — nothing about the file suggests the number was
// interpreted in nanoseconds — so the message has to say so, and it has to show the value they
// probably meant.
func TestUnitlessDurationErrorExplainsTheUnitRule(t *testing.T) {
	t.Parallel()

	cfg := NewDefault()
	cfg.Network.Timeouts.Read = 30

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a 30ns read timeout")
	}

	for _, want := range []string{"nanoseconds", `"30s"`, "30ns"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not contain %q, so it does not tell the operator what to "+
				"write instead:\n%v", want, err)
		}
	}
}

// TestValidateAcceptsZeroAndRealDurations pins the other half of the contract.
//
// Zero must stay valid: it means "use the built-in default", which is how a partial config file
// works, and every consumer already substitutes its own default at <= 0. Rejecting it would make
// overriding one duration require supplying all of them.
func TestValidateAcceptsZeroAndRealDurations(t *testing.T) {
	t.Parallel()

	t.Run("every duration zero", func(t *testing.T) {
		t.Parallel()

		cfg := NewDefault()
		cfg.Network.Timeouts = TimeoutConfig{}
		cfg.Network.Retry.BaseDelay = 0
		cfg.Network.Retry.MaxDelay = 0
		cfg.Network.CircuitBreaker.Timeout = 0
		cfg.Monitoring.HealthChecks.Interval = 0
		cfg.Monitoring.HealthChecks.Timeout = 0
		cfg.Cache.TTL = 0
		cfg.Cluster.Redis.TTL = 0

		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate rejected a config omitting every duration, so overriding one would "+
				"require supplying all of them: %v", err)
		}
	})

	t.Run("the shipped defaults", func(t *testing.T) {
		t.Parallel()

		if err := NewDefault().Validate(); err != nil {
			t.Fatalf("Validate rejected its own defaults: %v", err)
		}
	})

	t.Run("a duration exactly at the floor", func(t *testing.T) {
		t.Parallel()

		cfg := NewDefault()
		cfg.Network.Timeouts.Read = minSaneDuration

		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate rejected a read timeout of exactly %s, so the floor is exclusive "+
				"where it should be inclusive: %v", minSaneDuration, err)
		}
	})
}

// TestEveryDurationInTheSchemaIsValidated walks the config structs with reflection and fails for any
// time.Duration field validateDurations does not cover.
//
// A grep confirms the list is complete today and cannot confirm it stays complete. The failure this
// guards against is the one that produced the defect in the first place: a setting reaching the layer
// that acts on it without the layer that reads it having had an opinion. A duration added in a later
// release would inherit the same yaml.v2 trap, and nothing would report it — which is precisely how
// `network.timeouts.read` came to be shipped.
//
// The mechanism is to set each field to a value the check must reject and assert that Validate does.
// That tests the actual guard rather than a list of paths kept in step by hand, so a field added to
// the list but not to the switch, or vice versa, still fails.
func TestEveryDurationInTheSchemaIsValidated(t *testing.T) {
	t.Parallel()

	found := durationFields(reflect.TypeFor[Configuration](), "")
	if len(found) == 0 {
		t.Fatal("the walk found no time.Duration fields at all, so it is not walking anything")
	}

	for _, path := range found {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()

			// 7ns: a positive value under the floor, and distinctive enough in a message to be
			// traceable back to here.
			if err := setDuration(reflect.ValueOf(cfg).Elem(), path, 7*time.Nanosecond); err != nil {
				t.Fatalf("could not set %s: %v", path, err)
			}

			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s accepts 7ns, so a bare number in that key is read as nanoseconds and "+
					"nothing says so. Add it to validateDurations — a duration this schema exposes "+
					"and does not check is the shape of the defect that check exists for", path)
			}
		})
	}
}

// durationFields returns the dotted field paths of every time.Duration in a struct type, recursing
// through nested structs.
//
// Field names, not YAML tags, because the caller sets values through reflection. Maps, slices and
// pointers are not walked: no duration in this schema lives inside one, and a walk that pretended to
// cover them would be asserting more than it checks.
func durationFields(t reflect.Type, prefix string) []string {
	var out []string

	for f := range t.Fields() {
		path := f.Name
		if prefix != "" {
			path = prefix + "." + f.Name
		}

		switch {
		case f.Type == reflect.TypeFor[time.Duration]():
			out = append(out, path)
		case f.Type.Kind() == reflect.Struct:
			out = append(out, durationFields(f.Type, path)...)
		}
	}

	return out
}

// setDuration assigns d to the dotted field path within v.
func setDuration(v reflect.Value, path string, d time.Duration) error {
	for name := range strings.SplitSeq(path, ".") {
		v = v.FieldByName(name)
		if !v.IsValid() {
			return fmt.Errorf("no field %q along %s", name, path)
		}
	}

	if !v.CanSet() {
		return fmt.Errorf("%s is not settable", path)
	}

	v.SetInt(int64(d))

	return nil
}
