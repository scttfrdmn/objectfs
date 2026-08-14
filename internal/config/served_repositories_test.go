package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file gates the apt and yum repositories as *published, signed addresses*: objectfs.io/apt and
// objectfs.io/yum, both verified against the key at objectfs.io/objectfs.asc.
//
// served_install_script_test.go is the model, and the reasoning carries over exactly: the installer's
// failure mode was a documented address that 404s, and the repositories' failure mode is a documented
// address that serves an *unsigned or unverifiable* index. The second is worse. A 404 is visible on the
// first try; a repository whose signature nobody checks installs packages successfully forever, and the
// day it matters is the day someone has modified it.
//
// Three properties, and each one is here because its absence was measured in a container rather than
// reasoned about:
//
//  1. pages.yml builds and signs both repositories, and verifies its own signatures before publishing.
//  2. release.yml signs every rpm, and asserts the signature is present rather than trusting nfpm.
//  3. the two setup scripts turn signature verification on, and apt's key is scoped.
//
// (2) exists because the first end-to-end run on rockylinux:9 downloaded the package and failed with
// "Package objectfs-0.13.0-1.x86_64.rpm is not signed / Error: GPG check FAILED". nfpm builds unsigned
// rpms by default, and the repository being signed does not help: apt's InRelease signature covers the
// Packages indexes, which carry each .deb's checksum, so one signature at the top transitively covers
// the packages — rpm has no such chain, and each package stands alone.

// TestRepoSetupScriptsRequireSignatureVerification is about the two scripts users pipe into a shell.
//
// The settings asserted here are the ones that are *off by default* in the tool being configured, which
// is exactly the set that a rewrite would drop without anything failing. `gpgcheck=1` is dnf's default
// and is asserted anyway; `repo_gpgcheck=1` is not, and it is the one the container mutation proved is
// load-bearing: with it set to 0, a tampered repomd.xml is accepted and the install succeeds.
func TestRepoSetupScriptsRequireSignatureVerification(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	debian := readFile(t, filepath.Join(root, "scripts", "setup-repo-debian.sh"))
	rhel := readFile(t, filepath.Join(root, "scripts", "setup-repo-rhel.sh"))

	// The apt side. Signed-By is the entire reason the deb822 format is used here: a key dropped into
	// /etc/apt/trusted.gpg.d authorizes its holder to sign *any* package from any repository the machine
	// reads, Debian's own openssh-server included. Signed-By scopes it to this repository and nothing
	// else, which is a property rpm cannot offer at all.
	if !strings.Contains(debian, "Signed-By: $KEYRING") {
		t.Error("scripts/setup-repo-debian.sh writes a sources entry without `Signed-By:`. Without it " +
			"apt falls back to the machine-wide trusted keyring, and the ObjectFS signing key becomes a " +
			"valid signer for every repository on the system — including Debian's own. The script's " +
			"header promises the opposite in capitals, and a promise in a comment is not a scope")
	}

	// apt-key, explicitly. #138's spec says "apt-key add", which is deprecated since apt 2.4 for this
	// exact reason: it is the unscoped path. Asserted as an absence because the spec is the thing most
	// likely to pull a future edit back toward it.
	//
	// An *invocation*, not a mention. The script's header and its --help text both name apt-key in order
	// to say why they do not use it, and the first version of this check failed on that explanation —
	// which would have left only two ways to satisfy it: delete the reasoning, or weaken the test. A
	// line whose first word is the command is the thing being ruled out.
	if hasCommand(debian, "apt-key") {
		t.Error("scripts/setup-repo-debian.sh uses `apt-key add`, which installs the key machine-wide " +
			"and is deprecated since apt 2.4. #138's original text says apt-key; the scoped equivalent " +
			"is a keyring under /usr/share/keyrings named by Signed-By, which is what the script does " +
			"everywhere else")
	}

	// The rpm side, both settings, as whole lines of the stanza. A substring check would be satisfied by
	// the long comment above the stanza that explains what each one does — and that comment is precisely
	// what would survive a step being deleted.
	for _, want := range []string{"gpgcheck=1", "repo_gpgcheck=1"} {
		if !hasLine(rhel, want) {
			t.Errorf("scripts/setup-repo-rhel.sh's .repo stanza does not contain %q on a line of its "+
				"own. gpgcheck verifies each package's own embedded signature; repo_gpgcheck verifies "+
				"repomd.xml against repomd.xml.asc. They check different things and neither implies the "+
				"other: with repo_gpgcheck off, a tampered index is accepted — measured on rockylinux:9 "+
				"and opensuse/leap:15.6 by setting it to 0 and watching the install succeed", want)
		}
	}

	// zypper's directory. A yum-only script *succeeds* on openSUSE and configures nothing: zypper reads
	// /etc/zypp/repos.d and does not read /etc/yum.repos.d, so the .repo file sits there looking correct
	// while `zypper install objectfs` reports the package as not found. The clean-run-that-did-nothing
	// is the failure this line prevents, and #138's matrix includes opensuse/leap:15.6 for that reason.
	if !strings.Contains(rhel, "/etc/zypp/repos.d") {
		t.Error("scripts/setup-repo-rhel.sh never names /etc/zypp/repos.d, so on openSUSE it writes to " +
			"a directory zypper does not read. That failure is invisible from the script's own output: " +
			"every step succeeds and no repository is configured")
	}

	// Both scripts must fetch over HTTPS by default. The URL is overridable for the container harness,
	// which is why the default being a literal https:// matters — a piped one-liner whose address comes
	// from the environment can be redirected by whatever set it.
	for name, script := range map[string]string{
		"setup-repo-debian.sh": debian,
		"setup-repo-rhel.sh":   rhel,
	} {
		if !strings.Contains(script, "https://objectfs.io/objectfs.asc") {
			t.Errorf("scripts/%s does not default to https://objectfs.io/objectfs.asc for the signing "+
				"key. pages.yml exports the public key to that exact path", name)
		}
	}
}

// TestPagesWorkflowBuildsThePackageRepositories asserts the addresses the scripts name are published.
//
// The scripts above are the half a user runs; this is the half that has to exist for them to work. Both
// halves passing their own checks while naming different things is the drift served_install_script_test.go
// was written about — there the risk was serving a *different* install.sh, here it is the scripts
// pointing at /apt and /yum while the deploy publishes only the docs.
func TestPagesWorkflowBuildsThePackageRepositories(t *testing.T) {
	t.Parallel()

	workflow := withoutComments(pagesWorkflow(t))

	// The public key at the address both scripts fetch. Everything else here is worthless without it:
	// a perfectly signed repository whose key is not served cannot be verified by anyone.
	if !strings.Contains(workflow, "_site/objectfs.asc") {
		t.Error(".github/workflows/pages.yml does not export the signing key to _site/objectfs.asc. " +
			"Both setup scripts fetch https://objectfs.io/objectfs.asc and both fail closed without " +
			"it, so the repositories would publish and be unusable")
	}

	// The signatures themselves, one per repository format.
	//
	// InRelease (inline-signed) and Release.gpg (detached) are both produced because apt clients differ
	// in which they fetch; repomd.xml.asc is what repo_gpgcheck=1 verifies against.
	for _, want := range []string{
		"_site/apt/dists/stable/InRelease",
		"_site/apt/dists/stable/Release.gpg",
		"_site/yum/repodata/repomd.xml.asc",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf(".github/workflows/pages.yml never writes %s. An unsigned index is not a lesser "+
				"version of a signed one: setup-repo-debian.sh's Signed-By entry and "+
				"setup-repo-rhel.sh's repo_gpgcheck=1 both refuse it, so the repository would be "+
				"published and every `apt update` against it would fail", want)
		}
	}

	// The workflow verifying its own output. This is the difference between signing and having signed
	// something that verifies: `gpg --clearsign` succeeds against a key that the exported public key
	// does not match, and the mismatch is only discoverable at a user's `apt update`.
	if strings.Count(workflow, "gpg --batch --verify") < 2 {
		t.Error(".github/workflows/pages.yml signs the indexes without verifying the signatures it " +
			"just made. Both are needed — the apt InRelease and the yum repomd.xml.asc — because " +
			"signing cannot fail in a way that produces a file that does not verify, and publishing " +
			"one moves the discovery of the problem to a user's machine")
	}

	// Fail-closed, in the shell rather than in an `if:`.
	//
	// This is the mutation that produced a silently docs-only deploy: `if: ${{ env.GPG_SIGNING_KEY !=
	// '' }}` cannot read `secrets` at all, and the env-plus-comparison workaround is evaluated before
	// the step's own `env:` block is in scope — so it is *always* empty and the step is skipped forever.
	// A guard that can only answer "no key" is not a guard.
	repoStep := pagesStep(t, withoutComments(pagesWorkflow(t)), "Build the apt and yum repositories")

	if strings.Contains(repoStep, "if: ${{ env.GPG_SIGNING_KEY") {
		t.Error(".github/workflows/pages.yml guards the repository build with a step-level `if:` on " +
			"env.GPG_SIGNING_KEY. `secrets` is not readable from a step `if:`, and the env workaround " +
			"is evaluated before the step's own env block is in scope, so the expression is always " +
			"empty and the step never runs. The result is a deploy that publishes the docs, skips the " +
			"repositories, and reports success. The guard belongs in the shell")
	}

	if !strings.Contains(repoStep, `if [ -z "${GPG_SIGNING_KEY:-}" ]`) {
		t.Error(".github/workflows/pages.yml's repository step has no in-shell check for an absent " +
			"signing key. With no key it must publish no repository rather than an unsigned one: both " +
			"setup scripts require a signature, so an unsigned repository fails at the user's `apt " +
			"update` instead of in CI")
	}

	// The setup scripts themselves, served and byte-compared.
	//
	// This is here because the documentation was written before the copy step existed, and every
	// one-liner in it — README.md, docs/index.md, web/index.html, and both docs-platform pages — named
	// https://objectfs.io/setup-repo-debian.sh while pages.yml copied only install.sh. Five documented
	// commands, all of them a 404, and nothing anywhere failed: the scripts were committed, reviewed,
	// tested in three containers, and unreachable.
	//
	// The trigger matters as much as the copy, and more here than for the installer. A stale installer
	// installs an old binary; a stale setup script installs an old signing *key*, and that key remains a
	// trusted signer on the machine after a later correct run.
	for _, script := range []string{"setup-repo-debian.sh", "setup-repo-rhel.sh"} {
		if !copiesAndComparesFromScripts(workflow, script) {
			t.Errorf(".github/workflows/pages.yml does not copy scripts/%s into the site root and "+
				"compare it byte for byte. Five documented one-liners pipe "+
				"https://objectfs.io/%s into `sudo bash`, and without this step every one of them is a "+
				"404 — while the script itself sits in the repository, reviewed and tested", script,
				script)
		}

		if !hasSequenceEntry(workflow, "scripts/"+script) {
			t.Errorf(".github/workflows/pages.yml does not list scripts/%s in its push paths filter, "+
				"so editing it does not redeploy the site. The served copy then lags the repository "+
				"until an unrelated docs change happens to trigger a build, with every check green the "+
				"whole time — and for these two scripts the stale thing is the signing key, which stays "+
				"trusted on every machine that ran the old copy", script)
		}
	}

	// Retention has to be a number the workflow reads, not a slice of whatever `gh release list`
	// happens to return. Pages caps a site at 1 GB and four packages per release are ~33 MB, so five
	// releases is ~165 MB — a bound that exists on purpose and can be raised knowingly.
	if !strings.Contains(workflow, "REPO_KEEP") {
		t.Error(".github/workflows/pages.yml indexes release assets without a retention bound. " +
			"GitHub Pages caps a site at 1 GB and one release is roughly 33 MB of packages, so an " +
			"unbounded repository fails the deploy some releases from now, at which point the docs " +
			"stop deploying too")
	}
}

// TestRPMPackagesAreSigned is the gate for the defect a container found.
//
// nfpm builds unsigned rpms unless a signature block names a key, and an unsigned rpm inside a
// perfectly signed repository fails `gpgcheck=1` — which is the default on every RHEL-family machine.
// So the failure lands on the user, at install time, with "Error: GPG check FAILED", after the download
// has already succeeded.
//
// Three places have to agree for this to work, and this test checks all three because any one of them
// alone is silently insufficient: nfpm.yaml has to ask for a signature, release.yml has to provide the
// key, and something has to verify the result.
func TestRPMPackagesAreSigned(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	// nfpm.yaml asks. `signature` lives on the top-level `rpm:` section and not under `overrides.rpm:` —
	// nfpm rejects the whole config with "field signature not found in type nfpm.Overridables" if it is
	// written the other way, which at least fails loudly.
	nfpm := readFile(t, filepath.Join(root, "nfpm.yaml"))

	for _, want := range []string{"key_file:", "key_id:"} {
		if !strings.Contains(nfpm, want) {
			t.Errorf("nfpm.yaml has no rpm signature %s. Without both, nfpm builds an unsigned rpm, "+
				"and gpgcheck=1 — dnf's default, and set explicitly by setup-repo-rhel.sh — refuses "+
				"it after downloading it. The repository being signed does not help: apt's InRelease "+
				"signature transitively covers every .deb through the Packages checksums, and rpm has "+
				"no equivalent chain", want)
		}
	}

	release := withoutComments(readFile(t, filepath.Join(root, ".github", "workflows", "release.yml")))

	// release.yml provides the key, and the two environment variables have to be the ones nfpm.yaml
	// interpolates — a signature block reading variables nothing sets produces an unsigned package and
	// no error, because nfpm skips signing when key_file names nothing readable.
	for _, want := range []string{"OBJECTFS_SIGNING_KEY_FILE", "OBJECTFS_SIGNING_KEY_ID"} {
		if !strings.Contains(release, want) {
			t.Errorf(".github/workflows/release.yml never sets %s, which nfpm.yaml interpolates. An "+
				"unset key_file is not an error in nfpm: it skips signing and writes an unsigned "+
				"package, which is the exact outcome this whole path exists to prevent", want)
		}
	}

	// ${FPR: -16}, the 16-hex-digit long key ID. Both wrong forms were tried against nfpm 2.44 rather
	// than reasoned about, and the dangerous one names no key at all in its error:
	//
	//   40-char fingerprint: "is not a valid key id: strconv.ParseUint ... value out of range"
	//   8-char short ID:     "signing error: openpgp: invalid argument: no valid signing keys"
	if !strings.Contains(release, "${FPR: -16}") {
		t.Error(".github/workflows/release.yml does not derive the signing key ID as the last 16 " +
			"characters of the fingerprint. nfpm wants a 16-hex-digit long key ID: a full fingerprint " +
			"fails with strconv.ParseUint out of range, and an 8-character short ID fails with " +
			"\"openpgp: invalid argument: no valid signing keys\" — a message that names no key and " +
			"reads like a broken key file")
	}

	// The release build fails closed. pages.yml skips the repositories without a key, which is right for
	// a docs deploy; a *release* must not quietly ship unsigned packages, because those packages outlive
	// the run that built them and are what users install.
	// Inside the absent-key branch specifically, and that distinction is the whole value of this check.
	// The first version asked whether the step contained `exit 1` anywhere, and it does — the
	// imported-no-secret-key check a few lines below has one. So deleting the exit from the guard left a
	// step that continues past a missing key and a test that still passed. Caught by deleting it.
	signStep := pagesStep(t, release, "Import the package signing key")

	if !guardExits(signStep, `if [ -z "${GPG_SIGNING_KEY:-}" ]`) {
		t.Error(".github/workflows/release.yml notices an absent signing key without exiting non-zero " +
			"in that branch. Unlike pages.yml, which correctly skips publishing a repository it cannot " +
			"sign, a release that continues without a key uploads unsigned rpms as permanent assets — " +
			"and the first person to notice is a user whose dnf refuses them, having downloaded the " +
			"package successfully first")
	}

	// And something verifies. Asserting on the *text* rather than the exit status, because `rpm -K`
	// exits 0 for an unsigned package: unsigned prints "digests OK" and returns 0, signed prints
	// "digests signatures OK" and returns 0. Measured, not assumed. A status-only check passes on every
	// unsigned package ever built, which makes it worse than no check — it reports a verified signature
	// that was never verified.
	if !strings.Contains(release, "signatures OK") {
		t.Error(".github/workflows/release.yml does not check `rpm -K` output for \"signatures OK\". " +
			"The exit status cannot carry this: an unsigned package prints \"digests OK\" and exits 0, " +
			"a signed one prints \"digests signatures OK\" and also exits 0. A check on the status " +
			"alone passes for every unsigned package, and reports that it verified the signature")
	}

	// CI installs from the built repository with verification on, which is the check that would have
	// caught the unsigned rpm before a release rather than after one.
	ci := withoutComments(readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml")))

	if !strings.Contains(ci, "repo-install:") {
		t.Error(".github/workflows/ci.yml has no repo-install job. The unsigned-rpm defect was found " +
			"by installing from a real repository in a container, and nothing else finds it: every " +
			"unit-level check passes on an unsigned package, the repository signs and verifies " +
			"correctly, and the failure appears only at `dnf install`")
	}

	// A step that both sets the signing variables and runs `make package-linux` must `export` them.
	//
	// This is the mistake CI made on the first run of this job, and it is worth a gate because nothing
	// about it looks wrong: a `$GITHUB_ENV` write is the idiomatic way to pass a value between steps, and
	// it takes effect in *subsequent* steps only. ci.yml wrote the variables to $GITHUB_ENV and then ran
	// make in the same step, so nfpm saw neither, skipped signing without an error — an unset key_file is
	// not a failure to nfpm — and produced four packages, one of them an unsigned rpm.
	//
	// release.yml does not have the problem, because there the build is a separate step and $GITHUB_ENV
	// is the right mechanism. So the property is conditional: only a step that does both needs the
	// export.
	for name, workflow := range map[string]string{
		"ci.yml":      ci,
		"release.yml": release,
	} {
		for _, step := range stepsRunning(workflow, "make", "package-linux") {
			// Both variables, not just the file. nfpm needs each of them and ignores the signature block
			// if either is unset, so a check naming one passes on a step that exports one — which the
			// first version of this did.
			for _, v := range []string{"OBJECTFS_SIGNING_KEY_FILE", "OBJECTFS_SIGNING_KEY_ID"} {
				if !strings.Contains(step, v) {
					// A step that builds without touching the signing variables inherits them from an
					// earlier step, which is correct and is what release.yml does.
					continue
				}

				if !strings.Contains(step, "export "+v) {
					t.Errorf("a step in .github/workflows/%s sets %s and runs `make package-linux` in "+
						"the same shell without exporting it. A $GITHUB_ENV write applies to later steps "+
						"only, so make would run with the variable unset — and nfpm treats an unset "+
						"key_file or key_id as \"do not sign\" rather than as an error, so the build "+
						"succeeds and writes an unsigned rpm. That is this job's own subject matter, and "+
						"it happened", name, v)
				}
			}
		}
	}

	// The images, named. Each row catches something the others do not: ubuntu is the apt path,
	// rockylinux is dnf, and opensuse/leap is the row that catches /etc/zypp/repos.d.
	for _, image := range []string{"ubuntu:24.04", "rockylinux:9", "opensuse/leap:15.6"} {
		if !strings.Contains(ci, image) {
			t.Errorf(".github/workflows/ci.yml never runs against %s. #138's matrix names all three "+
				"and each row catches a distinct failure: apt's scoped keyring, dnf's package "+
				"signature, and zypper's separate repo directory — which a yum-only script misses "+
				"while succeeding", image)
		}
	}
}

// stepsRunning returns the bodies of every step whose shell runs cmd with arg.
//
// Steps rather than the whole file, because the property being checked is about what shares one shell:
// a variable set in one step and used in another is a different situation from both in the same step,
// and only the second needs an `export`.
func stepsRunning(workflow, cmd, arg string) []string {
	var (
		steps   []string
		current []string
	)

	flush := func() {
		if len(current) > 0 && hasCommandLine(strings.Join(current, "\n"), cmd, arg) {
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

// guardExits reports whether the shell block opened by guard reaches `exit 1` before its `fi`.
//
// A scoped search rather than a substring, because a step that fails closed and a step that logs and
// carries on differ by one line inside one branch, and every other `exit 1` in the step is unaffected by
// deleting it. Reads to the first `fi` at the guard's own indentation, which is enough for the flat
// guard-at-the-top-of-a-step shape both workflows use, and does not try to be a shell parser.
func guardExits(step, guard string) bool {
	var (
		inGuard bool
		indent  string
	)

	for line := range strings.SplitSeq(step, "\n") {
		trimmed := strings.TrimSpace(line)

		if !inGuard {
			if trimmed == guard+"; then" || trimmed == guard || strings.HasPrefix(trimmed, guard+";") {
				inGuard = true
				indent = line[:len(line)-len(strings.TrimLeft(line, " "))]
			}

			continue
		}

		if trimmed == "fi" && strings.HasPrefix(line, indent) {
			return false
		}

		if strings.HasPrefix(trimmed, "exit ") && trimmed != "exit 0" {
			return true
		}
	}

	return false
}

// hasCommand reports whether any line of a shell script runs cmd as its first word.
//
// So that a script can explain in prose why it avoids a command without that explanation failing the
// check that it avoids it. Handles the forms a real invocation takes — bare, under sudo, after a pipe or
// a `&&`, and inside `$(...)` — and deliberately not a mention inside a sentence or a quoted string.
func hasCommand(script, cmd string) bool {
	for line := range strings.SplitSeq(script, "\n") {
		trimmed := strings.TrimSpace(line)

		// A comment is prose by definition, and the reasoning this exists to protect lives in comments.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Each shell segment of the line, so `foo && apt-key add x` is caught while `echo "use apt-key"`
		// is not. Splitting on the separators that start a fresh command.
		for _, sep := range []string{"&&", "||", "|", ";", "$(", "`", "(", "then", "else", "do"} {
			trimmed = strings.ReplaceAll(trimmed, sep, "\n")
		}

		for segment := range strings.SplitSeq(trimmed, "\n") {
			fields := strings.Fields(segment)
			if len(fields) == 0 {
				continue
			}

			// sudo, env and command are wrappers rather than the command itself.
			for len(fields) > 1 && (fields[0] == "sudo" || fields[0] == "env" || fields[0] == "command") {
				fields = fields[1:]
			}

			if fields[0] == cmd {
				return true
			}
		}
	}

	return false
}

// hasLine reports whether any line of doc, trimmed, is exactly value.
//
// The same reasoning as hasSequenceEntry in served_install_script_test.go: what has to be distinguished
// is a line of the .repo stanza from the paragraph above it explaining what that line does — and the
// paragraph is what survives when the line is deleted.
func hasLine(doc, value string) bool {
	for line := range strings.SplitSeq(doc, "\n") {
		if strings.TrimSpace(line) == value {
			return true
		}
	}

	return false
}

// pagesStep returns the body of a named step, in any workflow.
//
// jobStep in release_packages_test.go does the same thing, and its t.Fatalf names release.yml — which
// is correct there and wrong here. Rather than change a message three other tests depend on, this is
// the same scan with a message that does not claim to know which file it is reading.
func pagesStep(t *testing.T, workflow, stepName string) string {
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

		if strings.HasPrefix(trimmed, "- name: ") {
			break
		}

		body = append(body, line)
	}

	if !inStep {
		t.Fatalf("found no step named %q. If it was renamed, point this test at the new name — a gate "+
			"that cannot find its subject passes for the wrong reason", stepName)
	}

	return strings.Join(body, "\n")
}

// TestNoSecondCopyOfTheRepositorySetupScripts is served_install_script_test.go's duplicate check, for
// the same reason and with a sharper edge.
//
// web/ is copied verbatim to the site root, so a committed web/setup-repo-debian.sh would be served
// instead of — or alongside — the reviewed one. An installer duplicate runs stale code; a *repository
// setup* duplicate configures stale trust, which means a key the project may have rotated away from
// remains a valid signer on every machine that ran the wrong copy.
func TestNoSecondCopyOfTheRepositorySetupScripts(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	for _, rel := range []string{
		filepath.Join("web", "setup-repo-debian.sh"),
		filepath.Join("web", "setup-repo-rhel.sh"),
		filepath.Join("docs", "setup-repo-debian.sh"),
		filepath.Join("docs", "setup-repo-rhel.sh"),
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			t.Errorf("%s exists. scripts/ holds the only copies, and a duplicate under a published "+
				"directory is the file users would pipe into a shell and the file no gate here reads. "+
				"A stale repository script installs a key that may have been rotated away from, and "+
				"that key stays trusted on the machine afterwards", rel)
		}
	}
}
