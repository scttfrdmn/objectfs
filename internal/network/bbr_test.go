package network

import (
	"testing"
)

func TestDefaultBBRConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultBBRConfig()

	if cfg.SendBufferSize <= 0 {
		t.Errorf("SendBufferSize = %d, want > 0", cfg.SendBufferSize)
	}
	if cfg.RecvBufferSize <= 0 {
		t.Errorf("RecvBufferSize = %d, want > 0", cfg.RecvBufferSize)
	}
	if cfg.InitialCongestionWindow <= 0 {
		t.Errorf("InitialCongestionWindow = %d, want > 0", cfg.InitialCongestionWindow)
	}

	// Verify 4 MiB defaults
	const fourMiB = 4 * 1024 * 1024
	if cfg.SendBufferSize != fourMiB {
		t.Errorf("SendBufferSize = %d, want %d (4 MiB)", cfg.SendBufferSize, fourMiB)
	}
	if cfg.RecvBufferSize != fourMiB {
		t.Errorf("RecvBufferSize = %d, want %d (4 MiB)", cfg.RecvBufferSize, fourMiB)
	}
	if cfg.InitialCongestionWindow != 10 {
		t.Errorf("InitialCongestionWindow = %d, want 10", cfg.InitialCongestionWindow)
	}
}

func TestNewBBRDialer_NotNil(t *testing.T) {
	t.Parallel()
	d := NewBBRDialer()
	if d == nil {
		t.Error("NewBBRDialer() returned nil")
	}
}

func TestNewCUBICDialer_NotNil(t *testing.T) {
	t.Parallel()
	d := NewCUBICDialer()
	if d == nil {
		t.Error("NewCUBICDialer() returned nil")
	}
}

func TestBestAvailableDialer_NotNil(t *testing.T) {
	t.Parallel()
	d := BestAvailableDialer()
	if d == nil {
		t.Error("BestAvailableDialer() returned nil")
	}
}

func TestIsBBRAvailable_NoPanic(t *testing.T) {
	t.Parallel()
	// Just verify it doesn't panic; actual value depends on host kernel.
	_ = IsBBRAvailable()
}

func TestMinKernelVersion(t *testing.T) {
	t.Parallel()
	if MinKernelVersion == "" {
		t.Error("MinKernelVersion should not be empty")
	}
	if MinKernelVersion != "4.9" {
		t.Errorf("MinKernelVersion = %q, want %q", MinKernelVersion, "4.9")
	}
}
