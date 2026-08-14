package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// This file is the durable half of the first finding on #138, and it is the same shape of gate as
// release_platforms_test.go is for #198: something the repository could build, that nothing shipped.
//
// nfpm.yaml has existed and worked for several releases. ci.yml's `packaging` job builds both packages
// on every PR, installs the deb, runs the postinstall scriptlet, and loads the installed modulefile
// under Lmod. And release.yml contained no reference to `package-linux`, `nfpm`, `.deb` or `.rpm`, so
// every published release was five tar.gz binaries and their checksums:
//
//	$ gh release view v0.13.0 --json assets --jq '[.assets[].name] | length'
//	10
//
// Ten assets, five archives and five checksums, no packages — the same for v0.12.0 and v0.11.0. A
// package that CI proves installable and no user can obtain is scripts/preremove.sh before #207:
// working code with nothing invoking it.
//
// The structural reason is the one #198 had. `make package-linux` was exercised only by a job that
// cannot publish anything, so nothing anywhere compared what CI builds against what a release
// attaches, and the two drifted in the direction that is invisible from a green tree. So this couples
// them: a package format ci.yml builds is one release.yml ships.
//
// Note the direction, as in release_platforms_test.go: this asserts ci ⊆ release for package formats.
// A format built in CI and not shipped is the defect. A format shipped and not built in CI would be a
// different and worse defect, and it cannot occur here — both come out of the same `make package-linux`
// invocation, which is itself asserted by TestMakefileBuildsPackages.

// TestReleaseAttachesTheLinuxPackages is the coupling itself.
func TestReleaseAttachesTheLinuxPackages(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	release := readFile(t, filepath.Join(root, ".github", "workflows", "release.yml"))

	// Comments are stripped before any of this, and that is not a detail. The first version of these two
	// checks ran strings.Contains over the whole file and both survived mutation: deleting the `run: make
	// package-linux` step still passed, because the comment above the job explains what `make
	// package-linux` builds, and removing the .deb and .rpm upload paths still passed, because a comment
	// and the summary step both name them. A gate satisfied by prose about a step is the same defect as a
	// documentation gate satisfied by prose about a config key — it reads as coverage and asserts nothing.
	effective := withoutComments(release)

	// The build has to happen, as an executable line. Checked by the Makefile target rather than by a job
	// name, because the target is the contract: TestMakefileBuildsPackages already asserts what it
	// produces, so a release that invokes it inherits that.
	//
	// `run: make package-linux` was the literal, and it broke when the step gained a second line — the
	// signing key's gpg.conf has to be written before make runs, so the one-line `run:` became a block.
	// The property held throughout; the spelling did not. What matters is that the target is invoked as a
	// command, so a line whose first field is `make` and which names the target is what is checked, with
	// comments already stripped above.
	if !hasCommandLine(effective, "make", "package-linux") {
		t.Error("release.yml never runs `make package-linux`, so no published release carries a " +
			".deb or an .rpm. Both are built and installed by ci.yml's packaging job on every PR, which " +
			"means the formats are proven and unobtainable at the same time — the #207 shape of defect: " +
			"working code with nothing invoking it")
	}

	// And the packages have to reach the release. Building them into an artifact nothing attaches is the
	// same outcome with more steps, and it is a plausible half-fix: the job goes green, the checks list
	// gains a reassuring name, and the release page is unchanged.
	//
	// The upload is what is asserted, not any mention of the path. `publish` also globs dist/ into the
	// summary, and a package that reaches the summary and not the upload is listed in a job log nobody
	// reads while being absent from the release.
	upload := jobStep(t, effective, "Upload the packages")
	for _, format := range []string{".deb", ".rpm"} {
		if !strings.Contains(upload, "dist/*"+format) {
			t.Errorf("the package upload step does not include dist/*%s. A package built into a workflow "+
				"artifact and not attached to the release is not shipped — the artifact expires and the "+
				"release page looks complete", format)
		}
	}

	// The publish job has to wait for it. Without this, a packaging failure lets the release publish
	// anyway, silently missing the packages — which is worse than a failed release, because the page
	// looks finished and the omission is visible only to someone who knew to expect a .deb.
	needs := needsList(t, release, "Publish Release")
	if !strings.Contains(needs, "package-linux") {
		t.Errorf("the Publish Release job's `needs` is %q and does not include package-linux, so a "+
			"packaging failure would publish a release with the tarballs and no packages. A release that "+
			"looks complete and is not is harder to notice than one that failed", needs)
	}
}

// TestReleaseChecksThePackageVersionAgainstTheTag guards the one link nfpm.yaml names as unchecked.
//
// nfpm.yaml's own comment states the gap this closes: "nothing reads a package's version back to
// compare it, so `objectfs version` inside objectfs_0.12.0_amd64.deb would say 0.13.0 and no gate
// anywhere would notice". Both halves of the chain were verified and the join was not — release.yml
// checks the tag against the version constant, and TestPackageVersionComesFromTheVersionConstant
// checks that nfpm.yaml reads the constant rather than a literal, but nothing read the version back
// out of a built package.
//
// A wrong version in a package is not cosmetic. `apt-get install --only-upgrade` and `dnf update`
// decide whether to act by comparing versions, so a package declaring a version it does not contain is
// an upgrade that silently does not happen.
func TestReleaseChecksThePackageVersionAgainstTheTag(t *testing.T) {
	t.Parallel()

	release := withoutComments(readFile(t, filepath.Join(repoRoot(t), ".github", "workflows",
		"release.yml")))

	if !strings.Contains(release, "dpkg-deb --field") {
		t.Error("release.yml builds the packages without reading a version back out of one. nfpm.yaml's " +
			"comment names this exact gap: the tag is checked against the version constant and the " +
			"constant against nfpm.yaml, and nothing checks either against what nfpm actually wrote. A " +
			"package declaring a version it does not contain makes `apt-get install --only-upgrade` a " +
			"no-op, which is an upgrade that silently does not happen")
	}

	// The -1 matters and is easy to get wrong: nfpm writes objectfs_0.13.0-1_amd64.deb and a Version
	// field of 0.13.0-1, because nfpm.yaml pins `release: '1'`. The first draft of that workflow step
	// spelled the filename without the suffix and would have failed the release it was added to protect
	// — caught by running `make package-linux` locally rather than by reasoning about the name.
	if !strings.Contains(release, "$TAG-1") {
		t.Error("release.yml compares a package version without the `-1` release suffix. nfpm.yaml pins " +
			"`release: '1'`, so both the filename and the Version field carry it: " +
			"objectfs_0.13.0-1_amd64.deb, Version 0.13.0-1. A comparison against a bare version fails " +
			"every release")
	}
}

// hasCommandLine reports whether any line runs cmd with arg among its words.
//
// Written for `make package-linux`, where the step may be a one-line `run:` or a block scalar with the
// invocation somewhere inside it, and both are correct. A leading `run: ` is stripped so the one-line
// form is recognized as the command it is.
func hasCommandLine(doc, cmd, arg string) bool {
	for line := range strings.SplitSeq(doc, "\n") {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "run: ")

		fields := strings.Fields(trimmed)
		if len(fields) == 0 || fields[0] != cmd {
			continue
		}

		if slices.Contains(fields[1:], arg) {
			return true
		}
	}

	return false
}

// withoutComments drops full-line YAML comments.
//
// Only full-line comments, and deliberately not trailing ones: a `#` inside a shell `run:` block is
// often part of the command rather than a comment, and cutting at the first `#` would mangle it. Every
// mutation these gates need to catch is a deleted or altered *line*, so full-line stripping is enough
// and does not risk changing what a step says.
func withoutComments(workflow string) string {
	var kept []string

	for line := range strings.SplitSeq(workflow, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		kept = append(kept, line)
	}

	return strings.Join(kept, "\n")
}

// jobStep returns the body of the step whose `- name:` is stepName, up to the next step.
//
// So an assertion can be made about one step rather than about the file. Checking the whole workflow
// for a path is how the .deb upload check first passed with no upload at all — the summary step's glob
// satisfied it.
func jobStep(t *testing.T, workflow, stepName string) string {
	t.Helper()

	var (
		body   []string
		inStep bool
	)

	for line := range strings.SplitSeq(workflow, "\n") {
		trimmed := strings.TrimSpace(line)

		if trimmed == "- name: "+stepName {
			inStep = true

			continue
		}

		if !inStep {
			continue
		}

		// The next step ends this one.
		if strings.HasPrefix(trimmed, "- name: ") {
			break
		}

		body = append(body, line)
	}

	if !inStep {
		t.Fatalf("found no step named %q in release.yml. If it was renamed, point this test at the new "+
			"name — a gate that cannot find its subject passes for the wrong reason", stepName)
	}

	return strings.Join(body, "\n")
}

// needsList returns the `needs:` line belonging to the job whose `name:` is jobName.
//
// A line scan rather than a YAML parse, matching how release_platforms_test.go reads the build matrix:
// this package has no YAML dependency for workflow files, and the shape being read is one line.
func needsList(t *testing.T, workflow, jobName string) string {
	t.Helper()

	inJob := false

	for line := range strings.SplitSeq(workflow, "\n") {
		trimmed := strings.TrimSpace(line)

		if trimmed == "name: "+jobName {
			inJob = true

			continue
		}

		if !inJob {
			continue
		}

		if after, found := strings.CutPrefix(trimmed, "needs:"); found {
			return strings.TrimSpace(after)
		}

		// A `steps:` key means this job's header is over and it declared no needs.
		if trimmed == "steps:" {
			break
		}
	}

	t.Fatalf("found no `needs:` for the job named %q in release.yml. If the job was renamed, point this "+
		"test at the new name — a gate that cannot find its subject passes for the wrong reason",
		jobName)

	return ""
}
