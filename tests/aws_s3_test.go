//go:build aws_s3
// +build aws_s3

package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	s3backend "github.com/scttfrdmn/objectfs/internal/storage/s3"
)

// AWSS3TestSuite tests ObjectFS against real AWS S3
type AWSS3TestSuite struct {
	suite.Suite
	ctx     context.Context
	client  *s3.Client
	backend *s3backend.Backend
	bucket  string
	region  string
	profile string
}

func TestAWSS3Integration(t *testing.T) {
	// Ensure we have the required AWS profile and test bucket
	profile := os.Getenv("AWS_PROFILE")
	if profile == "" {
		profile = "aws" // Default to 'aws' profile
	}

	bucket := os.Getenv("OBJECTFS_TEST_BUCKET")
	if bucket == "" {
		t.Skip("Skipping AWS S3 tests - OBJECTFS_TEST_BUCKET not set")
	}

	t.Logf("Running AWS S3 tests with profile '%s' in us-west-2 against bucket '%s'", profile, bucket)

	suite.Run(t, &AWSS3TestSuite{
		bucket:  bucket,
		region:  "us-west-2",
		profile: profile,
	})
}

func (s *AWSS3TestSuite) SetupSuite() {
	s.ctx = context.Background()

	// Load AWS config with specific profile and region
	cfg, err := config.LoadDefaultConfig(s.ctx,
		config.WithSharedConfigProfile(s.profile),
		config.WithRegion(s.region),
	)
	require.NoError(s.T(), err, "Failed to load AWS config")

	s.client = s3.NewFromConfig(cfg)

	// Verify bucket exists and we have access
	_, err = s.client.HeadBucket(s.ctx, &s3.HeadBucketInput{
		Bucket: &s.bucket,
	})
	require.NoError(s.T(), err, "Cannot access test bucket %s", s.bucket)

	backendConfig := &s3backend.Config{
		Region:         s.region,
		MaxRetries:     3,
		ConnectTimeout: 10 * time.Second,
		RequestTimeout: 60 * time.Second,
		PoolSize:       8,
	}

	s.backend, err = s3backend.NewBackend(s.ctx, s.bucket, backendConfig)
	require.NoError(s.T(), err, "Failed to create S3 backend")

	s.T().Logf("AWS S3 backend initialized")
}

func (s *AWSS3TestSuite) TearDownSuite() {
	if s.backend != nil {
		s.backend.Close()
	}
}

func (s *AWSS3TestSuite) SetupTest() {
	// Clean up any test objects from previous runs
	s.cleanupTestObjects()
}

func (s *AWSS3TestSuite) TearDownTest() {
	// Clean up test objects after each test
	s.cleanupTestObjects()
}

func (s *AWSS3TestSuite) cleanupTestObjects() {
	// List and delete objects with test prefix
	resp, err := s.client.ListObjectsV2(s.ctx, &s3.ListObjectsV2Input{
		Bucket: &s.bucket,
		Prefix: strPtr("objectfs-test/"),
	})
	if err != nil {
		return // Ignore cleanup errors
	}

	for _, obj := range resp.Contents {
		s.client.DeleteObject(s.ctx, &s3.DeleteObjectInput{
			Bucket: &s.bucket,
			Key:    obj.Key,
		})
	}
}

func (s *AWSS3TestSuite) TestBasicOperations() {
	t := s.T()

	// Test data
	key := "objectfs-test/basic-operations"
	data := []byte("Hello from ObjectFS!")

	start := time.Now()

	err := s.backend.PutObject(s.ctx, key, data, nil)
	assert.NoError(t, err)

	uploadDuration := time.Since(start)
	t.Logf("📤 Upload completed in %v", uploadDuration)

	// Test GetObject
	start = time.Now()
	retrieved, err := s.backend.GetObject(s.ctx, key, 0, 0)
	assert.NoError(t, err)
	assert.Equal(t, data, retrieved)

	downloadDuration := time.Since(start)
	t.Logf("📥 Download completed in %v", downloadDuration)

	// Test HeadObject
	info, err := s.backend.HeadObject(s.ctx, key)
	assert.NoError(t, err)
	assert.Equal(t, key, info.Key)
	assert.Equal(t, int64(len(data)), info.Size)

	// Test DeleteObject
	err = s.backend.DeleteObject(s.ctx, key)
	assert.NoError(t, err)

	// Verify deletion
	_, err = s.backend.GetObject(s.ctx, key, 0, 0)
	assert.Error(t, err)

	t.Logf("✅ Basic operations completed successfully")
}

func (s *AWSS3TestSuite) TestLargeFileUpload() {
	t := s.T()

	// 10MB, above the multipart threshold
	key := "objectfs-test/large-file-10mb"
	data := make([]byte, 10*1024*1024) // 10MB
	for i := range data {
		data[i] = byte(i % 256)
	}

	start := time.Now()

	err := s.backend.PutObject(s.ctx, key, data, nil)
	require.NoError(t, err)

	uploadDuration := time.Since(start)
	throughputMBps := float64(len(data)) / (1024 * 1024) / uploadDuration.Seconds()

	t.Logf("📤 10MB upload: %v (%.2f MB/s)", uploadDuration, throughputMBps)

	// Download and verify
	start = time.Now()
	retrieved, err := s.backend.GetObject(s.ctx, key, 0, 0)
	require.NoError(t, err)

	downloadDuration := time.Since(start)
	downloadThroughputMBps := float64(len(retrieved)) / (1024 * 1024) / downloadDuration.Seconds()

	t.Logf("📥 10MB download: %v (%.2f MB/s)", downloadDuration, downloadThroughputMBps)

	assert.Equal(t, len(data), len(retrieved))
	assert.Equal(t, data, retrieved)

	// Clean up
	s.backend.DeleteObject(s.ctx, key)

	t.Logf("✅ Large file test completed - Upload: %.2f MB/s, Download: %.2f MB/s",
		throughputMBps, downloadThroughputMBps)
}

func (s *AWSS3TestSuite) TestRangeRequests() {
	t := s.T()

	key := "objectfs-test/range-requests"
	data := make([]byte, 1024*1024) // 1MB
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Upload test data
	err := s.backend.PutObject(s.ctx, key, data, nil)
	require.NoError(t, err)

	// Test range request - first 1KB
	partial, err := s.backend.GetObject(s.ctx, key, 0, 1024)
	assert.NoError(t, err)
	assert.Equal(t, data[:1024], partial)

	// Test range request - middle 1KB
	partial, err = s.backend.GetObject(s.ctx, key, 512*1024, 1024)
	assert.NoError(t, err)
	assert.Equal(t, data[512*1024:512*1024+1024], partial)

	// Test range request - last 1KB
	partial, err = s.backend.GetObject(s.ctx, key, int64(len(data)-1024), 1024)
	assert.NoError(t, err)
	assert.Equal(t, data[len(data)-1024:], partial)

	// Clean up
	s.backend.DeleteObject(s.ctx, key)

	t.Logf("✅ Range requests working correctly")
}

func (s *AWSS3TestSuite) TestBatchOperations() {
	t := s.T()

	// Create test data
	objects := make(map[string][]byte)
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("objectfs-test/batch-%d", i)
		data := []byte(fmt.Sprintf("Batch test data %d", i))
		objects[key] = data
	}

	start := time.Now()

	// Test batch upload
	err := s.backend.PutObjects(s.ctx, objects)
	assert.NoError(t, err)

	batchUploadDuration := time.Since(start)
	t.Logf("📤 Batch upload (%d objects): %v", len(objects), batchUploadDuration)

	// Test batch download
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}

	start = time.Now()
	results, err := s.backend.GetObjects(s.ctx, keys)
	assert.NoError(t, err)
	assert.Len(t, results, len(objects))

	batchDownloadDuration := time.Since(start)
	t.Logf("📥 Batch download (%d objects): %v", len(keys), batchDownloadDuration)

	// Verify all objects
	for key, expectedData := range objects {
		actualData, exists := results[key]
		assert.True(t, exists, "Key %s should exist in results", key)
		assert.Equal(t, expectedData, actualData)
	}

	t.Logf("✅ Batch operations completed successfully")
}

func (s *AWSS3TestSuite) TestPerformanceBenchmark() {
	t := s.T()

	// Test various file sizes to evaluate CargoShip performance
	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1024},
		{"10KB", 10 * 1024},
		{"100KB", 100 * 1024},
		{"1MB", 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
	}

	t.Logf("🚀 Performance Benchmark with CargoShip Optimization")
	t.Logf("Network: 10Gbps local, 5Gbps+ internet | Target: 800 MB/s")
	t.Logf("%-10s | %-15s | %-15s | %-15s | %-15s", "Size", "Upload Time", "Upload MB/s", "Download Time", "Download MB/s")
	t.Logf("-----------|-----------------|-----------------|-----------------|------------------")

	for _, test := range sizes {
		key := fmt.Sprintf("objectfs-test/perf-%s", test.name)
		data := make([]byte, test.size)
		for i := range data {
			data[i] = byte(i % 256)
		}

		// Upload benchmark
		start := time.Now()
		err := s.backend.PutObject(s.ctx, key, data, nil)
		require.NoError(t, err)
		uploadDuration := time.Since(start)
		uploadThroughput := float64(test.size) / (1024 * 1024) / uploadDuration.Seconds()

		// Download benchmark
		start = time.Now()
		retrieved, err := s.backend.GetObject(s.ctx, key, 0, 0)
		require.NoError(t, err)
		downloadDuration := time.Since(start)
		downloadThroughput := float64(len(retrieved)) / (1024 * 1024) / downloadDuration.Seconds()

		assert.Equal(t, data, retrieved)

		t.Logf("%-10s | %-15v | %-15.2f | %-15v | %-15.2f",
			test.name, uploadDuration, uploadThroughput, downloadDuration, downloadThroughput)

		// Clean up
		s.backend.DeleteObject(s.ctx, key)
	}

	// Get backend metrics
	metrics := s.backend.GetMetrics()
	t.Logf("\n📊 Backend Metrics:")
	t.Logf("Total Requests: %d", metrics.Requests)
	t.Logf("Total Errors: %d", metrics.Errors)
	t.Logf("Bytes Uploaded: %d (%.2f MB)", metrics.BytesUploaded, float64(metrics.BytesUploaded)/(1024*1024))
	t.Logf("Bytes Downloaded: %d (%.2f MB)", metrics.BytesDownloaded, float64(metrics.BytesDownloaded)/(1024*1024))
	t.Logf("Average Latency: %v", metrics.AverageLatency)

	t.Logf("✅ Performance benchmark completed")
}

func (s *AWSS3TestSuite) TestRealLocalData() {
	t := s.T()

	t.Logf("📁 Testing with Real Local Data")
	t.Logf("Network: 10Gbps local → 5Gbps+ internet | CargoShip optimization enabled")

	// Search paths for real data files
	searchPaths := []string{
		os.ExpandEnv("$HOME/Downloads"),
		os.ExpandEnv("$HOME/src"),
		"/Volumes/Public/genomics_training", // Keep genomics as backup
	}

	var testFiles []string

	// Find interesting files from local directories
	for _, searchPath := range searchPaths {
		if _, err := os.Stat(searchPath); os.IsNotExist(err) {
			continue // Skip if path doesn't exist
		}

		err := filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // Skip errors
			}

			// Skip hidden files, cache, and system directories
			if strings.Contains(path, "/.") || strings.Contains(path, "@Recycle") ||
				strings.Contains(path, "node_modules") || strings.Contains(path, ".git") ||
				strings.Contains(path, "__pycache__") || strings.Contains(path, "venv") {
				return nil
			}

			// Look for various file types with reasonable sizes (1KB - 50MB)
			if info.Size() > 1024 && info.Size() < 50*1024*1024 {
				ext := strings.ToLower(filepath.Ext(info.Name()))
				if ext == ".pdf" || ext == ".xlsx" || ext == ".xls" ||
					ext == ".json" || ext == ".go" || ext == ".py" ||
					ext == ".md" || ext == ".txt" || ext == ".csv" ||
					ext == ".vcf" || strings.HasSuffix(info.Name(), ".vcf.gz") {
					testFiles = append(testFiles, path)
				}
			}

			// Limit to first 5 files for testing
			if len(testFiles) >= 5 {
				return filepath.SkipDir
			}

			return nil
		})

		if err != nil {
			continue // Skip paths with errors
		}

		// Stop searching if we found enough files
		if len(testFiles) >= 3 {
			break
		}
	}

	if len(testFiles) == 0 {
		t.Skip("No suitable local files found for testing")
	}

	t.Logf("Found %d local files for testing", len(testFiles))

	var totalBytes int64
	var totalUploadTime time.Duration
	var totalDownloadTime time.Duration

	for i, filePath := range testFiles {
		// Read the real local file
		file, err := os.Open(filePath)
		require.NoError(t, err)

		data, err := io.ReadAll(file)
		file.Close()
		require.NoError(t, err)

		fileName := filepath.Base(filePath)
		fileSize := len(data)
		totalBytes += int64(fileSize)

		// Get file type for better logging
		ext := strings.ToLower(filepath.Ext(fileName))
		t.Logf("\n📁 File %d: %s (%s, %.2f MB)", i+1, fileName, ext, float64(fileSize)/(1024*1024))

		// Test upload with CargoShip optimization
		key := fmt.Sprintf("objectfs-test/realdata/%s", fileName)

		start := time.Now()
		err = s.backend.PutObject(s.ctx, key, data, nil)
		require.NoError(t, err)
		uploadDuration := time.Since(start)
		totalUploadTime += uploadDuration

		uploadThroughput := float64(fileSize) / (1024 * 1024) / uploadDuration.Seconds()
		t.Logf("📤 Upload: %v (%.2f MB/s)", uploadDuration, uploadThroughput)

		// Test download
		start = time.Now()
		retrieved, err := s.backend.GetObject(s.ctx, key, 0, 0)
		require.NoError(t, err)
		downloadDuration := time.Since(start)
		totalDownloadTime += downloadDuration

		downloadThroughput := float64(len(retrieved)) / (1024 * 1024) / downloadDuration.Seconds()
		t.Logf("📥 Download: %v (%.2f MB/s)", downloadDuration, downloadThroughput)

		// Verify data integrity
		assert.Equal(t, len(data), len(retrieved))
		assert.Equal(t, data, retrieved)
		t.Logf("✅ Data integrity verified")

		// Test range request on real data
		if fileSize > 1024 {
			partial, err := s.backend.GetObject(s.ctx, key, 0, 1024)
			assert.NoError(t, err)
			assert.Equal(t, data[:1024], partial)
			t.Logf("✅ Range request verified")
		}

		// Clean up
		s.backend.DeleteObject(s.ctx, key)
	}

	// Calculate aggregate performance
	avgUploadThroughput := float64(totalBytes) / (1024 * 1024) / totalUploadTime.Seconds()
	avgDownloadThroughput := float64(totalBytes) / (1024 * 1024) / totalDownloadTime.Seconds()

	t.Logf("\n🚀 Aggregate Performance with Real Local Data:")
	t.Logf("Total Data: %.2f MB", float64(totalBytes)/(1024*1024))
	t.Logf("Total Upload Time: %v", totalUploadTime)
	t.Logf("Total Download Time: %v", totalDownloadTime)
	t.Logf("Average Upload Throughput: %.2f MB/s", avgUploadThroughput)
	t.Logf("Average Download Throughput: %.2f MB/s", avgDownloadThroughput)

	// Check if we're getting performance benefits
	if avgUploadThroughput > 100 {
		t.Logf("🎯 Excellent upload performance achieved!")
	} else if avgUploadThroughput > 50 {
		t.Logf("✅ Good upload performance")
	} else {
		t.Logf("⚠️  Upload performance below expectations")
	}

	t.Logf("✅ Real local data test completed successfully")
}

func (s *AWSS3TestSuite) TestHighThroughputStress() {
	t := s.T()

	t.Logf("🔥 High-Throughput Stress Test with CargoShip")
	t.Logf("Network: 10Gbps local → 5Gbps+ internet | Target: Approach 800 MB/s")

	// Create larger test files (50MB, 100MB, 200MB)
	sizes := []struct {
		name string
		size int
	}{
		{"50MB", 50 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"200MB", 200 * 1024 * 1024},
	}

	t.Logf("%-10s | %-15s | %-15s | %-15s | %-15s", "Size", "Upload Time", "Upload MB/s", "Download Time", "Download MB/s")
	t.Logf("-----------|-----------------|-----------------|-----------------|------------------")

	for _, test := range sizes {
		key := fmt.Sprintf("objectfs-test/stress-%s", test.name)

		// Create test data with pattern to verify integrity
		data := make([]byte, test.size)
		for i := range data {
			data[i] = byte((i / 1024) % 256) // Pattern every 1KB
		}

		// Upload stress test
		start := time.Now()
		err := s.backend.PutObject(s.ctx, key, data, nil)
		require.NoError(t, err)
		uploadDuration := time.Since(start)
		uploadThroughput := float64(test.size) / (1024 * 1024) / uploadDuration.Seconds()

		// Download stress test
		start = time.Now()
		retrieved, err := s.backend.GetObject(s.ctx, key, 0, 0)
		require.NoError(t, err)
		downloadDuration := time.Since(start)
		downloadThroughput := float64(len(retrieved)) / (1024 * 1024) / downloadDuration.Seconds()

		// Verify data integrity
		assert.Equal(t, len(data), len(retrieved))
		assert.Equal(t, data, retrieved)

		t.Logf("%-10s | %-15v | %-15.2f | %-15v | %-15.2f",
			test.name, uploadDuration, uploadThroughput, downloadDuration, downloadThroughput)

		// Performance analysis
		if uploadThroughput > 400 {
			t.Logf("🚀 Excellent performance: %.2f MB/s (>50%% of 800 MB/s target)", uploadThroughput)
		} else if uploadThroughput > 200 {
			t.Logf("✅ Good performance: %.2f MB/s", uploadThroughput)
		} else {
			t.Logf("⚠️  Performance below expectations: %.2f MB/s", uploadThroughput)
		}

		// Clean up
		s.backend.DeleteObject(s.ctx, key)
	}

	t.Logf("✅ High-throughput stress test completed")
}

func (s *AWSS3TestSuite) TestHealthCheck() {
	t := s.T()

	err := s.backend.HealthCheck(s.ctx)
	assert.NoError(t, err)

	t.Logf("✅ Health check passed")
}

// TestListObjects verifies that ListObjects returns expected keys after put.
func (s *AWSS3TestSuite) TestListObjects() {
	t := s.T()

	prefix := "objectfs-test/list-objects/"
	keys := []string{
		prefix + "alpha",
		prefix + "beta",
		prefix + "gamma",
	}

	// Upload three objects.
	for _, key := range keys {
		err := s.backend.PutObject(s.ctx, key, []byte("list-test-"+key), nil)
		require.NoError(t, err, "PutObject %s", key)
	}

	// List with the shared prefix.
	results, err := s.backend.ListObjects(s.ctx, prefix, 100)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(results), len(keys),
		"expected at least %d results, got %d", len(keys), len(results))

	// Verify all uploaded keys appear in the listing.
	listed := make(map[string]bool, len(results))
	for _, obj := range results {
		listed[obj.Key] = true
	}
	for _, key := range keys {
		assert.True(t, listed[key], "key %q should appear in ListObjects", key)
	}

	// Verify limit parameter is respected.
	limited, err := s.backend.ListObjects(s.ctx, prefix, 1)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(limited), 1, "limit=1 should return at most 1 result")

	// Clean up.
	for _, key := range keys {
		s.backend.DeleteObject(s.ctx, key) //nolint:errcheck
	}

	t.Logf("✅ ListObjects test passed (%d objects)", len(results))
}

// TestMultipartUpload explicitly exercises the multipart upload code path by
// configuring a 5 MB threshold and uploading a 6 MB object.
func (s *AWSS3TestSuite) TestMultipartUpload() {
	t := s.T()

	// Build a backend with a 5 MB multipart threshold to ensure multipart is used.
	mpCfg := &s3backend.Config{
		Region:             s.region,
		MaxRetries:         3,
		ConnectTimeout:     10 * time.Second,
		RequestTimeout:     120 * time.Second,
		PoolSize:           4,
		MultipartThreshold: 5 * 1024 * 1024, // 5 MB
		MultipartChunkSize: 5 * 1024 * 1024, // 5 MB parts
	}
	mpBackend, err := s3backend.NewBackend(s.ctx, s.bucket, mpCfg)
	require.NoError(t, err)
	defer mpBackend.Close() //nolint:errcheck

	key := "objectfs-test/multipart-6mb"
	const size = 6 * 1024 * 1024 // 6 MB — above the 5 MB threshold
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251) // prime-modulo pattern for easy verification
	}

	// Upload should use multipart path.
	start := time.Now()
	err = mpBackend.PutObject(s.ctx, key, data, nil)
	require.NoError(t, err)
	t.Logf("Multipart upload: 6 MB in %v", time.Since(start))

	// Download and verify full round-trip.
	retrieved, err := mpBackend.GetObject(s.ctx, key, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, size, len(retrieved))
	assert.Equal(t, data, retrieved, "data integrity check failed after multipart upload")

	// Partial read from a multipart-uploaded object.
	partial, err := mpBackend.GetObject(s.ctx, key, 1024, 512)
	require.NoError(t, err)
	assert.Equal(t, data[1024:1024+512], partial, "partial read after multipart upload")

	// Clean up.
	mpBackend.DeleteObject(s.ctx, key) //nolint:errcheck

	t.Logf("✅ Multipart upload test passed (6 MB object, 5 MB threshold)")
}

// TestZSTDCompression verifies that transparent ZSTD compression stores and
// retrieves objects correctly, with byte-for-byte data integrity.
func (s *AWSS3TestSuite) TestZSTDCompression() {
	t := s.T()

	// Build a backend with compression enabled.
	zstdCfg := &s3backend.Config{
		Region:         s.region,
		MaxRetries:     3,
		ConnectTimeout: 10 * time.Second,
		RequestTimeout: 60 * time.Second,
		PoolSize:       4,
		Compression: s3backend.CompressionConfig{
			Enabled:   true,
			Algorithm: "zstd",
			Level:     3,
			MinSize:   "1KB",
		},
	}
	zstdBackend, err := s3backend.NewBackend(s.ctx, s.bucket, zstdCfg)
	require.NoError(t, err)
	defer zstdBackend.Close() //nolint:errcheck

	// Use compressible text data to ensure compression is actually applied.
	key := "objectfs-test/zstd-compression"
	// 32 KB of repeated text — compresses to ~200 bytes with zstd.
	rawLine := []byte("ObjectFS transparent ZSTD compression integration test line.\n")
	data := make([]byte, 0, 32*1024)
	for len(data) < 32*1024 {
		data = append(data, rawLine...)
	}
	data = data[:32*1024]

	// Upload through the ZSTD-enabled backend.
	err = zstdBackend.PutObject(s.ctx, key, data, nil)
	require.NoError(t, err)

	// Download via the same ZSTD backend — should auto-decompress.
	retrieved, err := zstdBackend.GetObject(s.ctx, key, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, len(data), len(retrieved), "decompressed size mismatch")
	assert.Equal(t, data, retrieved, "data integrity check failed after ZSTD round-trip")

	// Partial read after ZSTD decompression.
	partial, err := zstdBackend.GetObject(s.ctx, key, 100, 200)
	require.NoError(t, err)
	assert.Equal(t, data[100:300], partial, "partial read after ZSTD decompression")

	// Confirm the object is NOT readable as raw bytes from a plain backend
	// (i.e. verify it was actually stored compressed).
	plainBackend, err := s3backend.NewBackend(s.ctx, s.bucket, &s3backend.Config{
		Region: s.region, MaxRetries: 1,
	})
	require.NoError(t, err)
	defer plainBackend.Close() //nolint:errcheck

	raw, err := plainBackend.GetObject(s.ctx, key, 0, 0)
	require.NoError(t, err)
	assert.NotEqual(t, data, raw, "object stored on S3 should be compressed, not plain text")

	// HeadObject must report the UNCOMPRESSED size. Reporting the compressed
	// ContentLength here is what made the kernel truncate every read of a
	// compressed file at the compressed length (issue #170). This assertion is
	// the one the original round-trip test was missing.
	info, err := zstdBackend.HeadObject(s.ctx, key)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), info.Size,
		"HeadObject must report the uncompressed size, not the compressed ContentLength")

	// Confirm the object really is stored compressed, so the assertion above is
	// meaningful rather than trivially true. This must use the raw S3 client:
	// HeadObject deliberately reports the uncompressed size to every caller,
	// since size is a property of the object rather than of the reader.
	rawHead, err := s.client.HeadObject(s.ctx, &s3.HeadObjectInput{
		Bucket: &s.bucket,
		Key:    strPtr(key),
	})
	require.NoError(t, err)
	assert.Less(t, *rawHead.ContentLength, int64(len(data)),
		"stored ContentLength should be smaller than the original (compression applied)")
	assert.Equal(t, "zstd", *rawHead.ContentEncoding,
		"Content-Encoding must be a real header, not user metadata")

	// Clean up.
	zstdBackend.DeleteObject(s.ctx, key) //nolint:errcheck

	t.Logf("✅ ZSTD compression test passed: %d bytes → compressed → decompressed correctly", len(data))
}

// TestCompressedObjectSizeReporting is the regression test for issue #170: a
// compressed object's reported size must be the uncompressed length on every
// read path the kernel consults, and must survive a multipart upload.
func (s *AWSS3TestSuite) TestCompressedObjectSizeReporting() {
	t := s.T()

	cfg := &s3backend.Config{
		Region:         s.region,
		MaxRetries:     3,
		ConnectTimeout: 10 * time.Second,
		RequestTimeout: 120 * time.Second,
		PoolSize:       4,
		Compression: s3backend.CompressionConfig{
			Enabled:   true,
			Algorithm: "zstd",
			Level:     3,
			MinSize:   "1KB",
		},
	}
	backend, err := s3backend.NewBackend(s.ctx, s.bucket, cfg)
	require.NoError(t, err)
	defer backend.Close() //nolint:errcheck

	line := []byte("ObjectFS issue-170 regression: uncompressed size must be reported.\n")

	// Sized to straddle the multipart boundary: the small case exercises the
	// single-part PutObject metadata, the large case exercises
	// initiateMultipartUpload, which builds its own metadata map.
	cases := []struct {
		name string
		size int
	}{
		{name: "single-part", size: 64 * 1024},
		{name: "multipart", size: 40 * 1024 * 1024}, // above the 32MB default threshold
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, 0, tc.size)
			for len(data) < tc.size {
				data = append(data, line...)
			}
			data = data[:tc.size]

			key := "objectfs-test/issue170-" + tc.name
			require.NoError(t, backend.PutObject(s.ctx, key, data, nil))
			defer backend.DeleteObject(s.ctx, key) //nolint:errcheck

			info, err := backend.HeadObject(s.ctx, key)
			require.NoError(t, err)
			assert.Equal(t, int64(len(data)), info.Size,
				"HeadObject must report the uncompressed size")

			// The checksum must be over the uncompressed content on BOTH the
			// single-part and multipart paths. putObjectMultipart previously
			// recomputed it over the compressed payload, so the two paths
			// disagreed about what objectfs-sha256 meant.
			wantSum := sha256.Sum256(data)
			assert.Equal(t, hex.EncodeToString(wantSum[:]), info.Checksum,
				"stored checksum must be the SHA-256 of the uncompressed content")

			// Full round-trip still returns every byte.
			got, err := backend.GetObject(s.ctx, key, 0, 0)
			require.NoError(t, err)
			assert.Equal(t, len(data), len(got))
			assert.Equal(t, data, got)

			// A read of exactly info.Size bytes — what the kernel issues once it
			// trusts the reported size — must return the whole object.
			full, err := backend.GetObject(s.ctx, key, 0, info.Size)
			require.NoError(t, err)
			assert.Equal(t, len(data), len(full),
				"a read of the reported size must return the whole object")
		})
	}

	t.Logf("✅ Issue #170 regression passed: uncompressed size reported on both upload paths")
}

// Helper function
func strPtr(s string) *string {
	return &s
}
