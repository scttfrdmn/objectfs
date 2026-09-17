package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is what survives of served_repositories_test.go, which gated apt and yum repositories that
// were built, tested on every pull request for several releases, and never published a single byte.
//
// The evidence for "never": `https://objectfs.io/apt/dists/stable/Release` and
// `https://objectfs.io/yum/objectfs.repo` both answered 404 for the whole life of the feature, while
// `https://objectfs.io/install.sh` answered 200 the entire time. The old file's header argued that the
// repositories were "dormant, not abandoned, and the difference between the two is whether the path is
// still checked" — which was a coherent position, and the thing it did not account for is that a path
// nobody was ever going to publish costs its upkeep forever. So the repositories, both setup scripts,
// ci.yml's repo-install job and pages.yml's repository-building step are gone, and what users get is
// what they were already getting: a .deb and an .rpm attached to each release, which
// `apt install ./objectfs_*.deb` and `dnf install ./objectfs-*.rpm` take directly.
//
// Two properties outlive that deletion, and both are here because each was measured in a container
// rather than reasoned about:
//
//  1. rpm signing still has to work end to end, because `gpgcheck=1` is dnf's default for a
//     *downloaded file* too — no repository required. nfpm builds unsigned rpms unless a signature
//     block names a key, and an unsigned one fails with "Error: GPG check FAILED" after the download
//     has already succeeded.
//  2. no page may document a repository address, which is now not a policy statement but a fact about
//     what the site serves.

// TestTheRPMSigningPathStaysIntact is the gate for the defect a container found.
//
// Three places have to agree, and this test checks all three because any one of them alone is silently
// insufficient: nfpm.yaml has to ask for a signature, release.yml has to provide the key, and something
// has to verify the result.
//
// All three are checked even though releases currently ship unsigned, because "intact" is the property
// worth keeping — and unlike the repositories, this path has a user the day anyone sets the secret.
//
// The job that originally found the missing signature was ci.yml's repo-install, which installed from
// throwaway-signed repositories with gpgcheck=1 on. That job is gone with the repositories, so the two
// assertions naming it are gone from this test: one required the job to exist, and one required ci.yml
// to name ubuntu:24.04, rockylinux:9 and opensuse/leap:15.6 for reasons about apt keyrings and zypper's
// repo directory that no longer apply. ci.yml's install-script job still runs all three images, for its
// own reasons — downloader and tar preflight — and asserting that from here would be a gate whose
// stated rationale had come apart from its subject.
func TestTheRPMSigningPathStaysIntact(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	// nfpm.yaml asks. `signature` lives on the top-level `rpm:` section and not under `overrides.rpm:` —
	// nfpm rejects the whole config with "field signature not found in type nfpm.Overridables" if it is
	// written the other way, which at least fails loudly.
	nfpm := readFile(t, filepath.Join(root, "nfpm.yaml"))

	for _, want := range []string{"key_file:", "key_id:"} {
		if !strings.Contains(nfpm, want) {
			t.Errorf("nfpm.yaml has no rpm signature %s. Without both, nfpm builds an unsigned rpm, "+
				"and gpgcheck=1 — dnf's default — refuses it after downloading it. Note this bites a "+
				"plain `dnf install ./objectfs-*.rpm` as much as it would a repository: each rpm carries "+
				"its own signature and stands alone, with no equivalent of apt's InRelease chain", want)
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

	// The absent-key branch skips signing and says so, rather than either failing the release or going
	// quiet about it.
	//
	// This assertion was the opposite one — that the branch reaches `exit 1` — and the reasoning behind it
	// was sound while a repository was going to be published: an unsigned rpm inside a signed repository
	// installs cleanly from a downloaded file and fails only for users who added the repository, which is
	// the failure mode most likely to ship unnoticed. There is no such repository and now no such
	// machinery, and the refusal blocked every release to protect nobody.
	//
	// So the property is now that the branch ends the *step* successfully and leaves a warning in the run.
	// Both halves matter. `exit 0` rather than falling through, because the rest of the step imports a key
	// it does not have. A warning rather than a bare skip, because unsigned-because-no-secret and
	// unsigned-because-something-broke look identical in a green run, and the annotation is what tells
	// the two apart at a glance six months from now.
	//
	// Scoped to inside the guard, and that scoping is the whole value of this check. Its first version
	// asked whether the step contained `exit 1` anywhere, and it does — the imported-no-secret-key check
	// below has one — so deleting the exit from the guard left a step that continued past a missing key
	// and a test that still passed.
	signStep := namedStep(t, release, "Import the package signing key")

	branch, found := guardBody(signStep, `if [ -z "${GPG_SIGNING_KEY:-}" ]`)
	if !found {
		t.Error(".github/workflows/release.yml's signing step has no in-shell check for an absent " +
			"signing key. Without one the step imports a key it does not have: `gpg --import` of an " +
			"empty string succeeds, the fingerprint lookup returns nothing, and the failure arrives " +
			"further down as a key ID that is the empty string")
	}

	if strings.Contains(branch, "exit 1") {
		t.Error(".github/workflows/release.yml refuses to release without a signing key. It used to, on " +
			"the reasoning that an unsigned rpm fails at `dnf install` for anyone who added the " +
			"repository — no repository was ever published, the machinery for one is deleted, so there " +
			"is nobody to protect and every release is blocked")
	}

	if !strings.Contains(branch, "exit 0") {
		t.Error(".github/workflows/release.yml notices an absent signing key and falls through into the " +
			"rest of the step, which imports the key and reads a fingerprint back from it. With no key " +
			"that yields an empty key ID, which nfpm accepts as \"do not sign\" — so the release " +
			"succeeds, the packages are unsigned, and the only trace is a step that claims to have " +
			"imported something")
	}

	if !strings.Contains(branch, "::warning::") {
		t.Error(".github/workflows/release.yml skips signing without annotating the run. An unsigned " +
			"release is the intended state today and it is also what a broken signing path produces, " +
			"and the two are indistinguishable in a green run. The annotation is the only thing that " +
			"says which one this was")
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

	// And it verifies the *unsigned* case too, rather than skipping when there is no key.
	//
	// Two things produce an unsigned rpm and they are far apart: no secret, which is today's intended
	// state, and a secret that was imported and then failed to reach nfpm — which happened, when a step
	// wrote the signing variables to $GITHUB_ENV and ran make in the same shell, where a $GITHUB_ENV write
	// does not apply. From outside they are the same run: four packages, green. So the branch has to be
	// chosen by whether a key was imported, and each branch has to insist on the state it implies. A skip
	// lets the two swap places in silence, which is how the defect shipped the first time.
	if !strings.Contains(release, "digests OK") {
		t.Error(".github/workflows/release.yml verifies signed packages but skips unsigned ones instead " +
			"of asserting they are unsigned. \"digests OK\" without \"signatures\" is the unsigned " +
			"answer, and checking for it is what distinguishes unsigned-because-there-is-no-secret from " +
			"unsigned-because-the-key-never-reached-nfpm. Those two look identical in a green run, and " +
			"the second one is a real defect this repository has already shipped once")
	}

	// A step that both sets the signing variables and runs `make package-linux` must `export` them.
	//
	// This is the mistake CI made on the first run of the repository job, and it is worth a gate because
	// nothing about it looks wrong: a `$GITHUB_ENV` write is the idiomatic way to pass a value between
	// steps, and it takes effect in *subsequent* steps only. The job wrote the variables to $GITHUB_ENV
	// and then ran make in the same step, so nfpm saw neither, skipped signing without an error — an
	// unset key_file is not a failure to nfpm — and produced four packages, one of them an unsigned rpm.
	//
	// release.yml does not have the problem, because there the build is a separate step and $GITHUB_ENV
	// is the right mechanism. So the property is conditional: only a step that does both needs the
	// export. Kept after the job that made the mistake was deleted, because the mistake is a property of
	// $GITHUB_ENV rather than of that job, and any future packaging step can make it again.
	ci := withoutComments(readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml")))

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
						"succeeds and writes an unsigned rpm. That has happened here", name, v)
				}
			}
		}
	}
}

// TestNoPackageRepositoryAddressIsDocumented is the second half of the deletion.
//
// The addresses below are not merely unrecommended, they are unserved and there is no longer any code
// that could serve them: pages.yml assembles the landing page, the MkDocs tree and install.sh, and
// nothing else. A page documenting `curl -fsSL https://objectfs.io/setup-repo-debian.sh | sudo bash`
// would be a documented command that 404s — which is exactly how #138's install.sh spent a year, and
// this one is piped into `sudo bash`.
//
// Walked rather than listed, for the reason TestDocumentedInstallOneLinersNameAServedAddress records:
// its first version was a list of two files and there were five, and the three it missed were the three
// nobody had thought of.
func TestNoPackageRepositoryAddressIsDocumented(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	unserved := []string{
		"objectfs.io/setup-repo-debian.sh",
		"objectfs.io/setup-repo-rhel.sh",
		"objectfs.io/apt",
		"objectfs.io/yum",
	}

	scanned := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "site", ".venv":
				return filepath.SkipDir
			}

			return nil
		}

		switch filepath.Ext(path) {
		case ".md", ".html":
		default:
			return nil
		}

		//nolint:gosec // A path from a WalkDir over the repository root, in a test.
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		// CHANGELOG.md is the one file that has to be able to name an address it no longer recommends:
		// the entries recording that the repositories were built, left unpublished, and then removed
		// cannot say so without naming them. Same exemption, for the same reason, as the installer's
		// gate makes.
		if rel == "CHANGELOG.md" {
			return nil
		}

		scanned++

		for i, line := range strings.Split(string(b), "\n") {
			for _, address := range unserved {
				if !strings.Contains(line, address) {
					continue
				}

				t.Errorf("%s:%d documents %s, which is not served and has no machinery left that could "+
					"serve it:\n  %s\nThe apt and yum repositories were removed after answering 404 for "+
					"their whole life — see the header of this file. Packages come from the release "+
					"page: `apt install ./objectfs_*.deb`, `dnf install ./objectfs-*.rpm`",
					rel, i+1, address, strings.TrimSpace(line))
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	// A floor on the walk rather than on what it found, since what it should find is nothing. An absence
	// test that visits no files passes, and would keep passing after a rename of the directory it was
	// pointed at. 42 files when this was written, CHANGELOG.md excluded — and measured rather than
	// estimated, because a `find` over the same extensions says 1,613 and nearly all of that is markdown
	// under node_modules, dist and site, which the skip list above drops at any depth.
	if scanned < 25 {
		t.Fatalf("scanned %d markdown and HTML files, and there were 42 when this test was written. The "+
			"walk has stopped matching, and an absence check that reads nothing reports a clean tree",
			scanned)
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

// guardBody returns the lines inside the shell block opened by guard, and whether the guard was found.
//
// A scoped read rather than a substring search over the step, because what has to be distinguished is
// one branch from the rest of the step: a step that fails closed and a step that warns and skips differ
// by one line inside that branch, and every other `exit` in the step is unaffected by changing it. Reads
// to the first `fi` at the guard's own indentation, which is enough for the flat
// guard-at-the-top-of-a-step shape both workflows use, and does not try to be a shell parser.
func guardBody(step, guard string) (string, bool) {
	var (
		body    []string
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
			break
		}

		body = append(body, line)
	}

	return strings.Join(body, "\n"), inGuard
}

// namedStep returns the body of a named step, in any workflow.
//
// jobStep in release_packages_test.go does the same thing, and its t.Fatalf names release.yml — which
// is correct there and wrong here. Rather than change a message three other tests depend on, this is
// the same scan with a message that does not claim to know which file it is reading. It was called
// pagesStep when its only caller read pages.yml; that workflow no longer has a step this asks about,
// and a helper named after a file it never reads is the kind of stale name this package keeps fixing.
func namedStep(t *testing.T, workflow, stepName string) string {
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
