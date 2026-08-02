package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

// yamlFence matches a fenced ```yaml block, capturing its body.
var yamlFence = regexp.MustCompile("(?s)```yaml\\n(.*?)```")

// TestDocumentedConfigYAMLMatchesTheSchema extends audit finding P-10 to the documentation.
//
// P-10 was about the files under configs/ and examples/, and TestShippedConfigsSetWhatTheySay
// guards those. But a YAML block in a markdown file that says `# /etc/objectfs/config.yaml` above
// it is a file a user will copy just as readily, and it was rotting the same way and for the same
// reason: nothing compared it to the schema. Measured when this test was written, 11 of the 31
// documented blocks that claim to be ObjectFS configuration did not match the schema —
// docs/index.md offered a top-level `s3:` block and a 100 GB persistent cache under keys the
// loader has never had, which is P-10's exact shape in the file the docs site opens on.
//
// Non-strict decoding made these harmless-looking. Strict decoding makes them *breaking*: a user
// who copies one now gets a startup failure. That raises the cost of a stale example from "the
// setting quietly did nothing" to "the filesystem does not start", which is the right trade for
// real config files and an argument for keeping the documented ones correct by machine rather
// than by review.
//
// Two properties, both cheap:
//
//   - strict decoding, so a key the schema does not define fails here rather than in a user's
//     terminal;
//   - and a check that the block does not assign the same top-level key twice, which yaml.v2
//     reports only in strict mode and which silently discards half a block otherwise. Two of the
//     11 were this.
//
// A block is only checked if it *claims* to be ObjectFS configuration — at least one top-level
// key the schema recognizes. Markdown in this repo also contains CI workflow YAML, Kubernetes
// manifests, and the cost-model discount table, none of which are Configuration documents, and a
// test that demanded they parse as one would be wrong rather than strict.
func TestDocumentedConfigYAMLMatchesTheSchema(t *testing.T) {
	t.Parallel()

	schema := make(map[string]bool, len(TopLevelKeys()))
	for _, k := range TopLevelKeys() {
		schema[k] = true
	}

	for _, path := range markdownFiles(t) {
		rel, err := filepath.Rel(repoRoot(t), path)
		if err != nil {
			rel = path
		}

		if reason, ok := docsExemptFromConfigSchema[rel]; ok {
			t.Run(rel, func(t *testing.T) {
				t.Parallel()
				t.Skipf("exempt: %s", reason)
			})

			continue
		}

		raw, err := os.ReadFile(path) //nolint:gosec // a path from a walk of this repository
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		for i, match := range yamlFence.FindAllStringSubmatch(string(raw), -1) {
			block := match[1]

			if !claimsToBeObjectFSConfig(block, schema) {
				continue
			}

			t.Run(fmt.Sprintf("%s/block-%d", rel, i+1), func(t *testing.T) {
				t.Parallel()

				var cfg Configuration
				if err := yaml.UnmarshalStrict([]byte(block), &cfg); err != nil {
					t.Errorf("this documented configuration does not match the schema, so a reader "+
						"who copies it gets a startup failure naming the key (audit finding "+
						"P-10):\n%v\n\nthe block:\n%s\nthe schema's top-level keys are: %s",
						err, block, strings.Join(TopLevelKeys(), ", "))
				}
			})
		}
	}
}

// nestedSectionNames are keys that appear *below* the top level of the schema and nowhere else in
// this repository's YAML, so a block rooted at one of them is a misplaced ObjectFS config rather
// than some other document.
//
// This exists because the admission test below was too strict in one direction while being loose in
// the other, and the gap had a real cost. docs/features/multipart-uploads.md carried five blocks
// rooted at a bare `s3:` — the right keys at the wrong depth, since the schema nests them under
// `storage.s3`. Not one of them was checked: `s3` is not a top-level key, so the block did not
// "claim to be ObjectFS config", so it was skipped, so its four *invented* keys
// (multipart_threshold, target_throughput, optimization_level, enable_cargoship_optimization) went
// unnoticed through the exact test written to notice that. A reader copying any of them got a
// startup failure.
//
// The lesson generalizes past this one file: an admission test that decides what to check is itself
// load-bearing, and "the check passed" means nothing until you know the check ran.
var nestedSectionNames = map[string]bool{
	"s3":               true, // belongs under storage:
	"multipart":        true, // belongs under storage.s3:
	"persistent_cache": true, // belongs under cache:
	"circuit_breaker":  true, // belongs under network:
	"retry":            true, // belongs under network:
	"compression":      true, // belongs under performance:
}

// claimsToBeObjectFSConfig reports whether a YAML block is trying to be a Configuration document.
//
// The test is "does it use at least one key the schema recognizes", which is deliberately loose in
// the direction of checking too much rather than too little: a block using one real key and four
// invented ones is exactly the failure worth catching, and it would escape a stricter admission
// test. Blocks that parse as something other than a mapping — a list, a scalar, a Go snippet
// mislabelled as yaml — are not configuration and are skipped.
//
// A block rooted at a nested section name counts too, and fails: being at the wrong depth is a
// defect of the same kind as naming a key that does not exist, and has the same consequence for
// someone who copies it.
func claimsToBeObjectFSConfig(block string, schema map[string]bool) bool {
	var top map[string]any
	if err := yaml.Unmarshal([]byte(block), &top); err != nil {
		return false
	}

	for k := range top {
		if schema[k] || nestedSectionNames[k] {
			return true
		}
	}

	return false
}

// docsExemptFromConfigSchema names markdown files whose YAML deliberately does not match the
// current schema, with the reason.
//
// It is down to one entry. The other two were historical release notes — documents describing what
// v0.4.0 accepted, which could not be corrected to today's schema without falsifying the record of
// that release, and so were exempted rather than fixed. They are now deleted instead, which is what
// CLAUDE.md asks for: tracking belongs on GitHub, not in local markdown. Deletion is the better
// outcome for an exemption of that shape, because an exemption keeps a file a user can still find
// and copy from while removing the one check that would have told them it no longer loads.
//
// An entry here must name a file whose YAML is wrong *on purpose*. Anything else belongs fixed.
var docsExemptFromConfigSchema = map[string]string{
	"OBJECTFS.md": "a design document describing a proposed schema (mount:, latency_profile:, " +
		"security.kms_key) that was never implemented; the gap is the finding, not the file",
}

// markdownFiles returns every tracked markdown file in the repository.
func markdownFiles(t *testing.T) []string {
	t.Helper()

	root := repoRoot(t)

	var paths []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasSuffix(path, ".md") {
			paths = append(paths, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(paths) == 0 {
		t.Fatalf("found no markdown under %s, and an empty set passes every assertion below", root)
	}

	return paths
}
