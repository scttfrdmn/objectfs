package config

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// This file is the second half of #240's fix. The first half was repairing four tagged files that
// had stopped compiling; this is the part that keeps them compiling.
//
// The failure mode is specific and worth naming, because it is not an ordinary missing test. A file
// behind a build tag is excluded from the default build, so `go build ./...`, `go vet ./...`, and
// `go test ./...` all pass without ever type-checking it. It is not that these files were untested —
// they were unbuilt. `tests/aws_s3_test.go` called a three-argument PutObject for however many
// months it took the interface to grow a fourth parameter; `test/benchmarks/cache_test.go` called
// `fmt.Itoa`, which has never existed in any version of Go. Both were committed green.
//
// The CI gate is a `go vet -tags=<tag> ./...` matrix cell per tag. That gate is only as good as its
// tag list, and a hand-maintained list of tags is exactly the kind of thing that goes stale — which
// is the defect this whole issue is about. So the list is checked against the tree: this test walks
// every //go:build line in the repository, extracts the tag identifiers, and fails if one is not in
// the workflow's matrix. Adding a tag without a cell to compile it is therefore a red PR rather than
// a file that quietly stops building six months from now.

// buildConstraint matches a //go:build line and captures its expression.
var buildConstraint = regexp.MustCompile(`^//go:build (.+)$`)

// toolchainTags are the build tags `go vet` already understands without being told, so a matrix
// cell for them would be meaningless. GOOS/GOARCH values are selected by the toolchain from
// the target platform, `cgo` from CGO_ENABLED, and `race`/`msan`/`asan` from their own flags. Only
// tags outside this set are ones a human invented, and only those need a cell.
//
// The list covers the values this repository actually uses rather than every GOOS Go supports: an
// unused entry here would silently excuse a tag that happened to share its name.
var toolchainTags = map[string]bool{
	"linux":   true,
	"darwin":  true,
	"windows": true,
	"unix":    true,
	"amd64":   true,
	"arm64":   true,
	"cgo":     true,
	"race":    true,
	"gc":      true,
	"gccgo":   true,
}

// TestEveryBuildTagIsGated is the mechanism that keeps the CI matrix honest.
//
// Every custom build tag in the tree must appear in the build-tags job's matrix, because a tag with
// no cell is a tag nothing compiles, and a tag nothing compiles is how four files in this repository
// came to carry code that does not build.
func TestEveryBuildTagIsGated(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	inTree := buildTagsInTree(t, root)
	if len(inTree) == 0 {
		t.Fatal("found no custom build tags anywhere in the tree — either they are all gone, or " +
			"this test stopped finding //go:build lines, and an empty set satisfies the assertion " +
			"below without checking anything")
	}

	workflow := readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml"))

	gated := matrixTags(t, workflow)
	if len(gated) == 0 {
		t.Fatal("could not read a tag list from the build-tags matrix in .github/workflows/ci.yml — " +
			"the job may have been renamed or its `tag:` line reformatted, and without it this test " +
			"cannot tell a gated tag from an ungated one")
	}

	for _, tag := range inTree {
		if !gated[tag] {
			t.Errorf("build tag %q appears on a //go:build line in %s but has no cell in the "+
				"build-tags matrix in .github/workflows/ci.yml, so nothing compiles it.\n"+
				"\tAdd it to the `tag:` list. A tag no job builds is #240's defect: the files "+
				"behind it are excluded from `go build ./...` and `go test ./...`, so they can stop "+
				"compiling without any check going red.",
				tag, strings.Join(filesWithTag(t, root, tag), ", "))
		}
	}
}

// TestGatedTagsStillExist is the converse, and it is not symmetric with the test above.
//
// A matrix cell for a tag no file carries is not a build failure — `go vet -tags=gone ./...` passes
// cleanly, because the tag simply selects nothing. It is a false sense of coverage: a green cell
// named after a suite that was deleted. Both `posix` and the `tests/` half of `integration` were
// deleted in v0.15.0, and a stale cell would have gone on reporting success for them.
//
// Note that `integration` legitimately remains: it marks real-AWS tests inside internal packages
// (internal/awsrates, internal/storage/s3), not just the LocalStack suite that was removed.
func TestGatedTagsStillExist(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	inTree := make(map[string]bool)
	for _, tag := range buildTagsInTree(t, root) {
		inTree[tag] = true
	}

	workflow := readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml"))

	for tag := range matrixTags(t, workflow) {
		if !inTree[tag] {
			t.Errorf("the build-tags matrix in .github/workflows/ci.yml has a cell for %q, but no "+
				"//go:build line in the tree names that tag.\n"+
				"\tThe cell passes — `go vet -tags=%s ./...` selects no files and reports success — "+
				"which is worse than failing: it reports coverage of a suite that is gone. Remove "+
				"the cell, or restore the files.",
				tag, tag)
		}
	}
}

// buildTagsInTree returns every custom build tag named on a //go:build line, sorted and deduplicated.
func buildTagsInTree(t *testing.T, root string) []string {
	t.Helper()

	found := make(map[string]bool)

	for _, path := range goFiles(t, root) {
		for _, tag := range tagsInFile(t, path) {
			found[tag] = true
		}
	}

	tags := make([]string, 0, len(found))
	for tag := range found {
		tags = append(tags, tag)
	}

	sort.Strings(tags)

	return tags
}

// filesWithTag lists the files whose //go:build line names tag, so a failure says where to look.
func filesWithTag(t *testing.T, root, tag string) []string {
	t.Helper()

	var matches []string

	for _, path := range goFiles(t, root) {
		if slices.Contains(tagsInFile(t, path), tag) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = path
			}

			matches = append(matches, rel)
		}
	}

	return matches
}

// tagsInFile extracts the custom tag identifiers from a file's //go:build line.
//
// The expression is a boolean one — `(linux || darwin) && fuse_mount` is a real line in this repo —
// so the operators and parentheses are stripped and what remains is the identifiers. A negated tag
// counts: `//go:build !integration` means the file is excluded when `integration` is set, so
// `-tags=integration` changes what compiles and the tag still needs a cell.
func tagsInFile(t *testing.T, path string) []string {
	t.Helper()

	// Only the constraint block matters, and it must precede the package clause. Reading past it
	// would pick up any //go:build inside a string literal or comment further down — this file
	// itself contains several.
	var tags []string

	for line := range strings.SplitSeq(readFile(t, path), "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "package ") {
			break
		}

		m := buildConstraint.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}

		expr := strings.NewReplacer("(", " ", ")", " ", "&&", " ", "||", " ", "!", " ").Replace(m[1])

		for field := range strings.FieldsSeq(expr) {
			if !toolchainTags[field] {
				tags = append(tags, field)
			}
		}
	}

	return tags
}

// matrixTags reads the `tag:` list out of the build-tags job.
//
// Parsed by locating the job and then its `tag:` line, rather than by unmarshalling the workflow:
// the point of this test is to read what CI will actually run, and a YAML round-trip through a
// hand-written struct is one more place for the two to disagree.
func matrixTags(t *testing.T, workflow string) map[string]bool {
	t.Helper()

	lines := strings.Split(workflow, "\n")

	inJob := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if trimmed == "build-tags:" {
			inJob = true

			continue
		}

		// A new job starts at two-space indentation ending in a colon. Stopping there keeps a
		// `tag:` key belonging to some later job from being read as this one's.
		if inJob && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(trimmed, ":") && trimmed != "build-tags:" {
			break
		}

		if !inJob || !strings.HasPrefix(trimmed, "tag:") {
			continue
		}

		list := strings.TrimPrefix(trimmed, "tag:")
		list = strings.TrimSpace(list)
		list = strings.TrimPrefix(list, "[")
		list = strings.TrimSuffix(list, "]")

		tags := make(map[string]bool)

		for tag := range strings.SplitSeq(list, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				tags[tag] = true
			}
		}

		return tags
	}

	return nil
}

// goFiles lists every .go file in the tree, skipping directories that hold no first-party source.
func goFiles(t *testing.T, root string) []string {
	t.Helper()

	var paths []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build":
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasSuffix(path, ".go") {
			paths = append(paths, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	return paths
}

// readFile reads a file the test cannot proceed without.
func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a path this test constructed from the repo root
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return string(data)
}
