// Every relative link in a markdown file must resolve to something that exists.
//
// This is the third mechanical documentation gate, alongside docs_test.go (config YAML, version
// claims) and docs_symbols_test.go (Go symbols, CLI invocations). It exists because a link target is
// a *path* rather than a symbol, so nothing the symbol gate does can see it: `[tuning](./perf.md)`
// is prose to the compiler, prose to vet, and prose to lint, and it stays prose after the file it
// names is renamed or was never written.
//
// # What this checks
//
// A markdown inline link `[text](target)` whose target is relative — not http(s), not mailto, not a
// bare fragment — must resolve, against the directory of the file containing it, to a path that
// exists on disk. A `#fragment` suffix is stripped before resolving; whether the anchor within the
// target file exists is markdownlint's MD051, which already runs in pre-commit.
//
// Root-absolute targets (`/guide/installation`) are checked as a separate class, because in
// docs-platform/ they are VitePress routes rather than filesystem paths — see
// vitePressRouteRoots below for the one thing that makes them checkable anyway.
//
// # What this does not check
//
// Links inside fenced code blocks are skipped. A fenced block is an example, and an example may
// legitimately show a path that does not exist here — a mount point, a user's config file, a URL
// from another project's documentation.
//
// Reference-style links (`[text][label]` with a `[label]: target` definition elsewhere) are not
// checked. There are none in this repository today; the check would be worth adding with the first
// one, and this comment is here so that whoever adds it knows the gate does not cover it yet.
//
// # Why a test rather than a link checker in CI
//
// Issue 208 proposed lychee in offline mode. A Go test was chosen instead for three reasons: it
// needs no network and no new tool, so it runs in pre-commit and locally at the same fidelity as in
// CI; it sits with the four gates a contributor already has to satisfy, in the package where
// CONTRIBUTING.md says to look; and the exemption mechanism can carry a *reason* in the code, which
// is what docsExemptFromConfigSchema demonstrates and what a tool's ignore-list cannot.
package config

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestDocumentedLinksResolve asserts that every relative markdown link points at something real.
//
// The count matters to how this was built. Issue 208 cataloged 24 dead links by walking docs/; this
// gate walks every tracked markdown file and found 45, in three classes the narrower walk could not
// see: 13 SDK example files that were never written, 8 root-absolute VitePress routes with no
// corresponding page, and the rest in docs/ as filed. Scoping a gate to where the defects were
// already known is how the next cluster stays invisible.
func TestDocumentedLinksResolve(t *testing.T) {
	t.Parallel()

	for _, path := range markdownFiles(t) {
		t.Run(shortName(t, path), func(t *testing.T) {
			t.Parallel()

			body, err := os.ReadFile(path) //nolint:gosec // a path from a walk of this repository
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			for _, link := range relativeLinks(string(body)) {
				resolved, kind := resolveLink(t, path, link.target)
				if kind == linkSkipped {
					continue
				}

				if _, err := os.Stat(resolved); err == nil {
					continue
				}

				switch kind {
				case linkRoute:
					t.Errorf("%s:%d: link %q resolves to no page: %s does not exist.\n"+
						"A root-absolute link in docs-platform/ is a VitePress route, so /guide/x "+
						"is docs-platform/guide/x.md. Either write the page, or link to what does "+
						"cover the topic.",
						shortName(t, path), link.line, link.target, shortName(t, resolved))
				default:
					t.Errorf("%s:%d: link %q points at a file that does not exist: %s.\n"+
						"Point it at the page that covers the topic, or delete the link. Do not "+
						"add a stub page to satisfy this test — an empty page that satisfies a "+
						"link is worse than a missing one, because it reads as documentation.",
						shortName(t, path), link.line, link.target, shortName(t, resolved))
				}
			}
		})
	}
}

// linkKind distinguishes the three ways a relative target is interpreted.
type linkKind int

const (
	// linkPath is a filesystem-relative target: ./perf.md, ../configuration/s3.md, examples/.
	linkPath linkKind = iota
	// linkRoute is a root-absolute target inside a documentation tree that serves routes rather
	// than files.
	linkRoute
	// linkSkipped is a target this gate deliberately does not resolve.
	linkSkipped
)

// resolveLink turns a link target into a path to stat, or reports that it is out of scope.
func resolveLink(t *testing.T, source, target string) (resolved string, kind linkKind) {
	t.Helper()

	// A target may be percent-encoded; a space in a filename is written %20. Resolve what the
	// renderer resolves, not the literal.
	cleaned, err := url.PathUnescape(target)
	if err != nil {
		cleaned = target
	}

	if i := strings.IndexAny(cleaned, "#?"); i >= 0 {
		cleaned = cleaned[:i]
	}

	// A pure fragment link is MD051's business, not this gate's.
	if cleaned == "" {
		return "", linkSkipped
	}

	if !strings.HasPrefix(cleaned, "/") {
		return filepath.Join(filepath.Dir(source), cleaned), linkPath
	}

	// Root-absolute. Interpretable only where a tree's routing rule is known.
	root := repoRoot(t)

	rel, err := filepath.Rel(root, source)
	if err != nil {
		return "", linkSkipped
	}

	for prefix, dir := range vitePressRouteRoots {
		if !strings.HasPrefix(filepath.ToSlash(rel), prefix) {
			continue
		}

		route := strings.TrimPrefix(cleaned, "/")

		// VitePress serves /guide/ from guide/index.md and /guide/x from guide/x.md.
		if route == "" || strings.HasSuffix(route, "/") {
			return filepath.Join(root, dir, route, "index.md"), linkRoute
		}

		return filepath.Join(root, dir, route+".md"), linkRoute
	}

	return "", linkSkipped
}

// vitePressRouteRoots maps a documentation tree to the directory its root-absolute links resolve
// against.
//
// Only docs-platform/ is here, and the distinction is worth stating because the two trees look
// alike and are not: docs-platform/ is VitePress, where `/guide/installation` is a route served
// from guide/installation.md, while docs/ is MkDocs, where links are written relative and a
// root-absolute one would be relative to the *site* root — a different thing again, and there are
// none, so no rule is guessed for it here.
var vitePressRouteRoots = map[string]string{
	"docs-platform/": "docs-platform",
}

// markdownLink matches an inline link. The optional trailing title (`[x](y "title")`) is excluded
// from the captured target, and a target may not contain whitespace or a closing paren — which is
// the common case and the one worth checking; a target with parens in it must be angle-bracketed to
// render, and there are none here.
var markdownLink = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// documentedLink is one link, with the line it appears on for the failure message.
type documentedLink struct {
	target string
	line   int
}

// relativeLinks returns every inline link in a markdown document that names a local target,
// excluding anything inside a fenced code block.
//
// Fences are located with the same alternation pairing fencedBlocks uses, for the reason recorded
// there: a regexp pairing gets out of phase and then reports the wrong lines as code. Here that
// would be the more dangerous direction — a gate that treats prose as code checks nothing and still
// passes.
func relativeLinks(markdown string) []documentedLink {
	var (
		links   []documentedLink
		inFence bool
	)

	for i, line := range strings.Split(markdown, "\n") {
		if fenceLine.MatchString(line) {
			inFence = !inFence

			continue
		}

		if inFence {
			continue
		}

		for _, match := range markdownLink.FindAllStringSubmatch(line, -1) {
			target := match[1]

			if externalLink.MatchString(target) || strings.HasPrefix(target, "#") {
				continue
			}

			links = append(links, documentedLink{target: target, line: i + 1})
		}
	}

	return links
}

// externalLink matches a target this gate has no way to check without the network. Deliberately
// scheme-based rather than a list of hosts: a link to any scheme is somebody else's to keep alive.
var externalLink = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// TestLinkGateSeesEveryMarkdownFile is the admission test for the gate above, and it exists because
// the admission rule is where the previous two documentation gates failed.
//
// docs_test.go's nestedSectionNames records one instance: five blocks of invented config keys passed
// the test written to catch them, because its admission test skipped them. Issue 208 records
// another, of the opposite shape — 24 dead links found by walking docs/, and 21 more outside it that
// the walk could not reach.
//
// So this asserts the walk's *reach* rather than its findings: every directory in the repository
// that contains markdown must be represented in the file set, and the count of files carrying
// checkable links must be non-trivial. A gate whose file set silently shrinks to zero passes every
// assertion in it.
func TestLinkGateSeesEveryMarkdownFile(t *testing.T) {
	t.Parallel()

	files := markdownFiles(t)

	dirs := make(map[string]int, len(files))
	withLinks := 0

	for _, path := range files {
		body, err := os.ReadFile(path) //nolint:gosec // a path from a walk of this repository
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		rel, err := filepath.Rel(repoRoot(t), path)
		if err != nil {
			t.Fatalf("rel %s: %v", path, err)
		}

		dirs[filepath.Dir(rel)]++

		if len(relativeLinks(string(body))) > 0 {
			withLinks++
		}
	}

	// The trees that carry the bulk of the documentation. Named explicitly so that a walk which
	// stops covering one of them fails here rather than passing quietly with less to check.
	for _, dir := range []string{".", "docs", "docs-platform", "sdks/python", "sdks/javascript"} {
		if dirs[dir] == 0 {
			t.Errorf("no markdown found in %s, so nothing there is checked — the walk in "+
				"markdownFiles has stopped reaching it", dir)
		}
	}

	if withLinks < 10 {
		t.Errorf("only %d markdown files carry a relative link; this repository has many more, "+
			"so relativeLinks has most likely stopped matching", withLinks)
	}
}

// TestMkDocsNavMatchesTheTree checks mkdocs.yml's nav in both directions, because that is the
// direction the defect ran in.
//
// A nav entry is a link target with a different syntax, which is exactly why it went unchecked: the
// gate above walks markdown, and nav: is YAML. When issue 224 measured it, 47 of 50 entries pointed
// at no file and 14 of the 17 pages in the tree were absent from the nav — including the four most
// substantial ones. So the navigation described a site nobody had written while omitting almost all
// of the one that had been.
//
// mkdocs build fails on an entry with no file, so this could never have built. Nothing builds it:
// mkdocs is not installed in this environment, no workflow runs it, and Pages is off on the
// repository. A configuration for a build that never runs has no way to be told it is wrong.
//
// Hence both directions. Checking only that entries resolve would have left the 14 orphans, which is
// the half a reader actually loses — a page absent from the nav is a page nobody finds.
func TestMkDocsNavMatchesTheTree(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	//nolint:gosec // a path built from the module root this test located itself
	raw, err := os.ReadFile(filepath.Join(root, "mkdocs.yml"))
	if err != nil {
		t.Fatalf("read mkdocs.yml: %v", err)
	}

	entries := navTargets(string(raw))

	if len(entries) < 10 {
		t.Fatalf("found %d nav entries in mkdocs.yml; the nav has more than that, so navTargets "+
			"has stopped matching and this test is checking nothing", len(entries))
	}

	inNav := make(map[string]bool, len(entries))

	for _, entry := range entries {
		inNav[entry.target] = true

		if _, err := os.Stat(filepath.Join(root, "docs", entry.target)); err == nil {
			continue
		}

		t.Errorf("mkdocs.yml:%d: nav entry %q has no file at docs/%s.\n"+
			"mkdocs build fails on this. Add the entry when the page is written — do not create a "+
			"stub page to satisfy the nav.", entry.line, entry.target, entry.target)
	}

	for _, path := range markdownFiles(t) {
		rel, err := filepath.Rel(filepath.Join(root, "docs"), path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue // outside docs_dir, so not mkdocs's to serve
		}

		if inNav[rel] || docsExemptFromNav[rel] != "" {
			continue
		}

		t.Errorf("docs/%s is not in mkdocs.yml's nav, so a built site would not link it.\n"+
			"Add it to the nav, or add it to docsExemptFromNav with the reason it is not a page.",
			rel)
	}
}

// docsExemptFromNav names a file under docs/ that is deliberately not a page in the site.
//
// An entry must say why. "Not written yet" is not a reason to be here — an unfinished page is
// absent from the tree, not exempt from the nav.
var docsExemptFromNav = map[string]string{
	"README.md": "instructions for building this documentation, addressed to a contributor " +
		"reading the repository; mkdocs would serve it at /README/ as a page about itself",
}

// navEntry is one nav target and the line it is on, so a failure can be clicked.
type navEntry struct {
	target string
	line   int
}

// navTargets extracts the .md targets from mkdocs.yml's nav.
//
// A line-scan rather than a YAML parse, deliberately: mkdocs.yml carries !!python/name: tags for the
// emoji and superfences extensions, which are not resolvable by a standard YAML decoder, so
// unmarshalling the file requires either a custom resolver or unsafe mode. The nav is a flat list of
// `- Title: path.md` lines and does not need either.
//
// External targets (http, https) are skipped: they are somebody else's to keep alive, the same rule
// externalLink applies above.
func navTargets(yaml string) []navEntry {
	var (
		targets []navEntry
		inNav   bool
	)

	for i, line := range strings.Split(yaml, "\n") {
		if strings.HasPrefix(line, "nav:") {
			inNav = true

			continue
		}

		// The nav runs to the next top-level key. Blank and comment lines do not end it.
		if inNav && strings.TrimSpace(line) != "" && !strings.HasPrefix(line, " ") &&
			!strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "#") {
			inNav = false
		}

		if !inNav {
			continue
		}

		match := navTarget.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		targets = append(targets, navEntry{target: match[1], line: i + 1})
	}

	return targets
}

// navTarget matches the path in `- Title: some/page.md`, and only a .md path — the Community section
// holds absolute GitHub URLs, which end in .md too but are not files in this tree.
var navTarget = regexp.MustCompile(`^\s*-\s+[^:]+:\s+(?:\./)?([A-Za-z0-9_][A-Za-z0-9_\-./]*\.md)\s*$`)

// TestNoDocumentedLinkPointsIntoAnExampleDirectory guards the specific repair this gate's first run
// forced, because the tempting fix for it is the one issue 208 warns against.
//
// Both SDK READMEs listed seven example files apiece — 13 links, since one is the directory itself —
// and neither sdks/python/examples/ nor sdks/javascript/examples/ has ever existed. The cheap way to
// make that green is to create the directories with stub files. That produces 13 files a reader can
// open, each of which teaches nothing, and the link gate then reports success. So: if an examples/
// directory is added, it must contain the programs the README describes, and this test says so at
// the point where the shortcut would be taken.
func TestNoDocumentedLinkPointsIntoAnExampleDirectory(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	for _, dir := range []string{"sdks/python/examples", "sdks/javascript/examples"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			// Absent is the current and correct state: the READMEs no longer link into it.
			continue
		}

		var empty []string

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				t.Fatalf("stat %s/%s: %v", dir, entry.Name(), err)
			}

			// A runnable example is not 200 bytes. The threshold is deliberately low: this is
			// meant to catch a placeholder, not to judge a short program.
			if info.Size() < 200 {
				empty = append(empty, entry.Name())
			}
		}

		sort.Strings(empty)

		if len(empty) > 0 {
			t.Errorf("%s contains %d file(s) too small to be a working example: %s.\n"+
				"These links were removed from the README because the examples did not exist. If "+
				"they are being restored, restore the programs — a stub that satisfies a link is "+
				"the documentation equivalent of a test that cannot fail.",
				dir, len(empty), strings.Join(empty, ", "))
		}
	}
}
