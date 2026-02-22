// Package network provides TCP congestion control detection and per-connection
// algorithm selection for ObjectFS S3 transfers.
//
// BBR (Bottleneck Bandwidth and Round-trip time) is Google's congestion control
// algorithm, available in Linux kernels ≥ 4.9.  It achieves significantly
// higher throughput than CUBIC on paths with non-trivial bandwidth-delay
// products (e.g. cloud storage transfers), while maintaining fairness.
//
// Usage:
//
//	// Detect best available algorithm and create a dialer.
//	dialer := network.BestAvailableDialer()
//
//	// Use with a custom http.Transport.
//	transport := http.DefaultTransport.(*http.Transport).Clone()
//	transport.DialContext = dialer.DialContext
package network

import (
	"net"
	"time"
)

// Algorithm represents a TCP congestion control algorithm name.
type Algorithm string

const (
	// AlgorithmAuto selects the best available algorithm at runtime.
	AlgorithmAuto Algorithm = "auto"
	// AlgorithmBBR uses Google's BBR v1 algorithm (Linux ≥ 4.9).
	AlgorithmBBR Algorithm = "bbr"
	// AlgorithmCUBIC uses the CUBIC algorithm (Linux default).
	AlgorithmCUBIC Algorithm = "cubic"
	// AlgorithmReno uses classic TCP Reno (conservative fallback).
	AlgorithmReno Algorithm = "reno"
)

// DialTimeout is the default dial timeout applied to dialers from this package.
const DialTimeout = 30 * time.Second

// KeepAlive is the default TCP keep-alive interval.
const KeepAlive = 30 * time.Second

// DetectionResult holds the outcome of probing the OS for available TCP
// congestion control algorithms.
type DetectionResult struct {
	// Available is the list of algorithms installed in the kernel.
	Available []Algorithm
	// SystemDefault is the kernel's current default congestion control algorithm.
	SystemDefault Algorithm
	// Recommended is the best algorithm for high-bandwidth S3 transfers,
	// chosen from Available in the order BBR > CUBIC > system default.
	Recommended Algorithm
	// Supported reports whether per-socket algorithm selection is supported
	// on this platform (true on Linux, false elsewhere).
	Supported bool
}

// Detect queries the OS for available TCP congestion control algorithms.
// On Linux the result is read from procfs; on other platforms an empty
// non-error result is returned (Supported == false).
func Detect() DetectionResult {
	return detect() // implemented in congestion_linux.go / congestion_stub.go
}

// Select returns the best algorithm to use from a DetectionResult.
// Preference order: BBR > CUBIC > system default > AlgorithmAuto.
func Select(result DetectionResult) Algorithm {
	for _, preferred := range []Algorithm{AlgorithmBBR, AlgorithmCUBIC} {
		for _, a := range result.Available {
			if a == preferred {
				return preferred
			}
		}
	}
	if result.SystemDefault != "" && result.SystemDefault != AlgorithmAuto {
		return result.SystemDefault
	}
	return AlgorithmAuto
}

// NewDialer returns a *net.Dialer that requests the given TCP congestion
// control algorithm for every new connection.
//
//   - On Linux the TCP_CONGESTION socket option is set in the dialer's Control
//     hook before each connection is established.  The option is set on a
//     best-effort basis; connection failures due to an unavailable algorithm
//     are avoided by falling back to the system default silently.
//   - On macOS and Windows the dialer is equivalent to &net.Dialer{} (no
//     per-socket congestion control is available).
//
// If algo is AlgorithmAuto the best available algorithm is selected via
// Detect() + Select() at the time of this call.
func NewDialer(algo Algorithm) *net.Dialer {
	if algo == AlgorithmAuto || algo == "" {
		result := Detect()
		algo = Select(result)
	}
	return newPlatformDialer(algo)
}
