package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every third-party action must be pinned to a full commit sha.
//
// A tag is mutable. Whoever controls an action's repository can repoint `v7` at different code, and
// that code runs with this repository's `GITHUB_TOKEN` — on the release path with `contents: write`,
// `packages: write` and `id-token: write`, which is enough to publish a release, push an image to
// ghcr, and mint a Sigstore certificate carrying this repository's identity. A sha is immutable, so
// pinning turns "trust the action's maintainer, permanently and prospectively" into "trust this
// exact tree, which was reviewed once".
//
// This test is the part that lasts. Pinning 66 references is a one-time edit; the failure mode is
// the *next* action someone adds, which will arrive as `@v1` because that is what every README
// shows, and nothing about a floating tag looks wrong in a diff. Dependabot updates sha pins in
// place and keeps the trailing comment in sync, so the pins do not go stale either.
//
// The directory is walked rather than enumerated. A list of workflow files checks the files that
// existed when the list was written, and a new workflow is exactly the thing most likely to arrive
// with an unpinned action — so the walk carries a floor instead, and fails if it finds implausibly
// few files.
//
//nolint:gocognit // one walk with four named failure modes; splitting it would hide the floor
func TestEveryActionIsPinnedToASha(t *testing.T) {
	t.Parallel()

	// `uses:` on its own line, capturing what follows. Comments after the reference are allowed and
	// are what carries the human-readable version.
	uses := regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s*(\S+)`)

	// A pin is 40 lowercase hex characters. Not `[0-9a-f]+`: a short sha is ambiguous, and GitHub
	// resolves an abbreviated ref, so a 7-character "pin" is only as immutable as the objects that
	// happen to share its prefix today.
	pinned := regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)

	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	// The floor. Five workflow files exist today; below four, this test has stopped looking at the
	// thing it claims to check and should say so rather than pass.
	const minWorkflowFiles = 4

	files := 0
	references := 0
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		files++

		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path) // #nosec G304 -- a directory entry from .github/workflows
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := strings.Split(string(body), "\n")

		for i, line := range lines {
			m := uses.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ref := m[1]

			// A local reusable workflow is this repository's own code at this repository's own
			// commit. There is nothing to pin and no third party to trust.
			if strings.HasPrefix(ref, "./") {
				continue
			}
			references++

			if !pinned.MatchString(ref) {
				t.Errorf("%s:%d: %s is not pinned to a 40-character commit sha\n"+
					"\tuse `uses: <action>@<sha> # <version>`; resolve the sha with\n"+
					"\t`gh api repos/<owner>/<repo>/git/ref/tags/<tag> -q .object.sha`",
					e.Name(), i+1, ref)
				continue
			}

			// The version comment is not decoration. A sha alone says nothing about what it is, so
			// nobody reviewing a bump can tell an upgrade from a downgrade, and Dependabot uses the
			// comment to name the version it is moving from.
			if !strings.Contains(line, "# ") {
				t.Errorf("%s:%d: %s is pinned but carries no version comment; "+
					"append `# vX.Y.Z` naming the release the sha is",
					e.Name(), i+1, ref)
			}
		}
	}

	if files < minWorkflowFiles {
		t.Fatalf("found %d workflow files in %s, expected at least %d — this test is not looking "+
			"at what it claims to", files, dir, minWorkflowFiles)
	}
	if references == 0 {
		t.Fatalf("found no third-party action references across %d workflow files; the `uses:` "+
			"pattern has stopped matching and this test now passes vacuously", files)
	}
	t.Logf("checked %d third-party action references across %d workflow files", references, files)
}
