package s3

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Benchmark configuration from environment variables
// Set these to run benchmarks against a real S3 bucket:
//
//	OBJECTFS_BENCH_BUCKET=your-bucket-name
//	OBJECTFS_BENCH_REGION=us-west-2  (optional, defaults to us-east-1)
const (
	envBenchBucket = "OBJECTFS_BENCH_BUCKET"
	envBenchRegion = "OBJECTFS_BENCH_REGION"
)

func getBenchConfig(b *testing.B) (*Config, string) {
	bucket := os.Getenv(envBenchBucket)
	if bucket == "" {
		b.Skipf("Skipping benchmark: set %s environment variable to run benchmarks", envBenchBucket)
	}

	region := os.Getenv(envBenchRegion)
	if region == "" {
		region = "us-east-1"
	}

	cfg := NewDefaultConfig()
	cfg.Region = region
	cfg.PoolSize = 10
	cfg.EnableCargoShipOptimization = false // Disable CargoShip for pure acceleration benchmarking

	return cfg, bucket
}

// BenchmarkGetObject_Standard benchmarks GetObject using standard S3 endpoint
func BenchmarkGetObject_Standard(b *testing.B) {
	benchmarkGetObject(b, false, 1024*1024) // 1MB objects
}

// BenchmarkGetObject_Accelerated benchmarks GetObject using S3 Transfer Acceleration
func BenchmarkGetObject_Accelerated(b *testing.B) {
	benchmarkGetObject(b, true, 1024*1024) // 1MB objects
}

// BenchmarkPutObject_Standard benchmarks PutObject using standard S3 endpoint
func BenchmarkPutObject_Standard(b *testing.B) {
	benchmarkPutObject(b, false, 1024*1024) // 1MB objects
}

// BenchmarkPutObject_Accelerated benchmarks PutObject using S3 Transfer Acceleration
func BenchmarkPutObject_Accelerated(b *testing.B) {
	benchmarkPutObject(b, true, 1024*1024) // 1MB objects
}

// BenchmarkGetObject_Large_Standard benchmarks GetObject for large objects (10MB)
func BenchmarkGetObject_Large_Standard(b *testing.B) {
	benchmarkGetObject(b, false, 10*1024*1024) // 10MB objects
}

// BenchmarkGetObject_Large_Accelerated benchmarks GetObject for large objects (10MB)
func BenchmarkGetObject_Large_Accelerated(b *testing.B) {
	benchmarkGetObject(b, true, 10*1024*1024) // 10MB objects
}

// BenchmarkPutObject_Large_Standard benchmarks PutObject for large objects (10MB)
func BenchmarkPutObject_Large_Standard(b *testing.B) {
	benchmarkPutObject(b, false, 10*1024*1024) // 10MB objects
}

// BenchmarkPutObject_Large_Accelerated benchmarks PutObject for large objects (10MB)
func BenchmarkPutObject_Large_Accelerated(b *testing.B) {
	benchmarkPutObject(b, true, 10*1024*1024) // 10MB objects
}

// BenchmarkFallback benchmarks the fallback mechanism when acceleration fails
func BenchmarkFallback(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping benchmark in short mode")
	}

	ctx := context.Background()
	cfg, bucket := getBenchConfig(b)
	cfg.UseAccelerate = true

	backend, err := NewBackend(ctx, bucket, cfg)
	if err != nil {
		b.Fatalf("Failed to create backend: %v", err)
	}
	defer backend.Close()

	data := make([]byte, 1024*1024) // 1MB
	key := fmt.Sprintf("benchmark-fallback-%d", time.Now().UnixNano())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Simulate acceleration error by temporarily disabling
		backend.clientManager.DisableAcceleration("benchmark test")

		err := backend.executeWithAccelerationFallback(ctx, "PutObject", func(client *s3.Client) error {
			_, err := client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
				Body:   bytes.NewReader(data),
			})
			return err
		})

		if err != nil {
			b.Fatalf("Fallback failed: %v", err)
		}

		// Re-enable for next iteration
		backend.clientManager.EnableAcceleration()
	}
	b.StopTimer()

	// Cleanup
	_ = backend.DeleteObject(ctx, key)
}

// BenchmarkAccelerationOverhead benchmarks the overhead of acceleration detection
func BenchmarkAccelerationOverhead(b *testing.B) {
	backend := &Backend{}

	testErrors := []error{
		fmt.Errorf("InvalidRequest: Transfer acceleration not enabled"),
		fmt.Errorf("s3-accelerate endpoint error"),
		fmt.Errorf("AccelerateNotSupported"),
		fmt.Errorf("normal S3 error"),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, err := range testErrors {
			_ = backend.isAccelerationError(err)
		}
	}
}

// Helper functions

func benchmarkGetObject(b *testing.B, useAccelerate bool, objectSize int) {
	if testing.Short() {
		b.Skip("Skipping benchmark in short mode")
	}

	ctx := context.Background()
	cfg, bucket := getBenchConfig(b)
	cfg.UseAccelerate = useAccelerate

	backend, err := NewBackend(ctx, bucket, cfg)
	if err != nil {
		b.Fatalf("Failed to create backend: %v", err)
	}
	defer backend.Close()

	// Setup: Create test object
	data := make([]byte, objectSize)
	key := fmt.Sprintf("benchmark-get-%d", time.Now().UnixNano())
	if err := backend.PutObject(ctx, key, data); err != nil {
		b.Fatalf("Failed to create test object: %v", err)
	}
	defer backend.DeleteObject(ctx, key)

	b.ResetTimer()
	b.SetBytes(int64(objectSize))

	for i := 0; i < b.N; i++ {
		_, err := backend.GetObject(ctx, key, 0, 0)
		if err != nil {
			b.Fatalf("GetObject failed: %v", err)
		}
	}

	b.StopTimer()

	// Report acceleration status
	if useAccelerate {
		metrics := backend.GetMetrics()
		b.ReportMetric(float64(metrics.AcceleratedRequests), "accelerated_requests")
		if metrics.FallbackEvents > 0 {
			b.ReportMetric(float64(metrics.FallbackEvents), "fallback_events")
		}
	}
}

func benchmarkPutObject(b *testing.B, useAccelerate bool, objectSize int) {
	if testing.Short() {
		b.Skip("Skipping benchmark in short mode")
	}

	ctx := context.Background()
	cfg, bucket := getBenchConfig(b)
	cfg.UseAccelerate = useAccelerate

	backend, err := NewBackend(ctx, bucket, cfg)
	if err != nil {
		b.Fatalf("Failed to create backend: %v", err)
	}
	defer backend.Close()

	data := make([]byte, objectSize)

	b.ResetTimer()
	b.SetBytes(int64(objectSize))

	keys := make([]string, b.N)
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("benchmark-put-%d-%d", time.Now().UnixNano(), i)
		keys[i] = key
		if err := backend.PutObject(ctx, key, data); err != nil {
			b.Fatalf("PutObject failed: %v", err)
		}
	}

	b.StopTimer()

	// Report acceleration status
	if useAccelerate {
		metrics := backend.GetMetrics()
		b.ReportMetric(float64(metrics.AcceleratedRequests), "accelerated_requests")
		if metrics.FallbackEvents > 0 {
			b.ReportMetric(float64(metrics.FallbackEvents), "fallback_events")
		}
	}

	// Cleanup
	for _, key := range keys {
		_ = backend.DeleteObject(ctx, key)
	}
}

// BenchmarkMultipart_32MB benchmarks multipart upload at threshold (32MB)
func BenchmarkMultipart_32MB(b *testing.B) {
	benchmarkPutObject(b, false, 32*1024*1024)
}

// BenchmarkMultipart_32MB_Accelerated benchmarks multipart upload with acceleration (32MB)
func BenchmarkMultipart_32MB_Accelerated(b *testing.B) {
	benchmarkPutObject(b, true, 32*1024*1024)
}

// BenchmarkMultipart_100MB benchmarks multipart upload for 100MB objects
func BenchmarkMultipart_100MB(b *testing.B) {
	benchmarkPutObject(b, false, 100*1024*1024)
}

// BenchmarkMultipart_100MB_Accelerated benchmarks multipart upload with acceleration (100MB)
func BenchmarkMultipart_100MB_Accelerated(b *testing.B) {
	benchmarkPutObject(b, true, 100*1024*1024)
}

// BenchmarkMultipart_500MB benchmarks multipart upload for 500MB objects
func BenchmarkMultipart_500MB(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping large multipart benchmark in short mode")
	}
	benchmarkPutObject(b, false, 500*1024*1024)
}

// BenchmarkMultipart_500MB_Accelerated benchmarks multipart upload with acceleration (500MB)
func BenchmarkMultipart_500MB_Accelerated(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping large multipart benchmark in short mode")
	}
	benchmarkPutObject(b, true, 500*1024*1024)
}

// BenchmarkSinglePartVsMultipart compares single-part and multipart upload performance
func BenchmarkSinglePartVsMultipart(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping comparison benchmark in short mode")
	}

	ctx := context.Background()
	cfg, bucket := getBenchConfig(b)
	cfg.UseAccelerate = false

	// Test just below threshold (single-part)
	b.Run("SinglePart_31MB", func(b *testing.B) {
		backend, err := NewBackend(ctx, bucket, cfg)
		if err != nil {
			b.Fatalf("Failed to create backend: %v", err)
		}
		defer backend.Close()

		size := 31 * 1024 * 1024
		data := make([]byte, size)
		b.ResetTimer()
		b.SetBytes(int64(size))

		for i := 0; i < b.N; i++ {
			key := fmt.Sprintf("benchmark-single-%d-%d", time.Now().UnixNano(), i)
			if err := backend.PutObject(ctx, key, data); err != nil {
				b.Fatalf("PutObject failed: %v", err)
			}
			_ = backend.DeleteObject(ctx, key)
		}
	})

	// Test just above threshold (multipart)
	b.Run("Multipart_33MB", func(b *testing.B) {
		backend, err := NewBackend(ctx, bucket, cfg)
		if err != nil {
			b.Fatalf("Failed to create backend: %v", err)
		}
		defer backend.Close()

		size := 33 * 1024 * 1024
		data := make([]byte, size)
		b.ResetTimer()
		b.SetBytes(int64(size))

		for i := 0; i < b.N; i++ {
			key := fmt.Sprintf("benchmark-multi-%d-%d", time.Now().UnixNano(), i)
			if err := backend.PutObject(ctx, key, data); err != nil {
				b.Fatalf("PutObject failed: %v", err)
			}
			_ = backend.DeleteObject(ctx, key)
		}
	})
}

// BenchmarkMultipartConcurrency tests multipart upload with different concurrency levels
func BenchmarkMultipartConcurrency(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping concurrency benchmark in short mode")
	}

	ctx := context.Background()
	_, bucket := getBenchConfig(b)

	concurrencyLevels := []int{1, 4, 8, 16}
	size := 100 * 1024 * 1024 // 100MB

	for _, concurrency := range concurrencyLevels {
		b.Run(fmt.Sprintf("Concurrency_%d", concurrency), func(b *testing.B) {
			cfg := NewDefaultConfig()
			cfg.Region = os.Getenv(envBenchRegion)
			if cfg.Region == "" {
				cfg.Region = "us-east-1"
			}
			cfg.PoolSize = 10
			cfg.EnableCargoShipOptimization = false
			cfg.MultipartConcurrency = concurrency

			backend, err := NewBackend(ctx, bucket, cfg)
			if err != nil {
				b.Fatalf("Failed to create backend: %v", err)
			}
			defer backend.Close()

			data := make([]byte, size)
			b.ResetTimer()
			b.SetBytes(int64(size))

			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("benchmark-concurrency-%d-%d-%d", concurrency, time.Now().UnixNano(), i)
				if err := backend.PutObject(ctx, key, data); err != nil {
					b.Fatalf("PutObject failed: %v", err)
				}
				_ = backend.DeleteObject(ctx, key)
			}
		})
	}
}

// BenchmarkSuite runs a comprehensive performance comparison
func BenchmarkSuite(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping comprehensive benchmark in short mode")
	}

	sizes := []int{
		1024,             // 1KB
		1024 * 1024,      // 1MB
		10 * 1024 * 1024, // 10MB
	}

	for _, size := range sizes {
		sizeName := fmt.Sprintf("%dKB", size/1024)
		if size >= 1024*1024 {
			sizeName = fmt.Sprintf("%dMB", size/(1024*1024))
		}

		b.Run(fmt.Sprintf("Get_%s_Standard", sizeName), func(b *testing.B) {
			benchmarkGetObject(b, false, size)
		})

		b.Run(fmt.Sprintf("Get_%s_Accelerated", sizeName), func(b *testing.B) {
			benchmarkGetObject(b, true, size)
		})

		b.Run(fmt.Sprintf("Put_%s_Standard", sizeName), func(b *testing.B) {
			benchmarkPutObject(b, false, size)
		})

		b.Run(fmt.Sprintf("Put_%s_Accelerated", sizeName), func(b *testing.B) {
			benchmarkPutObject(b, true, size)
		})
	}
}
