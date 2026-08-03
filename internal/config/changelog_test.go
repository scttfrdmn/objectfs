package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A released section heading — `## [0.10.3] - 2026-08-02`. `## [Unreleased]` is deliberately not
// matched: it is the one section with no version to compare against and no tag to link to.
var changelogSection = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`)

// A link-reference definition — `[0.10.3]: https://…/compare/v0.10.2...v0.10.3`. Captures the
// version and the target separately, because the two halves fail independently: a missing
// definition breaks the heading's link, and a definition comparing from the wrong tag produces a
// diff that silently spans two releases.
var changelogLinkRef = regexp.MustCompile(`(?m)^\[(\d+\.\d+\.\d+)\]:\s*(\S+)$`)

// The `[Unreleased]` definition, whose left-hand tag has to track the newest release.
var unreleasedLinkRef = regexp.MustCompile(`(?m)^\[Unreleased\]:\s*(\S+)$`)

// TestChangelogSectionsAllHaveLinkReferences pins the halves of CHANGELOG.md that are maintained by
// hand in two places at once.
//
// Keep a Changelog puts each version in a bracketed heading and defines the link separately at the
// bottom of the file. That is two edits per release with nothing connecting them, and markdown fails
// silently in both directions: an undefined reference renders as literal text `[0.10.2]` rather than
// as a link, and a definition for a section that does not exist renders as nothing at all. Neither
// is a broken build, a failing lint, or a visibly wrong page — the heading just stops being
// clickable, which is not something a reader reports.
//
// It had already happened. `[0.10.2]` was never defined, so its heading was inert text, and
// `[Unreleased]` still compared from `v0.10.1` — which meant the diff link that is supposed to show
// "what is on main but not released" spanned two releases and about 52 changelog entries of already
// released work. Found while cutting v0.10.3, not by anyone reading the page.
//
// This is the same shape as the defect v0.10.3 exists to correct, one file over: a fact restated in
// a second place, with no mechanism to notice when the two disagree.
func TestChangelogSectionsAllHaveLinkReferences(t *testing.T) {
	t.Parallel()

	body := changelogBody(t)

	sections := changelogSection.FindAllStringSubmatch(body, -1)
	if len(sections) == 0 {
		t.Fatal("CHANGELOG.md has no `## [X.Y.Z]` section headings. If the format changed, point " +
			"this test at the new one rather than deleting it — the check is what keeps the headings " +
			"and the link definitions from drifting apart")
	}

	refs := map[string]string{}
	for _, m := range changelogLinkRef.FindAllStringSubmatch(body, -1) {
		refs[m[1]] = m[2]
	}

	// Reach assertion, not a finding assertion. A regexp that stops matching leaves both loops below
	// iterating over nothing and the test green, which is exactly how the link-checker mutation in
	// docs_links_test.go passed on zero links.
	if len(refs) == 0 {
		t.Fatal("matched section headings but zero link-reference definitions, which means " +
			"changelogLinkRef no longer matches the file's format. Every assertion below would pass " +
			"vacuously")
	}

	t.Logf("%d released sections, %d link references", len(sections), len(refs))

	seen := map[string]bool{}
	for _, m := range sections {
		version := m[1]
		seen[version] = true

		if _, ok := refs[version]; !ok {
			t.Errorf("CHANGELOG.md has a `## [%s]` section with no `[%s]:` link definition.\n"+
				"The heading renders as the literal text \"[%s]\" rather than as a link, which is a "+
				"failure markdown reports nowhere. Add:\n"+
				"  [%s]: https://github.com/scttfrdmn/objectfs/compare/vPREVIOUS...v%s",
				version, version, version, version, version)
		}
	}

	for version := range refs {
		if !seen[version] {
			t.Errorf("CHANGELOG.md defines `[%s]: %s` with no `## [%s]` section to use it.\n"+
				"An unused link definition renders as nothing, so this is invisible in the published "+
				"file. Either the section was renamed or the definition outlived it.",
				version, refs[version], version)
		}
	}
}

// TestChangelogUnreleasedComparesFromTheNewestRelease pins the one link definition that has to
// change on every release rather than gain a sibling.
//
// `[Unreleased]` is a `compare/vX...HEAD` link, and X has to be the newest released tag for it to
// mean "not yet released". When a release is cut and this is left behind, the link keeps resolving
// and keeps rendering — it just answers a different question, showing released work as unreleased.
// It was two releases stale when this test was written, spanning v0.10.1...HEAD after both v0.10.2
// and v0.10.3 existed.
//
// Checked against the version *constant* rather than against the newest section heading, because the
// constant is the authority the rest of the repository is already held to (see
// TestNoDocumentRestatesTheCurrentVersion) — and because a release that bumps the constant and
// forgets the heading is a separate defect this would otherwise mask.
func TestChangelogUnreleasedComparesFromTheNewestRelease(t *testing.T) {
	t.Parallel()

	body := changelogBody(t)

	m := unreleasedLinkRef.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("CHANGELOG.md has no `[Unreleased]:` link definition. The `## [Unreleased]` heading " +
			"needs one, and it is the definition most likely to be forgotten at release time because " +
			"it is edited rather than added")
	}
	target := m[1]

	want := fmt.Sprintf("v%s...HEAD", declaredVersion(t))
	if !strings.HasSuffix(target, want) {
		t.Errorf("CHANGELOG.md's `[Unreleased]` link is %q, which should end in %q.\n"+
			"It has to compare from the newest released tag, or it presents already-released work as "+
			"unreleased — and it keeps resolving and rendering while doing so, which is why this went "+
			"unnoticed across two releases. The expected tag comes from the `version` constant in "+
			"cmd/objectfs/main.go.", target, want)
	}
}

// TestChangelogLinkReferencesCompareConsecutiveReleases asserts each release's diff link starts at
// the release immediately before it.
//
// A `compare/vA...vB` link where A is not B's predecessor is the subtlest of these failures: it
// renders, it resolves, and GitHub shows a real diff — just one covering more releases than the
// section it is attached to. Copying the previous release's definition and editing only the
// right-hand side produces exactly this, and that is the most natural way to add one by hand.
func TestChangelogLinkReferencesCompareConsecutiveReleases(t *testing.T) {
	t.Parallel()

	body := changelogBody(t)

	refs := map[string]string{}
	versions := []string{}
	for _, m := range changelogLinkRef.FindAllStringSubmatch(body, -1) {
		refs[m[1]] = m[2]
		versions = append(versions, m[1])
	}
	if len(versions) < 2 {
		t.Fatalf("found %d link-reference definitions, need at least 2 to check consecutiveness — "+
			"changelogLinkRef has probably stopped matching the file's format", len(versions))
	}

	sort.Sort(byVersion(versions))

	// The oldest release has no predecessor and links at its tag rather than at a comparison, so it
	// is skipped rather than exempted by name.
	for i := 1; i < len(versions); i++ {
		version, previous := versions[i], versions[i-1]

		target := refs[version]
		if !strings.Contains(target, "/compare/") {
			continue
		}

		want := fmt.Sprintf("v%s...v%s", previous, version)
		if !strings.HasSuffix(target, want) {
			t.Errorf("CHANGELOG.md's `[%s]` link is %q, which should end in %q.\n"+
				"%s is the release immediately before %s, so a diff starting anywhere else spans more "+
				"releases than this section documents. This renders and resolves either way, which is "+
				"why it needs a test rather than a reader.", version, target, want, previous, version)
		}
	}
}

// changelogBody reads CHANGELOG.md from the repository root.
func changelogBody(t *testing.T) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), "CHANGELOG.md")

	body, err := os.ReadFile(path) //nolint:gosec // a path built from the module root
	if err != nil {
		t.Fatalf("reading CHANGELOG.md: %v", err)
	}

	return string(body)
}

// declaredVersion returns the authoritative version — the `version` constant in
// cmd/objectfs/main.go. Shares versionConstant with version_test.go rather than re-deriving it,
// since two regexps for one authority is the defect this package's tests exist to catch.
func declaredVersion(t *testing.T) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), "cmd", "objectfs", "main.go")

	body, err := os.ReadFile(path) //nolint:gosec // a path built from the module root
	if err != nil {
		t.Fatalf("reading cmd/objectfs/main.go, which holds the authoritative version: %v", err)
	}

	m := versionConstant.FindSubmatch(body)
	if m == nil {
		t.Fatal("cmd/objectfs/main.go no longer declares `version = \"…\"`")
	}

	return string(m[1])
}

// byVersion orders dotted numeric versions numerically. Lexical order gets 0.9.0 and 0.10.0 the
// wrong way round, which would make the consecutiveness check above report the correct file as
// broken and an actually-broken one as fine.
type byVersion []string

func (v byVersion) Len() int      { return len(v) }
func (v byVersion) Swap(i, j int) { v[i], v[j] = v[j], v[i] }

func (v byVersion) Less(i, j int) bool {
	a, b := strings.Split(v[i], "."), strings.Split(v[j], ".")

	for k := 0; k < len(a) && k < len(b); k++ {
		if a[k] != b[k] {
			return atoiOrZero(a[k]) < atoiOrZero(b[k])
		}
	}

	return len(a) < len(b)
}

// atoiOrZero parses a version component, treating anything unparseable as 0. The regexp above only
// admits digits, so this cannot be reached with non-numeric input from this file.
func atoiOrZero(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}

		n = n*10 + int(c-'0')
	}

	return n
}
