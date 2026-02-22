//go:build !linux

package network

import "net"

// detect returns an empty DetectionResult on platforms that do not support
// per-socket TCP congestion control selection (macOS, Windows, etc.).
func detect() DetectionResult {
	return DetectionResult{
		Supported:   false,
		Recommended: AlgorithmAuto,
	}
}

// newPlatformDialer returns a standard *net.Dialer on platforms where
// TCP_CONGESTION is not available.  The algo parameter is intentionally
// ignored; the system's default congestion control algorithm is used.
func newPlatformDialer(_ Algorithm) *net.Dialer {
	return &net.Dialer{
		Timeout:   DialTimeout,
		KeepAlive: KeepAlive,
	}
}
