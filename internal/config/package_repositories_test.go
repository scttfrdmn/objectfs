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
// One property outlives that deletion: no page may document a repository address, which is not a policy
// statement but a fact about what the site serves.
//
// A second one was here and does not survive, and the reason is worth keeping because it was measured.
// TestTheRPMSigningPathStaysIntact gated the rpm's embedded GPG signature across three files, on the
// premise that "`gpgcheck=1` is dnf's default for a *downloaded file* too — no repository required".
// That premise is false. dnf has two settings, and in a rockylinux:9 container they read:
//
//	gpgcheck = True            # packages coming from a *repository*
//	localpkg_gpgcheck = False  # packages named as a local file  <-- the only path this project has
//
// A deliberately unsigned rpm was then installed with `dnf install ./objectfs-0.14.0-1.aarch64.rpm`: it
// installed, pulled fuse3 in as a Recommends, ran the postinstall scriptlet, reported
// `Signature : (none)` in `rpm -qi`, and answered `objectfs version`. dnf raised nothing. The failure
// that originally motivated the signature — "Error: GPG check FAILED" — came from ci.yml's repo-install
// job installing out of a throwaway-signed *yum repository*, which is the path the first setting governs
// and which no longer exists.
//
// So the signing key, the secret, the workflow steps importing it and that test are all gone, and
// .goreleaser.yml records the measurement where someone would go to add a signature back. What a
// downloaded package's integrity rests on is what the tarballs' rests on: a SHA-256 published beside it
// that scripts/install.sh has no flag to skip, plus a cosign bundle over the checksum list.
// release_signing_test.go is that gate.

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
