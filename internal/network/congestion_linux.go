//go:build linux

package network

import (
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
)

// tcpCongestion is the TCP_CONGESTION socket option number on Linux (0xd = 13).
// This constant is not exported by the syscall package, so it is defined here.
const tcpCongestion = 0xd

// The procfs files consulted by detect. They are variables rather than constants so that a test can
// point them at controlled content and at a path that does not exist.
//
// Both are worth testing against something other than the host's real procfs. The parse is the part
// that can be wrong — a kernel lists its algorithms space-separated on one line with a trailing
// newline, and the file is absent entirely in a container with a masked /proc/sys or on a kernel
// built without pluggable congestion control — and reading the host's own file exercises whichever
// single shape that host happens to have. That is how this package came to be at 86.9% on Linux
// while measuring 100% on macOS, where a 24-line stub stands in for these 66 lines: the coverage
// floor is per package but the file set is per platform, and CI is the authority because it runs the
// platform that compiles the larger half.
var (
	availableAlgorithmsPath = "/proc/sys/net/ipv4/tcp_available_congestion_control"
	systemDefaultPath       = "/proc/sys/net/ipv4/tcp_congestion_control"
)

// detect reads available congestion control algorithms and the system default
// from Linux procfs, then picks the recommended algorithm for S3 transfers.
//
// Read errors are deliberately discarded. A kernel that does not publish these files supports no
// per-socket selection to report, and Select falls back to AlgorithmAuto from the zero values — which
// is the kernel default, i.e. exactly what the caller gets by not setting the option at all. Failing
// detection would turn "cannot tell" into "cannot dial".
func detect() DetectionResult {
	available, _ := readAvailableAlgorithms()
	sysDefault, _ := readSystemDefault()

	result := DetectionResult{
		Available:     available,
		SystemDefault: sysDefault,
		Supported:     true,
	}
	result.Recommended = Select(result)
	return result
}

// readAvailableAlgorithms reads
// /proc/sys/net/ipv4/tcp_available_congestion_control and returns the list.
func readAvailableAlgorithms() ([]Algorithm, error) {
	data, err := os.ReadFile(availableAlgorithmsPath)
	if err != nil {
		return nil, fmt.Errorf("read available congestion algorithms: %w", err)
	}
	parts := strings.Fields(strings.TrimSpace(string(data)))
	algos := make([]Algorithm, 0, len(parts))
	for _, p := range parts {
		algos = append(algos, Algorithm(p))
	}
	return algos, nil
}

// readSystemDefault reads /proc/sys/net/ipv4/tcp_congestion_control.
func readSystemDefault() (Algorithm, error) {
	data, err := os.ReadFile(systemDefaultPath)
	if err != nil {
		return "", fmt.Errorf("read system congestion default: %w", err)
	}
	return Algorithm(strings.TrimSpace(string(data))), nil
}

// newPlatformDialer creates a *net.Dialer that sets the TCP_CONGESTION socket
// option before each connection.  The option is applied on a best-effort basis:
// if the kernel rejects the algorithm (e.g. not compiled in) the error is
// silently discarded so connections always succeed.
func newPlatformDialer(algo Algorithm) *net.Dialer {
	return &net.Dialer{
		Timeout:   DialTimeout,
		KeepAlive: KeepAlive,
		Control: func(network, address string, c syscall.RawConn) error {
			if algo == AlgorithmAuto || algo == "" {
				return nil // use kernel default
			}
			// Best-effort: ignore errors so we never fail a connection due
			// to congestion control configuration.
			_ = setTCPCongestion(c, string(algo))
			return nil
		},
	}
}

// setTCPCongestion sets the TCP_CONGESTION socket option on the raw connection.
func setTCPCongestion(rawConn syscall.RawConn, algo string) error {
	var sockOptErr error
	if err := rawConn.Control(func(fd uintptr) {
		sockOptErr = syscall.SetsockoptString(
			int(fd),
			syscall.IPPROTO_TCP,
			tcpCongestion,
			algo,
		)
	}); err != nil {
		return fmt.Errorf("congestion control: rawconn control: %w", err)
	}
	return sockOptErr
}
