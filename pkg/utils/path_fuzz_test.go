package utils

import (
	"path/filepath"
	"strings"
	"testing"
)

// FuzzValidatePath checks ValidatePath against an independent statement of what it should do.
//
// The oracle is deliberately not a second copy of the implementation. It asks a question the
// implementation does not ask — "does resolving this path against a directory leave that directory?" —
// answered by walking the cleaned path's components and tracking depth. That is the property a
// traversal check is *for*, and it is derivable without reference to how the check is written, which is
// what makes disagreement informative rather than tautological.
//
// #384 is the reason this exists. The defect was `strings.Contains(cleanPath, "..")`, which conflates
// "contains two dots in a row" with "escapes" — a false positive on `a..b.log`, and the table-driven
// test missed it for as long as it did because every dotted case someone thought to write was either
// real traversal or had non-adjacent dots. A fuzzer generating names is not limited to the cases
// someone thought to write, and this one finds the disagreement in well under a second.
func FuzzValidatePath(f *testing.F) {
	// Seeds: the #384 false positives, the traversals that must still be caught, and the shapes Clean
	// collapses. Seeded rather than left to chance because these are the cases with known answers, and
	// a seed corpus that contains the regression means the fuzz target is also a regression test.
	for _, seed := range []string{
		"a..b.log", "my..file", "v1..2/x", "./objectfs..staging.yaml", "..foo/bar", ".../x",
		"..", "../x", "a/../..", "sub/../../x", "./x/../../y", "../../../etc/passwd",
		"config/app.yaml", "/etc/passwd", "/var/../etc/passwd", "/../etc/passwd",
		".", "./", "/", "", "...", "....", "a/./b", "a//b", "a/..", "a/../b",
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}

	f.Fuzz(func(t *testing.T, path string, allowAbsolute bool) {
		err := ValidatePath(path, allowAbsolute)

		if path == "" {
			if err == nil {
				t.Fatal("an empty path was accepted")
			}

			return
		}

		clean := filepath.Clean(path)

		if filepath.IsAbs(clean) {
			// An absolute path's only question is whether the caller allows one. Clean resolves every
			// ".." in an absolute path — it treats the root as its own parent — so traversal cannot
			// survive to be judged here. Asserted rather than assumed: if that ever stopped holding,
			// this branch would start failing instead of the doc comment quietly becoming wrong.
			//
			// Checked per *component*, not with strings.Contains. The first version of this assertion
			// used Contains and the fuzzer failed it in under a second on "/..0" — whose single
			// component is "..0", an ordinary name. That is #384's own mistake, reproduced in the
			// oracle written to catch #384, which is a reasonable argument for having a fuzzer at all.
			for component := range strings.SplitSeq(clean, string(filepath.Separator)) {
				if component == ".." {
					t.Fatalf("Clean left a %q component in the absolute path %q -> %q, which "+
						"ValidatePath's documentation says cannot happen", "..", path, clean)
				}
			}

			if allowAbsolute && err != nil {
				t.Fatalf("absolute path %q rejected with allowAbsolute: %v", path, err)
			}

			if !allowAbsolute && err == nil {
				t.Fatalf("absolute path %q accepted without allowAbsolute", path)
			}

			return
		}

		// The relative case, where the oracle earns its keep. escapes reports whether resolving the
		// path against some directory ends up above it.
		wantErr := escapes(clean)

		if wantErr && err == nil {
			t.Fatalf("path %q (clean %q) resolves above its starting directory and was accepted",
				path, clean)
		}

		if !wantErr && err != nil {
			t.Fatalf("path %q (clean %q) stays within its starting directory and was rejected: %v",
				path, clean, err)
		}
	})
}

// escapes reports whether a cleaned relative path resolves to somewhere above where it started.
//
// Walks components and tracks depth. A ".." at depth zero is an escape; a component whose *name*
// merely contains dots is a component like any other, which is the distinction #384 was wrong about.
// Written against the meaning of the path rather than against ValidatePath's implementation, which is
// the point of an oracle — if it were the same expression, agreement would prove nothing.
func escapes(clean string) bool {
	depth := 0

	for component := range strings.SplitSeq(clean, string(filepath.Separator)) {
		switch component {
		case "", ".":
			// Clean removes both, so these are unreachable for a cleaned path. Handled anyway so the
			// function is honest about a non-cleaned argument rather than counting "" as a directory.
		case "..":
			if depth == 0 {
				return true
			}

			depth--
		default:
			depth++
		}
	}

	return false
}
