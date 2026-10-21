package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGoRetryScriptRetriesOnlyTransientModuleServerErrors runs .github/scripts/go-retry.sh against a
// stub `go` that replays a scripted sequence of outcomes, one per call.
//
// The attempt count is the assertion that matters, not just the exit status. A script that never
// retried and one that retried everything would both pass the happy path and both pass a permanent
// failure on exit status alone; only the number of times `go` was invoked tells "retried a transient
// error" apart from "failed fast on a real one". That distinction is the whole point (#633): a stream
// error from proxy.golang.org should not fail a PR, and a checksum mismatch must never be retried
// until it goes away.
func TestGoRetryScriptRetriesOnlyTransientModuleServerErrors(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH; skipping the go-retry script gate")
	}

	script := filepath.Join(repoRoot(t), ".github", "scripts", "go-retry.sh")

	const (
		streamErr = `read "https://proxy.golang.org/github.com/aws/smithy-go/@v/v1.28.5.zip": stream error: stream ID 15; INTERNAL_ERROR; received from peer`
		badGW     = `received unexpected HTTP status: 502 Bad Gateway`
		notFound  = `go: github.com/example/nope@v9.9.9: reading https://proxy.golang.org/github.com/example/nope/@v/v9.9.9.info: 404 Not Found`
		mismatch  = "verifying github.com/example/m@v1.0.0: checksum mismatch\n\tdownloaded: h1:aaa\n\tgo.sum:     h1:bbb\n\nSECURITY ERROR\n" + streamErr
	)

	tests := []struct {
		name      string
		outcomes  []string // each "exit|message", consumed one per `go` invocation
		wantExit  int
		wantCalls int
		wantLog   string
	}{
		{"success first time", []string{"0|ok"}, 0, 1, "ok"},
		{"a stream error, then success: retried and recovered", []string{"1|" + streamErr, "0|ok"}, 0, 2, "retrying"},
		{"a 502, then success: retried and recovered", []string{"1|" + badGW, "0|ok"}, 0, 2, "retrying"},
		{"transient on every attempt: fails after three", []string{"1|" + streamErr}, 1, 3, "after 3 attempts"},
		{"a missing module: fails on the first attempt", []string{"1|" + notFound}, 1, 1, "not retried"},
		{
			// The stream-error text is in the output too, deliberately: the mismatch check must win,
			// or a corrupted download would be retried until a lucky attempt hid it.
			"a checksum mismatch: never retried, even beside a transient-looking line",
			[]string{"1|" + mismatch}, 1, 1, "checksum verification failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scratch := t.TempDir()
			bin := filepath.Join(scratch, "bin")

			if err := os.MkdirAll(bin, 0o750); err != nil {
				t.Fatalf("mkdir bin: %v", err)
			}

			// One outcome per line; the last is repeated once the list runs out, so a single entry
			// means "always this".
			seq := filepath.Join(scratch, "outcomes")
			if err := os.WriteFile(seq, []byte(strings.Join(tc.outcomes, "\x1e")), 0o600); err != nil {
				t.Fatalf("write outcomes: %v", err)
			}

			calls := filepath.Join(scratch, "calls")

			stub := `#!/usr/bin/env bash
echo "$*" >> "` + calls + `"
all=$(cat "` + seq + `")
first=${all%%$'\x1e'*}
rest=${all#*$'\x1e'}
[ "$rest" = "$all" ] || printf '%s' "$rest" > "` + seq + `"
echo "${first#*|}" >&2
exit "${first%%|*}"
`
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(stub), 0o700); err != nil { //nolint:gosec // an executable stub is the point
				t.Fatalf("write stub go: %v", err)
			}

			//nolint:gosec // script path derived from the module root this test located itself
			cmd := exec.CommandContext(t.Context(), "bash", script, "mod", "download")
			cmd.Dir = scratch
			cmd.Env = []string{
				"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin:/bin",
				"GO_RETRY_BACKOFF=0",
			}

			out, err := cmd.CombinedOutput()

			gotExit := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !asExitError(err, &exitErr) {
					t.Fatalf("run script: %v\noutput:\n%s", err, out)
				}

				gotExit = exitErr.ExitCode()
			}

			recorded, _ := os.ReadFile(calls) //nolint:gosec // a file this test created in its own TempDir
			gotCalls := strings.Count(string(recorded), "\n")

			if gotExit != tc.wantExit {
				t.Errorf("exit status = %d, want %d\noutput:\n%s", gotExit, tc.wantExit, out)
			}

			if gotCalls != tc.wantCalls {
				t.Errorf("go was invoked %d times, want %d. This is the assertion that separates "+
					"retrying a transient error from failing fast on a real one\noutput:\n%s",
					gotCalls, tc.wantCalls, out)
			}

			if !strings.Contains(string(out), tc.wantLog) {
				t.Errorf("output does not mention %q, so the verdict is not the one this case is about\n"+
					"output:\n%s", tc.wantLog, out)
			}
		})
	}
}

// goInvocation matches a line that runs the go command directly. A `go` at the start of a line or
// after a shell separator; not `go-retry.sh`, not a word that merely contains "go".
var goInvocation = regexp.MustCompile(`(^|[;&|(]\s*|\s)go\s+(build|test|vet|run|install|get|mod\s+download|list|generate)\b`)

// goInstall matches a direct `go install` on one line.
var goInstall = regexp.MustCompile(`(^|[;&|(]\s*|\s)go\s+install\b`)

// TestEveryGoJobFetchesModulesThroughTheRetry asserts the retry is where it has to be to work.
//
// The script only helps if it runs before anything else fetches a module. So, in every workflow job
// that sets up Go:
//   - a `go-retry.sh mod download` step follows setup-go, and no step between them runs `go`;
//   - no step installs a tool with a bare `go install`, since that is a module fetch outside the
//     retry; and nothing installs `@latest`, which is uncached and moves without a commit.
//
// Walked, not enumerated: a new Go job gets checked on the PR that adds it.
func TestEveryGoJobFetchesModulesThroughTheRetry(t *testing.T) {
	t.Parallel()

	goJobs := 0

	for _, wf := range readWorkflows(t) {
		for _, id := range sortedMapKeys(wf.File.Jobs) {
			steps := jobSteps(wf.File.Jobs[id])

			setup := -1

			for i, s := range steps {
				if strings.Contains(s.Uses, "actions/setup-go@") {
					setup = i

					break
				}
			}

			if setup < 0 {
				continue
			}

			goJobs++

			download := -1

			for i := setup + 1; i < len(steps); i++ {
				code := withoutComments(steps[i].Run)

				if strings.Contains(code, "go-retry.sh mod download") {
					download = i

					break
				}

				if goInvocation.MatchString(code) {
					t.Errorf("%s job %q runs `go` in step %q before modules are fetched through "+
						".github/scripts/go-retry.sh. That step fetches modules itself, and a transient "+
						"module-server error there fails the job (#633).", wf.Name, id, steps[i].Name)
				}
			}

			if download < 0 {
				t.Errorf("%s job %q sets up Go and never runs `.github/scripts/go-retry.sh mod download`. "+
					"Add it as the step after setup-go.", wf.Name, id)
			}

			for _, s := range steps {
				code := withoutComments(s.Run)

				for line := range strings.SplitSeq(code, "\n") {
					if strings.Contains(line, "@latest") {
						t.Errorf("%s job %q step %q installs @latest: %q. Pin the version; @latest is "+
							"uncached, and a gate whose tool moves without a commit can change its verdict "+
							"on an unchanged tree.", wf.Name, id, s.Name, strings.TrimSpace(line))
					}

					if goInstall.MatchString(line) &&
						!strings.Contains(line, "go-retry.sh install") {
						t.Errorf("%s job %q step %q runs a bare `go install`: %q. Route it through "+
							"`.github/scripts/go-retry.sh install` so a transient fetch error is retried.",
							wf.Name, id, s.Name, strings.TrimSpace(line))
					}
				}
			}
		}
	}

	// 18 when this was written. A floor, so a parse that stops seeing setup-go cannot pass by
	// finding no Go jobs to check.
	if goJobs < 15 {
		t.Fatalf("found %d jobs that set up Go across the workflows, expected at least 15. The "+
			"setup-go match or the step parse has stopped working", goJobs)
	}
}
