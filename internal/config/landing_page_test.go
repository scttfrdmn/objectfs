package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This file gates web/, the landing page published at objectfs.io.
//
// The reason it exists is the reason docs_links_test.go exists, and the history is worse here. Every
// documentation tree in this repository accumulated claims that were false when written or went false
// later, and in each case nothing could tell: docs-platform/guide/getting-started.md opened with
// `curl -sSL https://get.objectfs.io | sh` — a getting-started guide whose first command is a domain
// that has never served anything — and listed an apt repository, a Homebrew tap and an AUR package
// alongside it, none of which exist. docs/index.md published five throughput figures of which zero
// were measured. mkdocs.yml's nav listed 50 pages of which 3 resolved.
//
// A landing page is the worst place for that failure, because it is the first thing a reader sees and
// the last thing anyone re-reads. So the three kinds of claim that went wrong before are checked
// here: a command that must run, a version that must not be transcribed, and a domain that must not
// be named unless it answers.
//
// What this cannot check is whether a sentence is *true* — only whether it agrees with something else
// in the repository that is itself checked. That is the same limit docs_links_test.go has, and the
// reason the page deliberately says nothing about throughput: a claim with nothing to agree with
// cannot be gated, so the page does not make it.

// docsPath is where the deploy puts the MkDocs tree, relative to the landing page. mkdocs.yml's
// site_url declares the same location; the two have to agree or the canonical links and the sitemap
// point somewhere the site does not answer.
const docsPath = "docs/"

// landingPage reads web/index.html.
func landingPage(t *testing.T) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), "web", "index.html")

	//nolint:gosec // A path built from the repository root, in a test.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}

	return string(b)
}

// TestLandingPageInstallCommandsMatchTheScript couples the Install tab to scripts/install.sh.
//
// The page shows two invocations, and both name flags the script has to parse. install.sh's own
// argument loop is the authority: a flag renamed there while the page still shows the old spelling
// produces a usage error from a copy-pasted command, which is the specific failure the four
// non-existent install channels in getting-started.md caused for a year.
//
// Only the flags are asserted, not the whole line. A test that pinned the exact command string would
// fail on a reworded comment and pass on a renamed flag, which is the wrong sensitivity in both
// directions.
func TestLandingPageInstallCommandsMatchTheScript(t *testing.T) {
	t.Parallel()

	page := landingPage(t)
	script := installScript(t)

	install := sectionOf(t, page, `id="install"`)

	// Only the flags on a line that is part of an install.sh invocation. The section also shows
	// `brew install --cask macfuse` and `apt install ./objectfs_*.deb`, whose flags belong to other
	// programs entirely — the first draft of this test asserted over the whole section and reported
	// --cask as a missing install.sh flag, which is a real defect in the assertion rather than in
	// either file. A continuation line is carried by `pipeline`, since the pinned-release example
	// puts `| bash -s -- --prefix ...` on the line after the backslash.
	flag := regexp.MustCompile(`--[a-z][a-z-]+`)

	seen := map[string]bool{}
	pipeline := false

	for line := range strings.SplitSeq(install, "\n") {
		switch {
		case strings.Contains(line, "install.sh"):
			pipeline = true
		case pipeline && strings.Contains(line, "bash"):
			// still the same invocation, continued past a backslash
		default:
			pipeline = false
		}

		if !pipeline {
			continue
		}

		for _, f := range flag.FindAllString(line, -1) {
			if seen[f] {
				continue
			}

			seen[f] = true

			// The script's parser matches a long flag either bare or with an =value form. Requiring
			// the bare `--flag)` case is what distinguishes a flag it handles from a string that
			// merely appears in its help text, or in a comment explaining why some other flag does
			// not exist.
			if strings.Contains(script, "        "+f+")") {
				continue
			}

			t.Errorf("web/index.html's install section shows %s on an install.sh line, and the "+
				"script has no case that parses it. A reader copying that command gets a usage "+
				"error. Either the script lost the flag or the page was written against a spelling "+
				"it never had", f)
		}
	}

	if len(seen) < 2 {
		t.Fatalf("found %d long flags in the install section of web/index.html; it shows --prefix and "+
			"--version at least, so the section anchor or the flag pattern has stopped matching and "+
			"this test is asserting nothing", len(seen))
	}
}

// TestLandingPageStatesNoVersion keeps the release number off the page.
//
// CLAUDE.md is explicit that the `version` constant in cmd/objectfs/main.go is the only authority,
// after five files claimed five different versions. A landing page is a file nobody edits at release
// time, so a version transcribed into its prose is stale by construction — and a stale version on the
// front page is worse than no version, because a reader has no way to tell it is wrong.
//
// The pinned-release example in the install tab is the one legitimate exception: it demonstrates the
// --version flag and any tag illustrates that equally well, so it is allowed inside a <pre> block and
// nowhere else.
func TestLandingPageStatesNoVersion(t *testing.T) {
	t.Parallel()

	page := landingPage(t)

	// Strip the code blocks first, then look for a version in what is left. A version inside <pre>
	// is an example of a flag's syntax; a version in a sentence is a claim about what is current.
	prose := regexp.MustCompile(`(?s)<pre>.*?</pre>`).ReplaceAllString(page, "")

	version := regexp.MustCompile(`\bv?\d+\.\d+\.\d+\b`)

	for _, m := range version.FindAllString(prose, -1) {
		// Apache 2.0 and SHA-256 are not release versions; neither is a two-part number that happens
		// to sit beside a third.
		if m == "2.0" || m == "0.0.0" {
			continue
		}

		t.Errorf("web/index.html names version %q outside a code block. The version constant in "+
			"cmd/objectfs/main.go is the only authority — a number copied into prose has no way to be "+
			"told it is stale, which is how five files came to claim five different versions", m)
	}

	// The packaging paragraph does say "from v0.14.0 onward", which is a statement about when a thing
	// started rather than about what is current, and it lives in a <pre>. If that moves into prose
	// this test should be the thing that notices, so assert the stripping actually removed something.
	if !strings.Contains(page, "<pre>") {
		t.Fatal("web/index.html contains no <pre> block, so the version check above stripped nothing " +
			"and is running against the whole file. The install section was restructured")
	}
}

// TestLandingPageNamesNoUnservedDomain is the get.objectfs.io check.
//
// Five subdomains of objectfs.io appear across this repository's history and exactly one of them has
// ever answered. They resolve — Porkbun serves a wildcard, so *every* name under objectfs.io resolves
// to the same parking record — which is precisely why resolution is not evidence and a test is
// needed. `get.objectfs.io` was the first command in the getting-started guide for a year.
//
// objectfs.io itself is allowed: it is what GitHub Pages serves this page at, recorded in web/CNAME.
// Any other subdomain has to earn its way in by being served, and this test is what asks.
func TestLandingPageNamesNoUnservedDomain(t *testing.T) {
	t.Parallel()

	page := landingPage(t)

	host := regexp.MustCompile(`https?://([a-z0-9.-]*objectfs\.io)`)

	found := false

	for _, m := range host.FindAllStringSubmatch(page, -1) {
		found = true

		if m[1] == "objectfs.io" {
			continue
		}

		t.Errorf("web/index.html links %s. Only objectfs.io is served — the subdomains all resolve "+
			"because Porkbun answers a wildcard, so a link to one looks fine from every angle except "+
			"loading it. get.objectfs.io was the first command in the getting-started guide for a "+
			"year on exactly this basis", m[1])
	}

	if !found {
		t.Log("no objectfs.io URL on the page; nothing to check, which is also a correct state")
	}
}

// TestLandingPageIsDeployedWithItsAssets couples web/ to the workflow that publishes it.
//
// Every local reference on the page has to resolve inside web/, because web/ is what the deploy
// copies — a stylesheet one directory up builds fine locally against the repository and 404s on the
// published site. web/CNAME must also name the custom domain: without that file a deploy clears the
// domain configured on the repository, and objectfs.io stops answering with no commit that looks
// responsible for it.
func TestLandingPageIsDeployedWithItsAssets(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	page := landingPage(t)

	ref := regexp.MustCompile(`(?:href|src)="([^"#]+)"`)

	checked := 0

	for _, m := range ref.FindAllStringSubmatch(page, -1) {
		target := m[1]

		if strings.HasPrefix(target, "http") || strings.HasPrefix(target, "mailto:") || target == "/" {
			continue
		}

		// docs/ is the one local path with nothing behind it in this directory: the workflow builds
		// the MkDocs tree into _site/docs after copying web/, so it exists on the published site and
		// not in the repository. TestLandingPageLinksToTheDocs is what holds up that end.
		if target == docsPath {
			continue
		}

		checked++

		if _, err := os.Stat(filepath.Join(root, "web", strings.TrimPrefix(target, "/"))); err != nil {
			t.Errorf("web/index.html references %q, and there is no such file under web/. The deploy "+
				"copies web/ and nothing else, so this resolves in a local checkout and 404s on the "+
				"published site", target)
		}
	}

	if checked < 2 {
		t.Fatalf("checked %d local references in web/index.html; it has a stylesheet and a favicon at "+
			"least, so the pattern has stopped matching", checked)
	}

	//nolint:gosec // A path built from the repository root, in a test.
	cname, err := os.ReadFile(filepath.Join(root, "web", "CNAME"))
	if err != nil {
		t.Fatalf("web/CNAME is missing: %v.\nGitHub Pages clears the custom domain on a deploy that "+
			"does not carry this file, which takes objectfs.io down with no commit that looks "+
			"responsible", err)
	}

	if got := strings.TrimSpace(string(cname)); got != "objectfs.io" {
		t.Errorf("web/CNAME names %q, and the domain is objectfs.io", got)
	}
}

// TestPagesWorkflowBuildsDocsStrictly asserts the deploy keeps --strict.
//
// Non-strict, `mkdocs build` emitted eight warnings and produced a site anyway: seven links from
// inside docs/ up to the repository-root README.md and SECURITY.md — correct on GitHub, dead in any
// built site — and an index conflict. Two of the seven also used `#supported-operations` where the
// heading is `## Supported filesystem operations`, so they were broken on GitHub as well. They
// survived because nothing had ever built the tree.
//
// Dropping --strict is the change that would make all of that publishable again while every check
// stayed green, so it is asserted rather than left to a comment.
func TestPagesWorkflowBuildsDocsStrictly(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), ".github", "workflows", "pages.yml")

	//nolint:gosec // A path built from the repository root, in a test.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}

	workflow := withoutComments(string(b))

	if !strings.Contains(workflow, "mkdocs build --strict") {
		t.Error(".github/workflows/pages.yml does not run `mkdocs build --strict`. Without --strict " +
			"the build emits warnings for dead links and publishes anyway, which is how seven broken " +
			"links into the repository root survived in docs/ until anything built it")
	}

	if !strings.Contains(workflow, "web/CNAME") && !strings.Contains(workflow, "_site/CNAME") {
		t.Error(".github/workflows/pages.yml does not check that a CNAME reaches the artifact. A " +
			"deploy without it clears the custom domain on the repository")
	}
}

// TestLandingPageLinksToTheDocs asserts the published docs are reachable from the page.
//
// This is a defect the first deploy actually shipped. `pages.yml` builds the MkDocs tree into
// `_site/docs`, `mkdocs.yml`'s site_url declares `https://objectfs.io/docs/`, every page under it
// returned 200 — and the landing page linked none of it. Its nav and its footer both sent a reader to
// `github.com/scttfrdmn/objectfs#readme` instead, so the documentation site this workflow exists to
// publish had no entry point but a typed URL.
//
// Nothing else could have caught it. The asset test skips `docs/` because there is no such directory
// in `web/`, the workflow asserts `_site/docs/index.html` exists rather than that anything points at
// it, and a link check over the built site would have found no broken link — the failure is an absent
// link, which is invisible to every check that starts from the links that are present.
//
// Both surfaces are asserted, because they failed together and for the same reason: the page was
// written before the docs had a home, and its GitHub links were correct at the time.
func TestLandingPageLinksToTheDocs(t *testing.T) {
	t.Parallel()

	page := landingPage(t)

	for _, surface := range []struct{ name, attr string }{
		{"nav", `class="nav-links"`},
		{"footer", `class="footer-links"`},
	} {
		links := sectionOfClosing(t, page, surface.attr, "</nav>")

		if !strings.Contains(links, `href="`+docsPath+`"`) {
			t.Errorf("web/index.html's %s does not link %q. pages.yml publishes the MkDocs tree there "+
				"and mkdocs.yml's site_url declares it, so the docs are served — the first deploy served "+
				"every page under /docs/ with nothing on the landing page pointing at any of them",
				surface.name, docsPath)
		}
	}

	// The docs are published from this repository now, so a reader sent to GitHub's rendered README for
	// "Documentation" is being sent to the older of two copies. Deep links into the repository are
	// fine — SECURITY.md, LICENSE, the supported-operations table — but not as the documentation link.
	readme := regexp.MustCompile(`<a href="[^"]*github\.com[^"]*"[^>]*>\s*Documentation\s*</a>`)
	if readme.MatchString(page) {
		t.Error("web/index.html points \"Documentation\" at GitHub while objectfs.io/docs/ serves the " +
			"MkDocs site. Two documentation destinations with no rule for which is current is the " +
			"arrangement that let four non-existent install channels survive in docs-platform/")
	}
}

// sectionOfClosing returns the markup from an element carrying attr to the next closing tag.
//
// sectionOf below assumes </section>; the nav and footer link lists are a <div> and a <nav>. Same
// reasoning as there: anchoring on the attribute means a reordered page fails to find its window
// rather than silently searching a different one.
func sectionOfClosing(t *testing.T, page, attr, closing string) string {
	t.Helper()

	start := strings.Index(page, attr)
	if start < 0 {
		t.Fatalf("web/index.html has no element with %s. It was renamed, and a search within a window "+
			"that does not exist would pass for the wrong reason", attr)
	}

	end := strings.Index(page[start:], closing)
	if end < 0 {
		t.Fatalf("web/index.html has no %s after %s", closing, attr)
	}

	return page[start : start+end]
}

// sectionOf returns the markup from an element carrying attr to the next </section>.
//
// A section-scoped assertion is the point: the install flags must appear in the install section, not
// merely somewhere in a 400-line file. Anchoring on the attribute rather than a line number means a
// reordered page does not silently move the window somewhere else — it fails to find it.
func sectionOf(t *testing.T, page, attr string) string {
	t.Helper()

	start := strings.Index(page, attr)
	if start < 0 {
		t.Fatalf("web/index.html has no element with %s. The section was renamed, and a search "+
			"within a window that does not exist would pass for the wrong reason", attr)
	}

	end := strings.Index(page[start:], "</section>")
	if end < 0 {
		t.Fatalf("web/index.html has no </section> after %s", attr)
	}

	return page[start : start+end]
}
