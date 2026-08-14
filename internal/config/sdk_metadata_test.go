package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This file gates the URLs in the SDK package manifests.
//
// TestLandingPageNamesNoUnservedDomain already makes this check for web/index.html, and the reason it
// exists applies at least as strongly here: a manifest URL is *published*. `project_urls` in
// setup.py becomes the "Documentation" link on a PyPI project page, and `homepage` in package.json
// becomes the same link on npm. A reader who clicks it has left the repository, so nothing they see
// afterwards can tell them the destination was never real.
//
// It was not real. setup.py named `https://docs.objectfs.io/python` — one of five objectfs.io
// subdomains in this repository's history, of which exactly one, the apex, has ever answered. They all
// resolve, because Porkbun serves a wildcard, which is precisely why resolution is not evidence and a
// test is what has to ask. `get.objectfs.io` was the first command in the getting-started guide for a
// year on that basis.
//
// Scoped to the manifests rather than to all of sdks/, because the two are different claims. A dead
// URL in a comment or a changelog entry explaining that the URL was dead is correct prose; a dead URL
// in the metadata a package index publishes is a defect regardless of what any prose says about it.

// sdkManifests are the files that carry published package metadata.
//
// sdks/java/pom.xml is here for the same reason as the other two even though it currently names only
// GitHub URLs: the check is about what may be added, not about what is there today. sdks/go has no
// manifest of its own — it is part of this module — and sdks/c ships no package metadata.
var sdkManifests = []string{
	filepath.Join("sdks", "python", "setup.py"),
	filepath.Join("sdks", "javascript", "package.json"),
	filepath.Join("sdks", "java", "pom.xml"),
}

// TestSDKManifestsNameNoUnservedDomain is the docs.objectfs.io check.
//
// objectfs.io itself is allowed — it is served, recorded in web/CNAME and gated by the landing-page
// tests. Any subdomain has to earn its way in by answering, and this test is what refuses it until
// then.
func TestSDKManifestsNameNoUnservedDomain(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	// The host, from any URL on a line that is not a comment.
	//
	// Comments are skipped because a comment recording that a domain was dead has to be able to name
	// it — the note in setup.py explaining why `docs.objectfs.io` was removed would otherwise be the
	// first thing this test failed on. The cost is that a dead URL inside a comment is invisible here,
	// which is the right trade: a comment is not published metadata, and this test is about what a
	// package index shows a reader who has left the repository.
	//
	// Detected by syntax rather than by matching phrases from the note itself, which is what the first
	// version of this did — keyed on "never served" and "not reinstate", so rewording the comment would
	// have failed CI on the comment.
	host := regexp.MustCompile(`https?://([a-z0-9.-]*objectfs\.io)`)

	checked := 0

	for _, rel := range sdkManifests {
		path := filepath.Join(root, rel)

		//nolint:gosec // A path built from the repository root, in a test.
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("could not read %s: %v. It is listed here as a published package manifest — if it "+
				"was deleted or renamed, this list has to change with it, because a manifest nothing "+
				"checks is how docs.objectfs.io reached PyPI metadata", rel, err)

			continue
		}

		for line := range strings.SplitSeq(string(b), "\n") {
			if isManifestComment(line) {
				continue
			}

			for _, m := range host.FindAllStringSubmatch(line, -1) {
				checked++

				if m[1] == "objectfs.io" {
					continue
				}

				t.Errorf("%s names %s. Only the apex is served — every subdomain resolves because "+
					"Porkbun answers a wildcard, so a dead one looks correct from every angle except "+
					"loading it. This metadata is published: it becomes the link on the PyPI or npm "+
					"project page, where a reader who clicks it has left the repository and nothing "+
					"can tell them the destination was never real", rel, m[1])
			}
		}
	}

	// setup.py's project_urls block is the reason this test exists, so if no objectfs.io URL is found
	// anywhere the pattern has to be reported rather than passing quietly. Unlike the landing page —
	// where "no URL at all" is a correct state — these files are expected to link the project.
	if checked == 0 {
		t.Log("no objectfs.io URL in any SDK manifest. That is a correct state: all three currently " +
			"link github.com, which needs no wildcard to answer")
	}
}

// isManifestComment reports whether a line is a full-line comment in any of the three manifest
// formats: `#` for setup.py, `<!-- -->` for pom.xml.
//
// package.json is not in that list because JSON has no comments, which is the whole reason the
// detection can be this crude — the one format where a "comment" could hide a published URL cannot
// have one. `//` is deliberately absent for the same reason: in JSON it would be invalid, and the
// only file it could match is one where it is not a comment at all.
//
// Full-line only, so `'Documentation': '...',  # was docs.objectfs.io` still fails. A trailing comment
// sits on a line that also carries the real value, and cutting at the `#` would have to be sure the
// `#` is not inside a string literal — which, for a file full of URLs, it frequently is.
func isManifestComment(line string) bool {
	t := strings.TrimSpace(line)

	return strings.HasPrefix(t, "#") || strings.HasPrefix(t, "<!--")
}

// TestPythonSDKDocumentationURLResolvesInTheRepository checks the replacement, not just the removal.
//
// Swapping a dead domain for a path that does not exist trades a link to nothing for a 404, which is
// the same defect with a different host. The Documentation URL now points into this repository, so it
// can be checked against the tree — which is the whole reason for preferring a repository URL over a
// docs-site one while the docs site has no per-SDK page.
func TestPythonSDKDocumentationURLResolvesInTheRepository(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	path := filepath.Join(root, "sdks", "python", "setup.py")

	//nolint:gosec // A path built from the repository root, in a test.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}

	// The value of the Documentation key, whichever quote style it uses.
	doc := regexp.MustCompile(`'Documentation':\s*'([^']+)'`)

	m := doc.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("sdks/python/setup.py has no 'Documentation' entry in project_urls. If it was removed " +
			"deliberately this test should go with it; if the quoting changed, the pattern below is " +
			"matching nothing and the check is asserting nothing")
	}

	const prefix = "https://github.com/scttfrdmn/objectfs/blob/main/"

	target, ok := strings.CutPrefix(m[1], prefix)
	if !ok {
		// Not a failure by itself — a real docs URL is a legitimate future value. But it cannot be
		// checked against the tree, so say so rather than passing as though it had been.
		t.Skipf("setup.py's Documentation URL is %q, which does not point into this repository, so "+
			"there is nothing here to check it against. If objectfs.io/docs/ grows a Python page, that "+
			"page's existence is what needs gating instead", m[1])
	}

	// A Stat, not a read, and on a path taken from a file in this repository, in a test. G703 flags it
	// as tainted because `target` came out of setup.py; the value is checked against a fixed
	// github.com/scttfrdmn/objectfs prefix above, and the only thing done with it is asking whether it
	// exists.
	//
	// Suppressed here rather than in the baseline, and the distinction matters: the baseline records
	// findings that are real and unfixed, so putting a deliberate one in it would make the two kinds
	// indistinguishable. Note also that standalone gosec does not report this at all — it skips
	// `_test.go` by default, and golangci-lint's copy does not, so the two disagree and only the
	// golangci-lint one is a required check.
	//nolint:gosec // A path from a repository file, Stat only, in a test.
	if _, err := os.Stat(filepath.Join(root, target)); err != nil {
		t.Errorf("setup.py's Documentation URL points at %s, and there is no such file: %v.\nA dead "+
			"domain replaced by a 404 is the same defect with a different host", target, err)
	}
}
