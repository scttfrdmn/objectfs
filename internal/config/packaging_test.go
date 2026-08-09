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
// The tests below are in two groups. The first reads nfpm.yaml against the filesystem and against
// the scripts, so a path can only be wrong in one place at a time. The second *runs* the scripts
// against a scratch root and asserts what they do, because "idempotent" is a claim about behavior
// and a claim about behavior that is only checked by reading the source is not checked.

// nfpmFile is the packaging configuration, relative to the module root.
const nfpmFile = "nfpm.yaml"

// nfpmConfig is the subset of nfpm's schema these tests assert on.
//
// A hand-written subset rather than nfpm's own Config type, deliberately: importing
// github.com/goreleaser/nfpm/v2 would add a packaging tool to this module's dependency graph — and
// to every downstream consumer's — to check four fields. The fields named here are the ones with a
// counterpart elsewhere in the repository, which is what these tests are about.
type nfpmConfig struct {
	Name     string `yaml:"name"`
	Version  string `yaml:"version"`
	Arch     string `yaml:"arch"`
	Platform string `yaml:"platform"`
	Contents []struct {
		Src    string `yaml:"src"`
		Dst    string `yaml:"dst"`
		Type   string `yaml:"type"`
		Expand bool   `yaml:"expand"`
	} `yaml:"contents"`
	Scripts struct {
		PreInstall  string `yaml:"preinstall"`
		PostInstall string `yaml:"postinstall"`
		PreRemove   string `yaml:"preremove"`
		PostRemove  string `yaml:"postremove"`
	} `yaml:"scripts"`
}

// readNfpm parses nfpm.yaml.
func readNfpm(t *testing.T) nfpmConfig {
	t.Helper()

	var cfg nfpmConfig
	if err := yaml.UnmarshalStrict([]byte(readFile(t, filepath.Join(repoRoot(t), nfpmFile))), &cfg); err != nil {
		// Not UnmarshalStrict's usual meaning here: this struct is a deliberate subset, so an
		// unknown key is expected. yaml.v2's strict mode errors on unknown keys, so the parse is
		// non-strict below and this branch only catches malformed YAML.
		if !strings.Contains(err.Error(), "not found in type") {
			t.Fatalf("%s does not parse as YAML: %v", nfpmFile, err)
		}

		if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(repoRoot(t), nfpmFile))), &cfg); err != nil {
			t.Fatalf("%s does not parse as YAML: %v", nfpmFile, err)
		}
	}

	if len(cfg.Contents) == 0 {
		t.Fatalf("%s lists no contents. Either the file was restructured or this test stopped "+
			"reading it, and an empty list satisfies every assertion below", nfpmFile)
	}

	return cfg
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
				"package.", nfpmFile, s.field, s.when)

			continue
		}

		abs := filepath.Join(repoRoot(t), filepath.Clean(s.path))
		info, err := os.Stat(abs)

		if err != nil {
			t.Errorf("%s sets %s: %s, which does not exist. nfpm fails at package time on this, so "+
				"it is caught either way — but it is caught here without needing nfpm installed.",
				nfpmFile, s.field, s.path)

			continue
		}

		if info.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable. dpkg requires the mode bit on a maintainer script and "+
				"refuses to run it otherwise, which surfaces as a package that installs and does "+
				"nothing.", s.path)
		}
	}
}

// TestPackagedFilesExist checks every src in nfpm.yaml against the filesystem.
//
// The binary is the one entry whose src cannot be stat'ed, because it is a build artifact —
// build/objectfs-linux-${OBJECTFS_ARCH}, produced by `make build-linux`. It is checked against the
// Makefile instead, which is the actual coupling: a rename of the artifact in one file and not the
// other produces a packaging step that fails only when someone runs it.
func TestPackagedFilesExist(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	cfg := readNfpm(t)
	makefile := expandMakeVariables(readFile(t, filepath.Join(root, "Makefile")))

	var checked int

	for _, c := range cfg.Contents {
		if c.Type == "dir" {
			if c.Src != "" {
				t.Errorf("%s: a `type: dir` entry for %s also names a src (%s), which nfpm ignores",
					nfpmFile, c.Dst, c.Src)
			}

			continue
		}

		if c.Src == "" {
			t.Errorf("%s: the entry for %s has no src", nfpmFile, c.Dst)

			continue
		}

		checked++

		// An entry carrying ${...} is a build artifact by construction: nfpm only expands a src that
		// sets `expand: true`, and the only variable substitution in this file selects the staged
		// binary per architecture.
		if strings.Contains(c.Src, "${") {
			if !c.Expand {
				t.Errorf("%s: src %s contains ${...} but does not set `expand: true`, so nfpm treats "+
					"it as a literal path and the glob fails at package time", nfpmFile, c.Src)
			}

			for _, arch := range []string{"amd64", "arm64"} {
				want := strings.ReplaceAll(c.Src, "${OBJECTFS_ARCH}", arch)
				want = strings.TrimPrefix(filepath.Clean(want), "./")

				if !strings.Contains(makefile, want) {
					t.Errorf("%s packages %s, and with OBJECTFS_ARCH=%s that is %s — a path the "+
						"Makefile never writes.\nNothing builds it, so `make package-linux` fails on a "+
						"missing file. The artifact name lives in build-linux's recipe; the two have to "+
						"agree.", nfpmFile, c.Src, arch, want)
				}
			}

			continue
		}

		if _, err := os.Stat(filepath.Join(root, filepath.Clean(c.Src))); err != nil {
			t.Errorf("%s packages %s → %s, and that source file does not exist in the repository",
				nfpmFile, c.Src, c.Dst)
		}
	}

	if checked == 0 {
		t.Fatalf("%s has no file entries at all — only directories. A package that installs no "+
			"files is not the thing #207 asks for", nfpmFile)
	}
}

// makeSimpleAssignment matches the `NAME := value` form. Only `:=`, not `=` or `?=`: a recursively
// expanded variable can reference one defined later, and resolving those properly means implementing
// make.
var makeSimpleAssignment = regexp.MustCompile(`(?m)^([A-Z_][A-Z0-9_]*)\s*:=\s*(.*)$`)

// expandMakeVariables substitutes $(NAME) for simply-expanded variables defined in the Makefile.
//
// Needed because build-linux writes its artifact as
// `$(BUILD_DIR)/$(BINARY_NAME)-linux-amd64`, so a literal search for "build/objectfs-linux-amd64"
// finds nothing while the recipe is perfectly correct. Without this the coupling check below reports
// a false failure — which is worse than no check, because the fix people reach for is deleting it.
//
// Deliberately not a make implementation: no functions, no conditionals, no recursive expansion of
// `=` variables. Two passes so that `$(BINARY_PATH)`, itself defined in terms of `$(BIN_DIR)`,
// resolves. Any variable it cannot resolve simply stays as-is, and the assertion that then fails is
// reporting a genuine "the Makefile does not write this path anywhere I can see", which is worth
// looking at.
func expandMakeVariables(makefile string) string {
	vars := make(map[string]string)

	for _, m := range makeSimpleAssignment.FindAllStringSubmatch(makefile, -1) {
		vars[m[1]] = strings.TrimSpace(m[2])
	}

	expanded := makefile

	for range 2 {
		for name, value := range vars {
			expanded = strings.ReplaceAll(expanded, "$("+name+")", value)
		}
	}

	return expanded
}

// TestPackageVersionComesFromTheVersionConstant pins CLAUDE.md's single-authority rule into the
// packaging.
//
// A literal here would be the sixth copy of a number this repository once gave five different
// answers to, and the worst-placed one: nothing reads a package's metadata back to compare it, so
// `objectfs version` inside objectfs_0.12.0_amd64.deb could report 0.13.0 and no gate would notice.
// .github/workflows/release.yml already checks the tag against the constant; this checks the package.
//
// Both halves are asserted, because either alone is satisfiable while being wrong: nfpm.yaml must
// take the version from the environment, and the Makefile must put the constant into that
// environment.
func TestPackageVersionComesFromTheVersionConstant(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	cfg := readNfpm(t)

	if !strings.Contains(cfg.Version, "${") {
		//nolint:gosec // a path built from the module root this test located
		mainGo, err := os.ReadFile(filepath.Join(root, "cmd", "objectfs", "main.go"))
		if err != nil {
			t.Fatalf("read cmd/objectfs/main.go: %v", err)
		}

		declared := "unknown"
		if m := versionConstant.FindSubmatch(mainGo); m != nil {
			declared = string(m[1])
		}

		t.Fatalf("%s hardcodes version: %q. The authority is the `version` constant in "+
			"cmd/objectfs/main.go (currently %s), and a number copied into a config file has no way "+
			"to be told it is stale — which is how this repository came to give five different "+
			"answers at once. Take it from ${OBJECTFS_VERSION} and let the Makefile supply it.",
			nfpmFile, cfg.Version, declared)
	}

	// The variable nfpm.yaml reads, e.g. "OBJECTFS_VERSION" out of "${OBJECTFS_VERSION}".
	envVar := strings.Trim(strings.TrimPrefix(strings.TrimSpace(cfg.Version), "$"), "{}")

	makefile := readFile(t, filepath.Join(root, "Makefile"))

	if !strings.Contains(makefile, envVar+"=") {
		t.Errorf("%s takes its version from ${%s}, and the Makefile never sets it. nfpm expands an "+
			"unset variable to the empty string, so the packaging step would build objectfs__amd64.deb "+
			"or fail on an invalid version — neither of which says what went wrong.", nfpmFile, envVar)
	}

	// The extraction itself. Not a literal in the Makefile either — it has to read main.go, with the
	// same expression release.yml uses, and $(VERSION) is specifically wrong here: it is `git
	// describe`, which yields v0.12.0-14-gabc123-dirty on an untagged or dirty tree, and neither dpkg
	// nor rpm accepts that.
	if !strings.Contains(makefile, "cmd/objectfs/main.go") {
		t.Errorf("the Makefile sets %s without reading cmd/objectfs/main.go. Wherever the value "+
			"comes from instead, it is a second authority for the version.", envVar)
	}
}

// TestPackagingAndPostinstallAgreeOnTheExampleConfigPath is the seam most likely to break silently.
//
// nfpm.yaml installs configs/example.yaml to a path under /usr/share, and postinstall.sh copies it
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
			"which is the one postinstall.sh copies from", nfpmFile, len(packaged), packaged)
	}

	if !strings.Contains(script, packaged[0]) {
		t.Errorf("%s installs the example config to %s, and scripts/postinstall.sh does not mention "+
			"that path — so it copies nothing and /etc/objectfs/config.yaml is never created.\n"+
			"Both files have to name the same path. The scriptlet exits 0 either way by design, so "+
			"this failure is invisible at install time.", nfpmFile, packaged[0])
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
			"owning the file they put settings into.", nfpmFile, c.Dst)
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
			"starts with systemctl is wrong.", nfpmFile)
	}

	if !strings.HasPrefix(found, "/usr/lib/systemd/system/") {
		t.Errorf("%s installs the systemd unit to %s. A package's units belong in "+
			"/usr/lib/systemd/system; /etc/systemd/system is where an operator's override goes, and "+
			"systemd gives that precedence — a package occupying the path leaves an override nowhere "+
			"to win from.", nfpmFile, found)
	}
}

// TestPackagingShipsTheModulefilesWhereTheModuleSystemsLookForThem checks the two entries whose dst
// is a computed path, which is the only place in nfpm.yaml where that is true.
//
// Every other rule has a fixed destination; these two put ${OBJECTFS_VERSION} in the *filename*,
// because that is how Lmod and TCL Modules decide what `module load objectfs/<version>` means.
// MODULEPATH names a directory, the directory below it is the module name, and the file inside that is
// the version. Three things can go wrong here and none of them fail at package time:
//
//   - **`expand` omitted.** nfpm treats the dst as a literal, and the package ships a file named
//     `${OBJECTFS_VERSION}.lua`. It installs successfully. `module avail` lists a version called
//     "${OBJECTFS_VERSION}" and nobody can load it.
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

	// The version variable nfpm.yaml uses for the package version, so this test names the same one
	// rather than assuming the spelling. TestPackageVersionComesFromTheVersionConstant is what asserts
	// it is a variable at all.
	versionRef := strings.TrimSpace(cfg.Version)

	for _, want := range []struct {
		src string
		// dst is the required destination, with the version reference substituted in.
		dst string
		// why is appended to a failure, naming what the module system does with a wrong path.
		why string
	}{
		{
			src: "configs/modules/objectfs.lua",
			dst: "/usr/share/modulefiles/objectfs/" + versionRef + ".lua",
			why: "Lmod requires the .lua extension to parse the file as Lua, and strips it before " +
				"reporting the version — so this one, and only this one, keeps its extension.",
		},
		{
			src: "configs/modules/objectfs.tcl",
			dst: "/usr/share/modulefiles/objectfs/" + versionRef,
			why: "For TCL Modules the filename is the version string, so a .tcl suffix here makes " +
				"`module avail` report a version called \"" + versionRef + ".tcl\".",
		},
	} {
		var found []string

		for _, c := range cfg.Contents {
			if c.Type == "dir" || !strings.HasSuffix(c.Src, want.src) {
				continue
			}

			found = append(found, c.Dst)

			if !c.Expand {
				t.Errorf("%s installs %s to %s without `expand: true`. nfpm only substitutes variables "+
					"in an entry that asks, so the package ships a file literally named %q — which "+
					"installs cleanly and cannot be loaded.",
					nfpmFile, want.src, c.Dst, filepath.Base(c.Dst))
			}
		}

		if len(found) != 1 {
			t.Errorf("%s installs %s to %d destinations (%v); expected exactly one, %s.\nWithout it, a "+
				"site that installs the package still has to fetch the modulefile out of a source "+
				"checkout, which is what #145 exists to stop.",
				nfpmFile, want.src, len(found), found, want.dst)

			continue
		}

		if found[0] != want.dst {
			t.Errorf("%s installs %s to %s, want %s.\n%s\nThe module systems read the version out of "+
				"this path, so the wrong one means `module load objectfs` exports the wrong "+
				"OBJECTFS_VERSION — with no error anywhere.", nfpmFile, want.src, found[0], want.dst, want.why)
		}
	}
}

// TestMakefileBuildsPackages asserts a target exists that produces both formats.
//
// #207 notes that `make package` only makes tarballs, and it still does — a tarball is the right
// artifact for a release download. The deb and the rpm need their own target, and it needs to build
// both formats, because the entire argument for nfpm over a debian/ directory plus a .spec is that
// one config describes both.
func TestMakefileBuildsPackages(t *testing.T) {
	t.Parallel()

	makefile := readFile(t, filepath.Join(repoRoot(t), "Makefile"))

	if !strings.Contains(makefile, "package-linux:") {
		t.Fatal("the Makefile has no package-linux target. `make package` builds tarballs, which " +
			"carry no maintainer scripts — so scripts/preremove.sh still has nothing that can invoke " +
			"it, which is #207 unresolved.")
	}

	if !strings.Contains(makefile, nfpmFile) {
		t.Errorf("the Makefile's packaging target does not reference %s", nfpmFile)
	}

	for _, format := range []string{"deb", "rpm"} {
		if !strings.Contains(makefile, format) {
			t.Errorf("the Makefile never mentions %s. Both formats come from one nfpm config; "+
				"building only one of them is half the deliverable", format)
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

	// 0755 and 0644 are the modes nfpm.yaml gives these two paths, and reproducing them is the
	// whole point of the fixture — a scratch root at a mode the package would never produce tests
	// the script against a system that cannot exist. gosec reads any 0644 write as a finding
	// (G301/G306) without a way to know the file is a copy of /usr/share/objectfs/configs/
	// example.yaml, which is world-readable by design: it is the example, and the secret-bearing
	// file is the 0600 /etc/objectfs/config.yaml the script derives from it.
	dir := filepath.Join(root, "usr", "share", "objectfs", "configs")
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- /usr/share must be traversable; matches nfpm.yaml
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	src := readFile(t, filepath.Join(repoRoot(t), "configs", "example.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "example.yaml"), []byte(src), 0o644); err != nil { // #nosec G306 -- the shipped example is world-readable; matches nfpm.yaml
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
