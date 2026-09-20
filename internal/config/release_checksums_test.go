package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// goreleaser's `checksum: split: true` writes a **bare** digest: 64 lowercase hex characters, no
// filename, no trailing newline, 64 bytes exactly. `sha256sum -c` and `shasum -a 256 -c` cannot read
// that at all — they need a `<hash>  <name>` line and fail the whole file with "no properly formatted
// checksum lines found".
//
// This is not a hypothetical. `release.yml` ran `sha256sum -c "$asset.sha256"` over the eleven
// siblings and it failed the v0.15.0 publish *after* the 40-job gate, the packaging, the Docker push
// and the security scan had all gone green — a tag that can never be moved, because the module proxy
// had already cached it, and no release page. The comment above the broken line asserted the false
// thing in so many words ("the `<hash>  <name>` form that both this file and the per-asset siblings
// are written in"), which is why review did not catch it: the code agreed with its own documentation.
//
// So the format has three consumers that must not drift apart:
//
//   - `.goreleaser.yml` produces it (`split: true`),
//   - `.github/workflows/release.yml` cross-checks the siblings against `checksums.txt` before
//     signing, and prints a verify command into the release notes,
//   - `scripts/install.sh` verifies every download users install with.
//
// A file this small has no test of its own anywhere else, and the failure is invisible until a real
// release is halfway published. These tests are the coupling.

// checksumFile is a repository file that reads or writes the per-asset `.sha256` format.
type checksumFile struct {
	path string // repo-relative
	body string
}

func releaseChecksumConsumers(t *testing.T) []checksumFile {
	t.Helper()

	root := repoRoot(t)
	var out []checksumFile

	add := func(rel string) {
		body, err := os.ReadFile(filepath.Join(root, rel)) // #nosec G304 -- a constant repo-relative path
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		out = append(out, checksumFile{path: rel, body: string(body)})
	}

	// Walked rather than enumerated for the workflows, because a new workflow is exactly the thing
	// most likely to arrive with a fresh `sha256sum -c`, and an enumerated list only ever checks the
	// files that existed when it was written. The walk and its floor are readWorkflowTexts' (#504);
	// this file used to carry its own copy of both.
	for _, wf := range readWorkflowTexts(t) {
		out = append(out, checksumFile{path: wf.Path, body: wf.Body})
	}

	add(filepath.Join("scripts", "install.sh"))
	return out
}

// A `sha256sum`/`shasum` invocation, up to the next pipe, semicolon, ampersand or newline. Bounded at
// the command boundary so that `... | sha256sum -c` on one line and a `.sha256` path on another are
// not read as one command — the release notes legitimately split exactly that way.
var checksumInvocation = regexp.MustCompile(`\b(?:sha256sum|shasum)\b[^\n|;&]*`)

// `-c`, `--check`, or a bundled short flag such as `-rc`. Not a bare "c" anywhere in the line.
var checkFlag = regexp.MustCompile(`(^|\s)-{1,2}[a-zA-Z-]*\bc(heck)?\b`)

// TestNoConsumerPassesASiblingToChecksumCheckMode is the regression test for the v0.15.0 publish
// failure. `-c` against `checksums.txt` is correct and stays allowed; `-c` against a bare-digest
// `.sha256` is the defect.
func TestNoConsumerPassesASiblingToChecksumCheckMode(t *testing.T) {
	t.Parallel()

	invocations := 0
	for _, f := range releaseChecksumConsumers(t) {
		for i, line := range strings.Split(f.body, "\n") {
			// Comments are not executed, and both consumers deliberately describe this bug in prose —
			// including the exact broken command, so that nobody reintroduces it.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}

			for _, cmd := range checksumInvocation.FindAllString(line, -1) {
				invocations++
				if !checkFlag.MatchString(cmd) {
					continue
				}
				if !strings.Contains(cmd, ".sha256") {
					continue // checksums.txt and friends: the `<hash>  <name>` form `-c` needs
				}
				t.Errorf("%s:%d: check mode against a per-asset sibling:\n\t%s\n"+
					"\tgoreleaser's `checksum: split: true` writes a bare 64-hex digest with no\n"+
					"\tfilename, so -c fails the file outright with \"no properly formatted checksum\n"+
					"\tlines found\". This exact line failed the v0.15.0 publish after everything else\n"+
					"\thad gone green. Compare the digest itself, or supply the filename:\n"+
					"\t\tprintf '%%s  %%s\\n' \"$(cat f.sha256)\" f | sha256sum -c",
					f.path, i+1, strings.TrimSpace(cmd))
			}
		}
	}

	if invocations == 0 {
		t.Fatal("found no sha256sum/shasum invocations across the release checksum consumers; the " +
			"pattern has stopped matching and this test now passes vacuously")
	}
	t.Logf("checked %d sha256sum/shasum invocations", invocations)
}

// TestGoreleaserStillWritesSplitChecksums pins the premise. Both consumers are written for a bare
// digest; if `split` is turned off, goreleaser publishes one `checksums.txt` and no siblings at all,
// which breaks `install.sh` on every platform (it treats a missing `.sha256` as fatal and refuses to
// install unverified). That is a release-day outage, so it fails here instead.
func TestGoreleaserStillWritesSplitChecksums(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), ".goreleaser.yml")
	body, err := os.ReadFile(path) // #nosec G304 -- a constant repo-relative path
	if err != nil {
		t.Fatalf("read .goreleaser.yml: %v", err)
	}

	// The `checksum:` block, to the next top-level key. Matching `split: true` anywhere in the file
	// would also match a `split` under some future section.
	block := regexp.MustCompile(`(?ms)^checksum:\n(?:[ \t]+.*\n|\n)*`)
	m := block.FindString(string(body))
	if m == "" {
		t.Fatal("no top-level `checksum:` block in .goreleaser.yml. If checksums moved or were " +
			"turned off, scripts/install.sh must change with them: it fetches `<asset>.sha256` by " +
			"exact name and dies if it is absent")
	}

	if !regexp.MustCompile(`(?m)^[ \t]+split:[ \t]+true\b`).MatchString(m) {
		t.Errorf("`checksum:` no longer sets `split: true`:\n%s\n"+
			"Without split, goreleaser publishes a single checksums.txt and no per-asset siblings.\n"+
			"scripts/install.sh fetches `<asset>.sha256` and treats its absence as fatal, so every\n"+
			"platform's install breaks. release.yml's cross-check also requires one sibling per asset.", m)
	}

	if !regexp.MustCompile(`(?m)^[ \t]+algorithm:[ \t]+sha256\b`).MatchString(m) {
		t.Errorf("`checksum:` no longer sets `algorithm: sha256`:\n%s\n"+
			"Both consumers assume SHA-256: install.sh computes with sha256sum/shasum -a 256 and\n"+
			"checks for a 64-character digest, and release.yml asserts that length explicitly.", m)
	}
}

// TestInstallScriptVerifiesByComparingTheDigest is the non-vacuity guard for the test above. Forbidding
// `-c` is only half the coupling: a rewrite that dropped verification altogether, or that compared a
// digest against nothing, would satisfy the prohibition perfectly.
func TestInstallScriptVerifiesByComparingTheDigest(t *testing.T) {
	t.Parallel()

	rel := filepath.Join("scripts", "install.sh")
	body, err := os.ReadFile(filepath.Join(repoRoot(t), rel)) // #nosec G304 -- a constant repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	src := string(body)

	for _, want := range []struct {
		pattern regexp.Regexp
		what    string
	}{
		{
			// The published digest is read out of the sibling by field, which parses a bare digest
			// and a `<hash>  <name>` line identically — the property that let this survive the format
			// changing underneath it.
			*regexp.MustCompile(`\$1[^\n]*\.sha256|\.sha256[^\n]*\$1`),
			"reads the first whitespace-delimited field out of the `.sha256` sibling",
		},
		{
			// A non-empty check, so a sibling served as an empty body or an error page cannot pass by
			// comparing equal to an equally empty computed value.
			*regexp.MustCompile(`-n[ \t]+"\$want"`),
			"rejects an empty or malformed checksum file before comparing",
		},
		{
			*regexp.MustCompile(`\bsha256_of\b`),
			"computes the digest of the downloaded file",
		},
		{
			*regexp.MustCompile(`\[[ \t]+"\$want"[ \t]+!=[ \t]+"\$got"[ \t]+\]`),
			"compares the published digest against the computed one",
		},
	} {
		if !want.pattern.MatchString(src) {
			t.Errorf("%s no longer %s (looked for %s).\n"+
				"\tIf the verification was restructured, update this test to match the new shape —\n"+
				"\tbut do not remove the check. This script is how users install, it verifies with no\n"+
				"\tflag to skip, and TestNoConsumerPassesASiblingToChecksumCheckMode only forbids the\n"+
				"\twrong way to verify; this test is what requires that any verification happens.",
				rel, want.what, want.pattern.String())
		}
	}
}
