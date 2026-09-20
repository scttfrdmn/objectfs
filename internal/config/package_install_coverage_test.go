package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every package format this project builds is installed *and removed* by CI.
//
// This is the durable half of #149, and the defect it describes is the one release_packages_test.go
// describes with the arrows the other way round. That file couples "a format ci.yml builds" to "a format
// release.yml ships". This one couples "a format the packaging produces" to "a format CI has ever
// installed" — and for the rpm, the answer was no, for every release this project has published.
//
// The shape of it is worth stating because it is invisible from a green tree. All three rpms are built
// on every pull request, which is where a malformed header would surface, and a built package proves
// nothing about its scriptlets: `scripts/postinstall.sh` and `scripts/preremove.sh` had been run by
// dpkg and by this package's own tests against a staged root, and by no rpm anywhere. So every
// published rpm carried maintainer scripts that no rpm had ever executed, and the two package managers
// call those scripts with *different arguments* — dpkg's "configure"/"remove"/"upgrade" against rpm's
// instance counts. The tests that drive both spellings are asserting what rpm passes; only a real rpm
// can confirm it.
//
// Driven off `nfpms.formats` rather than a list of two, which is the property that makes this last. A
// third format — nfpm also writes apk, archlinux, ipk — is a one-line change to .goreleaser.yml that
// would immediately be published by release.yml, because the upload globs dist/, and would be installed
// by nothing. That format fails this test until CI learns it, which is the only moment anyone is
// thinking about it.
func TestEveryPackagedFormatIsInstalledAndRemovedInCI(t *testing.T) {
	t.Parallel()

	// Comments stripped, for the reason release_packages_test.go records after two of its gates
	// survived mutation: this job's steps are heavily commented and several of those comments name the
	// very commands being looked for — "installing one is worse than nothing", "`rpm -qp` under a
	// container is the follow-on worth having". A gate satisfied by prose about a command is the same
	// defect as one satisfied by prose about a config key.
	job := withoutComments(jobSource(t, "ci.yml", "packaging"))

	// Per format, the commands that count as an install and as a removal. Alternatives rather than one
	// literal, because there is more than one right answer and this gate is about whether the format is
	// exercised, not about which tool did it: `apt-get install ./x.deb` and `dpkg -i x.deb` are both a
	// real dpkg running real maintainer scripts.
	//
	// The `.deb`/`.rpm` requirement is what keeps a format's evidence its own. Without it `dnf install`
	// in a step that only ever touches debs would satisfy the rpm row, and a job that installs one
	// format twice would read as covering both.
	installers := map[string]struct {
		artifact string
		install  []string
		remove   []string
	}{
		"deb": {
			artifact: ".deb",
			install:  []string{"apt-get install", "apt install", "dpkg -i"},
			remove:   []string{"apt-get remove", "apt-get purge", "dpkg -r", "dpkg --remove"},
		},
		"rpm": {
			artifact: ".rpm",
			install:  []string{"rpm -i", "rpm -U", "rpm --install", "dnf install", "yum install"},
			remove:   []string{"rpm -e", "rpm --erase", "dnf remove", "yum remove"},
		},
	}

	formats := readNfpm(t).Formats
	if len(formats) == 0 {
		t.Fatalf("%s's nfpms entry lists no formats, so this test has nothing to check and the "+
			"packaging builds nothing", packagingFile)
	}

	for _, format := range formats {
		want, known := installers[format]
		if !known {
			// Fatal rather than skipped, and this is the whole point of reading the formats rather than
			// naming them. A format nobody taught this test about is a format nobody taught CI about
			// either, and the release upload globs dist/ — so it would publish on the next tag with its
			// scriptlets never once executed by the package manager that will run them.
			t.Errorf("%s builds the %q format and this test does not know how to install it, so "+
				"nothing in CI does either. Add the install and removal commands here and the steps "+
				"that run them to ci.yml's packaging job — a package format is shipped by the release "+
				"upload glob whether or not anything has ever unpacked it.", packagingFile, format)

			continue
		}

		if !containsAny(job, want.artifact) {
			t.Errorf("ci.yml's packaging job never names a %s artifact, so the %q format is built on "+
				"every pull request and installed by nothing", want.artifact, format)

			continue
		}

		if !runsCommand(job, want.install...) {
			t.Errorf("ci.yml's packaging job names a %s artifact but runs none of %v, so the %q "+
				"format is never installed. Building a package is where a malformed header surfaces; "+
				"it says nothing about scripts/postinstall.sh, which the package manager runs and "+
				"which rpm and dpkg call with different arguments.",
				want.artifact, want.install, format)
		}

		// The removal half is not symmetry for its own sake. scripts/preremove.sh is the script #207
		// was filed about — working code that nothing in the repository could reach — and an install
		// with no matching removal leaves it in exactly that state for one of the two package
		// managers, which is the state the rpm was in until #149.
		if !runsCommand(job, want.remove...) {
			t.Errorf("ci.yml's packaging job installs the %q format and runs none of %v, so "+
				"scripts/preremove.sh is never executed by that package manager. That script spent "+
				"several releases unreachable (#207); an install-only path is how it got there.",
				format, want.remove)
		}
	}
}

// containsAny reports whether source contains any of the candidates anywhere.
func containsAny(source string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(source, candidate) {
			return true
		}
	}

	return false
}

// runsCommand reports whether any line of source *begins* a command with one of the candidates.
//
// A line prefix rather than a substring anywhere, and this was measured rather than reasoned about.
// The first version of this file used containsAny for the install and removal commands, and the
// mutation that deleted `rpm -e objectfs` from ci.yml **passed**: the job's own diagnostics say
// "/sbin/mount.objectfs survived rpm -e, dangling" and "a modulefile survived rpm -e", and an
// `echo "::error::..."` is executable text rather than a comment, so stripping comments does not
// remove it. The gate was reading a message *about* the removal while the removal itself was gone.
//
// That is the `cosign verify-blob` failure in release_signing_test.go, one level down, and it arrived
// in a new gate written by someone who had just read that comment — which is the argument for the
// structural rule over care. A command is at the start of a line; prose that names a command is not.
//
// `sudo` is stripped because it is not part of the command being asserted, and every install in this
// job that runs on the runner needs it while every install that runs in a container does not. Nothing
// else is stripped: a command behind `if`, `&&` or a `$(...)` is deliberately not matched, because a
// package install this gate should count is one the step runs unconditionally.
func runsCommand(source string, candidates ...string) bool {
	for line := range strings.SplitSeq(source, "\n") {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "sudo ")

		for _, candidate := range candidates {
			if strings.HasPrefix(trimmed, candidate) {
				return true
			}
		}
	}

	return false
}

// jobSource returns one job's text out of a workflow, from its own `  <id>:` line to the next job's.
//
// Scoped to the job, because ci.yml is eighteen jobs and 1500 lines: a whole-file search for `rpm -i`
// or `dpkg -i` is answered by the install-script job, which runs a package manager inside a container
// for an entirely different reason, and by the deploy-manifests job's YAML. The gate would then report
// coverage that the packaging job does not have — which is the `cosign verify-blob` failure in
// release_signing_test.go, one level up from a step.
//
// A text cut and not a YAML descent, deliberately and with a caveat. #504 converged this package's four
// workflow parsers into one, and the parsed form is the right way to reach a job's steps; the struct
// that lands with it grows a `Steps` field that is not on this tree yet, and adding a fifth parser here
// to bridge the gap would be the thing #504 exists to stop. So this cuts the text, and whoever holds
// both halves should replace it with a `steps:` walk — the assertions above do not depend on which way
// the job's body was obtained.
func jobSource(t *testing.T, workflow, jobID string) string {
	t.Helper()

	source := readFile(t, filepath.Join(repoRoot(t), ".github", "workflows", workflow))

	var (
		body  []string
		inJob bool
	)

	for line := range strings.SplitSeq(source, "\n") {
		if line == "  "+jobID+":" {
			inJob = true

			continue
		}

		if !inJob {
			continue
		}

		// The next job starts at the same indentation. Matched on the shape rather than on a list of
		// job ids, so this does not have to be told when one is added — and comment lines, which in
		// this file sit at the same two-space indentation, do not match because they do not end in a
		// colon after an identifier.
		if isJobHeader(line) {
			break
		}

		body = append(body, line)
	}

	if !inJob {
		t.Fatalf(".github/workflows/%s has no job `%s:` at two-space indentation. If it was renamed, "+
			"rename it here — a gate that cannot find its subject passes having read nothing", workflow, jobID)
	}

	// A floor, because the failure this cannot otherwise see is a cut that finds the header and stops
	// immediately: every per-format assertion above would then fail with a message about CI not
	// installing anything, which is a true statement about an empty string and a confusing one to debug.
	const shortestPlausibleJob = 20

	if len(body) < shortestPlausibleJob {
		t.Fatalf("job `%s` in .github/workflows/%s cut to %d lines, which is shorter than any job in "+
			"this repository. The cut is wrong, not the job", jobID, workflow, len(body))
	}

	cut := strings.Join(body, "\n")

	// A ceiling as well as a floor, and this is the direction a scoping bug actually goes. A cut that
	// runs past the next job — because the header pattern stopped matching — is not empty and does not
	// look wrong: every assertion above would pass, satisfied by the *other* jobs in a 1500-line file,
	// which is precisely the coverage this helper exists to deny them. Expressed as the one structural
	// fact that distinguishes the two, rather than as a line count: every job declares exactly one
	// `runs-on:`, so a body holding more than one is a body holding more than one job. Counted with
	// comments stripped, since a comment is free to discuss another job's runner.
	if n := strings.Count(withoutComments(cut), "runs-on:"); n != 1 {
		t.Fatalf("job `%s` in .github/workflows/%s cut to %d lines declaring %d `runs-on:` keys, want "+
			"exactly 1. More than one means the cut ran past this job into the next, and every "+
			"assertion scoped to this job is then being answered by a different one", jobID, workflow,
			len(body), n)
	}

	return cut
}

// isJobHeader reports whether a line is a `  <identifier>:` job key.
func isJobHeader(line string) bool {
	rest, ok := strings.CutPrefix(line, "  ")
	if !ok {
		return false
	}

	name, ok := strings.CutSuffix(rest, ":")
	if !ok || name == "" {
		return false
	}

	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}

	return true
}
