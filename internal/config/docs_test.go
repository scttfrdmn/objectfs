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

// claimsToBeObjectFSConfig reports whether a YAML block is trying to be a Configuration document.
//
// The test is "does it use at least one key from the schema at the top level", which is
// deliberately loose in the direction of checking too much rather than too little: a block using
// one real key and four invented ones is exactly the failure worth catching, and it would escape a
// stricter admission test. Blocks that parse as something other than a mapping — a list, a scalar,
// a Go snippet mislabelled as yaml — are not configuration and are skipped.
func claimsToBeObjectFSConfig(block string, schema map[string]bool) bool {
	var top map[string]any
	if err := yaml.Unmarshal([]byte(block), &top); err != nil {
		return false
	}

	for k := range top {
		if schema[k] {
			return true
		}
	}

	return false
}

// docsExemptFromConfigSchema names markdown files whose YAML deliberately does not match the
// current schema, with the reason.
//
// Historical release notes are the honest case: RELEASE_NOTES_v0.4.0.md documents what v0.4.0
// accepted, and rewriting it to match v0.11.0's schema would make it a false record of that
// release. The right fix for those is deletion — they are local tracking files, which CLAUDE.md
// says belong on GitHub — and that is tracked separately; until then, exempting them by name with
// a reason keeps the check meaningful for the files a user is actually likely to copy from.
var docsExemptFromConfigSchema = map[string]string{
	"RELEASE_NOTES_v0.4.0.md": "a historical release note describing the v0.4.0 schema; " +
		"rewriting it to today's schema would falsify the record. Slated for deletion",
	"docs/v0.4.0-COMPLETION-SUMMARY.md": "same: a v0.4.0-era document. Slated for deletion",
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
