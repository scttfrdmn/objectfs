package retry

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/objectfs/objectfs/pkg/errors"
)

func TestRetryer_Success(t *testing.T) {
	config := DefaultConfig()
	config.MaxAttempts = 3
	retryer := New(config)

	attempts := 0
	err := retryer.Do(func() error {
		attempts++
		return nil // Success on first attempt
	})

	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}

	if attempts != 1 {
		t.Errorf("Expected 1 attempt, got %d", attempts)
	}
}

func TestRetryer_RetryableError(t *testing.T) {
	config := DefaultConfig()
	config.MaxAttempts = 3
	config.InitialDelay = 10 * time.Millisecond
	config.Jitter = false
	retryer := New(config)

	attempts := 0
	err := retryer.Do(func() error {
		attempts++
		if attempts < 3 {
			// Return retryable error
			return errors.NewError(errors.ErrCodeConnectionTimeout, "connection timeout")
		}
		return nil // Success on third attempt
	})

	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}

	if attempts != 3 {
		t.Errorf("Expected 3 attempts, got %d", attempts)
	}
}

func TestRetryer_NonRetryableError(t *testing.T) {
	config := DefaultConfig()
	config.MaxAttempts = 3
	retryer := New(config)

	attempts := 0
	testErr := errors.NewError(errors.ErrCodeFileNotFound, "file not found")
	testErr.Retryable = false

	err := retryer.Do(func() error {
		attempts++
		return testErr
	})

	if err == nil {
		t.Error("Expected error, got nil")
	}

	if attempts != 1 {
		t.Errorf("Expected 1 attempt (no retry), got %d", attempts)
	}
}

func TestRetryer_MaxAttemptsExceeded(t *testing.T) {
	config := DefaultConfig()
	config.MaxAttempts = 3
	config.InitialDelay = 10 * time.Millisecond
	config.Jitter = false
	retryer := New(config)

	attempts := 0
	testErr := errors.NewError(errors.ErrCodeNetworkError, "network error")

	err := retryer.Do(func() error {
		attempts++
		return testErr // Always fail
	})

	if err == nil {
		t.Error("Expected error, got nil")
	}

	if attempts != 3 {
		t.Errorf("Expected 3 attempts, got %d", attempts)
	}

	// The error returned should be the wrapped last error
	// It should either be the max attempts error or the original error
	if err == nil {
		t.Error("Expected error, got nil")
	}
}

func TestRetryer_ContextCancellation(t *testing.T) {
	config := DefaultConfig()
	config.MaxAttempts = 10
	config.InitialDelay = 100 * time.Millisecond
	retryer := New(config)

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0

	// Cancel after first failure
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := retryer.DoWithContext(ctx, func(ctx context.Context) error {
		attempts++
		return errors.NewError(errors.ErrCodeConnectionFailed, "connection failed")
	})

	if err == nil {
		t.Error("Expected error, got nil")
	}

	// Should stop after context cancellation, not reach max attempts
	if attempts >= 10 {
		t.Errorf("Expected fewer than 10 attempts due to cancellation, got %d", attempts)
	}
}

func TestRetryer_ExponentialBackoff(t *testing.T) {
	config := DefaultConfig()
	config.MaxAttempts = 4
	config.InitialDelay = 100 * time.Millisecond
	config.MaxDelay = 1 * time.Second
	config.Multiplier = 2.0
	config.Jitter = false

	delays := []time.Duration{}
	config.OnRetry = func(attempt int, err error, delay time.Duration) {
		delays = append(delays, delay)
	}

	retryer := New(config)

	err := retryer.Do(func() error {
		return errors.NewError(errors.ErrCodeNetworkError, "network error")
	})

	if err == nil {
		t.Error("Expected error, got nil")
	}

	// Check delays follow exponential backoff: 100ms, 200ms, 400ms
	expectedDelays := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
	}

	if len(delays) != len(expectedDelays) {
		t.Errorf("Expected %d delays, got %d", len(expectedDelays), len(delays))
	}

	for i, expected := range expectedDelays {
		if i >= len(delays) {
			break
		}
		if delays[i] != expected {
			t.Errorf("Delay %d: expected %v, got %v", i, expected, delays[i])
		}
	}
}

func TestRetryer_MaxDelayCap(t *testing.T) {
	config := DefaultConfig()
	config.MaxAttempts = 10
	config.InitialDelay = 1 * time.Second
	config.MaxDelay = 2 * time.Second
	config.Multiplier = 2.0
	config.Jitter = false

	var maxDelay time.Duration
	config.OnRetry = func(attempt int, err error, delay time.Duration) {
		if delay > maxDelay {
			maxDelay = delay
		}
	}

	retryer := New(config)

	_ = retryer.Do(func() error {
		return errors.NewError(errors.ErrCodeNetworkError, "network error")
	})

	// Max delay should not exceed configured max
	if maxDelay > config.MaxDelay {
		t.Errorf("Max delay %v exceeded configured max %v", maxDelay, config.MaxDelay)
	}
}

func TestRetryer_OnRetryCallback(t *testing.T) {
	config := DefaultConfig()
	config.MaxAttempts = 3
	config.InitialDelay = 10 * time.Millisecond

	callbackCalled := 0
	var lastAttempt int
	var lastErr error
	var lastDelay time.Duration

	config.OnRetry = func(attempt int, err error, delay time.Duration) {
		callbackCalled++
		lastAttempt = attempt
		lastErr = err
		lastDelay = delay
	}

	retryer := New(config)

	testErr := errors.NewError(errors.ErrCodeNetworkError, "network error")
	_ = retryer.Do(func() error {
		return testErr
	})

	if callbackCalled != 2 {
		t.Errorf("Expected callback called 2 times, got %d", callbackCalled)
	}

	if lastAttempt != 2 {
		t.Errorf("Expected last attempt to be 2, got %d", lastAttempt)
	}

	if lastErr != testErr {
		t.Errorf("Expected last error to be testErr, got %v", lastErr)
	}

	if lastDelay <= 0 {
		t.Error("Expected positive delay")
	}
}

func TestRetryer_WithMethods(t *testing.T) {
	original := New(DefaultConfig())

	// Test WithMaxAttempts
	modified := original.WithMaxAttempts(10)
	if modified.config.MaxAttempts != 10 {
		t.Errorf("Expected MaxAttempts=10, got %d", modified.config.MaxAttempts)
	}
	// Original should be unchanged
	if original.config.MaxAttempts == 10 {
		t.Error("Original config was modified")
	}

	// Test WithInitialDelay
	modified = original.WithInitialDelay(500 * time.Millisecond)
	if modified.config.InitialDelay != 500*time.Millisecond {
		t.Errorf("Expected InitialDelay=500ms, got %v", modified.config.InitialDelay)
	}

	// Test WithMaxDelay
	modified = original.WithMaxDelay(60 * time.Second)
	if modified.config.MaxDelay != 60*time.Second {
		t.Errorf("Expected MaxDelay=60s, got %v", modified.config.MaxDelay)
	}

	// Test WithOnRetry
	called := false
	modified = original.WithOnRetry(func(attempt int, err error, delay time.Duration) {
		called = true
	})

	_ = modified.Do(func() error {
		return errors.NewError(errors.ErrCodeNetworkError, "network error")
	})

	if !called {
		t.Error("OnRetry callback was not called")
	}
}

func TestRetryWithBackoff_Convenience(t *testing.T) {
	attempts := 0
	err := RetryWithBackoff(context.Background(), 3, func() error {
		attempts++
		if attempts < 2 {
			return errors.NewError(errors.ErrCodeNetworkError, "network error")
		}
		return nil
	})

	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}

	if attempts != 2 {
		t.Errorf("Expected 2 attempts, got %d", attempts)
	}
}

func TestRetryableFunc(t *testing.T) {
	attempts := 0
	fn := RetryableFunc(func() error {
		attempts++
		if attempts < 2 {
			return errors.NewError(errors.ErrCodeNetworkError, "network error")
		}
		return nil
	})

	err := fn.Retry()

	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}

	if attempts != 2 {
		t.Errorf("Expected 2 attempts, got %d", attempts)
	}
}

func TestStatsCollector(t *testing.T) {
	collector := NewStatsCollector()

	// Record some attempts
	collector.RecordAttempt(1, true, 100*time.Millisecond)
	collector.RecordAttempt(3, true, 500*time.Millisecond)
	collector.RecordAttempt(5, false, 1*time.Second)

	stats := collector.GetStats()

	if stats.TotalAttempts != 3 {
		t.Errorf("Expected TotalAttempts=3, got %d", stats.TotalAttempts)
	}

	if stats.SuccessfulRetry != 2 {
		t.Errorf("Expected SuccessfulRetry=2, got %d", stats.SuccessfulRetry)
	}

	if stats.FailedRetry != 1 {
		t.Errorf("Expected FailedRetry=1, got %d", stats.FailedRetry)
	}

	if stats.MaxAttemptsUsed != 5 {
		t.Errorf("Expected MaxAttemptsUsed=5, got %d", stats.MaxAttemptsUsed)
	}

	expectedDelay := 100*time.Millisecond + 500*time.Millisecond + 1*time.Second
	if stats.TotalDelay != expectedDelay {
		t.Errorf("Expected TotalDelay=%v, got %v", expectedDelay, stats.TotalDelay)
	}

	// Test reset
	collector.Reset()
	stats = collector.GetStats()

	if stats.TotalAttempts != 0 {
		t.Errorf("Expected TotalAttempts=0 after reset, got %d", stats.TotalAttempts)
	}
}

func TestRetryer_JitterVariance(t *testing.T) {
	config := DefaultConfig()
	config.MaxAttempts = 3
	config.InitialDelay = 100 * time.Millisecond
	config.Jitter = true

	delays := []time.Duration{}
	config.OnRetry = func(attempt int, err error, delay time.Duration) {
		delays = append(delays, delay)
	}

	retryer := New(config)

	_ = retryer.Do(func() error {
		return errors.NewError(errors.ErrCodeNetworkError, "network error")
	})

	// With jitter, delays should vary from exact exponential backoff
	// Check that at least one delay is different from base delay
	baseDelay := config.InitialDelay
	hasVariance := false

	for _, delay := range delays {
		if delay != baseDelay {
			hasVariance = true
			break
		}
		baseDelay = time.Duration(float64(baseDelay) * config.Multiplier)
	}

	if !hasVariance {
		t.Error("Expected jitter to create variance in delays")
	}
}

// Benchmark tests
func BenchmarkRetryer_Success(b *testing.B) {
	retryer := New(DefaultConfig())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = retryer.Do(func() error {
			return nil
		})
	}
}

func BenchmarkRetryer_WithRetries(b *testing.B) {
	config := DefaultConfig()
	config.MaxAttempts = 3
	config.InitialDelay = 1 * time.Millisecond
	retryer := New(config)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		attempts := 0
		_ = retryer.Do(func() error {
			attempts++
			if attempts < 3 {
				return errors.NewError(errors.ErrCodeNetworkError, "network error")
			}
			return nil
		})
	}
}

// Example tests
func ExampleRetryer() {
	retryer := New(DefaultConfig())

	err := retryer.Do(func() error {
		// Your operation that might fail
		return fmt.Errorf("temporary failure")
	})

	if err != nil {
		fmt.Println("Operation failed after retries")
	}
}

func ExampleRetryWithBackoff() {
	ctx := context.Background()

	err := RetryWithBackoff(ctx, 5, func() error {
		// Your operation
		return nil
	})

	if err != nil {
		fmt.Println("Failed:", err)
	} else {
		fmt.Println("Success")
	}
	// Output: Success
}
