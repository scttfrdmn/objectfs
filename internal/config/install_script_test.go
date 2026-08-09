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

// This file couples scripts/install.sh to the assets a release actually publishes.
//
// It is part (b) of #138 and the same shape of gate as release_platforms_test.go is for #198 and
// release_packages_test.go is for the packages: two files that must agree, with nothing connecting
// them. install.sh builds a download URL out of a platform name it derives from `uname`, and
// release.yml decides what those names are. A platform the release publishes and the script cannot
// name is a user on that architecture reading a 404; a name the script produces and the release does
// not publish is the same 404 from the other direction.
//
// The CI job that runs the script in three containers cannot see either of these. It runs on
// x86_64, so it exercises exactly one row of the mapping — the armv7 and arm64 and darwin rows are
// unreached there, and a mapping wrong for arm64 passes every container in that matrix.
//
// One mutation is worth recording because this project's own harness could not catch it. Reversing
// the amd64 arm64 mapping — `x86_64) arch="arm64"` — was expected to fail the container run and did
// not: podman on an Apple Silicon host runs an arm64 binary inside an emulated amd64 container
// through binfmt, so the wrong-architecture binary executed and printed its version. On a real
// x86_64 runner the script's final `--version` check catches it, but the local harness could not
// demonstrate that, which is the argument for asserting the mapping statically here rather than
// relying on the run.

// installScript reads scripts/install.sh.
func installScript(t *testing.T) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), "scripts", "install.sh")

	//nolint:gosec // A path built from the repository root, in a test.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}

	return string(b)
}

// TestInstallScriptNamesEveryPublishedAsset is the coupling itself.
//
// Every asset in release.yml's build matrix must be a name install.sh can produce, and every name
// install.sh can produce must be a published asset. Both directions, because they fail differently
// and both fail as a 404 the user cannot interpret: a published platform the script cannot name is
// unreachable through the documented install path, and a name the script invents is a download that
// does not exist.
func TestInstallScriptNamesEveryPublishedAsset(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	// The asset names, taken from the same matrix release_platforms_test.go reads. `name:` is the
	// authority rather than goos/goarch, because the asset is called objectfs-linux-armv7 while the
	// matrix cell is goarch: arm with goarm: 7 — deriving the name from the pair would reproduce the
	// arithmetic release.yml already did and could disagree with it.
	published := releaseAssetNames(t, readFile(t, filepath.Join(root, ".github", "workflows",
		"release.yml")))
	if len(published) == 0 {
		t.Fatal("read no asset names out of release.yml's build matrix — the job may have been " +
			"renamed or its matrix reformatted, and an empty set satisfies everything below without " +
			"checking anything")
	}

	produced := installScriptPlatforms(t, installScript(t))
	if len(produced) == 0 {
		t.Fatal("read no platform names out of scripts/install.sh — its case statements may have " +
			"been reformatted, which makes this test vacuous rather than failing")
	}

	for _, asset := range published {
		if produced[asset] {
			continue
		}

		t.Errorf("release.yml publishes objectfs-%s.tar.gz and scripts/install.sh cannot produce the "+
			"name %q, so there is no `uname` result on that platform that leads to that asset. A user "+
			"there gets a 404 from the documented install path, or is told their machine is "+
			"unsupported on a platform this project ships a binary for", asset, asset)
	}

	for asset := range produced {
		if slices.Contains(published, asset) {
			continue
		}

		t.Errorf("scripts/install.sh can produce the platform name %q and release.yml publishes no "+
			"objectfs-%s.tar.gz, so a machine of that shape downloads a URL that does not exist. The "+
			"404 arrives with no explanation of which of the two files is wrong", asset, asset)
	}
}

// TestInstallScriptVerifiesTheChecksum guards the property the script's own header calls
// non-optional.
//
// Three separate ways to lose it, and each of them leaves a script that installs successfully:
// dropping the comparison, making the checksum download non-fatal, or adding a flag to skip it. All
// three are plausible edits under pressure — the checksum is exactly the step someone disables when
// a release is broken and they want the binary anyway.
func TestInstallScriptVerifiesTheChecksum(t *testing.T) {
	t.Parallel()

	script := installScript(t)

	if !strings.Contains(script, `[ "$want" != "$got" ]`) {
		t.Error("scripts/install.sh does not compare the published checksum against the downloaded " +
			"file. Publishing a .sha256 and not checking it spends the cost of checksums without " +
			"buying the property — and the failure is silent, because an unverified install succeeds")
	}

	// The comparison has to abort, not warn. A mismatch that prints a warning and installs anyway is
	// the same outcome as no check, plus a line in a log nobody reads.
	if !strings.Contains(script, "checksum mismatch for") {
		t.Error("scripts/install.sh has no checksum-mismatch failure message, so either the " +
			"comparison does not abort or its wording changed. A mismatch must stop the install: it " +
			"means the download is not what the release publishes")
	}

	// A missing .sha256 is fatal too. This is the half most likely to be softened, because it fires
	// on a release that is broken upstream of the user, and "install anyway" looks helpful there.
	if !strings.Contains(script, "Refusing to install an unverified binary") {
		t.Error("scripts/install.sh does not refuse when a checksum cannot be obtained. A release " +
			"asset with no .sha256 beside it is a broken release, not a reason to install without " +
			"verifying — release.yml writes one per asset in the step that builds it")
	}

	// And no escape hatch. Searched for as flag spellings rather than as prose, since the header
	// paragraph explaining why there is no such flag would satisfy a looser check — the same defect
	// release_packages_test.go was rewritten to avoid.
	for _, flag := range []string{"--skip-checksum", "--no-verify", "--insecure", "SKIP_CHECKSUM"} {
		if strings.Contains(script, flag) {
			t.Errorf("scripts/install.sh accepts %s. The checksum is the only thing establishing that "+
				"the download matches the release, and a flag to skip it will be pasted into the "+
				"one-liner by the first person whose download fails for an unrelated reason", flag)
		}
	}
}

// TestInstallScriptChecksItsToolsBeforeDownloading pins the fix for the two container defects.
//
// Both were the same failure with different subjects: the script proceeded without a tool it needed
// and then reported the resulting error, which named the wrong cause. ubuntu:24.04 has no downloader
// and was told the GitHub API was unreachable; opensuse/leap:15.6 has no tar and no gzip and was
// told its archive could not be read.
//
// A wrong cause is the expensive direction — it sends the reader to check the release, the network
// and the file, and never mentions the missing package — so the requirement is not merely that the
// script fails, but that it fails before downloading and names what is absent.
func TestInstallScriptChecksItsToolsBeforeDownloading(t *testing.T) {
	t.Parallel()

	script := installScript(t)

	if !strings.Contains(script, "preflight()") || !strings.Contains(script, "\n    preflight\n") {
		t.Fatal("scripts/install.sh has no preflight check, or does not call it. Without one the " +
			"script downloads first and then fails on a missing tar or gzip, reporting a corrupt " +
			"archive — a confident claim about the file when the real problem is an absent package")
	}

	// gzip specifically, because it is the one that looks redundant next to tar and is not: `tar
	// -xzf` shells out to gzip and reports the child's exit status as its own, so a machine with tar
	// and no gzip produced "a tar that cannot read the archive". opensuse/leap:15.6 is that machine.
	for _, tool := range []string{"curl", "wget", "sha256sum", "shasum", "tar", "gzip"} {
		if !strings.Contains(script, tool) {
			t.Errorf("scripts/install.sh never mentions %s, so it cannot be checking for it. Every "+
				"tool the install path uses has to be established up front: the two defects this "+
				"guards against were both a missing tool reported as something else", tool)
		}
	}

	// Preflight must run ahead of the first *network* call, which is what makes "nothing was
	// downloaded" true. Compared by position, since both orders contain both lines.
	//
	// Anchored to resolve_latest rather than to the download, and that distinction is a mutation this
	// test initially missed. Moving the call to sit immediately above `say "downloading"` still put it
	// before the download and the weaker check passed — but by then the script has already queried the
	// GitHub API to resolve the latest release, which on a machine with no downloader is the very
	// failure preflight exists to pre-empt, reported as "could not reach the GitHub API". The
	// requirement is not "before the tarball" but "before anything touches the network".
	preflightAt := strings.Index(script, "\n    preflight\n")
	resolveAt := strings.Index(script, `say "resolving the latest release"`)
	downloadAt := strings.Index(script, `say "downloading"`)

	if preflightAt < 0 || resolveAt < 0 || downloadAt < 0 {
		t.Fatal("could not locate the preflight call, the release resolution, or the download in " +
			"scripts/install.sh. One of them was renamed, and a position comparison against a missing " +
			"anchor is vacuous")
	}

	if preflightAt > resolveAt {
		t.Error("scripts/install.sh calls preflight after it queries the GitHub API for the latest " +
			"release. On a machine with neither curl nor wget that API call is what fails, and its " +
			"error names the API rather than the missing downloader — which is exactly the defect " +
			"preflight was added to fix, moved a few lines down")
	}

	if preflightAt > downloadAt {
		t.Error("scripts/install.sh calls preflight after it starts downloading. Checking afterwards " +
			"gets the error message right and still leaves a partially-done install on a machine " +
			"that was never going to finish")
	}
}

// releaseAssetNames reads the `name:` values out of release.yml's build matrix.
//
// The same walk releasePlatforms does, reading a different key. Kept separate rather than
// generalised: that function's callers want goos/goarch pairs to compare against ci.yml's
// cross-build cells, and this one wants the asset suffix, which is a third value in the same cell.
func releaseAssetNames(t *testing.T, workflow string) []string {
	t.Helper()

	var (
		names    []string
		inMatrix bool
		inEntry  bool
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

		if releaseMatrixEntry.MatchString(trimmed) {
			inEntry = true

			continue
		}

		if !inEntry {
			break
		}

		m := yamlKeyValue.FindStringSubmatch(trimmed)
		if m == nil {
			break
		}

		if m[1] == "name" {
			names = append(names, m[2])
		}
	}

	sort.Strings(names)

	return names
}

// installScriptPlatforms returns every `<os>-<arch>` name detect_platform can produce.
//
// Read out of the two case statements rather than by executing the script, because the whole point
// is to check the rows this host cannot reach: running it on any one machine exercises exactly one
// os and one arch.
func installScriptPlatforms(t *testing.T, script string) map[string]bool {
	t.Helper()

	oses := caseTargets(script, `case "$os" in`)
	arches := caseTargets(script, `case "$arch" in`)

	platforms := make(map[string]bool)

	for _, os := range oses {
		for _, arch := range arches {
			// armv7 is published for Linux only, and the script refuses it on darwin by name. Encoding
			// that here rather than taking the cross product blindly keeps this from demanding a
			// darwin-armv7 asset that deliberately does not exist.
			if arch == "armv7" && os != "linux" {
				continue
			}

			platforms[os+"-"+arch] = true
		}
	}

	return platforms
}

// caseTargets returns the values assigned by the arms of one case statement in install.sh.
//
// It reads the assigned value — `arch="amd64"` — not the pattern being matched, because the pattern
// is what `uname` prints and the value is what the URL is built from. Those differ on every row that
// matters: x86_64 becomes amd64, aarch64 becomes arm64, Darwin becomes darwin.
func caseTargets(script, opener string) []string {
	var (
		targets []string
		inCase  bool
	)

	// Both variables are assigned by the same two names in this script, so one pattern reads either
	// statement.
	assign := regexp.MustCompile(`(?:os|arch)="([a-z0-9]+)"`)

	for line := range strings.SplitSeq(script, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.Contains(trimmed, opener) {
			inCase = true

			continue
		}

		if !inCase {
			continue
		}

		// `esac` ends it, and so does the `*)` catch-all arm, whose body is a die() and assigns
		// nothing. Stopping at esac is what matters; the catch-all is skipped by the assign match.
		if trimmed == "esac" {
			break
		}

		if m := assign.FindStringSubmatch(trimmed); m != nil {
			targets = append(targets, m[1])
		}
	}

	sort.Strings(targets)

	return targets
}
