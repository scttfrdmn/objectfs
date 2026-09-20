package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
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

// Every released heading, with whatever follows the version captured loosely rather than matched.
//
// The tail is deliberately `(.*)` and not `- (\d{4}-\d{2}-\d{2})`. A heading whose date is malformed
// is exactly the heading these tests exist to report on, and a regexp that requires a well-formed
// date cannot see it — it drops out of the set and every assertion below passes on the headings that
// were already fine. Same failure as an absent link being invisible to a link checker: the traversal
// starts from what matched.
//
// The tail is also not empty in practice. `## [0.10.0] - 2026-02-23 — WITHDRAWN` carries an
// annotation after the date, so the date is a fixed-width prefix of the tail rather than the whole
// of it.
var changelogHeading = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\](.*)$`)

// changelogHeadingFloor is a non-vacuity floor, not an inventory. CHANGELOG.md had 20 released
// headings when this was written and the number only grows, so the floor never needs raising — its
// whole job is to fail loudly if changelogHeading stops matching the file's format, because that
// leaves every per-heading assertion trivially satisfied with nothing to show it read anything.
const changelogHeadingFloor = 15

// changelogRelease is one parsed `## [X.Y.Z] - YYYY-MM-DD` heading, in the order the file lists it.
type changelogRelease struct {
	version string
	date    time.Time
	tail    string // the raw text after the version, for failure messages
}

// changelogReleases parses every released heading in file order — newest first, which is the order
// Keep a Changelog prescribes and the order the monotonicity check below depends on.
//
// Headings whose tail cannot be parsed are reported and skipped rather than silently dropped, so a
// malformed date fails this function's caller instead of shrinking the set it examines.
func changelogReleases(t *testing.T) []changelogRelease {
	t.Helper()

	body := changelogBody(t)

	matches := changelogHeading.FindAllStringSubmatch(body, -1)
	if len(matches) < changelogHeadingFloor {
		t.Fatalf("found %d `## [X.Y.Z]` headings in CHANGELOG.md, expected at least %d.\n"+
			"changelogHeading has stopped matching the file's format. Point it at the new one rather "+
			"than lowering the floor — with a regexp that matches nothing, every assertion about "+
			"heading dates and ordering passes on an empty set",
			len(matches), changelogHeadingFloor)
	}

	releases := make([]changelogRelease, 0, len(matches))

	for _, m := range matches {
		version, tail := m[1], m[2]

		rest, ok := strings.CutPrefix(tail, " - ")
		if !ok {
			t.Errorf("CHANGELOG.md's `## [%s]%s` heading is missing its ` - YYYY-MM-DD` date.\n"+
				"Keep a Changelog puts the release date in the heading; without one there is nothing to "+
				"check the release order against.", version, tail)

			continue
		}

		// The date is a fixed-width prefix, because a heading may annotate what follows it — see
		// `## [0.10.0] - 2026-02-23 — WITHDRAWN`. Anything after the date has to be separated by a
		// space, or the ten characters taken here are the front of some longer token rather than a date.
		date, annotation := rest, ""
		if len(rest) > len(time.DateOnly) {
			date, annotation = rest[:len(time.DateOnly)], rest[len(time.DateOnly):]
		}

		when, err := time.Parse(time.DateOnly, date)
		if err != nil {
			t.Errorf("CHANGELOG.md's `## [%s]%s` heading has %q where a YYYY-MM-DD date belongs: %v.\n"+
				"This renders fine and reads fine to a human, which is why it needs a parser: a date "+
				"nothing parses is a date nothing can check for being in the future or out of order.",
				version, tail, date, err)

			continue
		}

		if annotation != "" && !strings.HasPrefix(annotation, " ") {
			t.Errorf("CHANGELOG.md's `## [%s]%s` heading runs %q straight into %q with no separator, so "+
				"the date parsed here is the front of a longer token rather than the whole field.",
				version, tail, date, annotation)

			continue
		}

		releases = append(releases, changelogRelease{version: version, date: when, tail: tail})
	}

	return releases
}

// TestChangelogNewestSectionIsTheDeclaredVersion asserts the top released heading names the version
// the binary reports.
//
// Cutting a release is two edits with nothing connecting them: promote `## [Unreleased]` to
// `## [X.Y.Z] - <date>`, and bump the `version` constant in cmd/objectfs/main.go. Doing only the
// second ships a binary reporting a version the changelog has never heard of; doing only the first
// documents a release that does not exist. Neither breaks a build.
//
// The constant is the authority — CLAUDE.md says so, and TestNoDocumentRestatesTheCurrentVersion
// holds the rest of the repository to it — so this is a one-directional check against it rather than
// a comparison of two equals.
func TestChangelogNewestSectionIsTheDeclaredVersion(t *testing.T) {
	t.Parallel()

	releases := changelogReleases(t)
	if len(releases) == 0 {
		t.Fatal("no released headings parsed, so there is no newest one to compare")
	}

	newest, want := releases[0].version, declaredVersion(t)
	if newest != want {
		t.Errorf("CHANGELOG.md's newest released heading is `## [%s]`, but cmd/objectfs/main.go "+
			"declares version %q.\n"+
			"One half of cutting a release was done without the other. If %s is the release being cut, "+
			"promote `## [Unreleased]` to `## [%s] - <today>`; if %s was released, bump the constant.\n"+
			"Checked in file order, not by sorting, so a new heading added below an old one fails here "+
			"too — which is a real way to write it and an invisible one to read.",
			newest, want, want, want, newest)
	}
}

// TestChangelogSectionDatesAreRealAndInOrder asserts every release date parses, none is in the
// future, and the file runs newest-first.
//
// All three were hand-checked when v0.16.0 was cut — that the new `## [0.16.0] - 2026-09-18` heading
// was a real date, was not in the future, and sat above v0.15.1 — and nothing in the repository
// checked any of it. A hand-check is a gate that exists once.
//
// The future case is the one worth having. A mistyped year renders identically to a correct one, and
// nothing downstream objects: the heading links, the diff resolves, the release ships. It only
// surfaces later as a changelog that claims work landed before it was written.
func TestChangelogSectionDatesAreRealAndInOrder(t *testing.T) {
	t.Parallel()

	releases := changelogReleases(t)

	// Midnight UTC tomorrow. A release cut from a timezone ahead of UTC can legitimately carry
	// tomorrow's UTC date, and failing CI over that would be a flake rather than a finding — while a
	// mistyped month or year, which is what this catches, is wrong by far more than a day.
	now := time.Now().UTC()
	limit := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)

	for i, r := range releases {
		if r.date.After(limit) {
			t.Errorf("CHANGELOG.md's `## [%s]%s` is dated %s, which is in the future (today is %s UTC).\n"+
				"A future date renders and links exactly like a correct one, so nothing else in the "+
				"release process objects to it.",
				r.version, r.tail, r.date.Format(time.DateOnly), now.Format(time.DateOnly))
		}

		if i == 0 {
			continue
		}

		previous := releases[i-1]

		// Equal dates are fine and common — 0.10.1, 0.10.2 and 0.10.3 all shipped on 2026-08-02. Only
		// an older release dated *after* a newer one is a contradiction.
		if r.date.After(previous.date) {
			t.Errorf("CHANGELOG.md lists `## [%s]` (%s) below `## [%s]` (%s), but its date is later.\n"+
				"The file runs newest-first, so either the sections are out of order or one of the two "+
				"dates is wrong. Equal dates are fine; this is strictly later.",
				r.version, r.date.Format(time.DateOnly),
				previous.version, previous.date.Format(time.DateOnly))
		}

		if !byVersion.Less([]string{r.version, previous.version}, 0, 1) {
			t.Errorf("CHANGELOG.md lists `## [%s]` below `## [%s]`, which is not descending order.\n"+
				"Compared numerically, not lexically — 0.9.0 sorts below 0.10.0 — so this is a genuine "+
				"ordering problem or a duplicated heading, not a sorting artifact.",
				r.version, previous.version)
		}
	}

	t.Logf("%d released headings, newest %s dated %s", len(releases), releases[0].version,
		releases[0].date.Format(time.DateOnly))
}

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
