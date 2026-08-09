package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/config"
)

// runArgs invokes run with captured output, which is the whole point of run existing.
//
// Before v0.11.0 this package had no tests, and .coverage-floors recorded the reason as
// "currently untestable because main() calls log.Fatalf directly" — log.Fatalf calls os.Exit, which
// takes the test binary down with it. main() is now three lines around run(), and run returns a code
// and writes to injected writers, so every argument-handling decision below is reachable.
func runArgs(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)

	return code, out.String(), errOut.String()
}

func TestVersionSubcommand(t *testing.T) {
	t.Parallel()

	// All three spellings, because #134 requires `objectfs version` to exit 0 and the flag forms are
	// what every previous invocation used. A packaging script that runs `objectfs --version` to check
	// the installed build — configs/systemd/objectfs@.service does exactly this in its ExecStartPre —
	// breaks silently if only one of them works.
	for _, arg := range []string{"version", "--version", "-version"} {
		t.Run(arg, func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runArgs(t, arg)

			if code != exitOK {
				t.Errorf("objectfs %s exited %d, want 0; a packaging check that runs this treats a "+
					"non-zero status as a broken install. stderr: %s", arg, code, stderr)
			}
			if !strings.Contains(stdout, version) {
				t.Errorf("objectfs %s printed %q, which does not contain the version %q",
					arg, stdout, version)
			}
			// On stdout, not stderr: it is the requested output, and a script captures it with $().
			if stderr != "" {
				t.Errorf("objectfs %s wrote to stderr: %q", arg, stderr)
			}
		})
	}
}

func TestHelpSubcommandNamesEveryCommand(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{"help", "--help", "-h", "-help"} {
		t.Run(arg, func(t *testing.T) {
			t.Parallel()

			code, stdout, _ := runArgs(t, arg)

			if code != exitOK {
				t.Errorf("objectfs %s exited %d, want 0", arg, code)
			}

			// #134's acceptance criterion: --help shows all subcommands. Asserted per command rather
			// than as one substring, because the failure mode is a command that exists and is
			// undocumented — which is how `mount` came to be named in configs/systemd/objectfs@.service
			// for several releases without existing at all.
			for _, cmd := range []string{"mount", "unmount", "cluster", "version", "help"} {
				if !strings.Contains(stdout, cmd) {
					t.Errorf("objectfs %s does not mention the %q command, so the only way to learn "+
						"it exists is to read the source", arg, cmd)
				}
			}
		})
	}
}

func TestNoArgumentsIsAUsageError(t *testing.T) {
	t.Parallel()

	code, _, stderr := runArgs(t)

	if code != exitUsage {
		t.Errorf("objectfs with no arguments exited %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "Usage") {
		t.Errorf("objectfs with no arguments did not print usage to stderr: %q", stderr)
	}
}

// TestAMisspelledSubcommandIsNotTreatedAsAURI is why dispatch routes on the scheme.
//
// The legacy form has no subcommand, so the fallback has to decide whether args[0] is a URI. Deciding it
// by "does not match a known subcommand" would make every typo a mount attempt: `objectfs moutn s3://b
// /mnt` would try to mount a bucket named "moutn" and fail with a URI error about a word the operator
// never wrote as a URI. Routing on "://" instead means a typo is a usage error naming the typo.
func TestAMisspelledSubcommandIsNotTreatedAsAURI(t *testing.T) {
	t.Parallel()

	for _, typo := range []string{"moutn", "mnt", "unmoutn", "mount-point", "vsersion"} {
		t.Run(typo, func(t *testing.T) {
			t.Parallel()

			code, _, stderr := runArgs(t, typo, "s3://my-bucket", "/mnt/point")

			if code != exitUsage {
				t.Errorf("objectfs %s ... exited %d, want %d (a usage error)", typo, code, exitUsage)
			}
			if !strings.Contains(stderr, "unknown command") || !strings.Contains(stderr, typo) {
				t.Errorf("objectfs %s ... should report an unknown command naming %q, got: %q",
					typo, typo, stderr)
			}
		})
	}
}

// TestLegacyFormIsStillAccepted is #134's backward-compatibility criterion.
//
// `objectfs s3://bucket /mnt/point` is what README.md documented, what every script written against
// v0.10.x and earlier calls, and what a user's shell history holds. Asserted through --dry-run so the
// test reaches argument routing and configuration without mounting anything.
func TestLegacyFormIsStillAccepted(t *testing.T) {
	t.Parallel()

	// Both legacy shapes: the global flags the old binary had, in front, and the bare two-argument form.
	//
	// Deliberately not `s3://b /mnt --dry-run`. That never worked: the old binary parsed with
	// flag.CommandLine, which stops at the first positional, so the trailing flag landed in flag.Args()
	// and the invocation failed its own "exactly 2 arguments" check. Verified against HEAD~ rather than
	// assumed, because a compatibility test that pins a form the old binary rejected would be asserting a
	// new feature while claiming to protect users. TestAFlagAfterThePositionalArgumentsSaysWhy covers
	// what that form does now, which is fail with an explanation instead of an argument count.
	for _, args := range [][]string{
		{"--dry-run", "s3://my-bucket", "/mnt/point"},
		{"--dry-run", "--cache-size", "4GB", "s3://my-bucket", "/mnt/point"},
		{"-dry-run", "s3://my-bucket", "/mnt/point"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runArgs(t, args...)
			if code != exitOK {
				t.Fatalf("the legacy form exited %d, want 0: every pre-v0.11.0 invocation looks like "+
					"this, including the ones in README.md and in whatever scripts users have "+
					"written.\nstderr: %s", code, stderr)
			}
			if !strings.Contains(stdout, "s3://my-bucket") || !strings.Contains(stdout, "/mnt/point") {
				t.Errorf("the legacy form did not report the URI and mount point it would use: %q", stdout)
			}
		})
	}
}

// TestAFlagAfterThePositionalArgumentsSaysWhy is about a message, and the message is the whole point.
//
// Go's flag package stops parsing at the first non-flag argument, so `objectfs mount s3://b /mnt
// --foreground` does not set --foreground: it becomes a third positional. That is not a regression — the
// old binary did the same, and its error said only "Expected exactly 2 arguments", which sends the
// operator counting arguments rather than moving a flag. The failure is invisible otherwise, because the
// flag they typed is simply not applied.
func TestAFlagAfterThePositionalArgumentsSaysWhy(t *testing.T) {
	t.Parallel()

	code, _, stderr := runArgs(t, "mount", "s3://my-bucket", "/mnt/point", "--foreground")

	if code != exitUsage {
		t.Fatalf("exited %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "looks like a flag") {
		t.Errorf("the error does not point at the flag, so the operator counts arguments instead of "+
			"moving it: %q", stderr)
	}
	// And it prints the corrected line, which is what they will retype.
	if !strings.Contains(stderr, "objectfs mount --foreground s3://my-bucket /mnt/point") {
		t.Errorf("the error does not show the corrected command: %q", stderr)
	}
}

func TestMountSubcommandFormsThatShouldWork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		why  string
	}{
		{
			name: "the documented form",
			args: []string{"mount", "--dry-run", "s3://my-bucket", "/mnt/point"},
		},
		{
			name: "flags before the arguments",
			args: []string{"mount", "--dry-run", "--cache-size", "4GB", "s3://my-bucket", "/mnt/point"},
			why:  "the form README.md documents for tuning",
		},
		{
			name: "a mount point given as a flag",
			args: []string{"mount", "--dry-run", "--mount-point", "/mnt/point", "s3://my-bucket"},
			why: "the systemd shape: the unit passes --mount-point /mnt/objectfs/%i and the URI comes " +
				"from the config file — here from the argument, since this test has no file",
		},
		{
			name: "--foreground is accepted",
			args: []string{"mount", "--dry-run", "--foreground", "s3://my-bucket", "/mnt/point"},
			why: "configs/systemd/objectfs@.service passes it, and it named a flag that did not exist " +
				"for several releases, so the unit failed at exec",
		},
		{
			name: "--debug",
			args: []string{"mount", "--dry-run", "--debug", "s3://my-bucket", "/mnt/point"},
		},
		{
			name: "equals-form flags",
			args: []string{"mount", "--dry-run", "--log-level=WARN", "s3://my-bucket", "/mnt/point"},
		},
		{
			name: "single-dash flags",
			args: []string{"mount", "-dry-run", "-foreground", "s3://my-bucket", "/mnt/point"},
			why:  "Go's flag package accepts both, and scripts written against the old binary used one dash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runArgs(t, tt.args...)

			if code != exitOK {
				t.Fatalf("objectfs %s exited %d, want 0. %s\nstderr: %s",
					strings.Join(tt.args, " "), code, tt.why, stderr)
			}
			if !strings.Contains(stdout, "s3://my-bucket") {
				t.Errorf("objectfs %s did not report the URI it would mount: %q",
					strings.Join(tt.args, " "), stdout)
			}
		})
	}
}

func TestMountRejectsAnIncompleteInvocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantCode int
		mustSay  string
		why      string
	}{
		{
			name:     "nothing at all",
			args:     []string{"mount"},
			wantCode: exitUsage,
			mustSay:  "no storage URI",
			why: "and the message has to name both places it could come from, because the systemd case " +
				"gets it from the file and the interactive case from the argument list",
		},
		{
			name:     "a URI and no mount point",
			args:     []string{"mount", "s3://my-bucket"},
			wantCode: exitUsage,
			mustSay:  "no mount point",
			why: "not defaulted to anything: a mount point guessed wrong is a mount that succeeds " +
				"somewhere the operator is not looking",
		},
		{
			name:     "too many positional arguments",
			args:     []string{"mount", "s3://my-bucket", "/mnt/a", "/mnt/b"},
			wantCode: exitUsage,
			mustSay:  "at most",
		},
		{
			name:     "conflicting mount points",
			args:     []string{"mount", "--mount-point", "/mnt/a", "s3://my-bucket", "/mnt/b"},
			wantCode: exitUsage,
			mustSay:  "twice and differently",
			why: "the same setting written two ways. Preferring one silently would mount /mnt/a while " +
				"the command line says /mnt/b, which is unreadable from the invocation",
		},
		{
			name:     "an unknown flag",
			args:     []string{"mount", "--parallel-reads", "s3://my-bucket", "/mnt/point"},
			wantCode: exitUsage,
			mustSay:  "",
			why:      "flag prints its own message; what matters is that it is a usage error and not a mount",
		},
		{
			name:     "a URI this build cannot mount",
			args:     []string{"mount", "--dry-run", "gs://my-bucket", "/mnt/point"},
			wantCode: exitUsage,
			mustSay:  "gs",
			why: "a usage error rather than a failure, because the wrong thing is the command line and " +
				"nothing was attempted — the same URI in a config file exits 1 instead, which " +
				"TestDryRunRefusesAURIItCannotMount pins from both sources",
		},
		{
			name:     "an unparseable cache size",
			args:     []string{"mount", "--dry-run", "--cache-size", "4 gigs", "s3://b", "/mnt/point"},
			wantCode: exitFailure,
			mustSay:  "cache_size",
			why: "this is the one that used to be silent: any unparseable size became 1 GiB, so a typo " +
				"configured a cache a twentieth of the intended size and said nothing (#159)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, _, stderr := runArgs(t, tt.args...)

			if code != tt.wantCode {
				t.Fatalf("objectfs %s exited %d, want %d. %s\nstderr: %s",
					strings.Join(tt.args, " "), code, tt.wantCode, tt.why, stderr)
			}
			if tt.mustSay != "" && !strings.Contains(stderr, tt.mustSay) {
				t.Errorf("objectfs %s said %q, which does not contain %q. %s",
					strings.Join(tt.args, " "), stderr, tt.mustSay, tt.why)
			}
		})
	}
}

// TestDryRunRefusesAURIItCannotMount is what makes --dry-run worth running.
//
// The flag exists so an operator or a config-management run can check an invocation before committing to
// it, and it returns before the adapter is built — which is where the URI used to be validated. So
// `objectfs mount --dry-run gs://bucket /mnt` printed "Configuration is valid" for a URI nothing in this
// build can mount. A validating dry run that does not validate is worse than no dry run: it is a check
// that answers yes.
//
// Both sources, because they are different code paths and produce different exit codes: a URI on the
// command line is a usage error (nothing was attempted), one in a config file is a configuration error.
func TestDryRunRefusesAURIItCannotMount(t *testing.T) {
	t.Parallel()

	t.Run("from the command line", func(t *testing.T) {
		t.Parallel()

		code, stdout, stderr := runArgs(t, "mount", "--dry-run", "gs://my-bucket", "/mnt/point")

		if code == exitOK {
			t.Fatalf("--dry-run reported success for a gs:// URI: %q", stdout)
		}
		if strings.Contains(stdout, "valid") {
			t.Errorf("--dry-run printed a validity claim for a URI it rejected: %q", stdout)
		}
		if !strings.Contains(stderr, "only s3://") {
			t.Errorf("the error does not say which schemes this build mounts: %q", stderr)
		}
	})

	t.Run("from the config file", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "config.yaml")
		doc := `mount:
  uri: gs://my-bucket
  mount_point: /mnt/point
`
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		code, stdout, stderr := runArgs(t, "mount", "--config", path, "--dry-run")

		if code != exitFailure {
			t.Fatalf("--dry-run over a config file naming a gs:// URI exited %d, want %d. stdout: %q",
				code, exitFailure, stdout)
		}
		// The YAML key, because the operator's next action is to edit that line.
		if !strings.Contains(stderr, "mount.uri") {
			t.Errorf("the error does not name the key to edit: %q", stderr)
		}
	})
}

// TestMountReadsTheTargetFromAConfigFile is the systemd shape end to end, and the reason #134 and #135
// were one problem.
//
// `systemctl start objectfs@research-data` gives the unit only %i. The unit selects a per-instance
// config file with it and passes --config; there is no URI on the command line and there cannot be one,
// because one unit file serves every instance. So this invocation — a config file and nothing else — is
// the one that has to work, and it is exactly the one that did not.
func TestMountReadsTheTargetFromAConfigFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "research-data.yaml")
	doc := `mount:
  uri: s3://research-data/lab
  mount_point: /mnt/objectfs/research-data
storage:
  s3:
    region: us-west-2
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, stdout, stderr := runArgs(t, "mount", "--config", path, "--foreground", "--dry-run")
	if code != exitOK {
		t.Fatalf("objectfs mount --config <file> --foreground exited %d, want 0. This is the whole "+
			"systemd invocation.\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "s3://research-data/lab") {
		t.Errorf("the URI from the config file did not reach the mount: %q", stdout)
	}
	if !strings.Contains(stdout, "/mnt/objectfs/research-data") {
		t.Errorf("the mount point from the config file did not reach the mount: %q", stdout)
	}
}

// TestTheCommandLineBeatsTheConfigFile pins the precedence, in the direction that matters.
//
// A config file naming a bucket must still be pointable at a different one for a one-off, without
// editing the file — that is what makes `--config` usable interactively as well as from a unit. The
// reverse precedence would mean an operator's explicit argument was silently ignored in favor of a file
// they may not have read.
func TestTheCommandLineBeatsTheConfigFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	doc := `mount:
  uri: s3://from-the-file
  mount_point: /mnt/from-the-file
storage:
  s3:
    region: us-west-2
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, stdout, stderr := runArgs(t, "mount", "--config", path, "--dry-run",
		"s3://from-the-command-line", "/mnt/from-the-command-line")
	if code != exitOK {
		t.Fatalf("exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "s3://from-the-command-line") {
		t.Errorf("the config file's URI won over the command line's: %q", stdout)
	}
	if strings.Contains(stdout, "from-the-file") {
		t.Errorf("the config file's values reached the mount despite the command line naming "+
			"others: %q", stdout)
	}
}

func TestResolveMountTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		flags      mountFlags
		cfgURI     string
		cfgMount   string
		wantURI    string
		wantMount  string
		wantErrSay string
		why        string
	}{
		{
			name:      "both from the command line",
			flags:     mountFlags{storageURI: "s3://cli", mountArg: "/mnt/cli"},
			wantURI:   "s3://cli",
			wantMount: "/mnt/cli",
		},
		{
			name:      "both from the file",
			cfgURI:    "s3://file",
			cfgMount:  "/mnt/file",
			wantURI:   "s3://file",
			wantMount: "/mnt/file",
			why:       "the systemd template case",
		},
		{
			name:      "a URI from the command line and a mount point from the file",
			flags:     mountFlags{storageURI: "s3://cli"},
			cfgURI:    "s3://file",
			cfgMount:  "/mnt/file",
			wantURI:   "s3://cli",
			wantMount: "/mnt/file",
			why:       "mixed sources resolve per setting, not all-or-nothing per source",
		},
		{
			name:      "--mount-point beats the file",
			flags:     mountFlags{storageURI: "s3://cli", mountPoint: "/mnt/flag"},
			cfgMount:  "/mnt/file",
			wantURI:   "s3://cli",
			wantMount: "/mnt/flag",
		},
		{
			name:      "--mount-point and an identical argument agree",
			flags:     mountFlags{storageURI: "s3://cli", mountPoint: "/mnt/x", mountArg: "/mnt/x"},
			wantURI:   "s3://cli",
			wantMount: "/mnt/x",
			why: "a systemd unit that passes both from the same template variable is not making a " +
				"mistake, so agreeing values are not a conflict",
		},
		{
			name:       "--mount-point and a different argument conflict",
			flags:      mountFlags{storageURI: "s3://cli", mountPoint: "/mnt/a", mountArg: "/mnt/b"},
			wantErrSay: "twice and differently",
		},
		{
			name:       "nothing anywhere",
			wantErrSay: "no storage URI",
		},
		{
			name:       "a URI and no mount point anywhere",
			flags:      mountFlags{storageURI: "s3://cli"},
			wantErrSay: "no mount point",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.NewDefault()
			cfg.Mount.URI = tt.cfgURI
			cfg.Mount.MountPoint = tt.cfgMount

			flags := tt.flags
			gotURI, gotMount, err := resolveMountTarget(&flags, cfg)

			if tt.wantErrSay != "" {
				if err == nil {
					t.Fatalf("resolveMountTarget returned %q, %q; want an error saying %q. %s",
						gotURI, gotMount, tt.wantErrSay, tt.why)
				}
				if !strings.Contains(err.Error(), tt.wantErrSay) {
					t.Errorf("resolveMountTarget error = %q, want it to contain %q", err, tt.wantErrSay)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveMountTarget = %v, want %q %q. %s", err, tt.wantURI, tt.wantMount, tt.why)
			}
			if gotURI != tt.wantURI {
				t.Errorf("URI = %q, want %q. %s", gotURI, tt.wantURI, tt.why)
			}
			if gotMount != tt.wantMount {
				t.Errorf("mount point = %q, want %q. %s", gotMount, tt.wantMount, tt.why)
			}
		})
	}
}

func TestUnmountRequiresExactlyOnePath(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"unmount"}, {"unmount", "/mnt/a", "/mnt/b"}, {"umount"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()

			code, _, stderr := runArgs(t, args...)

			if code != exitUsage {
				t.Errorf("objectfs %s exited %d, want %d. stderr: %s",
					strings.Join(args, " "), code, exitUsage, stderr)
			}
		})
	}
}

// TestUnmountAcceptsBothSpellings pins that `umount` is an alias.
//
// Both are in an operator's fingers: `umount` is the program name on Linux and macOS, `unmount` is the
// word. Asserted on a path that is not a mount point, so what is checked is that the command was
// dispatched — it must fail because the path is not mounted, not because the command is unknown.
func TestUnmountAcceptsBothSpellings(t *testing.T) {
	t.Parallel()

	for _, spelling := range []string{"unmount", "umount"} {
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			code, _, stderr := runArgs(t, spelling, dir)

			if code != exitFailure {
				t.Errorf("objectfs %s <an ordinary directory> exited %d, want %d: nothing is mounted "+
					"there, so it cannot be unmounted", spelling, code, exitFailure)
			}
			if strings.Contains(stderr, "unknown command") {
				t.Fatalf("%q was not dispatched as a command: %s", spelling, stderr)
			}
			// The error has to be actionable, which for this command means naming what was tried. An
			// operator seeing only "exit status 1" from fusermount3 has nothing to do next.
			if !strings.Contains(stderr, "Every method was tried") {
				t.Errorf("objectfs %s did not report what it tried: %s", spelling, stderr)
			}
		})
	}
}

func TestValidateMountPoint(t *testing.T) {
	t.Parallel()

	t.Run("an empty directory", func(t *testing.T) {
		t.Parallel()

		if err := validateMountPoint(t.TempDir()); err != nil {
			t.Errorf("an empty directory was refused: %v", err)
		}
	})

	t.Run("a missing directory", func(t *testing.T) {
		t.Parallel()

		err := validateMountPoint(filepath.Join(t.TempDir(), "nope"))
		if err == nil {
			t.Fatal("a missing mount point was accepted")
		}
		// The message contains the command to fix it, because that is the operator's next action.
		if !strings.Contains(err.Error(), "mkdir -p") {
			t.Errorf("the error does not say how to create it: %v", err)
		}
	})

	t.Run("a file", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		if err := validateMountPoint(path); err == nil {
			t.Error("a regular file was accepted as a mount point")
		}
	})

	t.Run("a non-empty directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "important.txt"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		err := validateMountPoint(dir)
		if err == nil {
			t.Fatal("a non-empty directory was accepted; mounting over it hides what is already " +
				"there for the life of the mount, and an operator who does this to /home gets a " +
				"system whose users have no files rather than an error")
		}
		// Naming the file it found is what makes this actionable — the operator can see whether it is
		// something they care about or a stray .DS_Store.
		if !strings.Contains(err.Error(), "important.txt") {
			t.Errorf("the error does not name what it found: %v", err)
		}
	})
}

func TestApplyCommandLineOverrides(t *testing.T) {
	t.Parallel()

	t.Run("--debug wins over --log-level", func(t *testing.T) {
		t.Parallel()

		cfg := config.NewDefault()
		applyCommandLineOverrides(cfg, &mountFlags{logLevel: "ERROR", debug: true})

		if cfg.Global.LogLevel != "DEBUG" {
			t.Errorf("log level = %q, want DEBUG: the two flags are contradictory and --debug is the "+
				"more specific request, so the order they are applied in is a decision and not an "+
				"accident", cfg.Global.LogLevel)
		}
	})

	t.Run("an unset flag does not overwrite the file", func(t *testing.T) {
		t.Parallel()

		cfg := config.NewDefault()
		cfg.Global.LogLevel = "WARN"
		cfg.Performance.CacheSize = "8GB"
		cfg.Performance.MaxConcurrency = 42

		applyCommandLineOverrides(cfg, &mountFlags{})

		if cfg.Global.LogLevel != "WARN" {
			t.Errorf("log level = %q, want the file's WARN: an absent flag is not a request for the "+
				"default", cfg.Global.LogLevel)
		}
		if cfg.Performance.CacheSize != "8GB" {
			t.Errorf("cache size = %q, want the file's 8GB", cfg.Performance.CacheSize)
		}
		if cfg.Performance.MaxConcurrency != 42 {
			t.Errorf("max concurrency = %d, want the file's 42", cfg.Performance.MaxConcurrency)
		}
	})
}
