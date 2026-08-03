// The supported-operations table is a contract, and these are the two ways it has actually gone
// stale — both mechanically checkable, so neither should get a third turn.
//
// This is the fifth documentation gate, alongside docs_test.go (config YAML, version claims),
// docs_links_test.go (link targets), docs_symbols_test.go (Go symbols, CLI invocations), and
// changelog_test.go. Same reasoning as all of them: prose has no mechanism for noticing it is stale,
// so the only thing that can notice is a test.
//
// # What went wrong twice
//
// **A restated operation count.** Eight documents said "roughly 10 of ~40 VFS operations are
// implemented", each having copied it from the audit that measured it once. It was true when
// written and false the moment Setattr, Statfs, Fsync, Unlink, Rmdir, and Rename landed — six
// operations in three releases, and not one of the eight sentences changed. This is the version
// constant problem exactly (see version_test.go): one number, many copies, no way for a copy to
// learn it is wrong.
//
// **A refusal that is no longer refused.** `docs/architecture/overview.md` said `unlink`, `rmdir`,
// and `rename` were unimplemented and that `rm` returned `EROFS`; `docs-platform` told users `mv`
// fails with `ENOTSUP` "because there is no rename"; the playground worked around `rm` by shelling
// out to `aws s3 rm`. All three were accurate descriptions of v0.10.3 being read by users of a
// version where `rm` and `mv` work. Understating is friendlier than overstating, but it is still
// wrong, and it sends people to build workarounds for a problem that is fixed.
//
// # Why the second gate is scoped to the README's own table
//
// The tempting check is repo-wide: flag any line pairing an implemented operation with a refusal
// errno. It was written and measured before being rejected — it produces ten hits of which eight
// are CHANGELOG entries correctly describing what a past release did, and one is the
// getting-started note that deliberately records the old behavior so a user on an older build is
// not confused. A gate whose output is 80% false is a gate someone deletes.
//
// So the check is narrow and load-bearing instead: the README's own "Not implemented" table may not
// name an operation whose go-fuse interface `internal/fuse` demonstrably implements. That table is
// the authority every other document defers to, so it is the one place where being wrong propagates
// — and it is the row that was actually stale.
package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// vfsOperationCounts matches a claim about how many VFS operations exist or are implemented.
//
// Both spellings the repository used, because it used both: the digit form from the audit
// ("roughly 10 of ~40 VFS operations") and the word form someone typed when rephrasing it ("ten of
// forty VFS operations"). A pattern that caught only the first would have passed docs/VISION.md,
// which is the point version_test.go's comment makes about a narrow regexp being indistinguishable
// from a clean repository.
//
// Scoped to the phrase "VFS operations" / "POSIX operations" rather than to bare numbers. "10 of
// 40" on its own appears in benchmark tables and issue references; it is the claim about the
// operation *surface* that has no business being a hardcoded number.
// The word list is written out in full rather than abbreviated to the ones the repository happened to
// contain. A first draft had "ten|twenty|thirty|forty" and passed on "sixteen of forty VFS
// operations", which is exactly the trap version_test.go's comment describes: a narrow pattern that
// passes is indistinguishable from a correct repository. The mutation was run, it survived, and this
// is the fix.
const numberWord = `(?:one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|` +
	`fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|` +
	`eighty|ninety|hundred)`

const countHedge = `(?:roughly|about|approximately|around|some|nearly|only)?\s*`

var vfsOperationCounts = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b` + countHedge + `~?\d+\s+of\s+` + countHedge + `~?\d+\s+` +
		`(?:VFS|POSIX|filesystem)\s+operations`),
	regexp.MustCompile(`(?i)\b` + countHedge + numberWord + `\s+of\s+` + countHedge +
		numberWord + `\s+(?:VFS|POSIX|filesystem)\s+operations`),
}

// countExemptDocs are files where a count is a historical record rather than a claim about now.
//
// One entry, and it is not a loophole. A changelog entry says what a *past release* did, and Keep a
// Changelog's whole premise is that released sections are immutable — "ten of roughly forty VFS
// operations" in the v0.10.1 section is the measurement that motivated the work in that release, and
// it stays true about that release forever. Editing it to match today's surface would falsify the
// record, which is worse than the staleness this test exists to prevent.
//
// The distinction is the same one version_test.go draws between a plan and a claim: a document may
// discuss numbers freely, and what it may not do is assert one about the present. A changelog only
// ever talks about the past.
//
// Do not add a file here because it currently fails. An entry has to name a file whose counts are
// historical *by construction*; anything else belongs fixed.
var countExemptDocs = map[string]string{
	"CHANGELOG.md": "released sections are an immutable record of what each release changed; the " +
		"counts in them are measurements of the moment that release was cut and stay true about it",
}

// TestNoDocumentCountsTheImplementedOperations pins the same single-source rule the version has.
//
// A document may say the surface is a subset, may say which operations are missing, and may point at
// the README's table. What it may not do is assert a count, because a count is a measurement with a
// timestamp it cannot carry.
//
// The README's table is exempt in the sense that it does not state a count at all — it lists rows, so
// it is its own answer and cannot disagree with itself.
func TestNoDocumentCountsTheImplementedOperations(t *testing.T) {
	t.Parallel()

	for _, path := range markdownFiles(t) {
		rel := shortName(t, path)

		t.Run(rel, func(t *testing.T) {
			t.Parallel()

			if reason, exempt := countExemptDocs[rel]; exempt {
				t.Skipf("exempt: %s", reason)
			}

			body, err := os.ReadFile(path) //nolint:gosec // a path from a walk of this repository
			if err != nil {
				t.Fatalf("reading %s: %v", rel, err)
			}

			for i, line := range strings.Split(string(body), "\n") {
				for _, pattern := range vfsOperationCounts {
					if match := pattern.FindString(line); match != "" {
						t.Errorf(`%s:%d states an operation count: %q

A number here is a measurement of one moment. Six operations landed between v0.10.1 and v0.11.0 —
Setattr, Statfs, Fsync, Unlink, Rmdir, Rename — and eight documents went on reporting the count from
before them, because a sentence cannot notice that it is stale.

Say that a subset is implemented and point at the supported-operations table in README.md, which is
derived from the methods that exist in internal/fuse and internal/vfs.`,
							rel, i+1, match)
					}
				}
			}
		})
	}
}

// implementedNodeInterfaces are the go-fuse node interfaces whose presence in internal/fuse means an
// operation is supported outright, mapped to how the README's table names it.
//
// Only interfaces where support is *total*. Three are deliberately absent, and the reason is the
// same in each case — the interface is implemented but some cases within it are not, so its presence
// proves less than this gate would conclude:
//
//   - NodeSetattrer: chmod/chown work on files and return ENOTSUP on directories (#165), because a
//     directory marker could carry the mode but Getattr does not read it back. The README correctly
//     lists the directory case under "Not implemented" while listing the file case under
//     "Implemented", and a gate keyed on the interface alone would call that a contradiction.
//   - NodeFsyncer: fsync on a file is durable; on a directory it succeeds and does nothing, which is
//     under "Errors by design" rather than "Not implemented".
//   - NodeGetattrer / NodeLookuper / NodeOpener / NodeReaddirer / NodeCreater: nothing in the
//     not-implemented table could plausibly name them, so including them buys no coverage.
//
// The value is the string the README row would have to contain. Matching is on the whole row, so a
// row reading "Unlink | unlink, rm | ..." matches on "unlink" wherever in the row it appears.
var implementedNodeInterfaces = map[string]string{
	"NodeUnlinker": "unlink",
	"NodeRmdirer":  "rmdir",
	"NodeRenamer":  "rename",
	"NodeMkdirer":  "mkdir",
	"NodeStatfser": "statfs",
}

// nodeAssertion matches a compile-time interface assertion, `_ fs.NodeUnlinker = (*DirectoryNode)(nil)`.
//
// Reading the assertions rather than the method set is deliberate. The assertions are what make the
// support real: go-fuse probes each interface with a type assertion and substitutes a default when it
// is absent, and three of those defaults are harmful — mode 0000 for a missing Getattr under
// NullPermissions, a zeroed statfs, and *success* for a missing Unlink or Rmdir. A method with a
// typo'd signature compiles fine and is silently never called; the assertion is what fails. So the
// assertion block is both the proof of support and the thing this gate should trust.
var nodeAssertion = regexp.MustCompile(`_\s+fs\.(Node\w+)\s+=`)

// TestReadmeDoesNotListAnImplementedOperationAsMissing checks the table against the code.
//
// The stale rows this catches were real: after Unlink and Rmdir landed, the README's not-implemented
// table still carried `unlink` and `rmdir` as EROFS, and the tools-that-do-not-work list still said
// `mv` fails with ENOTSUP because there is no rename. All three were caught by hand, late, while
// writing the rename documentation — which is to say by luck.
func TestReadmeDoesNotListAnImplementedOperationAsMissing(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	implemented := assertedNodeInterfaces(t, filepath.Join(root, "internal", "fuse"))

	if len(implemented) == 0 {
		t.Fatal("found no fs.Node* assertions in internal/fuse, and an empty set passes every " +
			"assertion below; the assertion block in attributes.go is what this reads")
	}

	//nolint:gosec // a path built from the module root this test located itself
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("reading README.md, which holds the authoritative operations table: %v", err)
	}

	rows := notImplementedRows(string(body))
	if len(rows) == 0 {
		t.Fatal(`found no rows under README.md's "### Not implemented" heading. Either the heading was
renamed — in which case fix notImplementedRows, since the table is what every other document defers
to — or the table is empty, which would be a surprising thing to have happened silently.`)
	}

	for iface, token := range implementedNodeInterfaces {
		if !implemented[iface] {
			continue
		}

		for _, row := range rows {
			if !strings.Contains(strings.ToLower(row.text), token) {
				continue
			}

			t.Errorf(`README.md:%d lists %q as not implemented, but internal/fuse asserts fs.%s:

  %s

The assertion is what makes the operation real — go-fuse substitutes a default for an absent
interface, and for Unlink and Rmdir that default is *success*. So if the assertion is there, the
operation works and the row is stale.

Move it to the "Implemented" table with whatever caveat it needs. If the operation is only partly
supported, the row belongs under "Implemented" or "Errors by design" with the unsupported case
named — see how chmod/chown on a directory is split, and add the interface to the documented
exclusions in implementedNodeInterfaces.`,
				row.line, token, iface, strings.TrimSpace(row.text))
		}
	}
}

// tableRow is one markdown table row: its 1-based line number in the file, and its text.
type tableRow struct {
	line int
	text string
}

// notImplementedRows returns the table rows under README.md's "### Not implemented" heading.
//
// Bounded by the next heading of any level, so it cannot bleed into "Tools known not to work" below
// it. Header and separator rows are dropped: `|---|---|` would match any token asked about it, and a
// gate that always fires is a gate that gets deleted.
func notImplementedRows(markdown string) []tableRow {
	var (
		rows    []tableRow
		inTable bool
	)

	for i, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "#") {
			// Enter on the heading itself, leave on the next one whatever its level.
			inTable = strings.EqualFold(trimmed, "### Not implemented")

			continue
		}

		if !inTable || !strings.HasPrefix(trimmed, "|") {
			continue
		}

		// The separator, `|---|---|` in any spacing, and the header row.
		//
		// The header is identified by its first cell being "Operation" rather than by any later
		// column name. The second column's heading is the British spelling of "behavior", which the
		// misspell linter rejects in Go source while the README is entitled to keep — so matching on
		// it would mean carrying a nolint for a string this test only needs in order to skip a row.
		// Keying on the first cell also survives a column being renamed.
		if strings.Trim(trimmed, "|-: ") == "" || firstCell(trimmed) == "operation" {
			continue
		}

		rows = append(rows, tableRow{line: i + 1, text: trimmed})
	}

	return rows
}

// firstCell returns a table row's first cell, lowercased and trimmed of markdown emphasis.
//
// `| **Operation** |` and `| Operation |` both answer "operation", so a header row stays recognized
// if someone bolds it.
func firstCell(row string) string {
	cells := strings.Split(strings.Trim(row, "|"), "|")
	if len(cells) == 0 {
		return ""
	}

	return strings.ToLower(strings.Trim(cells[0], " *`_"))
}

// assertedNodeInterfaces returns the set of go-fuse node interfaces asserted anywhere under dir.
//
// Across the whole package rather than one file, because the assertions are deliberately not
// centralized: NodeRenamer is asserted in rename.go next to the code that implements it, with a
// comment explaining that its absence is why every release through v0.10.3 answered ENOTSUP. That
// placement is right, and a gate that only read attributes.go would have missed rename entirely —
// which is the operation whose documentation was most wrong.
func assertedNodeInterfaces(t *testing.T, dir string) map[string]bool {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	found := make(map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		//nolint:gosec // a path from a directory read of this repository
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}

		for _, match := range nodeAssertion.FindAllStringSubmatch(string(body), -1) {
			found[match[1]] = true
		}
	}

	return found
}
