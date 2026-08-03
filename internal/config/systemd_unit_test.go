package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unitFile is the shipped systemd template, relative to the module root.
const unitFile = "configs/systemd/objectfs@.service"

// TestSystemdUnitInvokesTheRealBinary is the gate `systemd-analyze verify` cannot be.
//
// systemd-analyze checks that a unit is well-formed systemd. It does not run ExecStart, and it has no
// idea what flags the program accepts — so the unit this repository shipped before v0.11.0 passed it
// while calling `objectfs s3://%i /mnt/objectfs/%i`, a form that made the instance name and the
// bucket name one string, and `ExecStop=/bin/fusermount3 -u`, which is a program the unit has no way
// to know might not be installed. Both were wrong for a year in a file operators are told to copy
// into /etc/systemd/system.
//
// This checks the half that matters: every objectfs command line in the unit uses a subcommand the
// binary dispatches and flags the binary parses. Both sets are scraped from cmd/objectfs/main.go by
// the tests in docs_symbols_test.go, so this is a check against the binary rather than against a list
// somebody keeps up to date. A CI step running systemd-analyze is still worth having for the systemd
// half; it runs only on Linux, and this runs everywhere `go test` does.
func TestSystemdUnitInvokesTheRealBinary(t *testing.T) {
	t.Parallel()

	unit := readUnit(t)

	// The unit is not markdown, so the fenced-block machinery cannot reach it. Its Exec* lines are
	// command lines in exactly the sense parseInvocation means, though, so they go through the same
	// parser — one definition of "a flag this binary accepts", not two.
	//
	// Continuations are joined first, and that is not a nicety. systemd honors a trailing backslash,
	// and the shipped ExecStart uses one so the --mount-point line is readable. A loop over raw lines
	// sees `ExecStart=... --config ${OBJECTFS_CONFIG} \` and then a line starting with a space, which
	// has no Exec prefix and is skipped — so every flag after the backslash goes unchecked. Verified by
	// mutation: with the raw-line version of this loop, changing --mount-point to --mountpoint left the
	// test passing.
	var checked int

	for i, line := range joinContinuations(strings.Split(unit, "\n")) {
		directive, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || !strings.HasPrefix(directive, "Exec") {
			continue
		}

		// systemd's own prefixes: `-` ignores a failure, `@` overrides argv[0], `+` drops the sandbox.
		// Stripped rather than rejected, because a unit may legitimately use them.
		value = strings.TrimLeft(strings.TrimSpace(value), "-@+!:")

		binary, rest, _ := strings.Cut(value, " ")
		if filepath.Base(binary) != "objectfs" {
			continue
		}

		checked++

		invocation := parseInvocation(i+1, strings.TrimSpace(line), rest)

		if invocation.subcommand == "" {
			t.Errorf("%s:%d invokes objectfs with no subcommand:\n    %s\nThe bare form still works, "+
				"so this is not broken — but a unit file is the one place it should not be used. It "+
				"routes on whether the first argument looks like a URI, and a unit that relies on that "+
				"is a unit whose ExecStart changes meaning if %%i ever expands to a bare word.",
				unitFile, i+1, strings.TrimSpace(line))
		} else if !subcommands[invocation.subcommand] {
			t.Errorf("%s:%d uses the subcommand %q, which objectfs does not have:\n    %s\nThe set is: "+
				"%s. An unknown first word exits 2, so `systemctl start` fails with a usage error — and "+
				"for ExecStop, a filesystem stays mounted after `systemctl stop` reports success.",
				unitFile, i+1, invocation.subcommand, strings.TrimSpace(line),
				strings.Join(sortedKeys(subcommands), ", "))
		}

		for _, flag := range invocation.flags {
			if cliFlags[flag] {
				continue
			}

			t.Errorf("%s:%d passes --%s, which objectfs does not parse:\n    %s\nGo's flag package "+
				"exits 1 on an unrecognized flag, so this unit cannot start. The parsed set is: %s.",
				unitFile, i+1, flag, strings.TrimSpace(line), strings.Join(sortedKeys(cliFlags), ", "))
		}
	}

	if checked == 0 {
		t.Fatalf("found no objectfs invocations in %s. Either the unit stopped calling the binary, or "+
			"the Exec* parsing here no longer matches how it does — and this test reports success "+
			"either way, which is the failure mode it exists to prevent", unitFile)
	}
}

// TestSystemdUnitStopsByUnmounting asserts ExecStop is `objectfs unmount` and not a FUSE helper.
//
// It is a separate test because it is a separate claim, and the claim is about data. ExecStop is the
// flush: SIGTERM makes the mount process unmount, which writes buffered ranges to S3. Calling
// `fusermount3 -u` from the unit instead — which is what shipped — works on a machine that has
// libfuse 3 and fails silently in the two cases that matter, a minimal image where it is absent and a
// libfuse 2 system where it is spelled `fusermount`. In both, systemd gets a bare non-zero exit,
// falls through to SIGKILL after TimeoutStopSec, and buffered writes are lost with `systemctl stop`
// reporting nothing an operator can act on.
func TestSystemdUnitStopsByUnmounting(t *testing.T) {
	t.Parallel()

	unit := readUnit(t)

	var stop string

	for line := range strings.SplitSeq(unit, "\n") {
		if directive, value, found := strings.Cut(strings.TrimSpace(line), "="); found &&
			directive == "ExecStop" {
			stop = strings.TrimSpace(value)
		}
	}

	if stop == "" {
		t.Fatalf("%s has no ExecStop. Without it, `systemctl stop` sends SIGTERM and nothing runs the "+
			"unmount if that process is already gone — the mount survives its own service", unitFile)
	}

	if !strings.Contains(stop, "objectfs") {
		t.Errorf("%s: ExecStop is %q, which does not run objectfs. `objectfs unmount` tries the FUSE "+
			"helper, its libfuse 2 spelling, umount, and umount(2), and reports which ran and what is "+
			"holding the mount open; a single helper invocation gives systemd a bare exit status.",
			unitFile, stop)
	}

	for _, helper := range []string{"fusermount", "fusermount3", "/bin/umount", "diskutil"} {
		if strings.Contains(stop, helper) {
			t.Errorf("%s: ExecStop calls %s directly (%q). That is the first thing objectfs tries "+
				"itself, but it is one of several candidates — absent on a minimal image, spelled "+
				"differently on libfuse 2, and unnecessary for a root caller who can use umount(2). "+
				"Use `objectfs unmount`.", unitFile, helper, stop)
		}
	}

	// The flags that report a finished unmount before it has finished. Asserted here as well as in
	// internal/fuse, because this file is the one an operator edits: adding `-z` to make a stubborn
	// stop succeed is a plausible thing to do and would detach the mount with writes in flight.
	for _, flag := range []string{" -z", " -l", " -f", " --lazy", " --force"} {
		if strings.Contains(stop, flag) {
			t.Errorf("%s: ExecStop passes%s (%q). A lazy or forced unmount detaches the name while the "+
				"filesystem keeps serving open files, so `systemctl stop` reports a finished unmount "+
				"with writes still in flight.", unitFile, flag, stop)
		}
	}
}

// TestSystemdUnitAllowsTimeToFlush asserts the stop timeout is long enough to be one.
//
// TimeoutStopSec is how long systemd waits after SIGTERM before SIGKILL, and the work happening in
// that window is the flush of every dirty range to S3. A short value is not a conservative choice: it
// is a SIGKILL through buffered data, on a slow link or a large file, reported as a stop.
func TestSystemdUnitAllowsTimeToFlush(t *testing.T) {
	t.Parallel()

	unit := readUnit(t)

	if !strings.Contains(unit, "TimeoutStopSec=") {
		t.Errorf("%s does not set TimeoutStopSec. systemd's default is 90s, which is fine — but this "+
			"is the flush window, and a value it is worth stating is a value worth stating in the "+
			"file rather than inheriting from a default that a drop-in can change silently", unitFile)
	}

	// Restart=always remounts a filesystem an operator has just taken down, because a clean
	// `systemctl stop` counts as an exit.
	if strings.Contains(unit, "Restart=always") {
		t.Errorf("%s sets Restart=always, so a clean `systemctl stop` is followed by a remount. "+
			"Restart=on-failure is what this wants", unitFile)
	}
}

// readUnit returns the shipped systemd template.
func readUnit(t *testing.T) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), unitFile)

	body, err := os.ReadFile(path) //nolint:gosec // a path built from the module root this test located
	if err != nil {
		t.Fatalf("reading %s, which is the unit operators are told to install: %v", unitFile, err)
	}

	return string(body)
}
