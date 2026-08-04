package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFuzzSmokeScriptDistinguishesCounterexamplesFromShutdownNoise exercises
// .github/scripts/fuzz-smoke.sh against a stub `go` and asserts what it decides.
//
// The script exists because `go test -fuzz` exits non-zero for two unrelated reasons — it found a
// counterexample, or its own coordinator lost a shutdown race — and ~1 CI run in 10 was the second
// one (#218). A gate that fails on noise trains people to re-run it without reading it, which is
// worse than no gate; a gate that tolerates noise too broadly swallows real crashers, which is
// worse still. So the script's discrimination *is* the deliverable, and it needs a test rather than
// the observation that CI stopped being red.
//
// A stub `go` on PATH, not the real one: reproducing the coordinator race takes ~10 fuzzing runs on
// average and cannot be asked for on demand, and a real crasher would mean committing a broken
// target. Both are trivially expressible as output plus an exit status, which is all the script
// reads. This is the seam the script was written against — corpus paths are derived from the
// package argument rather than from `go list` precisely so the stub does not have to implement `go
// list` too.
//
// Each case asserts the exit status *and* a fragment of the reason, because the two failure
// verdicts ("counterexample" and "unexplained") are both exit 1 and confusing them would hide a
// regression where a real find is reported as an unexplained failure.
func TestFuzzSmokeScriptDistinguishesCounterexamplesFromShutdownNoise(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH; skipping the fuzz-smoke script gate")
	}

	script := filepath.Join(repoRoot(t), ".github", "scripts", "fuzz-smoke.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("stat %s: %v", script, err)
	}

	// The budget the script is invoked with, in seconds. The stubs return instantly regardless: the
	// script reads the elapsed time out of `go test`'s own `--- FAIL: <Target> (60.11s)` line rather
	// than off the wall clock, so a case can claim to have run for a minute without the test taking
	// one. That is also why it reads that line — wall clock includes compilation, so a target that
	// failed instantly inside a slow build would clear a wall-clock floor.
	const budget = "60"

	tests := []struct {
		name string
		// stub is the body of the case arm in the fake `go`. It runs with the scratch directory as
		// its working directory.
		stub     string
		wantExit int
		wantLog  string
	}{
		{
			name: "clean pass",
			stub: `echo "fuzz: elapsed: 60s, execs: 3000 (50/sec), new interesting: 0 (total: 1)"
				echo PASS
				exit 0`,
			wantExit: 0,
			wantLog:  "fuzz-smoke: ok",
		},
		{
			// The #218 shape: a deadline error at the deadline, no new inputs. The only thing the
			// script is meant to tolerate on a non-zero exit besides preemption.
			name: "coordinator shutdown race at the deadline",
			stub: `echo "fuzz: elapsed: 1m0s, execs: 194204 (3323/sec), new interesting: 1 (total: 24)"
				echo "--- FAIL: FuzzThing (60.11s)"
				echo "    context deadline exceeded"
				echo FAIL
				exit 1`,
			wantExit: 0,
			wantLog:  "shutdown race",
		},
		{
			// Same message, arriving immediately. Something other than the shutdown race is wearing
			// its clothes — a canceled context inside the target, say — and it must not be
			// tolerated just because the string matches.
			name: "deadline message too early to be the race",
			stub: `echo "--- FAIL: FuzzThing (0.01s)"
				echo "    context deadline exceeded"
				echo FAIL
				exit 1`,
			wantExit: 1,
			wantLog:  "too early to be the shutdown race",
		},
		{
			// A real find, announced in the output. Note the stub *also* prints the tolerated
			// deadline message, because that is what a genuine crasher discovered late in the run
			// looks like: the counterexample check has to win.
			name: "counterexample named in the output",
			stub: `echo "--- FAIL: FuzzThing (59.8s)"
				echo "    Failing input written to testdata/fuzz/FuzzThing/bbbb"
				echo "    context deadline exceeded"
				echo FAIL
				exit 1`,
			wantExit: 1,
			wantLog:  "counterexample",
		},
		{
			// A real find that only left a file — the shape `go test` produces when a worker dies
			// while minimizing, which prints no "Failing input written to" line. Keyed on the
			// filesystem, not on a message, so this is caught even when the wording changes.
			name: "counterexample only on disk",
			stub: `echo "new" > internal/target/testdata/fuzz/FuzzThing/cccc
				echo "--- FAIL: FuzzThing (60.11s)"
				echo "    context deadline exceeded"
				echo FAIL
				exit 1`,
			wantExit: 1,
			wantLog:  "that is a counterexample",
		},
		{
			name: "panic alongside the deadline message",
			stub: `echo "panic: runtime error: index out of range [5]"
				echo "--- FAIL: FuzzThing (60.11s)"
				echo "    context deadline exceeded"
				echo FAIL
				exit 1`,
			wantExit: 1,
			wantLog:  "a real failure, not a shutdown race",
		},
		{
			name: "data race alongside the deadline message",
			stub: `echo "WARNING: DATA RACE"
				echo "--- FAIL: FuzzThing (60.11s)"
				echo "    context deadline exceeded"
				echo FAIL
				exit 1`,
			wantExit: 1,
			wantLog:  "a real failure, not a shutdown race",
		},
		{
			// A seed-corpus input failing is a real failure that reports no *new* input, because
			// the input is already committed. It surfaces as a nested `--- FAIL:` subtest, which is
			// why the script counts subtest failures separately.
			name: "committed seed input fails",
			stub: `echo "--- FAIL: FuzzThing (60.11s)"
				echo "    --- FAIL: FuzzThing/aaaa (0.00s)"
				echo "        fuzz_test.go:9: mismatch"
				echo "    context deadline exceeded"
				echo FAIL
				exit 1`,
			wantExit: 1,
			wantLog:  "no subtest failures",
		},
		{
			// A build failure, a vet failure, a missing target: non-zero with no `--- FAIL:` line at
			// all. Must fail, and must say it does not understand why rather than guessing.
			name: "non-zero with no explanation",
			stub: `echo "FAIL	example/internal/target [build failed]"
				exit 1`,
			wantExit: 1,
			wantLog:  "expected exactly one",
		},
		{
			name: "runner preempted",
			stub: `echo "Error: The runner has received a shutdown signal."
				exit 143`,
			wantExit: 0,
			wantLog:  "preempted",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scratch := t.TempDir()

			corpus := filepath.Join(scratch, "internal", "target", "testdata", "fuzz", "FuzzThing")
			if err := os.MkdirAll(corpus, 0o750); err != nil {
				t.Fatalf("mkdir corpus: %v", err)
			}

			// A committed seed, so "no new files" is a real comparison against a non-empty set
			// rather than against nothing.
			if err := os.WriteFile(filepath.Join(corpus, "aaaa"), []byte("seed"), 0o600); err != nil {
				t.Fatalf("write seed: %v", err)
			}

			bin := filepath.Join(scratch, "bin")
			if err := os.MkdirAll(bin, 0o750); err != nil {
				t.Fatalf("mkdir bin: %v", err)
			}

			stub := "#!/usr/bin/env bash\n" + tc.stub + "\n"
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(stub), 0o700); err != nil { //nolint:gosec // an executable stub is the point
				t.Fatalf("write stub go: %v", err)
			}

			//nolint:gosec // script path derived from the module root this test located itself
			cmd := exec.CommandContext(t.Context(), "bash", script, "./internal/target", "FuzzThing", budget)
			cmd.Dir = scratch
			// PATH only, and deliberately not the ambient environment: the stub must be the `go`
			// the script finds, and nothing here should be able to reach a real toolchain or a real
			// GOCACHE.
			cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin:/bin"}

			out, err := cmd.CombinedOutput()

			gotExit := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !asExitError(err, &exitErr) {
					t.Fatalf("run script: %v\noutput:\n%s", err, out)
				}

				gotExit = exitErr.ExitCode()
			}

			if gotExit != tc.wantExit {
				t.Errorf("exit status = %d, want %d\noutput:\n%s", gotExit, tc.wantExit, out)
			}

			if !strings.Contains(string(out), tc.wantLog) {
				t.Errorf("output does not mention %q — the verdict is not the one this case is "+
					"about, even if the exit status matched\noutput:\n%s", tc.wantLog, out)
			}
		})
	}
}

// asExitError is errors.As specialised, kept local so the import list stays honest about there
// being exactly one use.
func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError) //nolint:errorlint // exec never wraps this
	if ok {
		*target = e
	}

	return ok
}

// TestCIRunsFuzzTargetsThroughTheSmokeScript keeps the workflow pointed at the script.
//
// The script's discrimination is worthless if the workflow calls `go test -fuzz` directly, and that
// is a one-line edit away in a file the test above never reads. Both halves are needed: the step
// must invoke the script, and it must not reintroduce a bare `-fuzz` invocation beside it.
func TestCIRunsFuzzTargetsThroughTheSmokeScript(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), ".github", "workflows", "ci.yml")

	//nolint:gosec // a path built from the module root this test located itself
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .github/workflows/ci.yml: %v", err)
	}

	workflow := string(raw)

	if !strings.Contains(workflow, ".github/scripts/fuzz-smoke.sh") {
		t.Error(".github/workflows/ci.yml does not invoke .github/scripts/fuzz-smoke.sh.\n" +
			"Calling `go test -fuzz` directly makes the job fail on go's own coordinator shutdown " +
			"race — measured at ~1 run in 10 (#218) — which is how a gate stops being read.")
	}

	// Line by line and skipping comments, because the job's header explains the whole failure mode
	// and necessarily quotes `go test -fuzz` while doing so. A whole-file Contains would therefore
	// fail on the documentation of the fix, which is the sort of check that gets deleted rather than
	// satisfied.
	for i, line := range strings.Split(workflow, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.Contains(trimmed, "go test") && strings.Contains(trimmed, "-fuzz") {
			t.Errorf(".github/workflows/ci.yml:%d invokes `go test ... -fuzz` directly:\n\t%s\n"+
				"Fuzz targets go through .github/scripts/fuzz-smoke.sh so that a deadline-shaped "+
				"failure with no counterexample is distinguishable from a real find.", i+1, trimmed)
		}
	}

	// The artifact upload is the only way a counterexample leaves the runner, and the script's
	// tolerated exits are zero — so `if: failure()` still fires exactly when there is something to
	// collect, and removing it would make a real find invisible.
	if !strings.Contains(workflow, "fuzz-failure-${{ matrix.target }}") {
		t.Error(".github/workflows/ci.yml no longer uploads a fuzz-failure artifact per target.\n" +
			"A counterexample has to leave the machine; the script fails the job but cannot " +
			"exfiltrate the input it found.")
	}
}
