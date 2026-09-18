package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runArgs calls run with captured output, the way cmd/objectfs's tests do.
func runArgs(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr)

	return code, stdout.String(), stderr.String()
}

func TestParseArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string

		wantDevice     string
		wantMountPoint string
		wantOptions    []string
		wantFake       bool
		wantSloppy     bool
		wantVerbose    bool
		wantHelp       bool

		// wantErr is a substring of the error, so that a test asserting a rejection says which rejection.
		wantErr string
	}{
		{
			name:           "the two positionals alone",
			args:           []string{"s3://bucket", "/mnt/data"},
			wantDevice:     "s3://bucket",
			wantMountPoint: "/mnt/data",
		},
		{
			// What mount(8) actually passes: the positionals first, then -o, then the redundant -t.
			name:           "mount(8)'s own calling convention",
			args:           []string{"s3://bucket", "/mnt/data", "-o", "_netdev,config=/etc/o.yaml", "-t", "objectfs"},
			wantDevice:     "s3://bucket",
			wantMountPoint: "/mnt/data",
			wantOptions:    []string{"_netdev", "config=/etc/o.yaml"},
		},
		{
			name:           "combined short flags",
			args:           []string{"-sfnv", "s3://bucket", "/mnt/data"},
			wantDevice:     "s3://bucket",
			wantMountPoint: "/mnt/data",
			wantFake:       true,
			wantSloppy:     true,
			wantVerbose:    true,
		},
		{
			name:           "separate short flags",
			args:           []string{"s3://bucket", "/mnt/data", "-f", "-v"},
			wantDevice:     "s3://bucket",
			wantMountPoint: "/mnt/data",
			wantFake:       true,
			wantVerbose:    true,
		},
		{
			// -oro would be five short flags to a naive reader, and four of them would be unknown.
			name:           "the attached -o form",
			args:           []string{"s3://bucket", "/mnt/data", "-odebug,log-level=DEBUG"},
			wantDevice:     "s3://bucket",
			wantMountPoint: "/mnt/data",
			wantOptions:    []string{"debug", "log-level=DEBUG"},
		},
		{
			name:           "repeated -o accumulates",
			args:           []string{"s3://bucket", "/mnt/data", "-o", "_netdev", "-o", "debug"},
			wantDevice:     "s3://bucket",
			wantMountPoint: "/mnt/data",
			wantOptions:    []string{"_netdev", "debug"},
		},
		{
			name:           "empty option list elements are dropped",
			args:           []string{"s3://bucket", "/mnt/data", "-o", "defaults,,"},
			wantDevice:     "s3://bucket",
			wantMountPoint: "/mnt/data",
			wantOptions:    []string{"defaults"},
		},
		{
			name:           "the attached -t form",
			args:           []string{"s3://bucket", "/mnt/data", "-tobjectfs"},
			wantDevice:     "s3://bucket",
			wantMountPoint: "/mnt/data",
		},
		{
			name:     "help",
			args:     []string{"--help"},
			wantHelp: true,
		},
		{
			// --help alone has no positionals, and must not be rejected for that.
			name:     "help wins over the missing positionals",
			args:     []string{"-h"},
			wantHelp: true,
		},
		{
			name:    "no arguments",
			args:    nil,
			wantErr: "needs a storage URI and a mount point",
		},
		{
			name:    "only a device",
			args:    []string{"s3://bucket"},
			wantErr: `needs a mount point after "s3://bucket"`,
		},
		{
			name:    "a third positional",
			args:    []string{"s3://bucket", "/mnt/data", "/mnt/other"},
			wantErr: "got 3 arguments",
		},
		{
			name:    "-o with nothing after it",
			args:    []string{"s3://bucket", "/mnt/data", "-o"},
			wantErr: "-o needs an option list",
		},
		{
			name:    "-t with nothing after it",
			args:    []string{"s3://bucket", "/mnt/data", "-t"},
			wantErr: "-t needs a type",
		},
		{
			name:    "an unknown short flag",
			args:    []string{"-z", "s3://bucket", "/mnt/data"},
			wantErr: "unknown flag -z",
		},
		{
			// The unknown letter inside a combined run has to be named, not swallowed by the letters
			// around it that are fine.
			name:    "an unknown letter inside a combined run",
			args:    []string{"-svz", "s3://bucket", "/mnt/data"},
			wantErr: "unknown flag -z",
		},
		{
			name:    "an unknown long flag",
			args:    []string{"--read-only", "s3://bucket", "/mnt/data"},
			wantErr: "unknown flag --read-only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inv, err := parseArgs(tt.args)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseArgs(%q) succeeded; want an error containing %q", tt.args, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("parseArgs(%q) failed with %q; want it to contain %q", tt.args, err, tt.wantErr)
				}

				return
			}
			if err != nil {
				t.Fatalf("parseArgs(%q): %v", tt.args, err)
			}

			if inv.device != tt.wantDevice {
				t.Errorf("device = %q, want %q", inv.device, tt.wantDevice)
			}
			if inv.mountPoint != tt.wantMountPoint {
				t.Errorf("mountPoint = %q, want %q", inv.mountPoint, tt.wantMountPoint)
			}
			if got, want := strings.Join(inv.options, ","), strings.Join(tt.wantOptions, ","); got != want {
				t.Errorf("options = %q, want %q", got, want)
			}
			if inv.fake != tt.wantFake {
				t.Errorf("fake = %v, want %v", inv.fake, tt.wantFake)
			}
			if inv.sloppy != tt.wantSloppy {
				t.Errorf("sloppy = %v, want %v", inv.sloppy, tt.wantSloppy)
			}
			if inv.verbose != tt.wantVerbose {
				t.Errorf("verbose = %v, want %v", inv.verbose, tt.wantVerbose)
			}
			if inv.help != tt.wantHelp {
				t.Errorf("help = %v, want %v", inv.help, tt.wantHelp)
			}
		})
	}
}

func TestTranslate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options []string
		fake    bool
		sloppy  bool

		// wantArgv is the whole `objectfs mount` command line, asserted exactly: the order matters,
		// because objectfs's own flag parser stops at the first positional argument.
		wantArgv    []string
		wantTimeout time.Duration
		wantWarning string

		// wantErr is a substring distinctive to one refusal. Asserted rather than just "it failed",
		// because every wrong reason to reject an option is still a rejection.
		wantErr string
	}{
		{
			name:        "no options",
			wantArgv:    []string{"mount", "s3://bucket", "/mnt/data"},
			wantTimeout: defaultTimeout,
		},
		{
			name:        "every translated option",
			options:     []string{"config=/etc/o.yaml", "cache-size=4GB", "log-level=WARN", "max-concurrency=32", "debug"},
			wantArgv:    []string{"mount", "--config", "/etc/o.yaml", "--cache-size", "4GB", "--log-level", "WARN", "--max-concurrency", "32", "--debug", "s3://bucket", "/mnt/data"},
			wantTimeout: defaultTimeout,
		},
		{
			name:        "flags land before the positionals",
			options:     []string{"config=/etc/o.yaml"},
			wantArgv:    []string{"mount", "--config", "/etc/o.yaml", "s3://bucket", "/mnt/data"},
			wantTimeout: defaultTimeout,
		},
		{
			// The whole point of the ignore list: mount(8) hands a helper the options it reads itself.
			name:        "the options mount(8), mount -a and systemd read for themselves",
			options:     []string{"_netdev", "noauto", "nofail", "defaults", "rw", "users", "owner", "group", "comment=research data", "x-systemd.automount", "X-mount.mkdir"},
			wantArgv:    []string{"mount", "s3://bucket", "/mnt/data"},
			wantTimeout: defaultTimeout,
		},
		{
			name:        "mount-timeout is consumed here and not passed on",
			options:     []string{"mount-timeout=90s", "config=/etc/o.yaml"},
			wantArgv:    []string{"mount", "--config", "/etc/o.yaml", "s3://bucket", "/mnt/data"},
			wantTimeout: 90 * time.Second,
		},
		{
			name:        "-f becomes --dry-run, ahead of the positionals",
			options:     []string{"config=/etc/o.yaml"},
			fake:        true,
			wantArgv:    []string{"mount", "--dry-run", "--config", "/etc/o.yaml", "s3://bucket", "/mnt/data"},
			wantTimeout: defaultTimeout,
		},

		// The three #136 specifies and ObjectFS cannot honor. Each asserts a phrase only its own message
		// has, so a refusal that fired for some other reason fails.
		{
			name:    "ro is refused, naming the absent flag",
			options: []string{"ro"},
			wantErr: "no --read-only flag",
		},
		{
			name:    "uid is refused, naming the absent remapping",
			options: []string{"uid=1000"},
			wantErr: "does not remap ownership",
		},
		{
			name:    "gid is refused",
			options: []string{"gid=1000"},
			wantErr: "the gid of the",
		},
		{
			// -s is "ignore options you do not recognize", which is not "ignore options you recognize and
			// cannot honor". If this ever passes, an fstab entry saying ro mounts read-write in silence.
			name:    "-s does not make ro acceptable",
			options: []string{"ro"},
			sloppy:  true,
			wantErr: "no --read-only flag",
		},
		{
			name:    "bind is refused",
			options: []string{"bind"},
			wantErr: "bind mount does not go through",
		},

		{
			name:    "an unrecognized option is refused by default",
			options: []string{"noexec"},
			wantErr: `unrecognized option "noexec"`,
		},
		{
			name:        "-s downgrades an unrecognized option to a warning",
			options:     []string{"noexec", "config=/etc/o.yaml"},
			sloppy:      true,
			wantArgv:    []string{"mount", "--config", "/etc/o.yaml", "s3://bucket", "/mnt/data"},
			wantTimeout: defaultTimeout,
			wantWarning: `ignoring unrecognized option "noexec"`,
		},

		{
			name:    "a value option with no value",
			options: []string{"config"},
			wantErr: `option "config" needs a value`,
		},
		{
			name:    "a value option with an empty value",
			options: []string{"config="},
			wantErr: `needs a value`,
		},
		{
			name:    "a boolean option given a value",
			options: []string{"debug=true"},
			wantErr: "takes no value",
		},
		{
			name:    "mount-timeout with no value",
			options: []string{"mount-timeout"},
			wantErr: "needs a value, as mount-timeout=<duration>",
		},
		{
			name:    "mount-timeout that is not a duration",
			options: []string{"mount-timeout=90"},
			wantErr: "is not a duration",
		},
		{
			// A zero or negative timeout would fail every mount before it was attempted, which looks
			// exactly like a broken filesystem rather than a broken fstab line.
			name:    "a zero mount-timeout",
			options: []string{"mount-timeout=0s"},
			wantErr: "would fail the mount before it was attempted",
		},
		{
			name:    "a negative mount-timeout",
			options: []string{"mount-timeout=-5s"},
			wantErr: "would fail the mount before it was attempted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inv := invocation{
				device:     "s3://bucket",
				mountPoint: "/mnt/data",
				options:    tt.options,
				fake:       tt.fake,
				sloppy:     tt.sloppy,
			}

			p, warnings, err := translate(inv)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("translate(%q) succeeded with argv %q; want an error containing %q",
						tt.options, p.argv, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("translate(%q) failed with %q; want it to contain %q",
						tt.options, err, tt.wantErr)
				}

				return
			}
			if err != nil {
				t.Fatalf("translate(%q): %v", tt.options, err)
			}

			if got, want := strings.Join(p.argv, " "), strings.Join(tt.wantArgv, " "); got != want {
				t.Errorf("argv:\n got %q\nwant %q", got, want)
			}
			if p.timeout != tt.wantTimeout {
				t.Errorf("timeout = %s, want %s", p.timeout, tt.wantTimeout)
			}

			joined := strings.Join(warnings, "; ")
			if tt.wantWarning == "" {
				if len(warnings) != 0 {
					t.Errorf("unexpected warnings: %q", joined)
				}
			} else if !strings.Contains(joined, tt.wantWarning) {
				t.Errorf("warnings = %q, want one containing %q", joined, tt.wantWarning)
			}
		})
	}
}

// TestTranslateRefusalsNameTheRemedy asserts every refusal says what to do instead.
//
// A message that only says no leaves an operator with an fstab entry they cannot fix without reading this
// source, which at boot means a machine that will not come up and a message that does not help.
func TestTranslateRefusalsNameTheRemedy(t *testing.T) {
	t.Parallel()

	for name, opt := range optionTable {
		if opt.kind != kindRefuse {
			continue
		}

		if opt.why == "" {
			t.Errorf("option %q is refused with no reason", name)

			continue
		}

		// "Remove" for the three an operator can simply delete; "does not go through" for bind, which is
		// not an fstab line this helper can ever serve.
		if !strings.Contains(opt.why, "Remove") && !strings.Contains(opt.why, "does not go through") {
			t.Errorf("option %q is refused with %q, which does not say what to do instead", name, opt.why)
		}
	}
}

// TestOptionTableFlagsExist couples this helper's table to `objectfs mount`'s actual flag set.
//
// An entry naming a flag objectfs does not have would translate cleanly, pass every test above, and fail
// at the moment of mounting with objectfs's own "flag provided but not defined" — inside a detached child,
// relayed through a log file, at boot. The five names here are newMountFlagSet's; if a flag is renamed
// there, this is what says so.
func TestOptionTableFlagsExist(t *testing.T) {
	t.Parallel()

	// Deliberately a literal and not a call into cmd/objectfs: package main cannot be imported, and a
	// second copy that has to be updated by hand is the point — it fails when the two drift.
	mountFlags := map[string]bool{
		"--config": true, "--foreground": true, "--mount-point": true, "--log-level": true,
		"--cache-size": true, "--max-concurrency": true, "--dry-run": true, "--debug": true,
	}

	for name, opt := range optionTable {
		switch opt.kind {
		case kindValue, kindBool:
			if !mountFlags[opt.flag] {
				t.Errorf("option %q translates to %q, which is not a flag `objectfs mount` accepts",
					name, opt.flag)
			}
		case kindIgnore, kindLocal, kindRefuse:
			if opt.flag != "" {
				t.Errorf("option %q is not translated but names the flag %q", name, opt.flag)
			}
		}
	}
}

func TestSplitOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{"a,,b", []string{"a", "b"}},
		{",", nil},
		{" a , b ", []string{"a", "b"}},
		{"config=/etc/a,b", []string{"config=/etc/a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			if got := strings.Join(splitOptions(tt.in), "|"); got != strings.Join(tt.want, "|") {
				t.Errorf("splitOptions(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Not parallel, and neither are its subtests: t.Setenv makes a process-wide change, and the testing
// package refuses to let a test that calls it run alongside anything.
func TestObjectfsPath(t *testing.T) {
	t.Run("OBJECTFS_BINARY wins", func(t *testing.T) {
		binary := fakeObjectfs(t, "exit 0")
		t.Setenv("OBJECTFS_BINARY", binary)

		got, err := objectfsPath()
		if err != nil {
			t.Fatalf("objectfsPath: %v", err)
		}
		if got != binary {
			t.Errorf("objectfsPath() = %q, want %q", got, binary)
		}
	})

	t.Run("OBJECTFS_BINARY pointing at nothing says so", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope")
		t.Setenv("OBJECTFS_BINARY", missing)

		_, err := objectfsPath()
		if err == nil {
			t.Fatal("objectfsPath succeeded with OBJECTFS_BINARY pointing at a missing file")
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error %q does not name the path it was given", err)
		}
	})

	t.Run("OBJECTFS_BINARY pointing at a directory says so", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("OBJECTFS_BINARY", dir)

		_, err := objectfsPath()
		if err == nil {
			t.Fatal("objectfsPath succeeded with OBJECTFS_BINARY pointing at a directory")
		}
		if !strings.Contains(err.Error(), "is a directory") {
			t.Errorf("error = %q, want it to say the path is a directory", err)
		}
	})

	t.Run("a non-executable file is not the binary", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "objectfs")
		if err := os.WriteFile(plain, []byte("not a program"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OBJECTFS_BINARY", plain)

		_, err := objectfsPath()
		if err == nil {
			t.Fatal("objectfsPath accepted a non-executable file")
		}
		if !strings.Contains(err.Error(), "not executable") {
			t.Errorf("error = %q, want it to say the file is not executable", err)
		}
	})

	t.Run("the failure names every path tried", func(t *testing.T) {
		// An empty PATH and an empty OBJECTFS_BINARY, so nothing is found and the message is all there is.
		t.Setenv("OBJECTFS_BINARY", "")
		t.Setenv("PATH", t.TempDir())

		_, err := objectfsPath()
		if err == nil {
			t.Skip("an objectfs binary is installed at a candidate path on this machine")
		}

		for _, want := range append([]string{"on PATH", "OBJECTFS_BINARY"}, candidatePaths...) {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

// TestRunRefusesBeforeForking is the property that makes the refusals worth having: an option that cannot
// be honored is reported as a bad invocation, exit 1, with no child started and no mount attempted.
//
// OBJECTFS_BINARY points at a script that would fail loudly if it ran, so a refusal that leaked through to
// the mount shows up as the wrong exit code rather than as a passing test.
func TestRunRefusesBeforeForking(t *testing.T) {
	t.Setenv("OBJECTFS_BINARY", fakeObjectfs(t, `echo "the mount should not have been attempted" >&2; exit 0`))

	// Nothing here should reach a log, since nothing here should reach a mount. Redirected anyway, so that
	// a regression which does reach one writes into this test's temp directory rather than into /var/log.
	withLogDir(t, t.TempDir())

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "ro",
			args:    []string{"s3://bucket", "/tmp", "-o", "ro"},
			wantErr: "no --read-only flag",
		},
		{
			name:    "uid",
			args:    []string{"s3://bucket", "/tmp", "-o", "uid=1000"},
			wantErr: "does not remap ownership",
		},
		{
			name:    "an unrecognized option",
			args:    []string{"s3://bucket", "/tmp", "-o", "noexec"},
			wantErr: "unrecognized option",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, tt.args...)

			// exitUsage, not exitMountFailed: nothing was attempted, and mount(8) distinguishes the two.
			if code != exitUsage {
				t.Errorf("exit code = %d, want %d (a bad invocation, not a failed mount)", code, exitUsage)
			}
			if !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantErr)
			}
			if strings.Contains(stderr, "should not have been attempted") {
				t.Error("the refused mount was attempted anyway")
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
		})
	}
}

func TestRunUsage(t *testing.T) {
	t.Parallel()

	t.Run("--help goes to stdout and exits 0", func(t *testing.T) {
		t.Parallel()

		code, stdout, stderr := runArgs(t, "--help")
		if code != exitOK {
			t.Errorf("exit code = %d, want %d", code, exitOK)
		}
		if !strings.Contains(stdout, "Usage: mount.objectfs") {
			t.Errorf("stdout does not contain usage: %q", stdout)
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want nothing", stderr)
		}
	})

	t.Run("a bad invocation goes to stderr and exits 1", func(t *testing.T) {
		t.Parallel()

		code, stdout, stderr := runArgs(t)
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if !strings.Contains(stderr, "needs a storage URI and a mount point") {
			t.Errorf("stderr does not explain the problem: %q", stderr)
		}
		if !strings.Contains(stderr, "Usage: mount.objectfs") {
			t.Errorf("stderr does not contain usage: %q", stderr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing", stdout)
		}
	})

	// The help text has to name every option the table translates, or an operator reading -o's list is
	// reading a subset of what works.
	t.Run("usage documents every translated option", func(t *testing.T) {
		t.Parallel()

		_, stdout, _ := runArgs(t, "--help")

		for name, opt := range optionTable {
			if opt.kind != kindValue && opt.kind != kindBool && opt.kind != kindLocal {
				continue
			}
			if !strings.Contains(stdout, name) {
				t.Errorf("usage does not mention the %q option", name)
			}
		}
	})
}

// TestRunFakeMount exercises -f, which mount(8) means as "check everything and mount nothing".
func TestRunFakeMount(t *testing.T) {
	t.Run("a fake mount that would succeed", func(t *testing.T) {
		// The script asserts its own arguments, so that a translation which produced the wrong command
		// line fails here rather than passing on the exit code alone.
		t.Setenv("OBJECTFS_BINARY", fakeObjectfs(t, `
			case "$*" in
			  "mount --dry-run --config /etc/o.yaml s3://bucket /mnt/data") exit 0 ;;
			  *) echo "unexpected argv: $*" >&2; exit 64 ;;
			esac`))

		code, _, stderr := runArgs(t, "s3://bucket", "/mnt/data", "-f", "-o", "config=/etc/o.yaml")
		if code != exitOK {
			t.Errorf("exit code = %d, want %d; stderr: %s", code, exitOK, stderr)
		}
	})

	t.Run("a fake mount that would fail is exit 32", func(t *testing.T) {
		t.Setenv("OBJECTFS_BINARY", fakeObjectfs(t, `echo "no such bucket" >&2; exit 1`))

		code, _, stderr := runArgs(t, "s3://bucket", "/mnt/data", "-f")
		if code != exitMountFailed {
			t.Errorf("exit code = %d, want %d", code, exitMountFailed)
		}
		// The child's own diagnosis is relayed, not replaced: -f runs in the foreground precisely so the
		// operator sees what objectfs said.
		if !strings.Contains(stderr, "no such bucket") {
			t.Errorf("stderr does not relay what objectfs said: %q", stderr)
		}
		if !strings.Contains(stderr, "would not mount") {
			t.Errorf("stderr = %q, want it to say the fake mount failed", stderr)
		}
	})

	t.Run("a missing binary is a failed mount, not a bad invocation", func(t *testing.T) {
		t.Setenv("OBJECTFS_BINARY", "")
		t.Setenv("PATH", t.TempDir())

		code, _, stderr := runArgs(t, "s3://bucket", "/mnt/data", "-f")
		if code == exitOK {
			t.Skip("an objectfs binary is installed at a candidate path on this machine")
		}
		if code != exitMountFailed {
			t.Errorf("exit code = %d, want %d", code, exitMountFailed)
		}
		if !strings.Contains(stderr, "cannot find the objectfs binary") {
			t.Errorf("stderr = %q, want it to say the binary was not found", stderr)
		}
	})

	t.Run("-v prints the command line it will run", func(t *testing.T) {
		t.Setenv("OBJECTFS_BINARY", fakeObjectfs(t, "exit 0"))

		_, _, stderr := runArgs(t, "s3://bucket", "/mnt/data", "-fv", "-o", "debug")
		if !strings.Contains(stderr, "mount --dry-run --debug s3://bucket /mnt/data") {
			t.Errorf("stderr = %q, want it to show the objectfs command line", stderr)
		}
	})
}

// fakeObjectfs writes a shell script standing in for the objectfs binary and returns its path.
func fakeObjectfs(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "objectfs")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("writing the fake objectfs: %v", err)
	}

	return path
}
