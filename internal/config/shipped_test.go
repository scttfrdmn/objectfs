package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

// shippedConfigs are the configuration files this repository publishes.
//
// They are located by glob rather than listed, because a file added to one of these directories is
// a file a user will copy, and a test that only checks a hand-written list would not see it.
var shippedConfigGlobs = []string{
	"configs/*.yaml",
	"examples/*.yaml",
	"examples/config/*.yaml",
}

// TestShippedConfigsLoadAndValidate asserts every published config file gets through the loader
// and the validator.
//
// This is the weaker of the two properties in this file and it is here because it is the one users
// experience: a config file shipped in this repository that ObjectFS refuses to start on is a
// broken example, and there is no reason for anyone to find that out at a mount attempt.
func TestShippedConfigsLoadAndValidate(t *testing.T) {
	t.Parallel()

	for _, path := range shippedConfigPaths(t) {
		t.Run(shortName(t, path), func(t *testing.T) {
			t.Parallel()

			if reason, ok := nonConfigurationFiles[filepath.Base(path)]; ok {
				t.Skipf("not a Configuration document: %s", reason)
			}

			cfg := NewDefault()

			if err := cfg.LoadFromFile(path); err != nil {
				t.Fatalf("a config file this repository publishes does not load: %v", err)
			}

			if err := cfg.Validate(); err != nil {
				t.Fatalf("a config file this repository publishes does not validate: %v", err)
			}
		})
	}
}

// TestShippedConfigsSetWhatTheySay is audit finding P-10, pinned.
//
// Before v0.10.1 LoadFromFile used non-strict unmarshalling, so a key the schema did not define
// was discarded in silence. The consequence was not theoretical and it was not small: measured
// against NewDefault, configs/example.yaml — 162 lines, the file the README told users to copy and
// the file scripts/postinstall.sh installed as /etc/objectfs/config.yaml — changed exactly *one*
// field from the built-in defaults, and nothing read that field. It opened with a top-level `s3:`
// block where the schema has `storage.s3`, so `region: us-west-2` loaded as `us-east-1`. Its
// `mount:`, `buffer:`, `compression:`, `metrics:`, `health:`, `logging:`, `archive:` and `cost:`
// blocks were all inert. That is also why the compression block documented `enable`, `zstd_level`
// and `min_file_size` against the real `enabled`, `level` and `min_size` and nobody noticed for
// four releases: the whole block was already being thrown away, so correcting the names would not
// have changed the behavior either.
//
// A config file that is rejected costs a user a minute. A config file that is ignored lets them
// believe they configured a 100 GB cache in us-west-2 while running a 1 GB cache in us-east-1, and
// nothing anywhere will ever tell them otherwise.
//
// The loader is strict now, so TestShippedConfigsLoadAndValidate above already fails on an unknown
// key. This test asserts the stronger property that test cannot: that a shipped file is not merely
// *accepted* but actually *changes something*. A file that parses cleanly and leaves every value
// at its default is still documentation of settings that do not apply — it is the same defect one
// step further in, and strict decoding alone does not catch it.
//
// Skipped-not-failed for files that are deliberately not Configuration documents — see
// nonConfigurationFiles.
func TestShippedConfigsSetWhatTheySay(t *testing.T) {
	t.Parallel()

	for _, path := range shippedConfigPaths(t) {
		name := shortName(t, path)

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if reason, ok := nonConfigurationFiles[filepath.Base(path)]; ok {
				t.Skipf("not a Configuration document: %s", reason)
			}

			// The strict half. Stated here as well as in the loader because this is the assertion
			// the finding is about, and a future change that relaxes LoadFromFile should have to
			// fail a test that says so rather than quietly widening what ships.
			raw, err := os.ReadFile(path) //nolint:gosec // a path from a glob over this repository
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			var strictCheck Configuration
			if err := yaml.UnmarshalStrict(raw, &strictCheck); err != nil {
				t.Fatalf("this file sets keys the configuration schema does not have, so they are "+
					"silently discarded — a user copying it gets defaults while believing they "+
					"configured something (audit finding P-10):\n%v\n\nthe schema's top-level keys "+
					"are: %s", err, strings.Join(TopLevelKeys(), ", "))
			}

			// The effect half.
			loaded := NewDefault()
			if err := loaded.LoadFromFile(path); err != nil {
				t.Fatalf("load: %v", err)
			}

			if _, exempt := allDefaultsByDesign[filepath.Base(path)]; exempt {
				return
			}

			if reflect.DeepEqual(*loaded, *NewDefault()) {
				t.Errorf("every value in this file is identical to the built-in default, so it "+
					"documents settings that do not apply — copying it and editing nothing is "+
					"indistinguishable from having no config file at all.\n\nIf that is "+
					"deliberate, add it to allDefaultsByDesign with the reason.\n\n"+
					"schema top-level keys: %s", strings.Join(TopLevelKeys(), ", "))
			}
		})
	}
}

// nonConfigurationFiles are shipped YAML files that are not ObjectFS Configuration documents, with
// the reason each one is not.
//
// This map exists so that "not a config file" is a claim someone had to write down, rather than a
// silent absence from a hand-maintained list of files to check. A new YAML file in these
// directories is checked by default and has to be excused explicitly.
var nonConfigurationFiles = map[string]string{
	"discount-config.yaml": "a cost-model discount table consumed by internal/cost, not a mount " +
		"configuration; its keys are volume tiers and discount rates",
}

// allDefaultsByDesign are shipped files whose every value equals NewDefault's, legitimately.
//
// The effect assertion above exists because a config file that changes nothing is documentation of
// settings that do not apply. A schema reference is the one honest exception: its purpose is to
// show every key next to the value ObjectFS uses when the key is absent, which means it must equal
// the defaults exactly. Exempting it by name, with the reason, keeps that a decision someone made
// rather than a hole in the check.
var allDefaultsByDesign = map[string]string{
	"config.yaml": "the full-schema reference: every key is present at its built-in default, which " +
		"is the entire point — configs/example.yaml is the copyable starting point",
}

// shippedConfigPaths returns absolute paths to every published config file.
//
// Absolute, because LoadFromFile validates its argument against directory traversal and rejects
// any path containing "..", which every relative path from this package to the repository root
// contains. That check is correct — a config path is often user-supplied — and the test has to
// work with it rather than around it.
func shippedConfigPaths(t *testing.T) []string {
	t.Helper()

	root := repoRoot(t)

	var paths []string

	for _, glob := range shippedConfigGlobs {
		matches, err := filepath.Glob(filepath.Join(root, glob))
		if err != nil {
			t.Fatalf("glob %q: %v", glob, err)
		}

		paths = append(paths, matches...)
	}

	if len(paths) == 0 {
		t.Fatalf("found no config files under %s — either they moved or this test stopped "+
			"looking where they are, and an empty set passes every assertion below", root)
	}

	return paths
}

// repoRoot walks up from the working directory to the module root.
//
// Locating it by go.mod rather than by a fixed "../.." keeps the test working if this package
// moves, and fails loudly rather than silently globbing an empty directory.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked to the filesystem root without finding go.mod")
		}

		dir = parent
	}
}

// shortName renders a path relative to the repository root, so a subtest is named
// "configs/example.yaml" rather than by an absolute path that differs per machine.
func shortName(t *testing.T, path string) string {
	t.Helper()

	rel, err := filepath.Rel(repoRoot(t), path)
	if err != nil {
		return filepath.Base(path)
	}

	return rel
}
