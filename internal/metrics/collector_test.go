package metrics

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewCollector(t *testing.T) {
	t.Parallel()

	t.Run("with valid config", func(t *testing.T) {
		config := &Config{
			Enabled:   true,
			Port:      9090,
			Path:      "/metrics",
			Namespace: "objectfs",
			Subsystem: "test",
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v, want nil", err)
		}
		if collector == nil {
			t.Fatal("NewCollector() returned nil collector")
		}
		if collector.config != config {
			t.Error("collector.config does not match input config")
		}
		if collector.registry == nil {
			t.Error("collector.registry is nil")
		}
		if collector.operations == nil {
			t.Error("collector.operations map is nil")
		}
	})

	t.Run("with nil config uses defaults", func(t *testing.T) {
		collector, err := NewCollector(nil)
		if err != nil {
			t.Fatalf("NewCollector(nil) error = %v, want nil", err)
		}
		if collector == nil {
			t.Fatal("NewCollector(nil) returned nil collector")
		}
		if collector.config == nil {
			t.Fatal("default config is nil")
		}
		if collector.config.Port != 8080 {
			t.Errorf("default port = %d, want 8080", collector.config.Port)
		}
		if collector.config.Path != "/metrics" {
			t.Errorf("default path = %q, want %q", collector.config.Path, "/metrics")
		}
		if collector.config.Namespace != "objectfs" {
			t.Errorf("default namespace = %q, want %q", collector.config.Namespace, "objectfs")
		}
	})

	t.Run("with disabled config", func(t *testing.T) {
		config := &Config{
			Enabled: false,
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v, want nil", err)
		}
		if collector == nil {
			t.Fatal("NewCollector() returned nil collector")
		}
		if collector.registry != nil {
			t.Error("disabled collector should not have registry")
		}
	})
}

func TestRecordOperation(t *testing.T) {
	t.Parallel()

	t.Run("record successful operation", func(t *testing.T) {
		config := &Config{
			Enabled:   true,
			Port:      9091,
			Namespace: "test",
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		// Record an operation
		collector.RecordOperation("read", 100*time.Millisecond, 1024, true)

		// Verify metrics
		metrics := collector.GetMetrics()
		operations, ok := metrics["operations"].(map[string]*OperationMetrics)
		if !ok {
			t.Fatal("operations not found in metrics")
		}

		op, exists := operations["read"]
		if !exists {
			t.Fatal("read operation not recorded")
		}
		if op.Count != 1 {
			t.Errorf("op.Count = %d, want 1", op.Count)
		}
		if op.TotalSize != 1024 {
			t.Errorf("op.TotalSize = %d, want 1024", op.TotalSize)
		}
		if op.Errors != 0 {
			t.Errorf("op.Errors = %d, want 0", op.Errors)
		}
		if op.AvgSize != 1024.0 {
			t.Errorf("op.AvgSize = %.2f, want 1024.00", op.AvgSize)
		}
	})

	t.Run("record failed operation", func(t *testing.T) {
		config := &Config{
			Enabled:   true,
			Port:      9092,
			Namespace: "test",
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		collector.RecordOperation("write", 50*time.Millisecond, 512, false)

		operations := collector.GetMetrics()["operations"].(map[string]*OperationMetrics)
		op := operations["write"]
		if op.Errors != 1 {
			t.Errorf("op.Errors = %d, want 1", op.Errors)
		}
	})

	t.Run("record multiple operations", func(t *testing.T) {
		config := &Config{
			Enabled:   true,
			Port:      9093,
			Namespace: "test",
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		// Record multiple operations
		collector.RecordOperation("read", 100*time.Millisecond, 1000, true)
		collector.RecordOperation("read", 200*time.Millisecond, 2000, true)
		collector.RecordOperation("read", 300*time.Millisecond, 3000, false)

		operations := collector.GetMetrics()["operations"].(map[string]*OperationMetrics)
		op := operations["read"]
		if op.Count != 3 {
			t.Errorf("op.Count = %d, want 3", op.Count)
		}
		if op.TotalSize != 6000 {
			t.Errorf("op.TotalSize = %d, want 6000", op.TotalSize)
		}
		if op.Errors != 1 {
			t.Errorf("op.Errors = %d, want 1", op.Errors)
		}
		expectedAvgSize := 6000.0 / 3.0
		if op.AvgSize != expectedAvgSize {
			t.Errorf("op.AvgSize = %.2f, want %.2f", op.AvgSize, expectedAvgSize)
		}
	})

	t.Run("disabled collector ignores operations", func(t *testing.T) {
		config := &Config{
			Enabled: false,
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		// Should not panic
		collector.RecordOperation("read", 100*time.Millisecond, 1024, true)

		// No operations should be tracked
		if len(collector.operations) != 0 {
			t.Error("disabled collector should not track operations")
		}
	})
}

func TestRecordCacheOperations(t *testing.T) {
	t.Parallel()

	t.Run("record cache hit", func(t *testing.T) {
		config := &Config{
			Enabled:   true,
			Port:      9094,
			Namespace: "test",
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		// Should not panic
		collector.RecordCacheHit("test-key", 1024)
	})

	t.Run("record cache miss", func(t *testing.T) {
		config := &Config{
			Enabled:   true,
			Port:      9095,
			Namespace: "test",
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		// Should not panic
		collector.RecordCacheMiss("test-key", 1024)
	})

	t.Run("disabled collector ignores cache operations", func(t *testing.T) {
		config := &Config{
			Enabled: false,
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		// Should not panic
		collector.RecordCacheHit("test-key", 1024)
		collector.RecordCacheMiss("test-key", 1024)
	})
}

func TestRecordError(t *testing.T) {
	t.Parallel()

	t.Run("record error", func(t *testing.T) {
		config := &Config{
			Enabled:   true,
			Port:      9096,
			Namespace: "test",
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		testErr := errors.New("test error")
		collector.RecordError("test-operation", testErr)
	})

	t.Run("disabled collector ignores errors", func(t *testing.T) {
		config := &Config{
			Enabled: false,
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		testErr := errors.New("test error")
		collector.RecordError("test-operation", testErr)
	})
}

func TestClassifyError(t *testing.T) {
	t.Parallel()

	config := &Config{
		Enabled:   true,
		Port:      9097,
		Namespace: "test",
	}
	collector, err := NewCollector(config)
	if err != nil {
		t.Fatalf("NewCollector() error = %v", err)
	}

	tests := []struct {
		name         string
		err          error
		expectedType string
	}{
		{
			name:         "timeout error",
			err:          errors.New("operation timeout"),
			expectedType: "timeout",
		},
		{
			name:         "connection error",
			err:          errors.New("connection refused"),
			expectedType: "connection",
		},
		{
			name:         "not found error",
			err:          errors.New("file not found"),
			expectedType: "not_found",
		},
		{
			name:         "permission error",
			err:          errors.New("permission denied"),
			expectedType: "permission",
		},
		{
			name:         "throttling error",
			err:          errors.New("rate throttled"),
			expectedType: "throttling",
		},
		{
			name:         "other error",
			err:          errors.New("unknown error"),
			expectedType: "other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := collector.classifyError(tt.err)
			if result != tt.expectedType {
				t.Errorf("classifyError() = %q, want %q", result, tt.expectedType)
			}
		})
	}
}

func TestUpdateCacheSize(t *testing.T) {
	t.Parallel()

	t.Run("update cache size", func(t *testing.T) {
		config := &Config{
			Enabled:   true,
			Port:      9098,
			Namespace: "test",
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		collector.UpdateCacheSize("L1", 1024*1024)
		collector.UpdateCacheSize("L2", 10*1024*1024)
	})

	t.Run("disabled collector ignores cache size", func(t *testing.T) {
		config := &Config{
			Enabled: false,
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		collector.UpdateCacheSize("L1", 1024*1024)
	})
}

func TestUpdateActiveConnections(t *testing.T) {
	t.Parallel()

	t.Run("update active connections", func(t *testing.T) {
		config := &Config{
			Enabled:   true,
			Port:      9099,
			Namespace: "test",
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		collector.UpdateActiveConnections(10)
		collector.UpdateActiveConnections(5)
	})

	t.Run("disabled collector ignores connections", func(t *testing.T) {
		config := &Config{
			Enabled: false,
		}
		collector, err := NewCollector(config)
		if err != nil {
			t.Fatalf("NewCollector() error = %v", err)
		}

		collector.UpdateActiveConnections(10)
	})
}

func TestGetMetrics(t *testing.T) {
	t.Parallel()

	config := &Config{
		Enabled:   true,
		Port:      9100,
		Namespace: "test",
	}
	collector, err := NewCollector(config)
	if err != nil {
		t.Fatalf("NewCollector() error = %v", err)
	}

	// Record some operations
	collector.RecordOperation("read", 100*time.Millisecond, 1024, true)
	collector.RecordOperation("write", 50*time.Millisecond, 512, true)

	metrics := collector.GetMetrics()

	if metrics == nil {
		t.Fatal("GetMetrics() returned nil")
	}

	if _, ok := metrics["operations"]; !ok {
		t.Error("metrics missing 'operations' key")
	}

	if _, ok := metrics["last_reset"]; !ok {
		t.Error("metrics missing 'last_reset' key")
	}

	if _, ok := metrics["uptime"]; !ok {
		t.Error("metrics missing 'uptime' key")
	}

	operations, ok := metrics["operations"].(map[string]*OperationMetrics)
	if !ok {
		t.Fatal("operations is not map[string]*OperationMetrics")
	}

	if len(operations) != 2 {
		t.Errorf("len(operations) = %d, want 2", len(operations))
	}

	if _, exists := operations["read"]; !exists {
		t.Error("read operation not in metrics")
	}

	if _, exists := operations["write"]; !exists {
		t.Error("write operation not in metrics")
	}
}

func TestResetMetrics(t *testing.T) {
	t.Parallel()

	config := &Config{
		Enabled:   true,
		Port:      9101,
		Namespace: "test",
	}
	collector, err := NewCollector(config)
	if err != nil {
		t.Fatalf("NewCollector() error = %v", err)
	}

	// Record some operations
	collector.RecordOperation("read", 100*time.Millisecond, 1024, true)
	collector.RecordOperation("write", 50*time.Millisecond, 512, true)

	// Verify operations are recorded
	metrics := collector.GetMetrics()
	operations := metrics["operations"].(map[string]*OperationMetrics)
	if len(operations) != 2 {
		t.Errorf("before reset: len(operations) = %d, want 2", len(operations))
	}

	oldResetTime := collector.lastReset

	// Reset metrics
	time.Sleep(10 * time.Millisecond) // Ensure time difference
	collector.ResetMetrics()

	// Verify metrics are cleared
	metrics = collector.GetMetrics()
	operations = metrics["operations"].(map[string]*OperationMetrics)
	if len(operations) != 0 {
		t.Errorf("after reset: len(operations) = %d, want 0", len(operations))
	}

	if !collector.lastReset.After(oldResetTime) {
		t.Error("lastReset should be updated after reset")
	}
}

func TestStopWithoutStart(t *testing.T) {
	t.Parallel()

	config := &Config{
		Enabled:   true,
		Port:      9102,
		Namespace: "test",
	}
	collector, err := NewCollector(config)
	if err != nil {
		t.Fatalf("NewCollector() error = %v", err)
	}

	ctx := context.Background()
	// Should not panic when stopping without starting
	err = collector.Stop(ctx)
	if err != nil {
		t.Errorf("Stop() without Start() error = %v, want nil", err)
	}
}

func TestContainsHelper(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		s      string
		substr string
		want   bool
	}{
		{
			name:   "substring at start",
			s:      "hello world",
			substr: "hello",
			want:   true,
		},
		{
			name:   "substring in middle",
			s:      "hello world",
			substr: "lo wo",
			want:   true,
		},
		{
			name:   "substring at end",
			s:      "hello world",
			substr: "world",
			want:   true,
		},
		{
			name:   "substring not found",
			s:      "hello world",
			substr: "foo",
			want:   false,
		},
		{
			name:   "empty substring",
			s:      "hello",
			substr: "",
			want:   true,
		},
		{
			name:   "exact match",
			s:      "hello",
			substr: "hello",
			want:   true,
		},
		{
			name:   "substring longer than string",
			s:      "hi",
			substr: "hello",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := contains(tt.s, tt.substr)
			if result != tt.want {
				t.Errorf("contains(%q, %q) = %v, want %v", tt.s, tt.substr, result, tt.want)
			}
		})
	}
}

func TestIndexOfHelper(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		s      string
		substr string
		want   int
	}{
		{
			name:   "substring at start",
			s:      "hello world",
			substr: "hello",
			want:   0,
		},
		{
			name:   "substring in middle",
			s:      "hello world",
			substr: "world",
			want:   6,
		},
		{
			name:   "substring not found",
			s:      "hello world",
			substr: "foo",
			want:   -1,
		},
		{
			name:   "empty substring",
			s:      "hello",
			substr: "",
			want:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := indexOf(tt.s, tt.substr)
			if result != tt.want {
				t.Errorf("indexOf(%q, %q) = %d, want %d", tt.s, tt.substr, result, tt.want)
			}
		})
	}
}
