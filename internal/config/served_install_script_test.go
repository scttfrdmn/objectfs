package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// This file gates the installer as a *published address*: objectfs.io/install.sh.
//
// #138's acceptance criterion is `curl -fsSL https://objectfs.io/install.sh | bash`, and for the whole
// life of that issue the address answered with a 404 — first because objectfs.io served nothing at
// all, then, once Pages was on, because pages.yml published web/ and the MkDocs tree and no installer.
// README.md and the landing page both pointed at raw.githubusercontent.com instead, which works and is
// not what the issue asked for.
//
// Serving it introduces a failure mode that did not exist while there was one copy, and it is the
// reason this file exists rather than a comment in the workflow. Every other gate in this package
// checks `scripts/install.sh`: the platform mapping against release.yml's matrix, the checksum
// refusals, the preflight ordering, the landing page's flags. If the served file is ever a *different*
// file — a committed duplicate under web/, a copy from the wrong path, a later step that rewrites it —
// then the copy users pipe into bash is the copy nothing tests, and every one of those gates is
// asserting against a file no user runs.
//
// So two things are asserted: the workflow copies the script and compares it byte for byte, and no
// second copy is committed anywhere the deploy would pick up instead.

// pagesWorkflow reads .github/workflows/pages.yml.
func pagesWorkflow(t *testing.T) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), ".github", "workflows", "pages.yml")

	//nolint:gosec // A path built from the repository root, in a test.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}

	return string(b)
}

// TestPagesWorkflowServesTheInstallScript asserts the address exists and serves the reviewed file.
//
// Comments are stripped first, for the reason release_packages_test.go's jobStep does it: this
// workflow's header explains at length why the script is copied rather than committed, and a test that
// matched the explanation instead of the step would pass on a workflow that had lost the step and kept
// the paragraph about it.
func TestPagesWorkflowServesTheInstallScript(t *testing.T) {
	t.Parallel()

	workflow := withoutComments(pagesWorkflow(t))

	if !strings.Contains(workflow, "cp scripts/install.sh _site/install.sh") {
		t.Error(".github/workflows/pages.yml does not copy scripts/install.sh to the site root. " +
			"`curl -fsSL https://objectfs.io/install.sh | bash` is #138's acceptance criterion and the " +
			"address 404s without this step — which is a worse outcome than the raw.githubusercontent.com " +
			"URL it replaced, because a documented one-liner that 404s reads as a broken project")
	}

	// The byte comparison, not just the copy. A `cp` from the wrong path succeeds, and a site serving
	// some other file called install.sh is the failure this whole file is about.
	if !strings.Contains(workflow, "cmp scripts/install.sh _site/install.sh") {
		t.Error(".github/workflows/pages.yml copies the installer without comparing it to " +
			"scripts/install.sh. Presence is not the property that matters: every gate in this package " +
			"checks scripts/install.sh, so a served copy that differs means the file users run is the " +
			"one nothing tests")
	}

	// And the trigger. Without scripts/install.sh in the paths filter the site keeps serving whatever
	// copy was current at the last docs change, and nothing about that is visible: the repository's
	// copy is correct, CI is green, and the served one is stale.
	//
	// Matched as a whole sequence entry rather than as a substring, because the path also appears in
	// the `cp` and `cmp` commands above — a `strings.Contains` here would be satisfied by the copy step
	// alone and would pass on a workflow with no trigger for the file at all. Unquoted, because
	// pre-commit's pretty-format-yaml unquotes and dedents every block sequence in this file: the first
	// version of this test asserted `'scripts/install.sh'` and the formatter would have failed it.
	if !hasSequenceEntry(workflow, "scripts/install.sh") {
		t.Error(".github/workflows/pages.yml does not list scripts/install.sh in its push paths " +
			"filter, so editing the installer does not redeploy the site. The served copy then lags " +
			"behind the repository until some unrelated docs change happens to trigger a build, and " +
			"every check is green the whole time")
	}
}

// hasSequenceEntry reports whether any line of a YAML document is exactly `- value`, at any indent.
//
// Deliberately not a YAML parse. What this needs to distinguish is a sequence entry from the same
// string appearing inside a `run:` block, and a line-level check does that without deciding which of
// several possible quoting and indentation forms the file is currently in — which pretty-format-yaml
// rewrites without asking.
func hasSequenceEntry(doc, value string) bool {
	// Both quoting styles, since the formatter's preference is not this test's business.
	forms := []string{
		"- " + value,
		"- '" + value + "'",
		`- "` + value + `"`,
	}

	for line := range strings.SplitSeq(doc, "\n") {
		if slices.Contains(forms, strings.TrimSpace(line)) {
			return true
		}
	}

	return false
}

// TestNoSecondCopyOfTheInstallScript is the duplicate check.
//
// The obvious way to serve a file from a static site is to commit it under the directory the site
// publishes, and it is the wrong way here: web/ is copied verbatim to the site root, so a web/install.sh
// would shadow the copy step above — silently, since `cp` would then overwrite it and `cmp` would pass,
// or, if the ordering ever changed, not overwrite it and the site would serve the committed one.
//
// The deeper reason is drift. This repository has a specific history with duplicated documentation
// (docs-platform/ deliberately unpublished; four install channels in getting-started.md that never
// existed), and an installer is the worst thing to duplicate: the stale copy still runs, still exits 0,
// and installs whatever it was written to install.
func TestNoSecondCopyOfTheInstallScript(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	// Every directory a deploy or a package could pick a file up from. web/ is the one that would
	// actually shadow the copy; the others are here because a duplicate anywhere is the same drift.
	for _, rel := range []string{
		filepath.Join("web", "install.sh"),
		filepath.Join("docs", "install.sh"),
		filepath.Join("web", "scripts", "install.sh"),
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			t.Errorf("%s exists. scripts/install.sh is the only copy, and pages.yml copies it to the "+
				"site root at build time — a committed duplicate is the file users would run and the "+
				"file no gate in this package checks", rel)
		}
	}
}

// TestDocumentedInstallOneLinersNameAServedAddress couples every documented one-liner to what is
// actually published.
//
// Two addresses are legitimate: raw.githubusercontent.com, which works and needs no site at all, and
// objectfs.io/install.sh, which works because of the copy step above — verified against the deployed
// site as a 200 of the exact bytes in scripts/install.sh, not assumed. Anything else — the
// get.objectfs.io that opened the getting-started guide for a year, or the packages.objectfs.io in
// #138's original spec — resolves, because Porkbun answers a wildcard for the whole domain, and serves
// nothing. That is why this is a test rather than a review habit: resolution is not evidence, and a
// dead one-liner looks correct from every angle except running it.
//
// Discovered by walking the tree rather than from a list of files, and the first version of this test
// was the list. It named README.md and web/index.html, passed, and missed three more surfaces
// documenting the same command — docs/index.md, docs-platform/index.md and
// docs-platform/guide/getting-started.md. An enumerated list can only ever check the surfaces whoever
// wrote it already knew about, which is the same shape as a link checker that starts from the links
// that exist: the sixth copy of a one-liner is invisible to it, and a sixth copy is exactly how four
// non-existent install channels survived in getting-started.md for a year.
func TestDocumentedInstallOneLinersNameAServedAddress(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	// The addresses that answer. Both are checked as substrings of the fetching line, because the
	// pinned-release example continues onto a following line and the URL is what identifies it.
	served := []string{
		"raw.githubusercontent.com/scttfrdmn/objectfs/main/scripts/install.sh",
		"https://objectfs.io/install.sh",
	}

	surfaces := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			// .git holds every historical version of every file, and node_modules and dist are
			// generated. A one-liner in any of them is not a surface a reader reads.
			switch d.Name() {
			case ".git", "node_modules", "dist", "site", ".venv":
				return filepath.SkipDir
			}

			return nil
		}

		// Prose and markup only. The script itself explains at length which addresses do not serve it,
		// and this test's own error messages name the two that do — both would fail a naive scan, and
		// neither is a command anybody copies.
		switch filepath.Ext(path) {
		case ".md", ".html":
		default:
			return nil
		}

		//nolint:gosec // A path from a WalkDir over the repository root, in a test.
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		for line := range strings.SplitSeq(string(b), "\n") {
			// A line that fetches the installer: names it, and names a URL. A sentence mentioning the
			// script, or a comment about where it is not served from, is prose and not an address.
			if !strings.Contains(line, "install.sh") || !strings.Contains(line, "http") {
				continue
			}

			// CHANGELOG.md records what an address used to be, which is the one place a superseded URL
			// is correct rather than stale — the entry explaining that the raw URL was replaced has to
			// be able to name what replaced it and what it replaced.
			if rel == "CHANGELOG.md" {
				continue
			}

			surfaces++

			if slices.ContainsFunc(served, func(s string) bool { return strings.Contains(line, s) }) {
				continue
			}

			t.Errorf("%s fetches the installer from an address that is not served:\n  %s\n"+
				"Two work: raw.githubusercontent.com/scttfrdmn/objectfs/main/scripts/install.sh, and "+
				"https://objectfs.io/install.sh because pages.yml copies it to the site root. Every "+
				"other name under objectfs.io resolves and serves nothing — the wildcard is why a dead "+
				"one cannot be told from a live one without loading it", rel, strings.TrimSpace(line))
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	// Five files documented the one-liner when this was written. A floor rather than an exact count,
	// since a new page documenting the install is a normal thing to add — but zero, or one, means the
	// walk stopped finding them and the check has gone quiet rather than clean.
	if surfaces < 5 {
		t.Fatalf("found %d lines fetching install.sh across the tree, and there were 5 when this test "+
			"was written (README.md, web/index.html, docs/index.md, docs-platform/index.md, "+
			"docs-platform/guide/getting-started.md). Either the install documentation moved or the "+
			"walk has stopped matching, and a check that finds nothing passes", surfaces)
	}
}
