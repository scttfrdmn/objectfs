package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

// This file is the durable half of #207 and #137, and the two are one file because they are one
// defect seen from two sides.
//
// #207: scripts/preremove.sh was a working uninstall script that nothing in the repository
// referenced. A grep for "preremove" returned the file itself and nothing else, because only a
// package manager can invoke a pre-removal hook and there was no packaging system in the tree — no
// nfpm.yaml, no debian/, no spec file. `make package` made tarballs, which have no scriptlets.
//
// #137: scripts/postinstall.sh was not idempotent. It ran `mkdir -p` and `chmod 755` over four
// directories on every invocation, and a package scriptlet is invoked on every reconfiguration and
// every upgrade — so an operator who tightened /etc/objectfs got it widened back by the next
// `apt upgrade`, silently.
//
// The tests below are in two groups. The first reads the packaging config against the filesystem and
// against the scripts, so a path can only be wrong in one place at a time. The second *runs* the scripts
// against a scratch root and asserts what they do, because "idempotent" is a claim about behavior
// and a claim about behavior that is only checked by reading the source is not checked.

// packagingFile is the packaging configuration, relative to the module root.
//
// It is `.goreleaser.yml` rather than the `nfpm.yaml` these tests were written against, and the
// section they read is `nfpms:` — goreleaser embeds nfpm, so the schema for a package is nfpm's own,
// one nesting level deeper. That move deleted a second description of the same install layout:
// nfpm.yaml was invoked by a Makefile loop over two architectures and two formats, while the release
// tarballs came from a five-cell matrix written out longhand inside release.yml, and the two agreed
// only by convention. Everything below is unchanged in substance, because the `contents:` schema is
// the same schema.
const packagingFile = ".goreleaser.yml"

// goreleaserConfig is the subset of goreleaser's schema these tests assert on.
//
// A hand-written subset rather than goreleaser's own Config type, deliberately: importing
// github.com/goreleaser/goreleaser/v2 would add a release tool to this module's dependency graph —
// and to every downstream consumer's — to read a handful of fields. The fields named here are the
// ones with a counterpart elsewhere in the repository, which is what these tests are about.
type goreleaserConfig struct {
	Builds   []goreleaserBuild `yaml:"builds"`
	Archives []struct {
		ID           string   `yaml:"id"`
		IDs          []string `yaml:"ids"`
		NameTemplate string   `yaml:"name_template"`
	} `yaml:"archives"`
	Nfpms []nfpmConfig `yaml:"nfpms"`
}

// goreleaserBuild is one entry of `builds:`.
type goreleaserBuild struct {
	ID     string   `yaml:"id"`
	Binary string   `yaml:"binary"`
	Goos   []string `yaml:"goos"`
	Goarch []string `yaml:"goarch"`
	Goarm  []string `yaml:"goarm"`
	// Ignore removes cells from the goos × goarch × goarm cross product. An entry matches a cell when
	// every field it names matches; fields it leaves empty are wildcards, which is goreleaser's rule
	// and not this test's invention.
	Ignore []struct {
		Goos   string `yaml:"goos"`
		Goarch string `yaml:"goarch"`
		Goarm  string `yaml:"goarm"`
	} `yaml:"ignore"`
}

// nfpmConfig is one entry of goreleaser's `nfpms:` list.
type nfpmConfig struct {
	ID string `yaml:"id"`
	// IDs names the builds this package takes its binary from. Which build that is decides what the
	// installed binary is *called*, and getting it wrong is the defect
	// TestThePackagedBinaryIsOnThePathAsObjectfs exists for.
	IDs         []string `yaml:"ids"`
	PackageName string   `yaml:"package_name"`
	Bindir      string   `yaml:"bindir"`
	Formats     []string `yaml:"formats"`
	Contents    []struct {
		Src  string `yaml:"src"`
		Dst  string `yaml:"dst"`
		Type string `yaml:"type"`
	} `yaml:"contents"`
	Scripts struct {
		PreInstall  string `yaml:"preinstall"`
		PostInstall string `yaml:"postinstall"`
		PreRemove   string `yaml:"preremove"`
		PostRemove  string `yaml:"postremove"`
	} `yaml:"scripts"`
}

// readGoreleaser parses .goreleaser.yml.
func readGoreleaser(t *testing.T) goreleaserConfig {
	t.Helper()

	body := readFile(t, filepath.Join(repoRoot(t), packagingFile))

	var cfg goreleaserConfig
	if err := yaml.UnmarshalStrict([]byte(body), &cfg); err != nil {
		// Not UnmarshalStrict's usual meaning here: this struct is a deliberate subset, so an
		// unknown key is expected. yaml.v2's strict mode errors on unknown keys, so the parse is
		// non-strict below and this branch only catches malformed YAML.
		if !strings.Contains(err.Error(), "not found in type") {
			t.Fatalf("%s does not parse as YAML: %v", packagingFile, err)
		}

		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatalf("%s does not parse as YAML: %v", packagingFile, err)
		}
	}

	return cfg
}

// readNfpm returns the one `nfpms:` entry.
//
// Exactly one, asserted rather than assumed. Two entries would be a legitimate way to express this —
// one per format, so each could carry its own `file_name_template` — and every test below reads the
// first, so a second one would be silently unchecked: a package shipping the wrong layout with a
// green suite.
func readNfpm(t *testing.T) nfpmConfig {
	t.Helper()

	cfg := readGoreleaser(t)

	if len(cfg.Nfpms) != 1 {
		t.Fatalf("%s has %d `nfpms:` entries, and these tests read one. Either the file was "+
			"restructured or they stopped reading it, and an absent entry satisfies every assertion "+
			"below", packagingFile, len(cfg.Nfpms))
	}

	nfpm := cfg.Nfpms[0]

	if len(nfpm.Contents) == 0 {
		t.Fatalf("%s's nfpms entry lists no contents. Either the file was restructured or this test "+
			"stopped reading it, and an empty list satisfies every assertion below", packagingFile)
	}

	return nfpm
}

// goreleaserTarget is one compiled cell: a goos, a goarch, and — for 32-bit ARM only — a goarm.
type goreleaserTarget struct {
	Goos   string
	Goarch string
	Goarm  string
}

// Platform is the `goos/goarch` pair, which is how a ci.yml cross-build cell names the same thing.
func (g goreleaserTarget) Platform() string { return g.Goos + "/" + g.Goarch }

// Asset is the platform suffix in a published asset name: the `linux-armv7` of
// `objectfs-linux-armv7.tar.gz`.
//
// This reimplements archiveNameTemplate in Go, which is a real risk — the two could disagree — so
// TestTheArchiveNameTemplateIsTheOneEverythingElseAssumes, in this file and therefore in every run
// that reaches any caller of this method, fails if the config's template is no longer that string.
func (g goreleaserTarget) Asset() string {
	if g.Goarch == "arm" {
		return g.Goos + "-armv" + g.Goarm
	}

	return g.Goos + "-" + g.Goarch
}

// archiveNameTemplate is the name_template Asset models.
//
// Held as a literal so a change to the config is a failure here rather than a silent divergence.
// Every published tarball name and the whole of scripts/install.sh's URL construction follow from it.
const archiveNameTemplate = "objectfs-{{ .Os }}-{{ .Arch }}{{ if .Arm }}v{{ .Arm }}{{ end }}"

// goreleaserTargets expands one build's goos × goarch × goarm cross product, minus its ignores.
//
// This is the authority release_platforms_test.go and install_script_test.go used to read out of
// release.yml's build matrix. That matrix was a five-cell `include:` list carrying `goos`, `goarch`,
// `goarm` and a hand-written `name` per cell, and both files parsed it line-wise. It is gone —
// goreleaser expands the cross product itself — so the platforms a release publishes are now a
// computation over three lists rather than an enumeration, and this function is that computation.
//
// The cost of the change is that the asset name is no longer written down per cell: `name:` was the
// authority precisely so nothing had to re-derive `linux-armv7` from `arm` plus `7`. Asset does
// re-derive it, and the template assertion below is what keeps that honest.
func goreleaserTargets(t *testing.T, buildID string) []goreleaserTarget {
	t.Helper()

	cfg := readGoreleaser(t)

	var build *goreleaserBuild

	for i := range cfg.Builds {
		if cfg.Builds[i].ID == buildID {
			build = &cfg.Builds[i]
		}
	}

	if build == nil {
		t.Fatalf("%s has no build with id %q. These tests read the platforms a release publishes out "+
			"of that build, and a build it cannot find means every platform assertion downstream is "+
			"vacuous rather than failing", packagingFile, buildID)
	}

	if len(build.Goos) == 0 || len(build.Goarch) == 0 {
		t.Fatalf("%s's %q build names no goos or no goarch. goreleaser defaults both to a list this "+
			"function does not model — linux, darwin, windows × amd64, arm64, 386 — so it would report "+
			"platforms the release does not publish", packagingFile, buildID)
	}

	// goreleaser's own default when `goarm:` is absent is ["6"], not the toolchain's. Modeled here
	// rather than treated as "no arm version", because getting it wrong would name the asset
	// objectfs-linux-armv6 and every check against install.sh would fail for the wrong reason.
	goarms := build.Goarm
	if len(goarms) == 0 {
		goarms = []string{"6"}
	}

	var targets []goreleaserTarget

	for _, goos := range build.Goos {
		for _, goarch := range build.Goarch {
			// goarm only exists for 32-bit ARM. Iterating it for amd64 would emit duplicate cells.
			versions := []string{""}
			if goarch == "arm" {
				versions = goarms
			}

			for _, goarm := range versions {
				ignored := false

				for _, ig := range build.Ignore {
					if (ig.Goos == "" || ig.Goos == goos) &&
						(ig.Goarch == "" || ig.Goarch == goarch) &&
						(ig.Goarm == "" || ig.Goarm == goarm) {
						ignored = true
					}
				}

				if !ignored {
					targets = append(targets, goreleaserTarget{Goos: goos, Goarch: goarch, Goarm: goarm})
				}
			}
		}
	}

	return targets
}

// TestTheArchiveNameTemplateIsTheOneEverythingElseAssumes guards the string every asset name comes
// from.
//
// goreleaserTarget.Asset, scripts/install.sh's URL construction, release.yml's per-tarball
// verification loop and the documented install one-liner all encode the same naming convention in
// four different languages. Three of them are checked against each other by other tests in this
// package; this is the one that pins the template itself, because a change to it renames every asset
// on the release page at once and goreleaser's own default — `{{.ProjectName}}_{{.Version}}_{{.Os}}_
// {{.Arch}}` — is a different convention entirely.
//
// The archive's name and the archived binary's name are two independent templates that have to be the
// same string, and this asserts both. `wrap_in_directory: false` means the tarball holds exactly one
// file, and install.sh extracts it and then looks up `objectfs-linux-amd64` by name before renaming it
// to `objectfs` — a rename it does because a user cannot invoke the platform-named binary. So a build
// whose `binary:` drifts from the archive's `name_template` produces a tarball that downloads,
// verifies its checksum and then dies on "does not contain objectfs-linux-amd64". Mutating one of the
// two templates and leaving the other passed every test in this package before this half existed.
func TestTheArchiveNameTemplateIsTheOneEverythingElseAssumes(t *testing.T) {
	t.Parallel()

	cfg := readGoreleaser(t)

	if len(cfg.Archives) != 1 {
		t.Fatalf("%s has %d `archives:` entries, and this package reads one", packagingFile,
			len(cfg.Archives))
	}

	if got := strings.TrimSpace(cfg.Archives[0].NameTemplate); got != archiveNameTemplate {
		t.Errorf("%s's archive name_template is\n\t%s\nand this package models\n\t%s\n\n"+
			"Every published tarball is renamed by that difference. scripts/install.sh builds the "+
			"download URL from `uname` output and has no way to discover the new shape: it fetches a "+
			"404 and reports that the release layout changed. If the rename is deliberate, install.sh, "+
			"release.yml's verification loop, the README one-liner and archiveNameTemplate here all "+
			"move together.", packagingFile, got, archiveNameTemplate)
	}

	var archived *goreleaserBuild

	for i := range cfg.Builds {
		if cfg.Builds[i].ID == "archives" {
			archived = &cfg.Builds[i]
		}
	}

	if archived == nil {
		t.Fatalf("%s has no build with id \"archives\", which is the one the tarballs are built from",
			packagingFile)
	}

	if got := strings.TrimSpace(archived.Binary); got != archiveNameTemplate {
		t.Errorf("%s's \"archives\" build names its binary\n\t%s\nand the archive it goes into is named"+
			"\n\t%s\n\nThose have to be the same string. The tarball holds one file and no directory, and "+
			"scripts/install.sh extracts it and then looks that exact name up before renaming it to "+
			"objectfs — so a release built this way downloads, passes its checksum, and dies on \"does "+
			"not contain objectfs-<platform>\" on every platform at once. goreleaser has no rename step "+
			"between a build and an archive, which is why the two templates exist separately and why "+
			"nothing else notices when they disagree.", packagingFile, got,
			strings.TrimSpace(cfg.Archives[0].NameTemplate))
	}
}

// TestPackagingInvokesBothScriptlets is #207's assertion, stated as a test rather than as a grep.
//
// The issue's own diagnosis was that `grep -r preremove` returned the file and nothing else. This is
// that grep, made permanent and made specific: the packaging must name both scripts, in the fields
// the package managers actually run, and both files must exist.
func TestPackagingInvokesBothScriptlets(t *testing.T) {
	t.Parallel()

	cfg := readNfpm(t)

	for _, s := range []struct {
		field, path, when string
	}{
		{"scripts.postinstall", cfg.Scripts.PostInstall, "deb postinst / rpm %post"},
		{"scripts.preremove", cfg.Scripts.PreRemove, "deb prerm / rpm %preun"},
	} {
		if s.path == "" {
			t.Errorf("%s does not set %s (%s).\nThat is #207 exactly: a maintainer script only a "+
				"package manager can invoke, referenced by nothing, so it never runs. A package that "+
				"installs cleanly and leaves mounted filesystems behind on removal is worse than no "+
				"package.", packagingFile, s.field, s.when)

			continue
		}

		abs := filepath.Join(repoRoot(t), filepath.Clean(s.path))
		info, err := os.Stat(abs)

		if err != nil {
			t.Errorf("%s sets %s: %s, which does not exist. nfpm fails at package time on this, so "+
				"it is caught either way — but it is caught here without needing goreleaser installed.",
				packagingFile, s.field, s.path)

			continue
		}

		if info.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable. dpkg requires the mode bit on a maintainer script and "+
				"refuses to run it otherwise, which surfaces as a package that installs and does "+
				"nothing.", s.path)
		}
	}
}

// TestThePackagedBinaryIsOnThePathAsObjectfs is the gate on a defect that a build log cannot show.
//
// goreleaser puts a build's binary into a package under `bindir` using the name that *build* gives
// it, and it has no rename step between a build and a package. The tarball and the package disagree
// about what that name should be, and both are right: a tarball's binary is named for its platform,
// so install.sh can find it and five downloads can coexist in one directory, while a package's
// binary goes on `PATH` and has to be `objectfs`.
//
// So .goreleaser.yml carries two builds of the same source, and this asserts the `nfpms:` entry
// points at the right one. Naming the wrong one produced `/usr/bin/objectfs-linux-amd64` — measured,
// in a rockylinux:9 container: the rpm built, installed, ran its scriptlet, exited 0, and then
// `objectfs version` answered "command not found". Every other test in this file passed, because
// every `contents:` destination was still correct; the binary is not a `contents:` entry.
//
// The same wrong name would also break the systemd unit (`ExecStart=/usr/bin/objectfs`), both
// modulefiles, and every command in scripts/postinstall.sh's own output.
func TestThePackagedBinaryIsOnThePathAsObjectfs(t *testing.T) {
	t.Parallel()

	cfg := readGoreleaser(t)
	nfpm := readNfpm(t)

	if len(nfpm.IDs) != 1 {
		t.Fatalf("%s's nfpms entry names %d builds in `ids:` (%v), and this test reads one. With no "+
			"`ids:` at all goreleaser packages *every* build, which here means the package would take "+
			"whichever of the two it saw first — and one of them names its binary for the platform.",
			packagingFile, len(nfpm.IDs), nfpm.IDs)
	}

	var binary string
	found := false

	for _, b := range cfg.Builds {
		if b.ID == nfpm.IDs[0] {
			binary = strings.TrimSpace(b.Binary)
			found = true
		}
	}

	if !found {
		t.Fatalf("%s's nfpms entry takes its binary from build %q, and no build has that id. "+
			"goreleaser fails on this, so it is caught either way — but it is caught here without a "+
			"release.", packagingFile, nfpm.IDs[0])
	}

	if binary != "objectfs" {
		t.Errorf("%s packages build %q, whose binary is %q, so the package installs "+
			"/usr/bin/%s.\nThat is not a command anyone types, and nothing fails at package time: the "+
			"package builds, installs, runs its scriptlets and exits 0, and then `objectfs` is not "+
			"found. configs/systemd/objectfs@.service, both modulefiles and scripts/postinstall.sh all "+
			"invoke `objectfs` by that exact name.\nThe platform-named build is for the tarballs, which "+
			"scripts/install.sh renames as it installs.", packagingFile, nfpm.IDs[0], binary, binary)
	}

	// bindir unset means goreleaser's default, /usr/bin, which is what everything above expects. A
	// value is only worth flagging if it is a different directory.
	if nfpm.Bindir != "" && nfpm.Bindir != "/usr/bin" {
		t.Errorf("%s sets bindir: %s. The systemd unit hardcodes /usr/bin/objectfs and the "+
			"modulefiles deliberately do not prepend to PATH because /usr/bin is already on it; "+
			"moving the binary breaks both without failing anything at package time.",
			packagingFile, nfpm.Bindir)
	}
}

// TestPackagedFilesExist checks every src in the packaging against the filesystem.
//
// Every src is a literal repository path now. Under nfpm.yaml one was not: the binary was a
// `contents:` entry whose src was `build/objectfs-linux-${OBJECTFS_ARCH}`, a build artifact that
// could not be stat'ed, so this test reached into the Makefile to confirm `build-linux` wrote that
// exact name. goreleaser places the binary itself from the build it names in `ids:`, so that
// coupling is gone and what replaced it is TestThePackagedBinaryIsOnThePathAsObjectfs.
func TestPackagedFilesExist(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	cfg := readNfpm(t)

	var checked int

	for _, c := range cfg.Contents {
		if c.Type == "dir" {
			if c.Src != "" {
				t.Errorf("%s: a `type: dir` entry for %s also names a src (%s), which nfpm ignores",
					packagingFile, c.Dst, c.Src)
			}

			continue
		}

		if c.Src == "" {
			t.Errorf("%s: the entry for %s has no src", packagingFile, c.Dst)

			continue
		}

		checked++

		// A src is a path, not a template. goreleaser expands `{{ }}` on both sides of an entry, and
		// two destinations use it — the modulefiles put the version in the filename — but a templated
		// *src* would mean the packaging reads a file whose name depends on the release, which nothing
		// here does and which this loop could not check.
		if strings.Contains(c.Src, "{{") {
			t.Errorf("%s: src %s is a template. Every source is a file in the repository, so this "+
				"test can stat it; a templated src is a path that only exists at release time.",
				packagingFile, c.Src)

			continue
		}

		if _, err := os.Stat(filepath.Join(root, filepath.Clean(c.Src))); err != nil {
			t.Errorf("%s packages %s → %s, and that source file does not exist in the repository",
				packagingFile, c.Src, c.Dst)
		}
	}

	if checked == 0 {
		t.Fatalf("%s has no file entries at all — only directories. A package that installs no "+
			"files is not the thing #207 asks for", packagingFile)
	}
}

// TestPackageVersionComesFromTheVersionConstant pins CLAUDE.md's single-authority rule into the
// packaging.
//
// A literal here would be the sixth copy of a number this repository once gave five different
// answers to, and the worst-placed one: nothing reads a package's metadata back to compare it, so
// `objectfs version` inside objectfs_0.12.0_amd64.deb could report 0.13.0 and no gate would notice.
//
// The chain is one hop shorter than it was. nfpm.yaml took its version from `${OBJECTFS_VERSION}` and
// the Makefile sed'd the constant out of main.go into that variable — so this test asserted both
// halves, since either alone is satisfiable while being wrong. goreleaser derives the version from
// the git tag instead, and there is no variable and no Makefile step in between. What is left to
// check is that nothing reintroduces a second authority:
//
//   - the packaging config must not name a version at all, and
//   - release.yml must compare the tag against the constant, because that comparison is now the
//     *only* thing tying the tag goreleaser reads to the number the binary prints.
func TestPackageVersionComesFromTheVersionConstant(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	//nolint:gosec // a path built from the module root this test located
	mainGo, err := os.ReadFile(filepath.Join(root, "cmd", "objectfs", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/objectfs/main.go: %v", err)
	}

	m := versionConstant.FindSubmatch(mainGo)
	if m == nil {
		t.Fatal("cmd/objectfs/main.go has no `version = \"...\"` constant, which is the authority " +
			"every check below is relative to")
	}

	declared := string(m[1])

	// A literal version in the packaging config, comments excluded. The comments are full of example
	// filenames — `objectfs_0.14.0-1_amd64.deb` — which are illustration and go stale harmlessly; a
	// version in a *value* is read by goreleaser and overrides the tag.
	// withoutComments is release_packages_test.go's, written for the same reason against a workflow
	// rather than against this config.
	body := withoutComments(readFile(t, filepath.Join(root, packagingFile)))

	if strings.Contains(body, declared) {
		t.Errorf("%s contains the literal version %s outside a comment. The authority is the "+
			"`version` constant in cmd/objectfs/main.go, and goreleaser takes the package version from "+
			"the git tag — a third copy here has no way to be told it is stale, which is how this "+
			"repository came to give five different answers at once.", packagingFile, declared)
	}

	// And no attempt to inject it at link time. `version` in main.go is an untyped **constant**, so
	// `-X main.version=...` is silently a no-op: the linker cannot rewrite a constant, and every
	// release before #375 passed that flag and shipped a binary reporting the hardcoded value anyway.
	// A green build plus a wrong `objectfs version` is the exact shape of failure this file exists for.
	if strings.Contains(body, "-X main.version") ||
		strings.Contains(body, "-X 'main.version") {
		t.Errorf("%s passes -X main.version in ldflags. `version` in cmd/objectfs/main.go is a "+
			"constant, so the linker cannot rewrite it — the flag is accepted, the build is green, and "+
			"the binary reports the hardcoded value. Whoever added it will believe the version is "+
			"injected. #375 removed exactly this.", packagingFile)
	}

	// The one remaining link, in the workflow that publishes. Asserted here rather than in
	// release_packages_test.go because it is this file's invariant: with the Makefile out of the chain,
	// a tag that disagrees with the constant is a release whose assets are all named for a version the
	// binary inside them denies.
	release := readFile(t, filepath.Join(root, ".github", "workflows", "release.yml"))

	if !strings.Contains(release, "cmd/objectfs/main.go") {
		t.Error("release.yml never reads cmd/objectfs/main.go. goreleaser names every asset, and " +
			"stamps every package's metadata, from the git tag — nothing checks that tag against the " +
			"constant the binary actually prints, so `objectfs version` inside " +
			"objectfs_0.15.0-1_amd64.deb can say 0.14.0 with every job green.")
	}
}

// TestPackagingAndPostinstallAgreeOnTheExampleConfigPath is the seam most likely to break silently.
//
// The packaging installs configs/example.yaml to a path under /usr/share, and postinstall.sh copies it
// from that path to /etc/objectfs/config.yaml if and only if the target does not already exist. If
// the two paths disagree, the package still installs, the scriptlet still exits 0 — postinstall.sh
// exits 0 unconditionally by design — and the operator gets no starting configuration, with one
// warning buried in apt's output.
func TestPackagingAndPostinstallAgreeOnTheExampleConfigPath(t *testing.T) {
	t.Parallel()

	cfg := readNfpm(t)
	script := readFile(t, filepath.Join(repoRoot(t), "scripts", "postinstall.sh"))

	var packaged []string

	for _, c := range cfg.Contents {
		if c.Type == "dir" {
			continue
		}

		if strings.HasSuffix(c.Src, "configs/example.yaml") {
			packaged = append(packaged, c.Dst)
		}
	}

	if len(packaged) != 1 {
		t.Fatalf("%s installs configs/example.yaml to %d destinations (%v); expected exactly one, "+
			"which is the one postinstall.sh copies from", packagingFile, len(packaged), packaged)
	}

	if !strings.Contains(script, packaged[0]) {
		t.Errorf("%s installs the example config to %s, and scripts/postinstall.sh does not mention "+
			"that path — so it copies nothing and /etc/objectfs/config.yaml is never created.\n"+
			"Both files have to name the same path. The scriptlet exits 0 either way by design, so "+
			"this failure is invisible at install time.", packagingFile, packaged[0])
	}

	// And the file that gets copied has to be one the loader accepts. TestShippedConfigsLoadAndValidate
	// in shipped_test.go already covers configs/*.yaml, which is why this only asserts the coupling.
	if _, err := os.Stat(filepath.Join(repoRoot(t), "configs", "example.yaml")); err != nil {
		t.Errorf("configs/example.yaml, the file the package ships as a starting configuration, does "+
			"not exist: %v", err)
	}
}

// TestPackagingDoesNotShipConfigFilesUnderEtc is a dpkg-conffile check.
//
// A packaged file under /etc is a conffile: dpkg records its checksum and, on upgrade, *prompts* the
// operator to choose between their edits and the package's version. An interactive prompt in the
// middle of an unattended `apt upgrade` is a hung machine. The copy-if-absent pattern in
// postinstall.sh exists to avoid exactly this, and shipping the file directly would silently undo it.
func TestPackagingDoesNotShipConfigFilesUnderEtc(t *testing.T) {
	t.Parallel()

	for _, c := range readNfpm(t).Contents {
		if c.Type == "dir" || !strings.HasPrefix(c.Dst, "/etc/") {
			continue
		}

		t.Errorf("%s ships a file to %s. A packaged file under /etc is a dpkg conffile, so an upgrade "+
			"prompts the operator to merge it — which hangs an unattended apt run. scripts/postinstall.sh "+
			"copies from /usr/share instead, only when the target is absent, which leaves the operator "+
			"owning the file they put settings into.", packagingFile, c.Dst)
	}
}

// TestPackagingShipsTheSystemdUnitUnderUsr checks where the unit lands.
//
// /usr/lib/systemd/system is the packager's directory and /etc/systemd/system is the operator's;
// systemd gives /etc precedence. Shipping to /etc means a local override has nowhere to go that wins,
// and it makes the unit a conffile as well.
func TestPackagingShipsTheSystemdUnitUnderUsr(t *testing.T) {
	t.Parallel()

	var found string

	for _, c := range readNfpm(t).Contents {
		if strings.HasSuffix(c.Src, "objectfs@.service") {
			found = c.Dst
		}
	}

	if found == "" {
		t.Fatalf("%s does not package configs/systemd/objectfs@.service. Without it, `systemctl "+
			"enable objectfs@name` fails after a clean install and every instruction in the docs that "+
			"starts with systemctl is wrong.", packagingFile)
	}

	if !strings.HasPrefix(found, "/usr/lib/systemd/system/") {
		t.Errorf("%s installs the systemd unit to %s. A package's units belong in "+
			"/usr/lib/systemd/system; /etc/systemd/system is where an operator's override goes, and "+
			"systemd gives that precedence — a package occupying the path leaves an override nowhere "+
			"to win from.", packagingFile, found)
	}
}

// versionTemplate is the reference the two modulefile destinations put in their filename.
//
// goreleaser's template, not nfpm's `${OBJECTFS_VERSION}`: goreleaser renders `contents:` through its
// own template engine before handing the config to nfpm, and its version comes from the git tag.
const versionTemplate = "{{ .Version }}"

// TestPackagingShipsTheModulefilesWhereTheModuleSystemsLookForThem checks the two entries whose dst
// is a computed path, which is the only place in the packaging where that is true.
//
// Every other rule has a fixed destination; these two put the version in the *filename*, because that
// is how Lmod and TCL Modules decide what `module load objectfs/<version>` means. MODULEPATH names a
// directory, the directory below it is the module name, and the file inside that is the version.
// Three things can go wrong here and none of them fail at package time:
//
//   - **The version written as a literal.** It installs successfully, and it is right for exactly one
//     release. Under nfpm this failure had a different shape — an entry that omitted `expand: true`
//     shipped a file literally named `${OBJECTFS_VERSION}.lua`, and `module avail` listed a version
//     nobody could load. goreleaser expands `{{ }}` in every dst unconditionally, so what is left to
//     get wrong is hardcoding.
//   - **The version moved into a directory.** objectfs/<version>/objectfs.lua adds a third level, and
//     Lmod then reads the *version* as the name.
//   - **The TCL file keeps its extension.** Installed as `0.13.0.tcl`, the version is reported as
//     "0.13.0.tcl", because for TCL Modules the filename *is* the version. Lmod is the exception —
//     it needs .lua to parse the file as Lua at all, and strips it before reporting. So the two
//     formats must disagree on the extension, which looks like a mistake and is not.
//
// The modulefiles read their own version back out of the install path (Lmod through
// myModuleVersion(), TCL through `file tail $ModulesCurrentModulefile`), so a wrong dst is not
// cosmetic: it is `module load objectfs` exporting the wrong OBJECTFS_VERSION, which is the single
// authority rule failing at the last hop. modulefiles_test.go covers everything decidable by reading
// or running the files; the install path is the part only the packaging can get right.
func TestPackagingShipsTheModulefilesWhereTheModuleSystemsLookForThem(t *testing.T) {
	t.Parallel()

	cfg := readNfpm(t)

	for _, want := range []struct {
		src string
		// dst is the required destination, with the version reference substituted in.
		dst string
		// why is appended to a failure, naming what the module system does with a wrong path.
		why string
	}{
		{
			src: "configs/modules/objectfs.lua",
			dst: "/usr/share/modulefiles/objectfs/" + versionTemplate + ".lua",
			why: "Lmod requires the .lua extension to parse the file as Lua, and strips it before " +
				"reporting the version — so this one, and only this one, keeps its extension.",
		},
		{
			src: "configs/modules/objectfs.tcl",
			dst: "/usr/share/modulefiles/objectfs/" + versionTemplate,
			why: "For TCL Modules the filename is the version string, so a .tcl suffix here makes " +
				"`module avail` report a version called \"" + versionTemplate + ".tcl\".",
		},
	} {
		var found []string

		for _, c := range cfg.Contents {
			if c.Type == "dir" || !strings.HasSuffix(c.Src, want.src) {
				continue
			}

			found = append(found, c.Dst)

			if !strings.Contains(c.Dst, "{{") {
				t.Errorf("%s installs %s to %s, with no version template in the path. That is correct "+
					"for one release and wrong for every release after it, and nothing fails: the "+
					"package installs, `module avail` lists a version, and it is the wrong number.",
					packagingFile, want.src, c.Dst)
			}
		}

		if len(found) != 1 {
			t.Errorf("%s installs %s to %d destinations (%v); expected exactly one, %s.\nWithout it, a "+
				"site that installs the package still has to fetch the modulefile out of a source "+
				"checkout, which is what #145 exists to stop.",
				packagingFile, want.src, len(found), found, want.dst)

			continue
		}

		if found[0] != want.dst {
			t.Errorf("%s installs %s to %s, want %s.\n%s\nThe module systems read the version out of "+
				"this path, so the wrong one means `module load objectfs` exports the wrong "+
				"OBJECTFS_VERSION — with no error anywhere.",
				packagingFile, want.src, found[0], want.dst, want.why)
		}
	}
}

// TestMakefileBuildsPackages asserts a target exists that produces both formats.
//
// #207 notes that `make package` only makes tarballs, and it still does — a tarball is the right
// artifact for a release download. The deb and the rpm need their own target, and it needs to build
// both formats, because the entire argument for one packaging config over a debian/ directory plus a
// .spec is that one config describes both.
//
// The reason a Makefile target matters more than it looks: it is the only way to build a package
// without pushing a tag. `package-linux` is what ci.yml's `packaging` job runs on every pull request,
// so the release path and the pull-request path invoke the same command against the same config —
// which is what makes a green PR evidence about a release.
func TestMakefileBuildsPackages(t *testing.T) {
	t.Parallel()

	makefile := readFile(t, filepath.Join(repoRoot(t), "Makefile"))

	if !strings.Contains(makefile, "package-linux:") {
		t.Fatal("the Makefile has no package-linux target. `make package` builds tarballs, which " +
			"carry no maintainer scripts — so scripts/preremove.sh still has nothing that can invoke " +
			"it, which is #207 unresolved.")
	}

	// goreleaser by name, not the config filename: it finds .goreleaser.yml itself, so a target that
	// referenced the file by name would be referencing it in a comment. The nfpm loop this replaced
	// passed `--config nfpm.yaml` explicitly, which is why this used to be a filename check.
	if !strings.Contains(makefile, "goreleaser") {
		t.Errorf("the Makefile's package-linux target does not invoke goreleaser, which is what "+
			"reads %s. Whatever it runs instead is a second packaging path, and the one CI exercises "+
			"on a pull request is then not the one a release uses.", packagingFile)
	}

	// Both formats, read out of the config rather than grepped for in the Makefile. The old assertion
	// looked for the strings "deb" and "rpm" anywhere in the Makefile, which the word "debug" satisfies.
	formats := readNfpm(t).Formats

	for _, format := range []string{"deb", "rpm"} {
		found := false

		for _, f := range formats {
			if f == format {
				found = true
			}
		}

		if !found {
			t.Errorf("%s does not build %s — its formats are %v. Both come from one config; building "+
				"only one of them is half the deliverable.", packagingFile, format, formats)
		}
	}
}

// ------------------------------------------------------------------------------------------------
// The behavioral half: the scripts are run, not read.
// ------------------------------------------------------------------------------------------------

// scriptRun is the outcome of one invocation.
type scriptRun struct {
	exit   int
	stdout string
	stderr string
}

// runScript runs a maintainer script against a scratch root.
//
// OBJECTFS_ROOT is the seam. Both scripts prefix every path they touch with it, and it is read from
// the environment rather than hardcoded to "" precisely so that this test exercises the file the
// package ships instead of a copy with the paths rewritten — the failure mode where a test agrees
// with itself and the shipped artifact is never checked.
//
// Env is set explicitly rather than inherited. An ambient OBJECTFS_ROOT, or the caller's PATH
// pointing at an objectfs binary, would change what these runs do.
func runScript(t *testing.T, name, root string, args []string, extraEnv ...string) scriptRun {
	t.Helper()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash is not on PATH: %v", err)
	}

	script := filepath.Join(repoRoot(t), "scripts", name)

	//nolint:gosec // a script path built from the module root this test located
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{script}, args...)...)
	cmd.Dir = root
	cmd.Env = append([]string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"OBJECTFS_ROOT=" + root,
	}, extraEnv...)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	run := scriptRun{stdout: stdout.String(), stderr: stderr.String()}

	if err != nil {
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) {
			t.Fatalf("running scripts/%s: %v\nstdout:\n%s\nstderr:\n%s", name, err, run.stdout, run.stderr)
		}

		run.exit = exitErr.ExitCode()
	}

	return run
}

// stagedRoot builds a scratch filesystem holding what the package would have installed by the time
// the postinstall scriptlet runs: the example config under /usr/share.
func stagedRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	// 0755 and 0644 are the modes the packaging gives these two paths, and reproducing them is the
	// whole point of the fixture — a scratch root at a mode the package would never produce tests
	// the script against a system that cannot exist. gosec reads any 0644 write as a finding
	// (G301/G306) without a way to know the file is a copy of /usr/share/objectfs/configs/
	// example.yaml, which is world-readable by design: it is the example, and the secret-bearing
	// file is the 0600 /etc/objectfs/config.yaml the script derives from it.
	dir := filepath.Join(root, "usr", "share", "objectfs", "configs")
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- /usr/share must be traversable; matches the packaging
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	src := readFile(t, filepath.Join(repoRoot(t), "configs", "example.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "example.yaml"), []byte(src), 0o644); err != nil { // #nosec G306 -- the shipped example is world-readable; matches the packaging
		t.Fatalf("stage example.yaml: %v", err)
	}

	return root
}

// treeState records the mode of every path under root, so two runs can be compared.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()

	state := make(map[string]string)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}

		size := "dir"
		if !d.IsDir() {
			size = fmt.Sprintf("%d bytes", info.Size())
		}

		state[rel] = fmt.Sprintf("mode=%04o %s", info.Mode().Perm(), size)

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	return state
}

// TestPostinstallIsIdempotent is #137's first acceptance criterion, run rather than reasoned about.
//
// "Running postinstall twice produces identical filesystem state as running once." Every path's mode
// and size is compared, not just its existence, because the defect this replaces was a mode change:
// the old script chmod'd four directories unconditionally on every invocation.
func TestPostinstallIsIdempotent(t *testing.T) {
	t.Parallel()

	root := stagedRoot(t)

	first := runScript(t, "postinstall.sh", root, []string{"configure"})
	if first.exit != 0 {
		t.Fatalf("first run exited %d, want 0\nstdout:\n%s\nstderr:\n%s", first.exit, first.stdout, first.stderr)
	}

	before := treeState(t, root)

	second := runScript(t, "postinstall.sh", root, []string{"configure"})
	if second.exit != 0 {
		t.Fatalf("second run exited %d, want 0\nstdout:\n%s\nstderr:\n%s", second.exit, second.stdout, second.stderr)
	}

	after := treeState(t, root)

	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s existed after one run and is gone after two", path)

			continue
		}

		if got != want {
			t.Errorf("%s changed between the first and second run: %s → %s\nA package scriptlet runs "+
				"on every reconfiguration and every upgrade, so anything that is not identical on the "+
				"second run is something an `apt upgrade` does to an operator's system unasked.",
				path, want, got)
		}
	}

	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s appeared only on the second run", path)
		}
	}
}

// TestPostinstallLeavesTightenedPermissionsAlone is the mutation of the defect itself.
//
// This is the case idempotency-by-comparison above cannot see: two runs from a clean start agree
// with each other while both being wrong, if the script chmods unconditionally. So the mode is
// tightened *between* the runs — which is what an operator does — and the assertion is that the
// second run does not undo it.
//
// Verified by mutation: replacing ensure_dir's early return with an unconditional `chmod "$mode"`
// (the previous script's behavior) fails this test on /etc/objectfs, 0700 → 0755.
func TestPostinstallLeavesTightenedPermissionsAlone(t *testing.T) {
	t.Parallel()

	root := stagedRoot(t)

	if run := runScript(t, "postinstall.sh", root, []string{"configure"}); run.exit != 0 {
		t.Fatalf("first run exited %d\nstderr:\n%s", run.exit, run.stderr)
	}

	// 0700 is the operator's choice being simulated, and it is *tighter* than the 0755 the package
	// ships — which is what makes it the interesting case. gosec's G302 wants 0600 or less on a
	// chmod and cannot distinguish a directory (where 0600 would remove the traverse bit and make
	// the directory's contents unreachable) from a file.
	etc := filepath.Join(root, "etc", "objectfs")
	if err := os.Chmod(etc, 0o700); err != nil { // #nosec G302 -- a directory needs its execute bit; 0700 is the tightening under test
		t.Fatalf("chmod %s: %v", etc, err)
	}

	if run := runScript(t, "postinstall.sh", root, []string{"configure"}); run.exit != 0 {
		t.Fatalf("second run exited %d\nstderr:\n%s", run.exit, run.stderr)
	}

	info, err := os.Stat(etc)
	if err != nil {
		t.Fatalf("stat %s: %v", etc, err)
	}

	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("/etc/objectfs was 0700 before the second run and is %04o after it.\n"+
			"The scriptlet reset a mode an operator chose. This is not a hypothetical: the version of "+
			"this script before #137 ran `chmod 755` over four directories on every invocation, and "+
			"dpkg runs postinst on every reconfiguration — so the widening happened on each upgrade, "+
			"with no output and no record.", got)
	}
}

// TestPostinstallDoesNotOverwriteAnExistingConfig is the other half of the same rule, on the file
// that matters most.
//
// /etc/objectfs/config.yaml is where an operator puts their region, their cache sizing, and possibly
// a credential. An upgrade that replaced it would be data loss noticed only at the next mount.
func TestPostinstallDoesNotOverwriteAnExistingConfig(t *testing.T) {
	t.Parallel()

	root := stagedRoot(t)

	if run := runScript(t, "postinstall.sh", root, []string{"configure"}); run.exit != 0 {
		t.Fatalf("first run exited %d\nstderr:\n%s", run.exit, run.stderr)
	}

	target := filepath.Join(root, "etc", "objectfs", "config.yaml")

	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the first run did not create /etc/objectfs/config.yaml, which is the one file this "+
			"scriptlet exists to install: %v", err)
	}

	const edited = "# an operator's configuration\nglobal:\n  log_level: DEBUG\n"

	if err := os.WriteFile(target, []byte(edited), 0o600); err != nil {
		t.Fatalf("write %s: %v", target, err)
	}

	if run := runScript(t, "postinstall.sh", root, []string{"configure"}); run.exit != 0 {
		t.Fatalf("second run exited %d\nstderr:\n%s", run.exit, run.stderr)
	}

	got := readFile(t, target)
	if got != edited {
		t.Errorf("the second run replaced /etc/objectfs/config.yaml.\nwant:\n%s\ngot:\n%s\n"+
			"This is the operator's file. The package ships its copy to /usr/share and copies it here "+
			"only when nothing is here.", edited, got)
	}
}

// TestPostinstallExitsZeroWithoutFUSE is #137's third acceptance criterion.
//
// "Script exits 0 whether FUSE is present or not." The scratch root has no /etc/fuse.conf and the
// PATH runScript sets has no fusermount3 on the test machine, which is the build-server case.
//
// The exit status is what dpkg reads: a non-zero postinst leaves the package half-configured, which
// blocks every subsequent apt operation until someone runs `dpkg --configure -a`. None of these
// checks is worth that. A machine with no FUSE can install objectfs, run `objectfs version`, and
// mount as root — and the mount command is where a missing prerequisite should be reported, with the
// mount point in hand.
func TestPostinstallExitsZeroWithoutFUSE(t *testing.T) {
	t.Parallel()

	root := stagedRoot(t)

	run := runScript(t, "postinstall.sh", root, []string{"configure"})

	if run.exit != 0 {
		t.Errorf("postinstall.sh exited %d on a root with no /etc/fuse.conf.\nstdout:\n%s\nstderr:\n%s",
			run.exit, run.stdout, run.stderr)
	}

	if !strings.Contains(run.stderr, "user_allow_other") {
		t.Errorf("no warning about user_allow_other. That is #137's second acceptance criterion: "+
			"without it, `allow_other` is refused for non-root callers, the mount still succeeds, and "+
			"every other user on the machine gets EACCES on the mount point with nothing in any log to "+
			"say why.\nstderr:\n%s", run.stderr)
	}

	if !strings.Contains(run.stderr, "echo user_allow_other >> /etc/fuse.conf") {
		t.Errorf("the warning does not carry the exact command that fixes it. The operator reading it "+
			"is mid-install and will not go looking for documentation.\nstderr:\n%s", run.stderr)
	}
}

// TestPostinstallWarningsGoToStderr is #137's explicit requirement, and it is the one a reader is
// most likely to think is cosmetic.
//
// It is not. A scriptlet's stdout is interleaved into apt's and dnf's own progress output, where a
// multi-line remediation block is indistinguishable from noise. stderr is what `2>` and a CI log
// scraper can separate — and the uninstall CI job #207 proposes needs to assert on these.
func TestPostinstallWarningsGoToStderr(t *testing.T) {
	t.Parallel()

	root := stagedRoot(t)
	run := runScript(t, "postinstall.sh", root, []string{"configure"})

	for _, marker := range []string{"WARNING", "user_allow_other"} {
		if strings.Contains(run.stdout, marker) {
			t.Errorf("%q appears on stdout:\n%s\nWarnings go to stderr — #137 is explicit, and the "+
				"reason is that a scriptlet's stdout is interleaved into the package manager's progress "+
				"output.", marker, run.stdout)
		}
	}
}

// TestPostinstallDetectsACommentedOutUserAllowOther is the check that would otherwise pass on
// exactly the systems it exists for.
//
// Every distribution ships /etc/fuse.conf with `#user_allow_other` commented out. #137's proposed
// implementation is `grep -q "^user_allow_other"`, which is right — but an unanchored grep, which is
// the natural thing to write, matches the comment. So the anchoring gets a test of its own, with the
// commented form as the input.
func TestPostinstallDetectsACommentedOutUserAllowOther(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fuseConf string
		wantWarn bool
	}{
		{
			name:     "commented out, as every distribution ships it",
			fuseConf: "# mount_max = 1000\n#user_allow_other\n",
			wantWarn: true,
		},
		{
			name:     "enabled",
			fuseConf: "# mount_max = 1000\nuser_allow_other\n",
			wantWarn: false,
		},
		{
			name:     "enabled with leading whitespace, which libfuse accepts",
			fuseConf: "  user_allow_other\n",
			wantWarn: false,
		},
		{
			name:     "a longer directive that merely starts with the same letters",
			fuseConf: "user_allow_other_thing\n",
			wantWarn: true,
		},
		{
			name:     "present but empty",
			fuseConf: "\n",
			wantWarn: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := stagedRoot(t)

			// The real /etc is 0755 and the real /etc/fuse.conf is 0644 — fusermount3 reads it as
			// the invoking unprivileged user, so it has to be world-readable. Writing it at 0600
			// here to satisfy gosec would make the fixture a file the system never has.
			if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil { // #nosec G301 -- /etc's real mode
				t.Fatalf("mkdir etc: %v", err)
			}

			conf := filepath.Join(root, "etc", "fuse.conf")
			if err := os.WriteFile(conf, []byte(tc.fuseConf), 0o644); err != nil { // #nosec G306 -- /etc/fuse.conf's real mode; fusermount3 reads it unprivileged
				t.Fatalf("write fuse.conf: %v", err)
			}

			run := runScript(t, "postinstall.sh", root, []string{"configure"})

			if run.exit != 0 {
				t.Fatalf("exited %d\nstderr:\n%s", run.exit, run.stderr)
			}

			gotWarn := strings.Contains(run.stderr, "does not enable user_allow_other")

			if gotWarn != tc.wantWarn {
				t.Errorf("warned=%v, want %v for /etc/fuse.conf:\n%q\nstderr:\n%s",
					gotWarn, tc.wantWarn, tc.fuseConf, run.stderr)
			}
		})
	}
}

// TestPreremoveLeavesMountsAloneOnUpgrade is the defect this rewrite found in the old preremove.sh.
//
// dpkg runs prerm with "upgrade <version>" when replacing a package, and rpm runs %preun with an
// instance count of 1 for the outgoing package of an upgrade. The previous script ignored its
// argument entirely, so `apt upgrade objectfs` stopped every objectfs@ unit and unmounted every
// filesystem on the machine — and nothing brought them back, because the incoming package's postinst
// does not start units and correctly cannot know which were running.
//
// An upgrade replaces a binary. Running mount processes keep the old inode until restarted, which is
// the ordinary story for every daemon on the system.
func TestPreremoveLeavesMountsAloneOnUpgrade(t *testing.T) {
	t.Parallel()

	// dpkg's spellings and rpm's instance count, which is 1 while an upgrade's old package is being
	// erased.
	for _, action := range []string{"upgrade", "failed-upgrade", "deconfigure", "1"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			root := rootWithMounts(t, "objectfs /mnt/objectfs/data fuse.s3 rw,nosuid,nodev 0 0\n")

			run := runScript(t, "preremove.sh", root, []string{action},
				"OBJECTFS_PREREMOVE_UNMOUNT_FAILS=1")

			if run.exit != 0 {
				t.Errorf("exited %d on action %q, which is an upgrade rather than a removal\n"+
					"stdout:\n%s\nstderr:\n%s", run.exit, action, run.stdout, run.stderr)
			}

			if !strings.Contains(run.stdout, "leaving running mounts") {
				t.Errorf("did not say it was leaving mounts alone. An upgrade that tears down every "+
					"mount on the machine turns a package update into an unannounced outage.\n"+
					"stdout:\n%s", run.stdout)
			}

			if strings.Contains(run.stdout, "unmounting") {
				t.Errorf("attempted an unmount during action %q\nstdout:\n%s", action, run.stdout)
			}
		})
	}
}

// TestPreremoveFailsWhenAMountSurvives is the decision #207 asks for, made explicitly.
//
// The issue: "the unmount loop should be checked for the case where a mount is busy — failing removal
// is better than reporting success while a mount survives." That is the choice made here, and the
// reason is what the machine looks like afterwards. The package's binary is deleted at removal, so a
// FUSE mount that outlives it has no server: every read hangs or returns EIO, `ls` on the mount point
// blocks in the kernel, and the only way out is a manual `fusermount -u` by someone who first has to
// work out that is what happened. A refusal names the mount and the process holding it; a success
// leaves no trace of the cause.
//
// What each package manager does with the non-zero status differs, and is documented in the script's
// header: dpkg aborts the removal and the package stays installed; rpm reports the scriptlet failure
// and erases anyway, because %preun is not a veto. So this is a refusal on deb and a loud error on
// rpm — worth stating, since "failing removal" is only literally available on one of the two.
func TestPreremoveFailsWhenAMountSurvives(t *testing.T) {
	t.Parallel()

	root := rootWithMounts(t, "objectfs /mnt/objectfs/data fuse.s3 rw,nosuid,nodev 0 0\n")

	run := runScript(t, "preremove.sh", root, []string{"remove"},
		"OBJECTFS_PREREMOVE_UNMOUNT_FAILS=1")

	if run.exit == 0 {
		t.Errorf("exited 0 with a mount still present.\nstdout:\n%s\nstderr:\n%s\n"+
			"#207 is specific about this: failing removal beats reporting success while a mount "+
			"survives, because the package's binary is about to be deleted and a FUSE mount whose "+
			"server is gone hangs every read against it.", run.stdout, run.stderr)
	}

	if !strings.Contains(run.stderr, "/mnt/objectfs/data") {
		t.Errorf("the failure does not name the surviving mount point.\nstderr:\n%s", run.stderr)
	}

	if !strings.Contains(run.stderr, "lsof +D /mnt/objectfs/data") {
		t.Errorf("the failure does not print the command that identifies what is holding the mount "+
			"open. That is the whole advantage of refusing over succeeding — the operator gets the "+
			"cause, not just the symptom.\nstderr:\n%s", run.stderr)
	}

	// The preserved-data notice has to print even on the failure path, because the operator retrying
	// the removal should not have to wonder whether the first attempt deleted their configuration.
	if !strings.Contains(run.stdout, "/etc/objectfs/") {
		t.Errorf("the run did not say which directories are preserved.\nstdout:\n%s", run.stdout)
	}
}

// TestPreremoveSucceedsWithNothingMounted is the ordinary case, and it is the one that must not
// regress into a failure — a removal that refuses on a machine with no mounts is a package that
// cannot be uninstalled.
func TestPreremoveSucceedsWithNothingMounted(t *testing.T) {
	t.Parallel()

	root := rootWithMounts(t, "proc /proc proc rw 0 0\ntmpfs /run tmpfs rw 0 0\n")

	run := runScript(t, "preremove.sh", root, []string{"remove"})

	if run.exit != 0 {
		t.Errorf("exited %d with nothing mounted\nstdout:\n%s\nstderr:\n%s",
			run.exit, run.stdout, run.stderr)
	}

	if strings.Contains(run.stdout, "unmounting") {
		t.Errorf("attempted an unmount with no ObjectFS filesystem in the mount table\nstdout:\n%s",
			run.stdout)
	}
}

// TestPreremoveRecognisesTheMountTypeObjectFSActuallyReports is the second defect this rewrite found,
// and it is the same shape as #207 itself: a correct-looking mechanism with nothing reaching it.
//
// The old script's unmount loop keyed on `type fuse.objectfs`. ObjectFS mounts do not report that.
// internal/adapter and internal/fuse both set Subtype "s3" alongside FSName "objectfs", and go-fuse
// passes the subtype as the filesystem type — so the kernel records `fuse.s3`, with `objectfs` as the
// device. This repository had already found the same assumption in the JavaScript SDK's isMounted and
// fixed it there; the shell script kept it.
//
// So the unmount loop #207 describes as "a working uninstall script" had never matched a real mount.
// Table-driven over the forms that do and do not count, because the union has to be wide enough to
// catch the real one and narrow enough to leave every other FUSE filesystem alone — an over-broad
// match here would unmount a user's sshfs during an objectfs removal.
func TestPreremoveRecognisesTheMountTypeObjectFSActuallyReports(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		line  string
		match bool
	}{
		{
			name:  "what ObjectFS actually reports: Subtype s3, FSName objectfs",
			line:  "objectfs /mnt/objectfs/data fuse.s3 rw,nosuid,nodev 0 0",
			match: true,
		},
		{
			name:  "fuse.objectfs, which the old script keyed on and which no mount produces today",
			line:  "objectfs /mnt/objectfs/data fuse.objectfs rw 0 0",
			match: true,
		},
		{
			name:  "some other fuse subtype, with objectfs as the device",
			line:  "objectfs /mnt/objectfs/data fuse.whatever rw 0 0",
			match: true,
		},
		{
			name:  "another user's sshfs, which a removal of objectfs must not touch",
			line:  "user@host:/ /home/u/remote fuse.sshfs rw 0 0",
			match: false,
		},
		{
			name:  "a fuse.s3 mount from a different tool — s3fs names itself as the device",
			line:  "s3fs /mnt/other fuse.s3fs rw 0 0",
			match: false,
		},
		{
			name:  "an ordinary filesystem whose device string contains objectfs",
			line:  "/dev/objectfs-vg/data /srv/data ext4 rw 0 0",
			match: false,
		},
		{
			name:  "a mount point with an escaped space, which /proc/mounts writes as \\040",
			line:  `objectfs /mnt/my\040bucket fuse.s3 rw 0 0`,
			match: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := rootWithMounts(t, tc.line+"\n")

			run := runScript(t, "preremove.sh", root, []string{"remove"},
				"OBJECTFS_PREREMOVE_UNMOUNT_FAILS=1")

			// A selected mount is one the script tried to unmount, and under a test root the
			// unmount is stubbed to fail — so selection shows up as a non-zero exit naming the path.
			selected := run.exit != 0

			if selected != tc.match {
				t.Errorf("selected=%v, want %v for /proc/mounts line:\n\t%s\nstdout:\n%s\nstderr:\n%s",
					selected, tc.match, tc.line, run.stdout, run.stderr)
			}

			// The unescaping is asserted on the output, because a path printed with a literal \040
			// would be passed to umount as a name that does not exist — reported as a failed unmount
			// of a mount that was fine.
			if tc.match && strings.Contains(tc.line, `\040`) {
				if !strings.Contains(run.stdout, "/mnt/my bucket") {
					t.Errorf("the escaped mount point was not decoded; /proc/mounts writes a space as "+
						"\\040 and umount needs the real name\nstdout:\n%s", run.stdout)
				}
			}
		})
	}
}

// rootWithMounts builds a scratch root carrying a /proc/mounts with the given content.
func rootWithMounts(t *testing.T, mounts string) string {
	t.Helper()

	root := t.TempDir()

	// /proc is 0555 and /proc/mounts is 0444 on a real Linux system: every process reads the mount
	// table, which is exactly why preremove.sh can. 0755/0644 here because the test also has to
	// write the fixture; the world-readable bit is the part that matches reality, and gosec's
	// G301/G306 see only the group and other bits.
	proc := filepath.Join(root, "proc")
	if err := os.MkdirAll(proc, 0o755); err != nil { // #nosec G301 -- /proc is world-traversable
		t.Fatalf("mkdir %s: %v", proc, err)
	}

	if err := os.WriteFile(filepath.Join(proc, "mounts"), []byte(mounts), 0o644); err != nil { // #nosec G306 -- /proc/mounts is world-readable
		t.Fatalf("write mounts: %v", err)
	}

	return root
}

// unmountWeakeningFlags are the options that report a finished unmount before it is finished.
var unmountWeakeningFlags = regexp.MustCompile(`(?m)\b(umount|fusermount3?)\b[^\n|&;]*\s-(-lazy|-force|[a-z]*[zlf])\b`)

// TestPreremoveDoesNotForceOrLazyUnmount is the same assertion internal/config's systemd tests make
// about ExecStop, in the other place a maintainer is tempted to make a stubborn unmount succeed.
//
// A lazy or forced unmount detaches the name while the filesystem keeps serving already-open files.
// Adding -z here would make this script exit 0 with writes still in flight, which is the exact
// outcome the failure path exists to prevent — achieved by lying rather than by working.
func TestPreremoveDoesNotForceOrLazyUnmount(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"preremove.sh", "postinstall.sh"} {
		script := readFile(t, filepath.Join(repoRoot(t), "scripts", name))

		for i, line := range strings.Split(script, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}

			if unmountWeakeningFlags.MatchString(trimmed) {
				t.Errorf("scripts/%s:%d passes a lazy or forced unmount flag:\n\t%s\n"+
					"That detaches the mount point while the filesystem keeps serving open files, so "+
					"the script reports a finished unmount with writes still in flight. This project "+
					"treats a SIGKILL through buffered data as the one unacceptable failure.",
					name, i+1, trimmed)
			}
		}
	}
}
