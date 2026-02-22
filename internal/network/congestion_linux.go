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

// detect reads available congestion control algorithms and the system default
// from Linux procfs, then picks the recommended algorithm for S3 transfers.
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
	data, err := os.ReadFile("/proc/sys/net/ipv4/tcp_available_congestion_control")
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
	data, err := os.ReadFile("/proc/sys/net/ipv4/tcp_congestion_control")
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
