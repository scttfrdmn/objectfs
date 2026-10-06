package config

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// fuzzTargetDecl matches a fuzz target declaration at the start of a line.
var fuzzTargetDecl = regexp.MustCompile(`(?m)^func (Fuzz[A-Za-z0-9_]+)\(`)

// fuzzCell is one fuzz target, identified the way the fuzz-smoke matrix identifies it.
type fuzzCell struct {
	Pkg    string
	Target string
}

func (c fuzzCell) String() string { return fmt.Sprintf("{pkg: %s, target: %s}", c.Pkg, c.Target) }

// TestFuzzSmokeCoversEveryFuzzTarget asserts the fuzz-smoke matrix and the tree's fuzz targets are
// the same set, in both directions.
//
// The matrix is a hand-maintained list, and a list checks what its author knew. It named 10 of 22
// targets, so for the other 12 "we fuzz this" was false: `go test` ran their seed corpus as unit tests
// and never mutated an input. Two of the twelve held real defects that a 180-second campaign found in
// one and seven seconds (#565, fixed in #571). The target that decides whether the SHA-256 gate runs
// at all, FuzzWholeObjectResponse, was among them.
//
// The other direction matters less but costs nothing: a row naming a target that no longer exists
// fails in fuzz-smoke.sh as "no tests to run", which reads as an infrastructure problem rather than a
// stale list.
//
// Walked, not enumerated — the walk is the point. Build-tagged files are included. None of the
// current targets has a tag, but one that did would still need a row (with the tag passed to the
// script), and skipping tagged files would let it go unnoticed.
func TestFuzzSmokeCoversEveryFuzzTarget(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	tree := map[fuzzCell]string{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			// testdata holds corpora, not packages; node_modules and .git hold no Go of ours.
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, _ := filepath.Rel(root, filepath.Dir(path))
		pkg := "./" + filepath.ToSlash(rel)

		for _, m := range fuzzTargetDecl.FindAllStringSubmatch(readFile(t, path), -1) {
			relFile, _ := filepath.Rel(root, path)
			tree[fuzzCell{Pkg: pkg, Target: m[1]}] = relFile
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// 22 when this was written. A floor, so a walk that stops matching cannot pass by finding an
	// empty tree that agrees with an empty matrix.
	if len(tree) < 20 {
		t.Fatalf("found %d fuzz targets in the tree, expected at least 20. The walk or the `^func Fuzz` "+
			"pattern has stopped matching, and the comparison below is between two things it cannot see",
			len(tree))
	}

	matrix := map[fuzzCell]bool{}

	for _, row := range readWorkflow(t, "ci.yml").Jobs["fuzz-smoke"].Strategy.Matrix.Include {
		pkg, _ := row["pkg"].(string)
		target, _ := row["target"].(string)

		// A row without both keys is reported as itself. Counting it as a cell with an empty package
		// was this test's first bug: renaming `pkg:` in every row reported all 22 targets "missing",
		// which is the right color for the wrong reason, and the reason is what someone acts on.
		if pkg == "" || target == "" {
			t.Errorf("ci.yml's fuzz-smoke matrix has a row without both `pkg` and `target`: %v. "+
				"fuzz-smoke.sh is called with those two keys, so this cell fuzzes nothing", row)

			continue
		}

		matrix[fuzzCell{Pkg: pkg, Target: target}] = true
	}

	if len(matrix) == 0 {
		t.Fatal("read no rows from ci.yml's fuzz-smoke matrix. The job, its `include:` list or its " +
			"`pkg`/`target` keys have moved, and every target below would be reported missing for that " +
			"reason rather than its own")
	}

	var missing, stale []string

	for cell, file := range tree {
		if !matrix[cell] {
			missing = append(missing, fmt.Sprintf("%s  (declared in %s)", cell, file))
		}
	}

	for cell := range matrix {
		if _, ok := tree[cell]; !ok {
			stale = append(stale, cell.String())
		}
	}

	sort.Strings(missing)
	sort.Strings(stale)

	if len(missing) > 0 {
		t.Errorf("%d fuzz target(s) exist in the tree and are not in ci.yml's fuzz-smoke matrix, so CI "+
			"runs their seed corpus as unit tests and never fuzzes them:\n\t%s\n"+
			"Add a row for each. Two targets in this state held real defects that 60 seconds of fuzzing "+
			"would have caught on the commit that introduced them (#565).",
			len(missing), strings.Join(missing, "\n\t"))
	}

	if len(stale) > 0 {
		t.Errorf("%d row(s) in ci.yml's fuzz-smoke matrix name a target the tree no longer declares:\n\t%s\n"+
			"Remove the row, or fix the package path if the target moved.",
			len(stale), strings.Join(stale, "\n\t"))
	}
}
