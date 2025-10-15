// Package retry provides retry logic with exponential backoff for ObjectFS operations
package retry

import (
	"context"
	stderr "errors"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/objectfs/objectfs/pkg/errors"
)

// Config defines retry behavior configuration
type Config struct {
	// MaxAttempts is the maximum number of retry attempts (including initial attempt)
	MaxAttempts int `yaml:"max_attempts" json:"max_attempts"`

	// InitialDelay is the delay before the first retry
	InitialDelay time.Duration `yaml:"initial_delay" json:"initial_delay"`

	// MaxDelay is the maximum delay between retries
	MaxDelay time.Duration `yaml:"max_delay" json:"max_delay"`

	// Multiplier is the factor by which delay increases after each retry
	Multiplier float64 `yaml:"multiplier" json:"multiplier"`

	// Jitter adds randomness to delay to prevent thundering herd
	Jitter bool `yaml:"jitter" json:"jitter"`

	// RetryableErrors is a list of error codes that should trigger retry
	RetryableErrors []errors.ErrorCode `yaml:"retryable_errors" json:"retryable_errors"`

	// OnRetry is called before each retry attempt
	OnRetry func(attempt int, err error, delay time.Duration) `yaml:"-" json:"-"`
}

// DefaultConfig returns a sensible default retry configuration
func DefaultConfig() Config {
	return Config{
		MaxAttempts:  5,
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     30 * time.Second,
		Multiplier:   2.0,
		Jitter:       true,
		RetryableErrors: []errors.ErrorCode{
			errors.ErrCodeConnectionTimeout,
			errors.ErrCodeConnectionFailed,
			errors.ErrCodeNetworkError,
			errors.ErrCodeOperationTimeout,
			errors.ErrCodeResourceExhausted,
			errors.ErrCodeWorkerBusy,
			errors.ErrCodeInternalError,
		},
	}
}

// Retryer handles retry logic with exponential backoff
type Retryer struct {
	config Config
}

// New creates a new Retryer with the given configuration
func New(config Config) *Retryer {
	// Apply defaults for zero values
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 5
	}
	if config.InitialDelay <= 0 {
		config.InitialDelay = 100 * time.Millisecond
	}
	if config.MaxDelay <= 0 {
		config.MaxDelay = 30 * time.Second
	}
	if config.Multiplier <= 0 {
		config.Multiplier = 2.0
	}

	return &Retryer{config: config}
}

// Do executes the given function with retry logic
func (r *Retryer) Do(fn func() error) error {
	return r.DoWithContext(context.Background(), func(ctx context.Context) error {
		return fn()
	})
}

// DoWithContext executes the given function with retry logic and context support
func (r *Retryer) DoWithContext(ctx context.Context, fn func(context.Context) error) error {
	var lastErr error

	for attempt := 1; attempt <= r.config.MaxAttempts; attempt++ {
		// Check context before attempting
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation canceled: %w", ctx.Err())
		default:
		}

		// Execute the function
		err := fn(ctx)
		if err == nil {
			return nil // Success
		}

		lastErr = err

		// Check if we should retry
		if !r.shouldRetry(err, attempt) {
			return err
		}

		// Calculate delay for next attempt
		if attempt < r.config.MaxAttempts {
			delay := r.calculateDelay(attempt)

			// Call OnRetry callback if provided
			if r.config.OnRetry != nil {
				r.config.OnRetry(attempt, err, delay)
			}

			// Wait for delay or context cancellation
			select {
			case <-ctx.Done():
				return fmt.Errorf("operation canceled after %d attempts: %w", attempt, ctx.Err())
			case <-time.After(delay):
				// Continue to next attempt
			}
		}
	}

	// All attempts exhausted
	return fmt.Errorf("max retry attempts (%d) exceeded: %w", r.config.MaxAttempts, lastErr)
}

// shouldRetry determines if an error is retryable
func (r *Retryer) shouldRetry(err error, attempt int) bool {
	// Don't retry if we've reached max attempts
	if attempt >= r.config.MaxAttempts {
		return false
	}

	// Check if error is an ObjectFS error with retryable flag
	var objErr *errors.ObjectFSError
	if stderr.As(err, &objErr) {
		// Check explicit retryable flag
		if objErr.Retryable {
			return true
		}

		// Check if error code is in retryable list
		for _, code := range r.config.RetryableErrors {
			if objErr.Code == code {
				return true
			}
		}
	}

	return false
}

// calculateDelay calculates the delay for the next retry attempt
func (r *Retryer) calculateDelay(attempt int) time.Duration {
	// Exponential backoff: initialDelay * multiplier^(attempt-1)
	delay := float64(r.config.InitialDelay) * math.Pow(r.config.Multiplier, float64(attempt-1))

	// Apply max delay cap
	if delay > float64(r.config.MaxDelay) {
		delay = float64(r.config.MaxDelay)
	}

	// Apply jitter to prevent thundering herd
	if r.config.Jitter {
		// Add random jitter of ±20%
		jitter := delay * 0.2 * (rand.Float64()*2 - 1)
		delay += jitter
	}

	return time.Duration(delay)
}

// WithMaxAttempts returns a new Retryer with modified max attempts
func (r *Retryer) WithMaxAttempts(attempts int) *Retryer {
	newConfig := r.config
	newConfig.MaxAttempts = attempts
	return New(newConfig)
}

// WithInitialDelay returns a new Retryer with modified initial delay
func (r *Retryer) WithInitialDelay(delay time.Duration) *Retryer {
	newConfig := r.config
	newConfig.InitialDelay = delay
	return New(newConfig)
}

// WithMaxDelay returns a new Retryer with modified max delay
func (r *Retryer) WithMaxDelay(delay time.Duration) *Retryer {
	newConfig := r.config
	newConfig.MaxDelay = delay
	return New(newConfig)
}

// WithOnRetry returns a new Retryer with a retry callback
func (r *Retryer) WithOnRetry(callback func(attempt int, err error, delay time.Duration)) *Retryer {
	newConfig := r.config
	newConfig.OnRetry = callback
	return New(newConfig)
}

// RetryWithBackoff is a convenience function for simple retry scenarios
func RetryWithBackoff(ctx context.Context, maxAttempts int, fn func() error) error {
	retryer := New(DefaultConfig())
	retryer.config.MaxAttempts = maxAttempts
	return retryer.DoWithContext(ctx, func(ctx context.Context) error {
		return fn()
	})
}

// RetryableFunc wraps a function to make it retryable
type RetryableFunc func() error

// Retry executes the function with default retry configuration
func (rf RetryableFunc) Retry() error {
	retryer := New(DefaultConfig())
	return retryer.Do(func() error {
		return rf()
	})
}

// RetryWithConfig executes the function with custom retry configuration
func (rf RetryableFunc) RetryWithConfig(config Config) error {
	retryer := New(config)
	return retryer.Do(func() error {
		return rf()
	})
}

// Stats tracks retry statistics
type Stats struct {
	TotalAttempts   int           `json:"total_attempts"`
	SuccessfulRetry int           `json:"successful_retry"`
	FailedRetry     int           `json:"failed_retry"`
	AverageAttempts float64       `json:"average_attempts"`
	TotalDelay      time.Duration `json:"total_delay"`
	MaxAttemptsUsed int           `json:"max_attempts_used"`
}

// StatsCollector collects retry statistics
type StatsCollector struct {
	stats Stats
}

// NewStatsCollector creates a new stats collector
func NewStatsCollector() *StatsCollector {
	return &StatsCollector{}
}

// RecordAttempt records a retry attempt
func (sc *StatsCollector) RecordAttempt(attempts int, success bool, delay time.Duration) {
	sc.stats.TotalAttempts++
	if success {
		sc.stats.SuccessfulRetry++
	} else {
		sc.stats.FailedRetry++
	}

	sc.stats.TotalDelay += delay
	if attempts > sc.stats.MaxAttemptsUsed {
		sc.stats.MaxAttemptsUsed = attempts
	}

	// Calculate average attempts
	if sc.stats.TotalAttempts > 0 {
		sc.stats.AverageAttempts = float64(sc.stats.SuccessfulRetry+sc.stats.FailedRetry) / float64(sc.stats.TotalAttempts)
	}
}

// GetStats returns current statistics
func (sc *StatsCollector) GetStats() Stats {
	return sc.stats
}

// Reset resets statistics
func (sc *StatsCollector) Reset() {
	sc.stats = Stats{}
}
