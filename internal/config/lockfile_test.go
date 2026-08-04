package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// npmManifest is the subset of package.json this test compares. Only the dependency tables: the
// lockfile's root entry mirrors these four and nothing else about the manifest.
type npmManifest struct {
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

// npmLockfile is the subset of package-lock.json this test reads.
//
// `packages[""]` — the empty-string key — is the lockfile's record of the root project, and npm
// writes the manifest's own dependency ranges into it verbatim. That is what makes this test
// possible from a single commit: the lockfile carries a copy of what the manifest said when it was
// generated, so the two can be compared without knowing what the base branch looked like.
type npmLockfile struct {
	LockfileVersion int                    `json:"lockfileVersion"`
	Packages        map[string]npmManifest `json:"packages"`
}

// npmDirectories are the directories in this repository with a package.json.
//
// Listed rather than discovered, and the first subtest below is what keeps the list honest: it
// globs for package.json and fails if it finds one not named here. A discovered list would have to
// exclude node_modules and would quietly pass on a directory nobody had noticed; a named list plus
// a completeness check fails loudly on the same input.
var npmDirectories = []string{
	"docs-platform",
	"sdks/javascript",
}

// TestNPMLockfilesAgreeWithTheirManifests is the durable half of #332.
//
// `npm ci` refuses to install when package.json and package-lock.json disagree, and it says so at
// install time — which means the failure lands in whichever job happens to install first, phrased
// as an npm error about a version mismatch rather than as "someone edited a manifest by hand".
// #332 hit exactly that: Dependabot PRs opened before the lockfile existed edited the manifest
// only, and after the lockfile landed they failed with
//
//	npm error `npm ci` can only install packages when your package.json and package-lock.json
//	npm error are in sync. [...] Invalid: lock file's @types/node@18.19.130 does not satisfy
//	npm error @types/node@26.1.2
//
// This runs the same comparison in the `test` job, so it fails in seconds with the directory and
// package named, before any install. It also runs for `docs-platform`, whose lockfile arrived later
// (#214) and which has the same exposure.
//
// Why compare the lockfile's own root entry rather than diffing the two files a PR touched: a
// changed-files check needs a base ref, does not work on a local `go test`, and passes a hand edit
// that touches both files without regenerating the tree. `packages[""]` is npm's own copy of the
// manifest's ranges, so comparing against it is the check npm makes, available from one checkout.
//
// What this does not check is the resolved tree below the root — whether the locked versions
// actually satisfy the ranges, and whether every transitive dependency is present. Only `npm ci`
// answers that, and the `sdk-metrics` and `docs-site` jobs run it. This is the cheap gate that
// catches the common case: a manifest edited without regenerating the lock.
func TestNPMLockfilesAgreeWithTheirManifests(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	t.Run("every-package-json-is-listed", func(t *testing.T) {
		t.Parallel()

		listed := make(map[string]bool, len(npmDirectories))
		for _, dir := range npmDirectories {
			listed[dir] = true
		}

		// Tracked files, via the same helper the markdown gates use, so node_modules and any other
		// ignored tree are excluded by .gitignore rather than by a skip-list here.
		//
		// `*package.json`, with the leading star, because a git pathspec without one is anchored:
		// bare `package.json` matches the repository root and nothing else, which found zero files
		// here and would have made this check vacuous rather than failing. The star matches any
		// path *ending* in that string, so `my-package.json` would match too — hence the base-name
		// test below.
		for _, path := range trackedFiles(t, "*package.json") {
			if filepath.Base(path) != "package.json" {
				continue
			}

			dir := filepath.Dir(path)
			if !listed[dir] {
				t.Errorf("%s/package.json is not in npmDirectories, so nothing checks it against a "+
					"lockfile; add it there (and commit a lockfile for it if there is none)", dir)
			}
		}
	})

	for _, dir := range npmDirectories {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			var manifest npmManifest
			readJSON(t, filepath.Join(root, dir, "package.json"), &manifest)

			var lock npmLockfile
			readJSON(t, filepath.Join(root, dir, "package-lock.json"), &lock)

			// v2 and v3 both carry `packages`; v1 carried only `dependencies` and has no root entry
			// to compare against. npm 7 shipped v2 in 2020, so requiring v2+ costs nothing and
			// means the assertions below cannot silently read a zero value.
			if lock.LockfileVersion < 2 {
				t.Fatalf("lockfileVersion is %d; this test reads packages[\"\"], which v1 lockfiles "+
					"do not have. Regenerate with npm 7 or later", lock.LockfileVersion)
			}

			rootEntry, ok := lock.Packages[""]
			if !ok {
				t.Fatal(`the lockfile has no packages[""] entry, so there is nothing recording what ` +
					`the manifest said when it was generated`)
			}

			for _, field := range []struct {
				name           string
				inManifest     map[string]string
				inLockfileRoot map[string]string
			}{
				{"dependencies", manifest.Dependencies, rootEntry.Dependencies},
				{"devDependencies", manifest.DevDependencies, rootEntry.DevDependencies},
				{"optionalDependencies", manifest.OptionalDependencies, rootEntry.OptionalDependencies},
				{"peerDependencies", manifest.PeerDependencies, rootEntry.PeerDependencies},
			} {
				for name, want := range field.inManifest {
					got, present := field.inLockfileRoot[name]

					switch {
					case !present:
						t.Errorf("%s: package.json requires %s@%s and the lockfile does not record it "+
							"at all, so `npm ci` will refuse to install. Run `npm install` in %s and "+
							"commit the lockfile alongside the manifest change",
							field.name, name, want, dir)
					case got != want:
						t.Errorf("%s: package.json requires %s@%s but the lockfile was generated "+
							"against %s@%s, so `npm ci` will refuse to install. Run `npm install` in "+
							"%s and commit the lockfile alongside the manifest change",
							field.name, name, want, name, got, dir)
					}
				}

				for name, stale := range field.inLockfileRoot {
					if _, present := field.inManifest[name]; !present {
						t.Errorf("%s: the lockfile records %s@%s but package.json no longer requires "+
							"it, so the lockfile predates a removal. Run `npm install` in %s and "+
							"commit the result", field.name, name, stale, dir)
					}
				}
			}
		})
	}
}

// trackedFiles returns the absolute path of every file in the repository whose name matches a
// `git ls-files` pathspec.
//
// Tracked, and not a filesystem walk, for the reason `markdownFiles` records: `git ls-files`
// inherits .gitignore, so `node_modules` is excluded because the repository ignores it rather than
// because a skip-list here happens to name it. That matters more for package.json than for
// markdown — a walk would return several hundred of them, all vendored, and the completeness check
// above would be a wall of errors about other people's packages.
func trackedFiles(t *testing.T, pathspec string) []string {
	t.Helper()

	root := repoRoot(t)

	// G204 fires because `pathspec` is a variable rather than a literal, which it was before this
	// helper was shared. Suppressed rather than reworked: both call sites pass a compile-time
	// constant, and `exec` builds an argv directly with no shell, so a pathspec cannot become a
	// second command. If a caller ever passes something derived from a file or an environment
	// variable, this suppression stops being honest and the check should come back.
	cmd := exec.CommandContext(t.Context(), "git", "ls-files", "-z", pathspec) //nolint:gosec // literal pathspec, no shell
	cmd.Dir = root

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files %q in %s: %v", pathspec, root, err)
	}

	var paths []string

	// -z, so NUL-separated: a path may contain a newline, which `git ls-files` would otherwise
	// quote rather than emit.
	for rel := range strings.SplitSeq(string(out), "\x00") {
		if rel == "" {
			continue
		}

		paths = append(paths, rel)
	}

	if len(paths) == 0 {
		t.Fatalf("no tracked files match %q, and an empty set passes every assertion above", pathspec)
	}

	return paths
}

// readJSON decodes a file, failing the test with the path on any error.
func readJSON(t *testing.T, path string, into any) {
	t.Helper()

	raw, err := os.ReadFile(path) //nolint:gosec // a fixed path under this repository
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}
