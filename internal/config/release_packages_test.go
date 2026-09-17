package config

import (
	"path/filepath"
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
// different and worse defect, and it cannot occur here — both come out of one `.goreleaser.yml`,
// invoked by `make package-linux` in CI and by the goreleaser action on a tag, running the same
// binary over the same config. TestMakefileBuildsPackages asserts the CI half of that.

// TestReleaseAttachesTheLinuxPackages is the coupling itself.
func TestReleaseAttachesTheLinuxPackages(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	workflow := readFile(t, filepath.Join(root, ".github", "workflows", "release.yml"))

	// Comments are stripped before any of this, and that is not a detail. The first version of these two
	// checks ran strings.Contains over the whole file and both survived mutation: deleting the `run: make
	// package-linux` step still passed, because the comment above the job explains what `make
	// package-linux` builds, and removing the .deb and .rpm upload paths still passed, because a comment
	// and the summary step both name them. A gate satisfied by prose about a step is the same defect as a
	// documentation gate satisfied by prose about a config key — it reads as coverage and asserts nothing.
	effective := withoutComments(workflow)

	// The build has to happen. `make package-linux` was the literal here, and hasCommandLine existed to
	// recognize it whether the step was a one-line `run:` or a block scalar — the signing key's gpg.conf
	// had to be written before make ran, so the spelling changed while the property held.
	//
	// release.yml no longer shells out to make. It runs the goreleaser action, which downloads a pinned
	// goreleaser and hands it the same config `make package-linux` hands a developer's copy. So the step
	// is found by the action it uses rather than by its name or by a command line: the action is the
	// contract, and a step name is prose.
	steps := stepsUsing(effective, "goreleaser/goreleaser-action")
	if len(steps) == 0 {
		t.Fatal("release.yml never runs goreleaser, so no published release carries a .deb or an " +
			".rpm — or a tarball. Both package formats are built and installed by ci.yml's packaging job " +
			"on every PR, which would leave them proven and unobtainable at the same time: the #207 " +
			"shape of defect, working code with nothing invoking it")
	}

	// And it has to be asked to `release`, not to `build`. `goreleaser build` compiles the binaries and
	// produces no archive, no package and no checksum — a step that succeeds, a job that goes green, and
	// a dist/ holding five bare binaries under per-target directories that no upload glob matches. The
	// release page would come out empty of everything except the notes.
	//
	// `--snapshot` is checked for too, and it is the subtler mistake of the two, because it is what
	// `make package-linux` correctly passes: a snapshot ignores the tag and stamps the version as
	// `0.14.1-next`, so every asset on a v0.14.0 release page would be named for a version that does not
	// exist.
	releaseStep := ""

	for _, step := range steps {
		if strings.Contains(step, "release") {
			releaseStep = step
		}
	}

	if releaseStep == "" {
		t.Errorf("release.yml runs the goreleaser action without `release` in its args:\n%s\n\n"+
			"`goreleaser build` compiles the binaries and stops: no tarballs, no packages, no checksums. "+
			"The step passes and dist/ holds bare binaries under per-target directories that no upload "+
			"glob matches", strings.Join(steps, "\n---\n"))
	}

	if strings.Contains(releaseStep, "--snapshot") {
		t.Error("release.yml passes --snapshot to goreleaser. A snapshot build ignores the tag and " +
			"derives its version from the previous one — v0.14.0 would publish assets named 0.14.1-next, " +
			"with package metadata to match. `make package-linux` passes it deliberately, because a " +
			"developer building from an untagged tree has no other version to use; a release must not")
	}

	// And the packages have to reach the release. Building them into an artifact nothing attaches is the
	// same outcome with more steps, and it is a plausible half-fix: the job goes green, the checks list
	// gains a reassuring name, and the release page is unchanged.
	//
	// The upload is what is asserted, not any mention of the path. `publish` also globs dist/ into the
	// summary, and a package that reaches the summary and not the upload is listed in a job log nobody
	// reads while being absent from the release.
	upload := jobStep(t, effective, "Upload the artifacts")
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
	//
	// The job named is `artifacts`, which is where the five-cell build matrix and the nfpm loop were
	// merged: one job now produces every tarball, every package and every checksum, so there is no
	// longer a packaging job that can fail while the tarball job succeeds. That merge removed a failure
	// mode and this assertion is what stops the remaining one.
	needs := needsList(t, workflow, "Publish Release")
	if !strings.Contains(needs, "artifacts") {
		t.Errorf("the Publish Release job's `needs` is %q and does not include artifacts, so a "+
			"packaging failure would publish a release with no assets at all \u2014 the notes are built from "+
			"CHANGELOG.md and do not need the build to have run. A release that looks complete and is "+
			"not is harder to notice than one that failed", needs)
	}
}

// TestReleaseChecksThePackageVersionAgainstTheTag guards the one link nothing else covers.
//
// nfpm.yaml's own comment stated the gap this closes: "nothing reads a package's version back to
// compare it, so `objectfs version` inside objectfs_0.12.0_amd64.deb would say 0.13.0 and no gate
// anywhere would notice". Both halves of the chain were verified and the join was not — release.yml
// checks the tag against the version constant, and TestPackageVersionComesFromTheVersionConstant
// checks that the packaging config holds no version of its own, but nothing read the version back out
// of a built package.
//
// goreleaser makes the chain shorter and does not make this redundant: the version now comes from the
// tag through goreleaser's own template rather than through an environment variable, and reading it
// back out of the .deb is still the only check that the value which reached the package metadata is
// the value the tag named.
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
			"comment named this exact gap: the tag is checked against the version constant and nothing " +
			"checks either against what the packaging actually wrote. A " +
			"package declaring a version it does not contain makes `apt-get install --only-upgrade` a " +
			"no-op, which is an upgrade that silently does not happen")
	}

	// The -1 matters and is easy to get wrong: the deb is objectfs_0.13.0-1_amd64.deb with a Version
	// field of 0.13.0-1, because the packaging pins `release: "1"`. The first draft of that workflow step
	// spelled the filename without the suffix and would have failed the release it was added to protect
	// — caught by running `make package-linux` locally rather than by reasoning about the name.
	if !strings.Contains(release, "$TAG-1") {
		t.Error("release.yml compares a package version without the `-1` release suffix. .goreleaser.yml " +
			"pins `release: \"1\"`, so both the filename and the Version field carry it: " +
			"objectfs_0.13.0-1_amd64.deb, Version 0.13.0-1. A comparison against a bare version fails " +
			"every release")
	}
}

// stepsUsing returns the body of every step whose `uses:` names action.
//
// Steps rather than the whole file, because what has to be read is one step's `with:` block: `args:`
// belongs to the step that sets it, and a workflow that runs goreleaser twice — once with `--snapshot`
// for a dry run, once for real — would satisfy any file-wide substring check in both directions at
// once.
//
// Split on `- name: `, which every step in this repository's workflows has. A step that used an action
// without naming it would be invisible here; that is a lint failure in this project's own conventions
// before it is a gap in this test.
func stepsUsing(workflow, action string) []string {
	var (
		steps   []string
		current []string
	)

	flush := func() {
		if len(current) > 0 && strings.Contains(strings.Join(current, "\n"), "uses: "+action) {
			steps = append(steps, strings.Join(current, "\n"))
		}

		current = nil
	}

	for line := range strings.SplitSeq(workflow, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- name: ") {
			flush()
		}

		current = append(current, line)
	}

	flush()

	return steps
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
