//go:build linux

package network

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// This file covers the Linux half of this package: the procfs parse, the dialer's Control hook, and
// the setsockopt itself. None of it compiles on macOS, where congestion_stub.go supplies 24 lines in
// place of these 66 — which is why the package measured 100% on a developer's machine and 86.9% in
// CI, below its own floor. The floor is per package; the file set is per platform. CI is the
// authority because it runs the platform that compiles the larger half.
//
// The three functions below were the uncovered ones: setTCPCongestion at 0%, newPlatformDialer at
// 20% (its Control closure never invoked, because nothing dialed), and the procfs readers at 87.5%
// and 75% — the absent-file arms, which is the case that actually varies between hosts.

// TestReadAvailableAlgorithmsParsesProcfs pins the parse against the shape a kernel writes: names
// separated by spaces on a single line with a trailing newline.
func TestReadAvailableAlgorithmsParsesProcfs(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []Algorithm
		why     string
	}{
		{
			name:    "the usual pair",
			content: "reno cubic\n",
			want:    []Algorithm{AlgorithmReno, AlgorithmCUBIC},
			why:     "what a stock kernel without the bbr module reports",
		},
		{
			name:    "bbr present",
			content: "reno cubic bbr\n",
			want:    []Algorithm{AlgorithmReno, AlgorithmCUBIC, AlgorithmBBR},
			why:     "the case the whole package exists for",
		},
		{
			name:    "no trailing newline",
			content: "cubic",
			want:    []Algorithm{AlgorithmCUBIC},
			why:     "procfs supplies one; a file that does not must not yield a name with a \\n in it",
		},
		{
			name:    "irregular whitespace",
			content: "  reno \t cubic  \n\n",
			want:    []Algorithm{AlgorithmReno, AlgorithmCUBIC},
			why:     "strings.Fields collapses runs, so this must not produce empty algorithm names",
		},
		{
			name:    "empty file",
			content: "\n",
			want:    []Algorithm{},
			why: "a kernel with no pluggable algorithms compiled in. Select must fall through to " +
				"AlgorithmAuto rather than returning an empty name as a choice",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Not parallel: these subtests rewrite the package-level path variables. t.Setenv-style
			// serialization by omission is deliberate — a parallel subtest here would race the
			// variable it just set against a sibling's.
			withAvailablePath(t, tc.content)

			got, err := readAvailableAlgorithms()
			if err != nil {
				t.Fatalf("readAvailableAlgorithms: %v", err)
			}

			if !slices.Equal(got, tc.want) {
				t.Errorf("parsed %q as %v, want %v. %s", tc.content, got, tc.want, tc.why)
			}
		})
	}
}

// TestReadSystemDefaultParsesProcfs is the same for the one-value file.
func TestReadSystemDefaultParsesProcfs(t *testing.T) {
	withSystemDefaultPath(t, "bbr\n")

	got, err := readSystemDefault()
	if err != nil {
		t.Fatalf("readSystemDefault: %v", err)
	}

	if got != AlgorithmBBR {
		t.Errorf("read %q, want %q — the trailing newline must be trimmed, or every comparison "+
			"against an Algorithm constant fails", got, AlgorithmBBR)
	}
}

// TestProcfsReadsFailSoftly covers the absent-file arms.
//
// This is the case that varies by host rather than by code: a container with a masked /proc/sys, a
// kernel built without CONFIG_TCP_CONG_ADVANCED, or a sandbox that denies the read. Detect must
// still return a usable result, because the alternative is that a mount fails over a tuning hint.
func TestProcfsReadsFailSoftly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	t.Run("readers report the error", func(t *testing.T) {
		setPaths(t, missing, missing)

		if _, err := readAvailableAlgorithms(); err == nil {
			t.Error("readAvailableAlgorithms returned nil error for a path that does not exist")
		} else if !strings.Contains(err.Error(), "available congestion algorithms") {
			t.Errorf("the error does not say which read failed: %v", err)
		}

		if _, err := readSystemDefault(); err == nil {
			t.Error("readSystemDefault returned nil error for a path that does not exist")
		} else if !strings.Contains(err.Error(), "system congestion default") {
			t.Errorf("the error does not say which read failed: %v", err)
		}
	})

	t.Run("detect still returns a usable result", func(t *testing.T) {
		setPaths(t, missing, missing)

		result := detect()

		if !result.Supported {
			t.Error("Supported is false on Linux. It reports whether the platform has per-socket " +
				"selection at all, which does not stop being true because procfs could not be read")
		}

		if result.Recommended != AlgorithmAuto {
			t.Errorf("Recommended is %q with nothing detected, want %q. Auto means \"leave the "+
				"kernel default alone\", which is the only honest answer when detection found "+
				"nothing", result.Recommended, AlgorithmAuto)
		}
	})
}

// TestDetectPrefersBBRFromProcfs is the whole path end to end: parse, select, report.
func TestDetectPrefersBBRFromProcfs(t *testing.T) {
	dir := t.TempDir()
	setPaths(t,
		writeFile(t, dir, "available", "reno cubic bbr\n"),
		writeFile(t, dir, "default", "cubic\n"),
	)

	result := detect()

	if result.Recommended != AlgorithmBBR {
		t.Errorf("Recommended is %q, want %q: bbr is available, and it is preferred over both cubic "+
			"and the system default", result.Recommended, AlgorithmBBR)
	}

	if result.SystemDefault != AlgorithmCUBIC {
		t.Errorf("SystemDefault is %q, want %q", result.SystemDefault, AlgorithmCUBIC)
	}
}

// TestPlatformDialerSetsCongestionOnRealConnection dials a real listener so the Control hook runs.
//
// A dialer whose hook is never invoked is what left newPlatformDialer at 20%: the closure is where
// every decision in the function lives, and constructing the dialer proves only that a struct
// literal compiles. This connects over loopback, which needs no network and no privileges.
//
// The assertion is that the connection succeeds and, where the kernel allows it, that the option
// took. It cannot be that the option always takes: setting TCP_CONGESTION to an algorithm the kernel
// does not have returns ENOENT, and a container may not permit it at all. That is precisely the
// contract — best effort, never fail the dial — so a hostile kernel is a pass here and a failed
// dial is not.
func TestPlatformDialerSetsCongestionOnRealConnection(t *testing.T) {
	ln := loopbackListener(t)

	for _, algo := range []Algorithm{AlgorithmAuto, "", AlgorithmCUBIC, AlgorithmReno, AlgorithmBBR,
		"definitely-not-a-real-algorithm"} {
		t.Run(string(algo), func(t *testing.T) {
			t.Parallel()

			conn, err := newPlatformDialer(algo).Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatalf("dial with algo %q failed: %v. The Control hook must never fail a "+
					"connection over congestion control — a tuning preference the kernel declines "+
					"is not a reason to be unable to reach S3, and this dialer is the one every S3 "+
					"request goes through", algo, err)
			}
			t.Cleanup(func() { _ = conn.Close() })

			if algo == AlgorithmAuto || algo == "" {
				// The hook returns early without setting anything, so a successful dial is the
				// entire contract.
				return
			}

			// Read the option back where the kernel permits it. A mismatch is not a failure here:
			// an algorithm this kernel lacks legitimately leaves the default in place, which is what
			// best-effort means. What must hold is that the dial succeeded, asserted above.
			if got, err := readCongestion(t, conn); err == nil {
				t.Logf("requested %q, socket reports %q", algo, got)
			}
		})
	}
}

// TestSetTCPCongestionAppliesAnAvailableAlgorithm covers setTCPCongestion directly, in both
// directions: a name the kernel lists must be accepted and must be readable back, and a name it
// cannot have must come back as an error rather than as silent success.
//
// The function was at 0%. Nothing called it directly and its only caller discards the result, so its
// error return was pure claim — the difference between "we asked and the kernel declined" and "we
// never asked" was unobserved either way.
//
// Both assertions are conditional on the kernel, and deliberately so. A container may deny
// setsockopt outright, and a kernel built without CONFIG_TCP_CONG_ADVANCED lists nothing to set. The
// test skips in those cases rather than asserting a privilege it does not have — but it does not skip
// on the interesting outcome, so on a normal Linux host the round trip really is checked.
func TestSetTCPCongestionAppliesAnAvailableAlgorithm(t *testing.T) {
	available, err := readAvailableAlgorithms()
	if err != nil || len(available) == 0 {
		t.Skipf("this kernel lists no congestion algorithms to set (err=%v)", err)
	}

	conn := loopbackConn(t)
	raw := rawConn(t, conn)
	want := available[0]

	if err := setTCPCongestion(raw, string(want)); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skipf("this environment does not permit setting TCP_CONGESTION: %v", err)
		}

		t.Fatalf("setTCPCongestion(%q) failed for an algorithm the kernel itself lists as "+
			"available: %v", want, err)
	}

	got, err := readCongestion(t, conn)
	if err != nil {
		t.Fatalf("cannot read TCP_CONGESTION back after setting it: %v", err)
	}

	if got != string(want) {
		t.Errorf("set %q but the socket reports %q, so the option did not take. That is the whole "+
			"function: an option silently not applied is indistinguishable from this package not "+
			"existing", want, got)
	}

	// The other direction. ENOENT is what a kernel returns for a module it does not have, and
	// getting it here is what makes the success above attributable to the algorithm rather than to
	// setsockopt accepting anything.
	err = setTCPCongestion(raw, "definitely-not-a-real-algorithm")
	if err == nil {
		t.Error("setTCPCongestion accepted an algorithm name no kernel has. Either the option is " +
			"not being set at all, or its error is being swallowed below this function — and the " +
			"caller discards the result, so nothing else would ever report it")
	}
}

// --- helpers ---

// setPaths points the procfs readers at the given files for the duration of the test.
func setPaths(t *testing.T, available, systemDefault string) {
	t.Helper()

	origAvailable, origDefault := availableAlgorithmsPath, systemDefaultPath
	availableAlgorithmsPath, systemDefaultPath = available, systemDefault

	t.Cleanup(func() {
		availableAlgorithmsPath, systemDefaultPath = origAvailable, origDefault
	})
}

func withAvailablePath(t *testing.T, content string) {
	t.Helper()
	setPaths(t, writeFile(t, t.TempDir(), "available", content), systemDefaultPath)
}

func withSystemDefaultPath(t *testing.T, content string) {
	t.Helper()
	setPaths(t, availableAlgorithmsPath, writeFile(t, t.TempDir(), "default", content))
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

// loopbackListener returns a listener that accepts and immediately closes, so a dial completes.
func loopbackListener(t *testing.T) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback in this environment: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // the listener closed
			}
			_ = c.Close()
		}
	}()

	return ln
}

// loopbackConn returns a connected TCP socket, which is what setsockopt needs.
func loopbackConn(t *testing.T) net.Conn {
	t.Helper()

	conn, err := net.Dial("tcp", loopbackListener(t).Addr().String())
	if err != nil {
		t.Skipf("cannot dial loopback in this environment: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

func rawConn(t *testing.T, conn net.Conn) syscall.RawConn {
	t.Helper()

	sc, ok := conn.(syscall.Conn)
	if !ok {
		t.Fatalf("%T does not implement syscall.Conn", conn)
	}

	raw, err := sc.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}

	return raw
}

// readCongestion reads TCP_CONGESTION back off a connection.
//
// x/sys/unix rather than syscall: the standard library has SetsockoptString but no
// GetsockoptString, so there is no way to read a string option back through it.
func readCongestion(t *testing.T, conn net.Conn) (string, error) {
	t.Helper()

	var (
		got    string
		optErr error
	)

	err := rawConn(t, conn).Control(func(fd uintptr) {
		got, optErr = unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, tcpCongestion)
	})
	if err != nil {
		return "", err
	}

	return got, optErr
}
