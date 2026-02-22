package network

import "net"

// MinKernelVersion is the minimum Linux kernel version required for BBR support.
// BBR was merged into the mainline kernel in v4.9 (December 2016).
const MinKernelVersion = "4.9"

// BBRConfig holds socket-level tunables that work well with BBR congestion
// control for high-bandwidth S3 transfers over long-distance paths.
//
// These values are recommendations; applying them typically requires
// kernel-level sysctl changes (e.g. net.core.rmem_max) in addition to the
// per-socket values set by a dialer.
type BBRConfig struct {
	// SendBufferSize is the recommended SO_SNDBUF value in bytes.
	// Larger buffers allow BBR to fill the bandwidth-delay product.
	// Default: 4 MiB.
	SendBufferSize int

	// RecvBufferSize is the recommended SO_RCVBUF value in bytes.
	// Default: 4 MiB.
	RecvBufferSize int

	// InitialCongestionWindow is the preferred TCP initial congestion window
	// size measured in segments (MSS).  A larger value (e.g. 10–100) allows
	// BBR to probe available bandwidth faster on high-latency paths.
	// Note: this is a hint — it requires kernel support for TCP_INIT_CWND.
	// Default: 10 segments.
	InitialCongestionWindow int
}

// DefaultBBRConfig returns conservative BBR tuning suitable for most
// high-bandwidth S3 upload/download workloads.
func DefaultBBRConfig() BBRConfig {
	return BBRConfig{
		SendBufferSize:          4 * 1024 * 1024, // 4 MiB
		RecvBufferSize:          4 * 1024 * 1024, // 4 MiB
		InitialCongestionWindow: 10,              // segments
	}
}

// NewBBRDialer returns a *net.Dialer pre-configured to request the BBR TCP
// congestion control algorithm.
//
// On Linux ≥ 4.9 with BBR compiled in, each new connection will use BBR.
// On other platforms (or Linux without BBR) the dialer falls back to the
// system default transparently.
func NewBBRDialer() *net.Dialer {
	return NewDialer(AlgorithmBBR)
}

// NewCUBICDialer returns a *net.Dialer pre-configured to request CUBIC TCP
// congestion control.  This is useful when explicitly preferring CUBIC over
// the kernel default (which may be Reno on some distributions).
func NewCUBICDialer() *net.Dialer {
	return NewDialer(AlgorithmCUBIC)
}

// BestAvailableDialer detects the best available TCP congestion control
// algorithm at runtime (BBR > CUBIC > system default) and returns a
// *net.Dialer configured for it.
//
// This is the recommended entry point for callers that want maximum
// throughput without pinning a specific algorithm in their configuration.
func BestAvailableDialer() *net.Dialer {
	result := Detect()
	algo := Select(result)
	return newPlatformDialer(algo)
}

// IsBBRAvailable returns true if the BBR algorithm is listed as available
// by the current OS.  On non-Linux platforms this always returns false.
func IsBBRAvailable() bool {
	result := Detect()
	for _, a := range result.Available {
		if a == AlgorithmBBR {
			return true
		}
	}
	return false
}
