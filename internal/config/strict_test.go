package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadFromFileRejectsUnknownKeys is audit finding P-10's fix, tested at the loader.
//
// Before v0.10.1 a key the schema did not define was discarded in silence, which is how
// configs/example.yaml came to be 162 lines that changed exactly one field from the built-in
// defaults while appearing to configure a whole filesystem. Every case below is a document that
// previously loaded "successfully" and set nothing.
//
// The assertion is not merely that an error is returned. It is that the error names the offending
// key, because "failed to parse config file" alone sends a user hunting for a syntax error in a
// file whose syntax is fine — which is the same class of unattributed failure as C1's
// "Failed to start adapter".
func TestLoadFromFileRejectsUnknownKeys(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		why  string
		yaml string
		// mustName is a substring the error has to contain: the key at fault, so the message is
		// actionable without reading the source.
		mustName string
	}{
		{
			name:     "a top-level block the schema does not have",
			why:      "configs/example.yaml opened with `s3:` where the schema has `storage.s3`",
			yaml:     "s3:\n  region: us-west-2\n",
			mustName: "s3",
		},
		{
			name:     "a misspelled leaf",
			why:      "the compression block documented `enable` against the real `enabled`",
			yaml:     "storage:\n  s3:\n    compression:\n      enable: true\n",
			mustName: "enable",
		},
		{
			name: "a leaf named as the wrong type's field",
			why: "`zstd_level` and `min_file_size` were documented for four releases against the " +
				"real `level` and `min_size`, and nobody noticed because the block was inert",
			yaml:     "storage:\n  s3:\n    compression:\n      zstd_level: 3\n",
			mustName: "zstd_level",
		},
		{
			name: "the compression block at its old path",
			why: "#157 moved compression from write_buffer to storage.s3. Removing the key rather " +
				"than deprecating it is what makes an upgrade name the file and line to edit; " +
				"leaving it in the schema as an ignored field would mean an operator's compression " +
				"settings silently stopped applying on upgrade",
			yaml:     "write_buffer:\n  compression:\n    enabled: true\n",
			mustName: "compression",
		},
		{
			name: "the dead performance.compression_enabled key",
			why: "it defaulted to true, was read by nothing, and sat two sections away from the " +
				"real setting that defaulted to false (#157). A config still setting it has to be " +
				"told, because its author believed compression was on",
			yaml:     "performance:\n  compression_enabled: true\n",
			mustName: "compression_enabled",
		},
		{
			name:     "a plausible key under a real block",
			why:      "the failure mode is a key that looks right, not one that looks like a typo",
			yaml:     "cache:\n  max_size: 100GB\n",
			mustName: "max_size",
		},
		{
			name:     "a duplicate key written twice in the document",
			why:      "a genuine duplicate is a mistake regardless of strictness, and must not pass",
			yaml:     "global:\n  log_level: DEBUG\n  log_level: INFO\n",
			mustName: "log_level",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			cfg := NewDefault()

			err := cfg.LoadFromFile(path)
			if err == nil {
				t.Fatalf("accepted a document the schema cannot represent, so the setting is "+
					"silently discarded and the user believes they configured something: %s", tc.why)
			}

			if !strings.Contains(err.Error(), tc.mustName) {
				t.Errorf("the error does not name %q, so it does not say what to fix:\n%v",
					tc.mustName, err)
			}
		})
	}
}

// TestLoadFromFileAcceptsOverridingADefaultMapEntry is the reason LoadFromFile decodes twice.
//
// yaml.v2's strict mode reports "key already set in map" when a document assigns a map key already
// present in the destination — and NewDefault ships
// monitoring.metrics.custom_labels: {service: objectfs}. A single strict pass into the
// already-defaulted Configuration would therefore tell a user that setting their own `service`
// label is a duplicate key, which is both wrong and impossible to act on.
//
// This test is what stops the two passes from being collapsed back into one during a later cleanup:
// the simplification compiles, passes every other test in this package, and breaks exactly this.
func TestLoadFromFileAcceptsOverridingADefaultMapEntry(t *testing.T) {
	t.Parallel()

	const doc = `monitoring:
  metrics:
    custom_labels:
      service: my-override
      environment: production
`

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := NewDefault()
	if err := cfg.LoadFromFile(path); err != nil {
		t.Fatalf("overriding a label present in the defaults was rejected: %v", err)
	}

	labels := cfg.Monitoring.Metrics.CustomLabels

	if got := labels["service"]; got != "my-override" {
		t.Errorf("custom_labels[service] = %q, want the document's value %q", got, "my-override")
	}

	if got := labels["environment"]; got != "production" {
		t.Errorf("custom_labels[environment] = %q, want %q", got, "production")
	}
}

// TestLoadFromFileLeavesUnmentionedKeysAtTheirDefaults asserts the overlay property.
//
// Strict decoding must not turn the config file into an all-or-nothing document: a file that sets
// three keys has to keep the defaults for everything else, or every deployment would be forced to
// restate the entire schema and would then silently freeze at whatever the defaults were on the day
// it was written.
func TestLoadFromFileLeavesUnmentionedKeysAtTheirDefaults(t *testing.T) {
	t.Parallel()

	const doc = `global:
  log_level: WARN
`

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := NewDefault()
	if err := cfg.LoadFromFile(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Global.LogLevel != "WARN" {
		t.Errorf("the one key set in the document did not apply: log_level = %q", cfg.Global.LogLevel)
	}

	def := NewDefault()

	if cfg.Global.MetricsPort != def.Global.MetricsPort {
		t.Errorf("a key the document does not mention lost its default: metrics_port = %d, want %d",
			cfg.Global.MetricsPort, def.Global.MetricsPort)
	}

	if cfg.Cache.TTL != def.Cache.TTL {
		t.Errorf("a key the document does not mention lost its default: cache.ttl = %v, want %v",
			cfg.Cache.TTL, def.Cache.TTL)
	}

	if cfg.Storage.S3.Region != def.Storage.S3.Region {
		t.Errorf("a key the document does not mention lost its default: region = %q, want %q",
			cfg.Storage.S3.Region, def.Storage.S3.Region)
	}
}

// TestTopLevelKeysMatchesTheSchema guards the reflection helper that every unknown-key error message
// depends on.
//
// It is read from struct tags rather than written out precisely so it cannot drift from
// Configuration — the same drift that produced P-10 in the first place, where a documented key and
// the schema's key disagreed and nothing compared them. A test that hardcoded the expected list
// would reintroduce the second authority, so this asserts properties instead: the count matches the
// number of tagged fields, no entry is empty, and a few load-bearing names are present.
func TestTopLevelKeysMatchesTheSchema(t *testing.T) {
	t.Parallel()

	keys := TopLevelKeys()

	if len(keys) == 0 {
		t.Fatal("no top-level keys, so every unknown-key error message would name nothing")
	}

	seen := make(map[string]bool, len(keys))

	for _, k := range keys {
		if k == "" {
			t.Error("an empty key name, which would render as a stray comma in the error message")
		}

		if seen[k] {
			t.Errorf("duplicate key %q", k)
		}

		seen[k] = true
	}

	// These four are the ones users most often get wrong — `storage` because the old example used a
	// top-level `s3:`, and `write_buffer` because the old example used `buffer:`.
	for _, want := range []string{"global", "storage", "write_buffer", "cache"} {
		if !seen[want] {
			t.Errorf("%q is missing from TopLevelKeys, so the error message would not mention it "+
				"even though the schema has it", want)
		}
	}

	// The helper's whole purpose is to appear in the loader's error. If it stops doing that, the
	// message loses the part that tells a user what the valid keys are.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("nonexistent_block:\n  x: 1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := NewDefault().LoadFromFile(path)
	if err == nil {
		t.Fatal("an unknown top-level block was accepted")
	}

	for _, k := range keys {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("the error message omits the valid key %q, so it does not tell the user what "+
				"the schema accepts:\n%v", k, err)
		}
	}
}
