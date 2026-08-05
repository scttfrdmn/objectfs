package config

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file is #198's durable half, and it is the same shape of gate as build_tags_test.go is for
// #240.
//
// The proximate defect was one line: `safeIntToUint32` bounded a 32-bit `int` against `0xFFFFFFFF`,
// so internal/fuse did not compile for linux/arm. The reason it survived to a release is structural.
// linux/armv7 was in release.yml's matrix and in *nothing* else: ci.yml's cross-build cells were all
// 64-bit, and no test on a 64-bit host can see the difference, because the defect is a compile error
// rather than a wrong answer. So the break only appeared when a tag was pushed, at which point the
// cheapest response was to delete the cell — which is what happened, and it took the platform with
// it.
//
// The fix is to couple the two matrices: a platform we ship is a platform every PR compiles. Then a
// 32-bit break is one red cell on the PR that caused it, and removing a platform is a deliberate edit
// to two files rather than the path of least resistance during a release.
//
// Note the direction. This asserts release ⊆ ci, not equality: ci.yml carries linux/386 as a second
// 32-bit word-width canary and we do not publish a 386 binary. An extra CI cell costs a minute and
// catches things; an unbuilt release cell ships them.

// releaseMatrixEntry matches the `- goos: linux` line that opens a matrix entry.
var releaseMatrixEntry = regexp.MustCompile(`^-\s+goos:\s*(\S+)`)

// yamlKeyValue matches a `key: value` line, with optional quotes around the value. GOARCH values
// like '386' are quoted in YAML because bare 386 parses as an integer.
var yamlKeyValue = regexp.MustCompile(`^(\w+):\s*'?"?([^'"]+)'?"?$`)

// TestEveryReleasePlatformIsCompiledInCI is the coupling itself.
func TestEveryReleasePlatformIsCompiledInCI(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	shipped := releasePlatforms(t, readFile(t, filepath.Join(root, ".github", "workflows", "release.yml")))
	if len(shipped) == 0 {
		t.Fatal("read no platforms out of release.yml's build matrix — the job may have been renamed " +
			"or its matrix reformatted, and an empty set satisfies the assertion below without " +
			"checking anything")
	}

	compiled := crossBuildPlatforms(t, readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml")))
	if len(compiled) == 0 {
		t.Fatal("read no platforms out of ci.yml's cross-build matrix — same problem as above, in " +
			"the direction that makes this test vacuously pass")
	}

	for _, p := range shipped {
		if compiled[p] {
			continue
		}

		goos, goarch, _ := strings.Cut(p, "/")

		t.Errorf("release.yml builds and publishes a %s binary, but ci.yml's cross-build matrix has "+
			"no cell for it, so no PR compiles it.\n"+
			"\tAdd `- {goos: %s, goarch: %s}` to that matrix. This is #198's failure mode: "+
			"linux/armv7 was shipped from a matrix nothing else built, it stopped compiling on a "+
			"32-bit `int`, and the break did not surface until a release tag — where the cheapest fix "+
			"was to drop the platform.",
			p, goos, goarch)
	}
}

// TestThirtyTwoBitIsStillInTheCrossBuildMatrix guards the word width, not a specific platform.
//
// `int` is 32 bits on GOARCH=arm and GOARCH=386 and 64 bits everywhere else this project targets, and
// every defect in #198's class — a constant that does not fit, a conversion that truncates — is
// invisible unless something compiles for a 32-bit word. A matrix of six 64-bit cells looks thorough
// and tests one word width.
//
// Stated as "at least one 32-bit cell" rather than by naming arm: if arm is ever genuinely dropped
// while 386 stays, that is fine and this should keep passing. What must not happen silently is the
// matrix going all-64-bit again.
func TestThirtyTwoBitIsStillInTheCrossBuildMatrix(t *testing.T) {
	t.Parallel()

	// GOARCHes on which Go's `int` is 32 bits. Not exhaustive over everything Go supports — mips,
	// mipsle, riscv32 and others qualify too — but this project builds for linux and darwin, and
	// listing architectures it does not target would let an unrelated cell satisfy the check.
	thirtyTwoBit := map[string]bool{"arm": true, "386": true}

	root := repoRoot(t)

	var found []string

	for p := range crossBuildPlatforms(t, readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml"))) {
		if arch := strings.SplitN(p, "/", 2); len(arch) == 2 && thirtyTwoBit[arch[1]] {
			found = append(found, p)
		}
	}

	if len(found) == 0 {
		t.Error("ci.yml's cross-build matrix has no 32-bit cell (no GOARCH of arm or 386).\n" +
			"\tEvery remaining cell has a 64-bit `int`, which makes the whole #198 class of defect " +
			"invisible: `if i > 0xFFFFFFFF` compiles and behaves correctly on all of them and does " +
			"not compile on any 32-bit target. Restore a linux/arm or linux/386 cell.")
	}

	sort.Strings(found)
	t.Logf("32-bit cells compiled by CI: %s", strings.Join(found, ", "))
}

// releasePlatforms reads goos/goarch pairs out of release.yml's build matrix.
//
// Parsed line-wise rather than by unmarshalling, matching build_tags_test.go's reasoning: the point
// is to read what CI will actually run, and a YAML round-trip through a hand-written struct is one
// more place for the two to disagree.
func releasePlatforms(t *testing.T, workflow string) []string {
	t.Helper()

	var (
		platforms []string
		goos      string
		inMatrix  bool
	)

	for line := range strings.SplitSeq(workflow, "\n") {
		trimmed := strings.TrimSpace(line)

		if trimmed == "include:" {
			inMatrix = true

			continue
		}

		if !inMatrix {
			continue
		}

		// Entries are `- goos: x` followed by indented keys. A line that is neither ends the matrix —
		// including `steps:`, which is what actually follows it.
		if m := releaseMatrixEntry.FindStringSubmatch(trimmed); m != nil {
			goos = m[1]

			continue
		}

		if goos == "" {
			break
		}

		m := yamlKeyValue.FindStringSubmatch(trimmed)
		if m == nil {
			break
		}

		if m[1] == "goarch" {
			platforms = append(platforms, goos+"/"+m[2])
		}
	}

	sort.Strings(platforms)

	return platforms
}

// crossBuildPlatforms reads goos/goarch pairs out of ci.yml's cross-build matrix, whose cells are
// written in YAML flow style: `- {goos: linux, goarch: arm, goarm: '7'}`.
func crossBuildPlatforms(t *testing.T, workflow string) map[string]bool {
	t.Helper()

	platforms := make(map[string]bool)

	inJob := false

	for line := range strings.SplitSeq(workflow, "\n") {
		trimmed := strings.TrimSpace(line)

		if trimmed == "cross-build:" {
			inJob = true

			continue
		}

		if !inJob {
			continue
		}

		// A new job starts at two-space indentation ending in a colon.
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(trimmed, ":") {
			break
		}

		if !strings.HasPrefix(trimmed, "- {") {
			continue
		}

		fields := make(map[string]string)

		for field := range strings.SplitSeq(strings.Trim(strings.TrimPrefix(trimmed, "- "), "{}"), ",") {
			k, v, ok := strings.Cut(field, ":")
			if !ok {
				continue
			}

			fields[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `'"`)
		}

		if fields["goos"] != "" && fields["goarch"] != "" {
			platforms[fields["goos"]+"/"+fields["goarch"]] = true
		}
	}

	return platforms
}
