package network

import (
	"testing"
)

func TestAlgorithm_Constants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		algo Algorithm
		want string
	}{
		{AlgorithmAuto, "auto"},
		{AlgorithmBBR, "bbr"},
		{AlgorithmCUBIC, "cubic"},
		{AlgorithmReno, "reno"},
	}
	for _, tt := range tests {
		t.Run(string(tt.algo), func(t *testing.T) {
			t.Parallel()
			if string(tt.algo) != tt.want {
				t.Errorf("Algorithm(%q) = %q, want %q", tt.algo, string(tt.algo), tt.want)
			}
		})
	}
}

func TestSelect_PrefersBBR(t *testing.T) {
	t.Parallel()
	result := DetectionResult{
		Available:     []Algorithm{AlgorithmCUBIC, AlgorithmBBR, AlgorithmReno},
		SystemDefault: AlgorithmCUBIC,
	}
	got := Select(result)
	if got != AlgorithmBBR {
		t.Errorf("Select() = %q, want %q", got, AlgorithmBBR)
	}
}

func TestSelect_FallsBackToCUBIC(t *testing.T) {
	t.Parallel()
	result := DetectionResult{
		Available:     []Algorithm{AlgorithmReno, AlgorithmCUBIC},
		SystemDefault: AlgorithmReno,
	}
	got := Select(result)
	if got != AlgorithmCUBIC {
		t.Errorf("Select() = %q, want %q", got, AlgorithmCUBIC)
	}
}

func TestSelect_FallsBackToSystemDefault(t *testing.T) {
	t.Parallel()
	result := DetectionResult{
		Available:     []Algorithm{AlgorithmReno},
		SystemDefault: AlgorithmReno,
	}
	got := Select(result)
	if got != AlgorithmReno {
		t.Errorf("Select() = %q, want %q", got, AlgorithmReno)
	}
}

func TestSelect_EmptyAvailable(t *testing.T) {
	t.Parallel()
	result := DetectionResult{
		Available:     []Algorithm{},
		SystemDefault: "",
	}
	got := Select(result)
	if got != AlgorithmAuto {
		t.Errorf("Select() = %q, want %q", got, AlgorithmAuto)
	}
}

func TestSelect_SystemDefaultExcludesAuto(t *testing.T) {
	t.Parallel()
	// When Available is empty but SystemDefault is "auto", should return AlgorithmAuto
	// (system default of "auto" is treated as no selection).
	result := DetectionResult{
		Available:     []Algorithm{},
		SystemDefault: AlgorithmAuto,
	}
	got := Select(result)
	if got != AlgorithmAuto {
		t.Errorf("Select() = %q, want %q", got, AlgorithmAuto)
	}
}

func TestDetect_ReturnsResult(t *testing.T) {
	t.Parallel()
	// Detect() should not panic on any platform.
	result := Detect()
	// On Linux: Supported == true; on other platforms: Supported == false.
	// Just verify the struct is valid.
	_ = result.Supported
	_ = result.Recommended
	_ = result.Available
	_ = result.SystemDefault
}

func TestNewDialer_DoesNotPanic(t *testing.T) {
	t.Parallel()
	for _, algo := range []Algorithm{
		AlgorithmAuto,
		AlgorithmBBR,
		AlgorithmCUBIC,
		AlgorithmReno,
	} {
		t.Run(string(algo), func(t *testing.T) {
			t.Parallel()
			d := NewDialer(algo)
			if d == nil {
				t.Errorf("NewDialer(%q) returned nil", algo)
			}
		})
	}
}

func TestNewDialer_EmptyAlgo(t *testing.T) {
	t.Parallel()
	d := NewDialer("")
	if d == nil {
		t.Error("NewDialer(\"\") returned nil")
	}
}
