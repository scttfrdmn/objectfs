package config

// The HPC modulefiles, held against each other and against where the software is actually installed.
//
// `module load objectfs` is how research computing sites consume software, so these two files are the
// entry point for NCAR, NERSC, and university HPC centers. They are also the least verifiable thing
// this repository ships: Lua and TCL that no Go build compiles, no vet inspects, and no linter reads.
// A modulefile is a program the toolchain cannot see, which is the same category as the fenced code
// blocks docs_symbols_test.go exists for and the systemd unit systemd_unit_test.go exists for.
//
// # The defect class this is aimed at
//
// A modulefile that puts a directory on PATH where no binary was installed. It is worse than a broken
// build because it reports success: `module load objectfs` returns 0, `module list` shows objectfs
// loaded, and the failure surfaces hours later as "command not found" inside a batch job — where the
// module system looks correct and ObjectFS looks broken.
//
// The draft in issue #145 had exactly that defect, twice over, and the two halves concealed each
// other. It computed `pathJoin("/usr/lib/objectfs", version)` as a base and then did
// `prepend_path("PATH", "/usr/bin")`. Those disagree about where the binary lives; nothing installs
// to /usr/lib/objectfs/<version>; and /usr/bin needs no prepending because it is already on PATH.
// The third measured problem is the one nobody would guess: prepending a directory that is already
// on PATH is not a no-op under Lmod, it HOISTS that directory to the front, ahead of any site
// directory earlier in PATH, and Lmod's unload does not put it back.
//
// # Why the checks are structural rather than an interpreter run
//
// The valuable test would be `module load objectfs && objectfs version` under a real Lmod. That runs
// in CI on a container with Lmod installed, and it should — it is the acceptance criterion on #145.
// It cannot be *this* test, because this test has to run everywhere `go test` does, on a developer
// Mac with no Lmod and no TCL Modules. TestModulefilesLoadUnderRealModuleSystems below does run the
// real thing when the interpreters happen to be present, and skips when they are not; the checks
// that always run are the structural ones.
//
// So the properties asserted here are the ones that are decidable by reading the files:
//
//   - the two files export the same variable names, so a site running Lmod and a site running TCL
//     Modules get the same environment;
//   - every directory either file puts on PATH is a directory something in this repository actually
//     installs a binary to, or is derived from the module's own location rather than hardcoded;
//   - neither file writes a version literal, since the version constant is the only authority;
//   - every objectfs command line in the help text uses a subcommand the binary dispatches and flags
//     the binary parses — reusing the scrapers in docs_symbols_test.go, so there is one definition of
//     "a flag this binary accepts" rather than three;
//   - neither file exports a variable whose name implies the binary reads it when it does not.
//
// The last one is not hypothetical either. OBJECTFS_CONFIG appears in configs/systemd/objectfs@.service
// and looks like a variable cmd/objectfs consults; it is not — the unit expands it into
// `--config ${OBJECTFS_CONFIG}` itself. A modulefile exporting it would set a variable that looks like
// it selects a configuration file and selects nothing.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The two shipped modulefiles, relative to the module root.
const (
	lmodModuleFile = "configs/modules/objectfs.lua"
	tclModuleFile  = "configs/modules/objectfs.tcl"
)

// moduleInstallDir is where both files install, relative to a prefix.
//
// Both files derive their prefix by splitting their own path on this string, so it is load-bearing
// rather than documentation: a packaging rule that installs them anywhere else silently changes which
// directory they search for the binary. TestModulefilesDeriveTheirPrefixFromTheirLocation asserts
// both files actually contain it.
const moduleInstallDir = "/share/modulefiles/"

// binDirsSomethingInstallsTo are the directories this repository puts an objectfs binary into.
//
// /usr/bin is what configs/systemd/objectfs@.service invokes — ExecStart is /usr/bin/objectfs — which
// makes it the packaged location. /usr/local/bin is where the Dockerfile puts it (COPY --from=builder
// /bin/objectfs /usr/local/bin/objectfs) and the conventional destination for a local build.
//
// The point of the list is the *absence* of /usr/lib/objectfs/<version>, which the #145 draft
// computed and which nothing anywhere installs to.
var binDirsSomethingInstallsTo = map[string]string{
	"/usr/bin":       "configs/systemd/objectfs@.service invokes /usr/bin/objectfs",
	"/usr/local/bin": "the Dockerfile installs to /usr/local/bin/objectfs",
	"/bin":           "the builder stage of the Dockerfile builds to /bin/objectfs",
}

// setenvCall matches an exported variable name in either syntax: Lmod's setenv("NAME", value) and
// TCL's setenv NAME value. One pattern over both files, because the property being checked is that
// the two agree, and reading them with two different parsers invites the parsers to disagree instead.
var setenvCall = regexp.MustCompile(`(?m)^\s*setenv\s*(?:\(\s*"([A-Z_][A-Z0-9_]*)"|([A-Z_][A-Z0-9_]*)\b)`)

// pathPrepend matches a PATH modification in either syntax, capturing the argument.
var pathPrepend = regexp.MustCompile(
	`(?m)^\s*(?:prepend_path\s*\(\s*"(?:PATH|MANPATH)"\s*,\s*(.+?)\s*\)|` +
		`(?:prepend|append)-path\s+(?:PATH|MANPATH)\s+(\S+))`)

// TestModulefilesExportTheSameEnvironment is the agreement check, and it is the reason this file
// exists rather than a note in a README saying to keep the two in sync.
//
// A site runs Lmod or it runs TCL Modules; almost none runs both. So a divergence between these two
// files is invisible at the site that has it — everything works, and the environment is simply
// different from what the documentation describes and from what the other half of the user base gets.
// A hand-comparison of two 150-line files in different languages rots on the first edit.
func TestModulefilesExportTheSameEnvironment(t *testing.T) {
	t.Parallel()

	lua := exportedVariables(readModulefile(t, lmodModuleFile))
	tcl := exportedVariables(readModulefile(t, tclModuleFile))

	if len(lua) == 0 || len(tcl) == 0 {
		t.Fatalf("found %d exported variables in %s and %d in %s. At least one file exports "+
			"nothing, which means this parser no longer matches how they are written — and an "+
			"agreement test that parses nothing reports agreement, which is the failure mode it "+
			"exists to prevent", len(lua), lmodModuleFile, len(tcl), tclModuleFile)
	}

	for name := range lua {
		if !tcl[name] {
			t.Errorf("%s exports %s and %s does not.\nA site running TCL Modules would not get it, "+
				"and nothing at that site would say so. Export it from both files or from neither.",
				lmodModuleFile, name, tclModuleFile)
		}
	}

	for name := range tcl {
		if !lua[name] {
			t.Errorf("%s exports %s and %s does not.\nA site running Lmod would not get it, and "+
				"nothing at that site would say so. Export it from both files or from neither.",
				tclModuleFile, name, lmodModuleFile)
		}
	}
}

// TestModulefilesOnlyAddRealBinaryDirectoriesToPath is the defect in the #145 draft, pinned.
//
// Every PATH argument must be either a variable — meaning the directory is derived from where the
// module actually is, and checked for the binary before being added — or a literal directory that
// something in this repository installs a binary to. A hardcoded path nothing installs to is the
// whole defect: PATH gains a directory with no binary in it, the load succeeds, and the failure moves
// to a batch job.
func TestModulefilesOnlyAddRealBinaryDirectoriesToPath(t *testing.T) {
	t.Parallel()

	for _, file := range []string{lmodModuleFile, tclModuleFile} {
		body := readModulefile(t, file)

		for _, m := range pathPrepend.FindAllStringSubmatch(body, -1) {
			arg := m[1]
			if arg == "" {
				arg = m[2]
			}

			// A quoted literal is the only form this can judge. Anything else — a Lua local, a TCL
			// $variable, a pathJoin/file-join call — is derived at load time from the module's own
			// location, which is the form that cannot be wrong about a fixed prefix. Both files
			// verify the binary is present in it before adding it, and fail closed when it is not;
			// TestModulefilesFailClosedWhenTheBinaryIsAbsent covers that half.
			literal, quoted := strings.CutPrefix(arg, `"`)
			if !quoted {
				continue
			}

			literal = strings.TrimSuffix(literal, `"`)

			if _, ok := binDirsSomethingInstallsTo[literal]; !ok {
				t.Errorf("%s puts the literal directory %q on PATH, and nothing in this repository "+
					"installs an objectfs binary there.\nThe directories something does install to "+
					"are: %s.\nThis is the #145 draft's defect: it computed a base of "+
					"/usr/lib/objectfs/<version>, which no packaging rule, no Makefile target, and "+
					"no Dockerfile stage writes to. A PATH entry with no binary in it makes `module "+
					"load` succeed and `objectfs` fail later, inside a job, where the module system "+
					"looks correct and ObjectFS looks broken.",
					file, literal, strings.Join(sortedKeys(installDirNames()), ", "))
			}
		}
	}
}

// TestModulefilesDoNotHoistSystemDirectoriesOntoPath is the measured surprise, pinned.
//
// The #145 draft's `prepend_path("PATH", "/usr/bin")` reads as a harmless no-op — /usr/bin is on
// every PATH already. It is not one. Measured against Lmod 9.3: PATH
// "/opt/site/bin:/usr/local/bin:/usr/bin:/bin" becomes "/usr/bin:/opt/site/bin:/usr/local/bin:/bin".
// The directory is moved to the front, ahead of any site directory before it — and at an HPC center
// /opt/site/bin is precisely how a center shadows a distro tool with its own build. Loading ObjectFS
// would silently re-point every such command at the distro version.
//
// It also does not come back. Lmod's unload removes the entry it recorded adding, so /usr/bin stays
// hoisted after `module unload objectfs`; the session is changed for good. Measured, not reasoned.
//
// TCL Modules 5.6.1's prepend-path does skip a directory already in PATH, leaving the order alone. So
// this is a defect on Lmod only — which is the case that matters, since Lmod is the more common of the
// two at the sites this targets, and since a modulefile must not depend on one implementation being
// more forgiving than the other.
func TestModulefilesDoNotHoistSystemDirectoriesOntoPath(t *testing.T) {
	t.Parallel()

	// The directories on every login PATH already. Adding one of these unconditionally reorders a
	// user's PATH; it never makes a binary reachable that was not already reachable.
	systemBinDirs := []string{"/bin", "/usr/bin", "/usr/local/bin", "/sbin", "/usr/sbin"}

	for _, file := range []string{lmodModuleFile, tclModuleFile} {
		body := readModulefile(t, file)

		for _, m := range pathPrepend.FindAllStringSubmatch(body, -1) {
			arg := m[1]
			if arg == "" {
				arg = m[2]
			}

			literal, quoted := strings.CutPrefix(arg, `"`)
			if !quoted {
				continue
			}

			literal = strings.TrimSuffix(literal, `"`)

			for _, dir := range systemBinDirs {
				if literal != dir {
					continue
				}

				t.Errorf("%s unconditionally puts %s on PATH, which is already on every login "+
					"PATH.\nThis is not the no-op it looks like. Under Lmod it hoists %s to the "+
					"front of PATH, ahead of any site directory before it — which is how an HPC "+
					"center shadows a distro tool with its own build — and Lmod's unload does not "+
					"put it back, so `module unload objectfs` leaves the session reordered.\nIf the "+
					"binary is in a system directory it is already reachable and PATH needs no "+
					"change. Guard the prepend on the directory not being a system one.",
					file, dir, dir)
			}
		}
	}
}

// TestModulefilesDoNotHardcodeAVersion pins the single-source-of-truth rule for these two files.
//
// version_test.go enforces it for markdown; these are not markdown, so nothing reached them. The
// authority is the `version` constant in cmd/objectfs/main.go, and a modulefile learns the version
// from its own filename — myModuleVersion() under Lmod, `file tail $ModulesCurrentModulefile` under
// TCL — so the number lives in the install path and nowhere else.
//
// A literal here would be worse than a stale document: the modulefile for version N would export
// OBJECTFS_VERSION=M, so a job's provenance record would name a build it did not run.
func TestModulefilesDoNotHardcodeAVersion(t *testing.T) {
	t.Parallel()

	// A dotted version number in any form. Deliberately matched everywhere in the file, comments
	// included: a comment saying "for example objectfs/0.13.0" is the same staleness with a smaller
	// blast radius, and the two files are short enough that writing <version> instead costs nothing.
	semverish := regexp.MustCompile(`\b[vV]?\d+\.\d+\.\d+\b`)

	// A version attributed to the module system it describes. These files carry measured-against
	// notes — the refusal comment in objectfs.tcl reports that Modules 5.6.1 exits 1 where Modules
	// 5.4.0 exits 0 for identical output — and those are a *tool's* version, not ObjectFS's.
	//
	// Attribution is required rather than allowlisting the releases by number, which is what this
	// test did first and what made it fail against a correct file: it exempted the exact strings
	// "Lmod 9.3" and "Modules 5.6.1", so the first comment to measure a second Modules release
	// tripped it. An allowlist has to be edited every time a behavior is verified somewhere new,
	// and the edit is indistinguishable from suppressing a real finding.
	toolVersion := regexp.MustCompile(`\b(?:Lmod|Modules)\s+[vV]?\d+(?:\.\d+)+`)

	for _, file := range []string{lmodModuleFile, tclModuleFile} {
		for i, line := range strings.Split(readModulefile(t, file), "\n") {
			// The modulefile magic cookie `#%Module1.0` is a format version, required verbatim.
			if strings.Contains(line, "#%Module") {
				continue
			}

			// Attributed tool versions are removed from the line rather than skipping the line
			// wholesale, so a comment that names both — "under Modules 5.6.1, objectfs/0.13.0
			// resolves" — still fails on the ObjectFS literal.
			line = toolVersion.ReplaceAllString(line, "")

			if match := semverish.FindString(line); match != "" {
				t.Errorf("%s:%d writes the version literal %q:\n    %s\nThe authority is the "+
					"`version` constant in cmd/objectfs/main.go. These files learn the version from "+
					"their own filename — myModuleVersion() under Lmod, `file tail "+
					"$ModulesCurrentModulefile` under TCL — so the number exists once, in the "+
					"install path. A literal here exports OBJECTFS_VERSION naming a build the job "+
					"did not run. Write <version> in prose.",
					file, i+1, match, strings.TrimSpace(line))
			}
		}
	}
}

// TestModulefilesDeriveTheirPrefixFromTheirLocation asserts the mechanism the previous test relies on.
//
// Both files split their own path on "/share/modulefiles/" to find the install prefix. That is what
// makes them relocatable — the same file works at /usr, at /opt/sw, and in a user's home directory —
// and it is what makes the version come from the install path. If a packaging rule installs them
// somewhere that does not contain that segment, the split fails and the files fall back to walking up
// three levels, which is right for a <prefix>/modulefiles/objectfs/<version> tree and wrong for
// anything else. Asserting the segment is present here means the fallback stays a fallback.
func TestModulefilesDeriveTheirPrefixFromTheirLocation(t *testing.T) {
	t.Parallel()

	// The Lmod and TCL primitives that name the running modulefile. Neither file may hardcode its own
	// location, so one of these has to appear.
	selfReference := map[string]string{
		lmodModuleFile: "myFileName()",
		tclModuleFile:  "$ModulesCurrentModulefile",
	}

	for file, primitive := range selfReference {
		body := readModulefile(t, file)

		if !strings.Contains(body, primitive) {
			t.Errorf("%s does not call %s, so it cannot know where it was installed.\nA modulefile "+
				"that hardcodes its prefix works at exactly one site. The prefix has to be derived "+
				"from the file's own location, which is also where the version comes from.",
				file, primitive)
		}

		if !strings.Contains(body, moduleInstallDir) {
			t.Errorf("%s does not mention %q, which is the segment it splits its own path on to find "+
				"the install prefix.\nIf the derivation changed, point this test at the new one — "+
				"without it, the packaging rule and the modulefile can disagree about the install "+
				"path with nothing to notice.", file, moduleInstallDir)
		}
	}
}

// TestModulefilesFailClosedWhenTheBinaryIsAbsent asserts the degradation rule CLAUDE.md sets out.
//
// Locating the binary is a correctness capability, not a performance one: a PATH that does not lead
// to a binary is not a slower correct outcome, it is a load that reports success and defers a failure.
// So it fails closed with an operator-facing reason, the way conditional writes return ErrNotSupported
// when the probe fails.
//
// Measured, both halves: LmodError exits 1 and emits no environment changes at all, and `break` in a
// TCL modulefile makes modulecmd.tcl exit 1 with "ERROR: Module evaluation aborted" and emit none.
// Neither can half-apply — a refused load leaves PATH untouched rather than adding the directory and
// then complaining.
func TestModulefilesFailClosedWhenTheBinaryIsAbsent(t *testing.T) {
	t.Parallel()

	// The abort primitive each system needs. A `puts stderr` with no `break` is the failure mode this
	// guards: the warning scrolls past in a job's stderr and the module loads anyway.
	abort := map[string]string{
		lmodModuleFile: "LmodError",
		tclModuleFile:  "break",
	}

	for file, primitive := range abort {
		body := readModulefile(t, file)

		if !strings.Contains(body, primitive) {
			t.Errorf("%s does not use %s, so it has no way to refuse to load.\nLocating the binary "+
				"is a correctness property: if it is not found, the right outcome is a refused load "+
				"naming the paths that were searched. A warning is not enough — it scrolls past in a "+
				"batch job's stderr and the module loads with a PATH that leads nowhere.",
				file, primitive)
		}

		// isFile under Lmod, `file isfile` under TCL. Without a presence check there is nothing for
		// the abort to be conditional on, and a file could carry LmodError on an unreachable branch.
		if !strings.Contains(body, "isFile") && !strings.Contains(body, "file isfile") {
			t.Errorf("%s never tests for the binary's presence, so whatever it does with PATH is "+
				"unconditional.\nThe check and the refusal are one mechanism: search the candidate "+
				"directories, and refuse if none of them holds an objectfs binary.", file)
		}
	}
}

// TestModulefileHelpTextIsRunnable holds the help text to the same standard as documentation.
//
// This reuses parseInvocation, subcommands, and cliFlags from docs_symbols_test.go — the same
// scrapers systemd_unit_test.go uses on the unit's Exec lines. One definition of "a flag this binary
// accepts", checked against cmd/objectfs/main.go by TestCLIFlagsMatchTheBinary, rather than a third
// hand-maintained copy.
//
// `module help objectfs` is the first thing a user at an HPC site runs, often the only documentation
// they will read, and a command that fails there teaches them the filesystem is broken. The #145
// draft's help text showed:
//
//	objectfs mount --config /etc/objectfs/site.yaml --mount-point /mnt/data s3://my-bucket /mnt/data
//
// which passes --mount-point AND a positional mount point in the same command. Run against the real
// binary that happens to exit 0 — resolveMountTarget accepts the duplication when the two agree — but
// it is an example teaching a redundant form whose one-character-different sibling is a hard error:
// with the flag and the argument naming different directories it exits with "mount point given twice
// and differently". Verified by running both.
func TestModulefileHelpTextIsRunnable(t *testing.T) {
	t.Parallel()

	var checked int

	for _, file := range []string{lmodModuleFile, tclModuleFile} {
		body := readModulefile(t, file)

		for i, line := range joinContinuations(strings.Split(body, "\n")) {
			// The help text of the TCL file is `puts stderr "..."` and of the Lua file is a bare line
			// inside help([[...]]). Both are matched by looking for the command anywhere in the line
			// and taking what follows, which also catches a command named in a comment — deliberately,
			// since a comment showing an invocation that does not parse is the same wrong instruction.
			_, rest, found := strings.Cut(line, "objectfs ")
			if !found {
				continue
			}

			// An inline-code command in prose ends at its closing backtick; the words after it are a
			// sentence, not arguments. Cut there before parsing, because the first run of this test
			// read the line "so `objectfs mount s3://b /mnt --debug` leaves --debug unparsed" and
			// reported that the binary does not parse "--debug`" — a flag name with a backtick in it,
			// which exists in no spelling and pointed the reader at nothing.
			if end := strings.Index(rest, "`"); end >= 0 {
				rest = rest[:end]
			}

			rest = strings.TrimRight(strings.TrimSpace(rest), `".,`)

			invocation := parseInvocation(i+1, strings.TrimSpace(line), rest)

			// A line mentioning the word without a command after it — "objectfs mounts a bucket",
			// "the objectfs binary". A subcommand the binary does not have is reported below only
			// when the line looks like a command line, which a first positional that is a known
			// subcommand establishes.
			if invocation.subcommand == "" || !subcommands[invocation.subcommand] {
				continue
			}

			checked++

			for _, flag := range invocation.flags {
				if cliFlags[flag] {
					continue
				}

				t.Errorf("%s:%d passes --%s, which objectfs does not parse:\n    %s\nGo's flag "+
					"package exits 1 on an unrecognized flag, so this command fails. The parsed set "+
					"is: %s. `module help objectfs` is often the only documentation an HPC user "+
					"reads, and a command that fails there reads as a broken filesystem rather than "+
					"a wrong example.",
					file, i+1, flag, strings.TrimSpace(line), strings.Join(sortedKeys(cliFlags), ", "))
			}

			// --mount-point together with a positional mount point. Two positionals plus the flag
			// means the mount point is given twice; the binary accepts it when they agree and exits
			// with an error when they do not, so it is a form no example should teach.
			if invocation.hasFlag("mount-point") && invocation.positionalCount() >= 2 {
				t.Errorf("%s:%d gives the mount point twice, as --mount-point and as a positional "+
					"argument:\n    %s\nThe binary accepts this only while the two strings are "+
					"identical; change one and it exits with \"mount point given twice and "+
					"differently\". Show one form or the other. --mount-point is for a systemd "+
					"template unit, which knows where to mount but not what to mount.",
					file, i+1, strings.TrimSpace(line))
			}
		}
	}

	// Both files must actually contain usage examples. A help text with no commands in it passes every
	// check above trivially, which is the shape of a gate that has stopped looking at anything.
	if checked < 2 {
		t.Errorf("found only %d objectfs command lines across both modulefiles. `module help "+
			"objectfs` is the documentation an HPC user gets, so each file should show how to mount "+
			"and how to unmount — and a help-text checker that finds no commands is not checking "+
			"anything", checked)
	}
}

// TestModulefilesDoNotExportMisleadingVariables pins the variables that must not be set.
//
// Each entry is a variable whose name says it configures ObjectFS and which nothing in ObjectFS
// reads, or which something else reads to its harm. Setting one in every user's login environment is
// not a neutral act.
func TestModulefilesDoNotExportMisleadingVariables(t *testing.T) {
	t.Parallel()

	forbidden := map[string]string{
		"OBJECTFS_CONFIG": "cmd/objectfs never reads it — configs/systemd/objectfs@.service " +
			"expands it into `--config ${OBJECTFS_CONFIG}` itself, and the binary takes its config " +
			"file from that flag only. Exporting it here would set a variable that looks like it " +
			"selects a configuration file and selects nothing, so a site that set it would believe " +
			"its config was applied. Reference it in help text as the --config flag instead",
		"OBJECTFS_ROOT": "scripts/postinstall.sh and scripts/preremove.sh read it as a prefix for " +
			"every path they touch. A login shell exporting OBJECTFS_ROOT=/usr sends the next " +
			"`apt upgrade` scriptlet at /usr/usr/share and /usr/etc/objectfs",
	}

	for _, file := range []string{lmodModuleFile, tclModuleFile} {
		exported := exportedVariables(readModulefile(t, file))

		for name, reason := range forbidden {
			if exported[name] {
				t.Errorf("%s exports %s, and it should not: %s.", file, name, reason)
			}
		}
	}
}

// TestModulefilesLoadUnderRealModuleSystems is the acceptance criterion, when the tools are present.
//
// Everything above reads the files. This one runs them: it stages a prefix with a fake objectfs
// binary and the modulefile at the path the packaging rule installs to, invokes the real Lmod or
// modulecmd.tcl, and asserts the emitted shell code puts the binary's directory on PATH and exports
// the version from the filename.
//
// Skipped when the interpreter is absent, which is the normal case on a developer machine and is why
// the structural checks above exist and are not conditional. `brew install lmod modules` or
// `apt-get install lmod environment-modules` makes it run. It found two real defects while these
// files were being written, both of which the structural checks cannot see: the three-level layout
// the #145 draft implied, under which `module load objectfs` fails outright with "the following
// module(s) are unknown", and a PATH-membership guard that leaked its directory on unload.
func TestModulefilesLoadUnderRealModuleSystems(t *testing.T) {
	t.Parallel()

	// The staged version. A literal is fine here and only here: it is this test's fixture, chosen to
	// be recognizably not a real release so that a failure message cannot be mistaken for one, and
	// the property being asserted is that whatever the directory says is what gets exported.
	const stagedVersion = "9.9.9"

	systems := []struct {
		name string
		// candidates are the interpreter paths to try, in order.
		candidates []string
		// installName is what the file is called inside objectfs/ for this system.
		installName string
		// source is the repository file to stage.
		source string
	}{
		{
			name: "lmod",
			candidates: []string{
				"/opt/homebrew/opt/lmod/libexec/lmod",
				"/usr/local/opt/lmod/libexec/lmod",
				"/usr/share/lmod/lmod/libexec/lmod",
			},
			installName: stagedVersion + ".lua",
			source:      lmodModuleFile,
		},
		{
			name: "tcl-modules",
			candidates: []string{
				"/opt/homebrew/opt/modules/libexec/modulecmd.tcl",
				"/usr/local/opt/modules/libexec/modulecmd.tcl",
				"/usr/share/Modules/libexec/modulecmd.tcl",
				"/usr/lib/x86_64-linux-gnu/modulecmd.tcl",
			},
			installName: stagedVersion,
			source:      tclModuleFile,
		},
	}

	for _, system := range systems {
		t.Run(system.name, func(t *testing.T) {
			t.Parallel()

			interpreter := firstExecutable(system.candidates)
			if interpreter == "" {
				t.Skipf("no %s interpreter found; looked for %s. The structural checks in this file "+
					"cover what can be decided by reading the modulefiles; this one needs the real "+
					"thing. Install it to run this: `brew install lmod modules` or `apt-get install "+
					"lmod environment-modules`.", system.name, strings.Join(system.candidates, ", "))
			}

			// A prefix laid out the way the packaging rule installs: <prefix>/bin/objectfs and
			// <prefix>/share/modulefiles/objectfs/<version>.
			prefix := t.TempDir()
			binDir := filepath.Join(prefix, "bin")
			moduleDir := filepath.Join(prefix, "share", "modulefiles", "objectfs")

			for _, dir := range []string{binDir, moduleDir} {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatalf("staging %s: %v", dir, err)
				}
			}

			// A stub, not the real binary. The modulefile tests for a file at bin/objectfs and does
			// not run it, so building the real one would add a `go build` to this test for nothing.
			stub := filepath.Join(binDir, "objectfs")
			if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // a stub in a temp dir that must be executable for the modulefile's isFile check to be meaningful
				t.Fatalf("staging the stub binary: %v", err)
			}

			body := readModulefile(t, system.source)
			staged := filepath.Join(moduleDir, system.installName)

			if err := os.WriteFile(staged, []byte(body), 0o644); err != nil { //nolint:gosec // a modulefile in a temp dir, world-readable as an installed one is
				t.Fatalf("staging %s: %v", staged, err)
			}

			modulePath := filepath.Join(prefix, "share", "modulefiles")

			//nolint:gosec // an interpreter path from the fixed candidate list above
			cmd := exec.CommandContext(t.Context(), interpreter, "bash", "load", "objectfs")
			// A clean environment but for MODULEPATH and a minimal PATH, so the result cannot depend
			// on the developer's shell. OBJECTFS_MODULE_PREFIX is deliberately absent: this asserts
			// the derived-from-location path, which is the one every site uses.
			cmd.Env = []string{
				"MODULEPATH=" + modulePath,
				"PATH=/usr/bin:/bin",
				"HOME=" + prefix,
			}

			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("`%s bash load objectfs` failed: %v\nstdout:\n%s\nThe modulefile staged at "+
					"%s did not load. A non-zero exit here means an operator running `module load "+
					"objectfs` gets the same failure.", system.name, err, out, staged)
			}

			emitted := string(out)

			// The binary's directory on PATH. Asserted as a PATH= assignment containing it rather
			// than by parsing the shell, because both systems emit `PATH=...; export PATH;` and the
			// question is only whether the directory is in there.
			if !strings.Contains(emitted, binDir) {
				t.Errorf("loading the module did not put %s on PATH.\nEmitted:\n%s\nThe binary is at "+
					"%s, so a user who ran `module load objectfs` would get 'command not found' — "+
					"which is the entire defect this file exists to prevent.", binDir, emitted, stub)
			}

			// The version, from the filename and not from a literal.
			//
			// The value is bound to the variable rather than looked for anywhere in the output, and
			// that is the difference between this test working and not. A substring search for
			// stagedVersion passes on output where OBJECTFS_VERSION is wrong, because both systems
			// emit their own bookkeeping — `LOADEDMODULES='objectfs/9.9.9'` and
			// `_LMFILES_=.../objectfs/9.9.9` — which contains the version regardless of what the
			// modulefile computed. Measured: with the TCL file reverted to the #145 draft's
			// `file tail [file dirname ...]`, which yields the module *name* instead of its version,
			// the emitted output carried `OBJECTFS_VERSION='objectfs'` and the substring form of this
			// assertion passed. The mutation was run, it survived, and this is the fix.
			if got := emittedValue(emitted, "OBJECTFS_VERSION"); got != stagedVersion {
				t.Errorf("loading the module exported OBJECTFS_VERSION=%q, want %q.\nEmitted:\n%s\n"+
					"The version has to come from the install path — the file was staged as "+
					"objectfs/%s — so that the `version` constant in cmd/objectfs/main.go stays the "+
					"only place a version is written down. Under Lmod that is myModuleVersion(); "+
					"under TCL it is `file tail $ModulesCurrentModulefile`, and NOT `file tail [file "+
					"dirname $ModulesCurrentModulefile]`, which returns the module name.",
					got, stagedVersion, emitted, system.installName)
			}
		})
	}
}

// TestModulefilesRefuseToLoadWithoutABinaryUnderRealModuleSystems is the fail-closed half, run for
// real.
//
// Same staging as above with the binary omitted. Asserts a non-zero exit AND that no PATH assignment
// is emitted — the second is the part that matters and the part a reading of the file cannot
// establish. A modulefile that printed a warning and then modified PATH anyway would satisfy every
// structural check in this file; only running it shows that nothing was applied.
func TestModulefilesRefuseToLoadWithoutABinaryUnderRealModuleSystems(t *testing.T) {
	t.Parallel()

	const stagedVersion = "9.9.9"

	systems := []struct {
		name        string
		candidates  []string
		installName string
		source      string
	}{
		{
			name: "lmod",
			candidates: []string{
				"/opt/homebrew/opt/lmod/libexec/lmod",
				"/usr/local/opt/lmod/libexec/lmod",
				"/usr/share/lmod/lmod/libexec/lmod",
			},
			installName: stagedVersion + ".lua",
			source:      lmodModuleFile,
		},
		{
			name: "tcl-modules",
			candidates: []string{
				"/opt/homebrew/opt/modules/libexec/modulecmd.tcl",
				"/usr/local/opt/modules/libexec/modulecmd.tcl",
				"/usr/share/Modules/libexec/modulecmd.tcl",
				"/usr/lib/x86_64-linux-gnu/modulecmd.tcl",
			},
			installName: stagedVersion,
			source:      tclModuleFile,
		},
	}

	for _, system := range systems {
		t.Run(system.name, func(t *testing.T) {
			t.Parallel()

			interpreter := firstExecutable(system.candidates)
			if interpreter == "" {
				t.Skipf("no %s interpreter found", system.name)
			}

			// If this machine has a real objectfs in /usr/bin or /usr/local/bin, the modulefile finds
			// it by design and there is no absent-binary case to test.
			for _, dir := range []string{"/usr/bin", "/usr/local/bin"} {
				if _, err := os.Stat(filepath.Join(dir, "objectfs")); err == nil {
					t.Skipf("%s/objectfs exists on this machine, so the modulefile's fallback search "+
						"finds a binary and there is no absent-binary case to exercise", dir)
				}
			}

			prefix := t.TempDir()
			moduleDir := filepath.Join(prefix, "share", "modulefiles", "objectfs")

			if err := os.MkdirAll(moduleDir, 0o750); err != nil {
				t.Fatalf("staging %s: %v", moduleDir, err)
			}

			// No bin/objectfs. That is the whole fixture.
			staged := filepath.Join(moduleDir, system.installName)
			if err := os.WriteFile(staged, []byte(readModulefile(t, system.source)), 0o644); err != nil { //nolint:gosec // a modulefile in a temp dir
				t.Fatalf("staging %s: %v", staged, err)
			}

			//nolint:gosec // an interpreter path from the fixed candidate list above
			cmd := exec.CommandContext(t.Context(), interpreter, "bash", "load", "objectfs")
			cmd.Env = []string{
				"MODULEPATH=" + filepath.Join(prefix, "share", "modulefiles"),
				"PATH=/usr/bin:/bin",
				"HOME=" + prefix,
			}

			// The interpreter's own exit status is deliberately NOT the assertion, and that took a CI
			// failure to establish. TCL Modules 5.6.1 (homebrew) exits 1 on an aborted load and
			// 5.4.0 (Ubuntu 24.04, which is what the runner has) exits **0** for the same refusal —
			// so this test passed locally and failed on CI against a modulefile that was correct.
			//
			// What both versions do, and what Lmod does, is emit a shell command that fails when the
			// caller evals it: TCL writes `test 0 = 1;` and Lmod writes `false`. That is the actual
			// contract, because `module` is a shell function whose whole body is an eval of this
			// output — an operator's `module load objectfs || exit 1` branches on the eval, never on
			// modulecmd's status. So the eval is what gets run here.
			out, _ := cmd.Output()

			// bash, not sh: both systems emit bash-flavored output when asked for it above, and the
			// modulefile is loaded through the `module` bash function on any real site.
			//
			//nolint:gosec // G204 is the point of the test, not a flaw in it: running the emitted
			// shell code is exactly what the `module` function does, so evaluating it is the only way
			// to assert the refusal reaches the caller. The input is this repository's own modulefile
			// under a temporary MODULEPATH, not anything a user supplies.
			eval := exec.CommandContext(t.Context(), "bash", "-c", string(out))
			eval.Env = []string{"PATH=/usr/bin:/bin"}

			if err := eval.Run(); err == nil {
				t.Errorf("eval of `%s bash load objectfs` succeeded with no binary installed.\n"+
					"Emitted:\n%s\nLocating the binary is a correctness property, so a load that "+
					"cannot find one has to fail with a reason rather than adding a directory to PATH "+
					"and letting the failure surface inside a batch job. The refusal has to reach the "+
					"caller through the emitted shell code — `test 0 = 1;` from TCL, `false` from "+
					"Lmod — because that is all the `module` shell function evaluates. Note that "+
					"modulecmd.tcl's own exit status is not this, and cannot be: 5.4.0 exits 0 for a "+
					"refused load and 5.6.1 exits 1.", system.name, out)
			}

			// The load must emit nothing, not merely exit non-zero. A refusal that has already
			// written PATH= to stdout is a refusal the shell has already applied, since the caller
			// evals this output.
			if strings.Contains(string(out), "PATH=") {
				t.Errorf("the refused load still emitted a PATH assignment:\n%s\nThe caller evals "+
					"this output, so a half-applied refusal changes the environment it claimed not "+
					"to. Both LmodError and TCL's `break` abort before any environment change is "+
					"written; something in the file is modifying PATH before the check.", out)
			}
		})
	}
}

// hasFlag reports whether an invocation passes the named flag.
func (c cliInvocation) hasFlag(name string) bool {
	return slices.Contains(c.flags, name)
}

// positionalCount returns how many positional arguments an invocation has after its subcommand.
//
// Recomputed from the text rather than stored, because cliInvocation keeps only the subcommand —
// docs_symbols_test.go has no use for the rest, and widening that struct would touch the two tests
// that share it.
func (c cliInvocation) positionalCount() int {
	var n int

	fields := strings.Fields(c.text)
	for i := 0; i < len(fields); i++ {
		field := fields[i]

		if strings.HasPrefix(field, "-") {
			name, _, hasValue := strings.Cut(strings.TrimLeft(field, "-"), "=")
			if !hasValue && cliFlagsTakingAValue[name] {
				i++
			}

			continue
		}

		// The command word itself and the subcommand are not positional arguments to it.
		if field == "objectfs" || field == c.subcommand || shellNoise[field] {
			continue
		}

		n++
	}

	return n
}

// emittedValue returns the value a module system's emitted shell code assigns to a variable, or "".
//
// Both systems emit `NAME=value; export NAME;`, differing only in whether the value is
// single-quoted — Lmod does not quote a value needing no quoting, Modules always does. Parsed rather
// than substring-matched because the value is the assertion: see the comment at the call site for the
// mutation that survived a substring check.
func emittedValue(emitted, name string) string {
	for line := range strings.SplitSeq(emitted, "\n") {
		rest, found := strings.CutPrefix(strings.TrimSpace(line), name+"=")
		if !found {
			continue
		}

		// Trailing `;` and the `export NAME;` that may follow on the same line.
		if idx := strings.Index(rest, ";"); idx >= 0 {
			rest = rest[:idx]
		}

		return strings.Trim(strings.TrimSpace(rest), `'"`)
	}

	return ""
}

// exportedVariables returns the environment variable names a modulefile sets.
func exportedVariables(body string) map[string]bool {
	names := make(map[string]bool)

	for _, m := range setenvCall.FindAllStringSubmatch(body, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}

		names[name] = true
	}

	return names
}

// installDirNames returns binDirsSomethingInstallsTo as a set, for sortedKeys in a failure message.
func installDirNames() map[string]bool {
	names := make(map[string]bool, len(binDirsSomethingInstallsTo))
	for dir := range binDirsSomethingInstallsTo {
		names[dir] = true
	}

	return names
}

// firstExecutable returns the first candidate path that exists and is executable, or "".
func firstExecutable(candidates []string) string {
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}

		return path
	}

	return ""
}

// readModulefile returns one of the shipped modulefiles.
func readModulefile(t *testing.T, file string) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), file)

	body, err := os.ReadFile(path) //nolint:gosec // a path built from the module root this test located
	if err != nil {
		t.Fatalf("reading %s, which is a file HPC sites install and load: %v", file, err)
	}

	return string(body)
}
