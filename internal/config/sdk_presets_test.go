package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sdkPresetGlob locates the config documents the Python SDK emits.
//
// sdks/python/tests/test_config.py's WritePresetFixturesTest writes one file per preset here, plus
// default.yaml for the unmodified Configuration. Located by glob rather than by a list of preset
// names, for the same reason shippedConfigGlobs is: a preset added on the Python side should be
// checked here without anyone editing this file, and a hand-written list is precisely what drifted
// to produce the defect below.
const sdkPresetGlob = "sdks/testdata/presets/*.yaml"

// TestSDKPresetsLoadUnderTheGoLoader is the consuming half of a cross-language contract, and it is
// the assertion #385 needed.
//
// The Python SDK's documented path is: build a Configuration, save_to_file, mount with it. That path
// was broken for every preset and for the default. Configuration.to_yaml() emitted sixteen keys the
// Go schema does not define, across seven sections — global.pid_file, global.daemon,
// storage.s3.timeout, performance.read_ahead_size, performance.max_write_buffer,
// cluster.election_timeout, cluster.heartbeat_interval, cluster.join_timeout, security.tls_ca_path,
// monitoring.opentelemetry.headers, and all six keys of the fuse block — and LoadFromFile decodes
// strictly, so the daemon refused the file at startup naming the first one.
//
// #385 named three of the sixteen: it was filed from a Go-side reading of ClusterConfig, which is
// where somebody happened to look. The other thirteen were found by this test on its first run, which
// is the argument for its shape. It asserts a *property* — the Go loader accepts what the SDK writes —
// rather than comparing the emitted keys against a list, because a list is a second copy of the schema
// and would have had to be right about all sixteen to catch them.
//
// The direction matters. internal/metrics generates sdks/testdata/metrics-scrape.txt for the SDKs to
// parse, because the Go exporter is the authority on metric names; here the Go loader is the authority
// on config keys, so the SDK generates and Go checks. Either way the producing side is the one that
// defines the contract, and both halves run in CI — the Python half in the `sdk-metrics` job.
func TestSDKPresetsLoadUnderTheGoLoader(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	paths, err := filepath.Glob(filepath.Join(root, sdkPresetGlob))
	if err != nil {
		t.Fatalf("glob %q: %v", sdkPresetGlob, err)
	}

	if len(paths) == 0 {
		t.Fatalf("found no SDK preset documents under %s.\n\n"+
			"They are written by sdks/python/tests/test_config.py's WritePresetFixturesTest and "+
			"committed. An empty set passes every assertion below, so this is a failure rather than a "+
			"skip: regenerate with\n\n"+
			"    cd sdks/python && pytest tests/test_config.py\n\n"+
			"and commit what it writes.", filepath.Join(root, sdkPresetGlob))
	}

	for _, path := range paths {
		t.Run(shortName(t, path), func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()

			if err := cfg.LoadFromFile(path); err != nil {
				t.Fatalf("the Python SDK emits a config document this loader refuses, so "+
					"Configuration.save_to_file produces a file the daemon will not start on "+
					"(#385):\n\n%v\n\nFix the dataclass in sdks/python/objectfs/config.py — do not add "+
					"the key here to make this pass unless the setting has somewhere to reach. The "+
					"top-level keys this schema accepts are: %s",
					err, strings.Join(TopLevelKeys(), ", "))
			}

			if err := cfg.Validate(); err != nil {
				t.Fatalf("the Python SDK emits a config document that loads but does not validate, so "+
					"the daemon rejects it one step later (#385): %v", err)
			}
		})
	}
}

// TestSDKPresetsAreNotAllDefaults asserts each emitted document actually sets something.
//
// This is the same property internal/config's TestShippedConfigsSetWhatTheySay checks for the files
// this repository ships, and it is here for the same reason: a document that loads cleanly and leaves
// every value at its default is not evidence the contract holds. If the Python side ever emitted an
// empty `{}` — or a document whose every key the loader happened to ignore — the test above would pass
// while the SDK produced nothing usable.
//
// default.yaml is exempt by name and by design: it is Configuration() unmodified, so equalling the Go
// defaults is what it is *for*. That it equals them is itself worth asserting, since the two languages
// arrived at those defaults independently — but they are not required to agree on every value, so the
// assertion here is only that the file loads (above) and that the presets differ from it.
func TestSDKPresetsAreNotAllDefaults(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	paths, err := filepath.Glob(filepath.Join(root, sdkPresetGlob))
	if err != nil {
		t.Fatalf("glob %q: %v", sdkPresetGlob, err)
	}

	for _, path := range paths {
		if filepath.Base(path) == "default.yaml" {
			continue
		}

		t.Run(shortName(t, path), func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(path) //nolint:gosec // a path from a glob over this repository
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			if len(strings.TrimSpace(string(raw))) == 0 {
				t.Fatal("this preset document is empty, so the loader accepting it says nothing")
			}

			cfg := NewDefault()
			if err := cfg.LoadFromFile(path); err != nil {
				t.Fatalf("load: %v", err) // reported properly by the test above
			}

			// Compared against the SDK's own default document rather than against NewDefault, because
			// the two languages are not required to pick the same defaults — only to agree on the set
			// of keys. What must hold is that a preset differs from the SDK's baseline: a preset that
			// does not is documentation of settings that do not apply.
			base := NewDefault()
			basePath := filepath.Join(filepath.Dir(path), "default.yaml")

			if err := base.LoadFromFile(basePath); err != nil {
				t.Skipf("no SDK default document to compare against: %v", err)
			}

			if reflect.DeepEqual(*cfg, *base) {
				t.Errorf("this preset is byte-for-byte the SDK's default configuration once loaded, so "+
					"selecting it changes nothing — from_preset(%q) sets fields the emitted document "+
					"does not carry, or sets none at all",
					strings.TrimSuffix(filepath.Base(path), ".yaml"))
			}
		})
	}
}
