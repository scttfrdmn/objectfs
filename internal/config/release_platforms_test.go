package config

import (
	"path/filepath"
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
// Note the direction. This asserts release ⊆ ci, not equality: an extra CI cell costs a minute and
// catches things; an unbuilt release cell ships them.
//
// 32-bit Linux is unsupported as of v0.18.0 (the maintainer's decision, 2026-10-19). linux/armv7 was
// #198's platform and was restored after it; this time it was removed deliberately, from every file
// that promised it, and TestNoThirtyTwoBitLinuxIsBuiltOrShipped below holds that line.

// TestEveryReleasePlatformIsCompiledInCI is the coupling itself.
func TestEveryReleasePlatformIsCompiledInCI(t *testing.T) {
	t.Parallel()

	shipped := releasePlatforms(t)
	if len(shipped) == 0 {
		t.Fatal("read no platforms out of .goreleaser.yml's archive build — the build may have been " +
			"renamed, and an empty set satisfies the assertion below without checking anything")
	}

	compiled := crossBuildPlatforms(t, workflowSource(t, "ci.yml"))
	if len(compiled) == 0 {
		t.Fatal("read no platforms out of ci.yml's cross-build matrix — same problem as above, in " +
			"the direction that makes this test vacuously pass")
	}

	for _, p := range shipped {
		if compiled[p] {
			continue
		}

		goos, goarch, _ := strings.Cut(p, "/")

		t.Errorf(".goreleaser.yml builds and publishes a %s binary, but ci.yml's cross-build matrix has "+
			"no cell for it, so no PR compiles it.\n"+
			"\tAdd `- {goos: %s, goarch: %s}` to that matrix. This is #198's failure mode: "+
			"linux/armv7 was shipped from a matrix nothing else built, it stopped compiling on a "+
			"32-bit `int`, and the break did not surface until a release tag — where the cheapest fix "+
			"was to drop the platform.",
			p, goos, goarch)
	}
}

// thirtyTwoBitGoarch are the GOARCH values on which Go's `int` is 32 bits, among those this project
// has ever targeted.
var thirtyTwoBitGoarch = map[string]bool{"arm": true, "386": true}

// TestNoThirtyTwoBitLinuxIsBuiltOrShipped asserts 32-bit Linux is absent everywhere it could be
// promised: every goreleaser build, ci.yml's cross-build matrix, and install.sh's arch table.
//
// This replaces TestThirtyTwoBitIsStillInTheCrossBuildMatrix, which required at least one 32-bit CI
// cell as a word-width canary for #198's class of defect. That was right while a 32-bit binary
// shipped. As of v0.18.0 none does, and the hazard runs the other way. A platform half-removed —
// still built by goreleaser but not compiled by CI, or still offered by install.sh with no asset
// behind it — is the state #198 was, and it fails here rather than on a release tag.
func TestNoThirtyTwoBitLinuxIsBuiltOrShipped(t *testing.T) {
	t.Parallel()

	checked := 0

	for _, build := range []string{"archives", "packages", "mount-helper"} {
		for _, target := range goreleaserTargets(t, build) {
			checked++

			if thirtyTwoBitGoarch[target.Goarch] {
				t.Errorf(".goreleaser.yml's %q build produces %s. 32-bit Linux is unsupported as of "+
					"v0.18.0; reinstating it means restoring the CI cells, install.sh's arch row and "+
					"release.yml's artifact checks together, and this test with them.",
					build, target.Platform())
			}
		}
	}

	if checked < 6 {
		t.Fatalf("expanded %d goreleaser targets across three builds, expected at least 6. The build "+
			"ids moved or the expansion broke, and the assertion above checked nothing", checked)
	}

	compiled := crossBuildPlatforms(t, workflowSource(t, "ci.yml"))
	if len(compiled) == 0 {
		t.Fatal("read no platforms out of ci.yml's cross-build matrix")
	}

	for p := range compiled {
		if _, arch, _ := strings.Cut(p, "/"); thirtyTwoBitGoarch[arch] {
			t.Errorf("ci.yml's cross-build matrix compiles %s, a 32-bit target, though no release ships "+
				"one. A cell for an unsupported platform is a required check guarding nothing.", p)
		}
	}

	for p := range installScriptPlatforms(t, readFile(t, filepath.Join(repoRoot(t), "scripts", "install.sh"))) {
		if strings.HasSuffix(p, "-armv7") || strings.HasSuffix(p, "-386") {
			t.Errorf("scripts/install.sh resolves a machine to %s, which no release publishes. It should "+
				"refuse 32-bit machines by name instead of building a URL that 404s.", p)
		}
	}
}

// releasePlatforms returns the goos/goarch pairs a release compiles and publishes.
//
// It reads .goreleaser.yml's archive build, which is where release.yml's five-cell `include:` matrix
// went. That matrix was parsed line-wise here, deliberately — "read what CI will actually run" — and
// the same reasoning still applies to the file it moved to: goreleaserTargets expands the cross
// product goreleaser expands, out of the config goreleaser reads, rather than out of a description of
// it. The parsing this replaced is gone from both this file and install_script_test.go.
//
// Duplicates are possible in principle (two goos entries cannot collide, but a malformed list could)
// and are collapsed, because the callers ask set questions.
func releasePlatforms(t *testing.T) []string {
	t.Helper()

	seen := make(map[string]bool)

	var platforms []string

	for _, target := range goreleaserTargets(t, "archives") {
		if !seen[target.Platform()] {
			seen[target.Platform()] = true
			platforms = append(platforms, target.Platform())
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
