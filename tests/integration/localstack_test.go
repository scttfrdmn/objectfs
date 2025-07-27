//go:build integration
// +build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	s3backend "github.com/objectfs/objectfs/internal/storage/s3"
	"github.com/objectfs/objectfs/pkg/optimization"
)

// LocalStackIntegrationSuite tests ObjectFS against LocalStack S3
type LocalStackIntegrationSuite struct {
	suite.Suite
	ctx      context.Context
	client   *s3.Client
	backend  *s3backend.Backend
	bucket   string
	endpoint string
}

func TestLocalStackIntegration(t *testing.T) {
	// Skip if not running in CI with LocalStack
	if os.Getenv("AWS_ENDPOINT_URL") == "" {
		t.Skip("Skipping LocalStack integration tests - no endpoint configured")
	}
	
	suite.Run(t, new(LocalStackIntegrationSuite))
}

func (s *LocalStackIntegrationSuite) SetupSuite() {
	s.ctx = context.Background()
	s.bucket = "test-bucket"
	s.endpoint = os.Getenv("AWS_ENDPOINT_URL")
	
	// Configure AWS client for LocalStack
	cfg, err := config.LoadDefaultConfig(s.ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"test", "test", "",
		)),
		config.WithRegion("us-east-1"),
		// Note: Endpoint resolver configuration removed for LocalStack compatibility
	)
	require.NoError(s.T(), err)
	
	s.client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = &s.endpoint
		o.UsePathStyle = true
	})
	
	// Create S3 backend
	backendConfig := &s3backend.Config{
		Region:         "us-east-1",
		Endpoint:       s.endpoint,
		ForcePathStyle: true,
		MaxRetries:     3,
		ConnectTimeout: 10 * time.Second,
		RequestTimeout: 30 * time.Second,
		PoolSize:       4,
	}
	
	s.backend, err = s3backend.NewBackend(s.ctx, s.bucket, backendConfig)
	require.NoError(s.T(), err)
}

func (s *LocalStackIntegrationSuite) TearDownSuite() {
	if s.backend != nil {
		s.backend.Close()
	}
}

func (s *LocalStackIntegrationSuite) SetupTest() {
	// Ensure bucket exists and is empty
	_, err := s.client.CreateBucket(s.ctx, &s3.CreateBucketInput{
		Bucket: &s.bucket,
	})
	// Ignore error if bucket already exists
	
	// Clean up any existing objects
	resp, err := s.client.ListObjectsV2(s.ctx, &s3.ListObjectsV2Input{
		Bucket: &s.bucket,
	})
	require.NoError(s.T(), err)
	
	for _, obj := range resp.Contents {
		_, err := s.client.DeleteObject(s.ctx, &s3.DeleteObjectInput{
			Bucket: &s.bucket,
			Key:    obj.Key,
		})
		require.NoError(s.T(), err)
	}
}

func (s *LocalStackIntegrationSuite) TestBasicS3Operations() {
	t := s.T()
	
	// Test data
	key := "test-object"
	data := []byte("Hello, ObjectFS!")
	
	// Test PutObject
	err := s.backend.PutObject(s.ctx, key, data)
	assert.NoError(t, err)
	
	// Test GetObject
	retrieved, err := s.backend.GetObject(s.ctx, key, 0, 0)
	assert.NoError(t, err)
	assert.Equal(t, data, retrieved)
	
	// Test HeadObject
	info, err := s.backend.HeadObject(s.ctx, key)
	assert.NoError(t, err)
	assert.Equal(t, key, info.Key)
	assert.Equal(t, int64(len(data)), info.Size)
	
	// Test ListObjects
	objects, err := s.backend.ListObjects(s.ctx, "", 10)
	assert.NoError(t, err)
	assert.Len(t, objects, 1)
	assert.Equal(t, key, objects[0].Key)
	
	// Test DeleteObject
	err = s.backend.DeleteObject(s.ctx, key)
	assert.NoError(t, err)
	
	// Verify deletion
	_, err = s.backend.GetObject(s.ctx, key, 0, 0)
	assert.Error(t, err)
}

func (s *LocalStackIntegrationSuite) TestRangeRequests() {
	t := s.T()
	
	key := "test-range"
	data := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	
	// Put test data
	err := s.backend.PutObject(s.ctx, key, data)
	require.NoError(t, err)
	
	// Test range request - first 10 bytes
	partial, err := s.backend.GetObject(s.ctx, key, 0, 10)
	assert.NoError(t, err)
	assert.Equal(t, data[:10], partial)
	
	// Test range request - middle 10 bytes
	partial, err = s.backend.GetObject(s.ctx, key, 10, 10)
	assert.NoError(t, err)
	assert.Equal(t, data[10:20], partial)
	
	// Test range request - last 10 bytes
	partial, err = s.backend.GetObject(s.ctx, key, int64(len(data)-10), 10)
	assert.NoError(t, err)
	assert.Equal(t, data[len(data)-10:], partial)
}

func (s *LocalStackIntegrationSuite) TestBatchOperations() {
	t := s.T()
	
	// Test batch put
	objects := map[string][]byte{
		"batch1": []byte("data1"),
		"batch2": []byte("data2"),
		"batch3": []byte("data3"),
	}
	
	err := s.backend.PutObjects(s.ctx, objects)
	assert.NoError(t, err)
	
	// Test batch get
	keys := []string{"batch1", "batch2", "batch3"}
	results, err := s.backend.GetObjects(s.ctx, keys)
	assert.NoError(t, err)
	assert.Len(t, results, 3)
	
	for key, expectedData := range objects {
		actualData, exists := results[key]
		assert.True(t, exists, "Key %s should exist in results", key)
		assert.Equal(t, expectedData, actualData)
	}
}

func (s *LocalStackIntegrationSuite) TestConnectionPooling() {
	t := s.T()
	
	// Test concurrent operations to verify connection pooling
	const numOperations = 20
	done := make(chan error, numOperations)
	
	for i := 0; i < numOperations; i++ {
		go func(id int) {
			key := fmt.Sprintf("concurrent-%d", id)
			data := []byte(fmt.Sprintf("data-%d", id))
			
			// Put object
			err := s.backend.PutObject(s.ctx, key, data)
			if err != nil {
				done <- err
				return
			}
			
			// Get object
			retrieved, err := s.backend.GetObject(s.ctx, key, 0, 0)
			if err != nil {
				done <- err
				return
			}
			
			if !bytes.Equal(data, retrieved) {
				done <- fmt.Errorf("data mismatch for key %s", key)
				return
			}
			
			done <- nil
		}(i)
	}
	
	// Wait for all operations to complete
	for i := 0; i < numOperations; i++ {
		err := <-done
		assert.NoError(t, err)
	}
}

func (s *LocalStackIntegrationSuite) TestHealthCheck() {
	t := s.T()
	
	err := s.backend.HealthCheck(s.ctx)
	assert.NoError(t, err)
}

func (s *LocalStackIntegrationSuite) TestMetricsCollection() {
	t := s.T()
	
	// Perform some operations to generate metrics
	key := "metrics-test"
	data := []byte("test data for metrics")
	
	err := s.backend.PutObject(s.ctx, key, data)
	require.NoError(t, err)
	
	_, err = s.backend.GetObject(s.ctx, key, 0, 0)
	require.NoError(t, err)
	
	// Check metrics
	metrics := s.backend.GetMetrics()
	assert.Greater(t, metrics.Requests, int64(0))
	assert.Greater(t, metrics.BytesUploaded, int64(0))
	assert.Greater(t, metrics.BytesDownloaded, int64(0))
}

func (s *LocalStackIntegrationSuite) TestErrorHandling() {
	t := s.T()
	
	// Test getting non-existent object
	_, err := s.backend.GetObject(s.ctx, "non-existent", 0, 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	
	// Test head of non-existent object
	_, err = s.backend.HeadObject(s.ctx, "non-existent")
	assert.Error(t, err)
	
	// Test deleting non-existent object (should not error)
	err = s.backend.DeleteObject(s.ctx, "non-existent")
	assert.NoError(t, err) // S3 returns success for deleting non-existent objects
}

// OptimizationIntegrationSuite tests the optimization interfaces
type OptimizationIntegrationSuite struct {
	suite.Suite
	ctx context.Context
}

func TestOptimizationInterfaces(t *testing.T) {
	suite.Run(t, new(OptimizationIntegrationSuite))
}

func (s *OptimizationIntegrationSuite) SetupSuite() {
	s.ctx = context.Background()
}

func (s *OptimizationIntegrationSuite) TestOptimizationInterfaceCompleteness() {
	t := s.T()
	
	// Test that all required interfaces are defined
	var (
		_ optimization.S3Optimizer        = (*MockS3Optimizer)(nil)
		_ optimization.NetworkOptimizer   = (*MockNetworkOptimizer)(nil)
		_ optimization.AdaptiveEngine     = (*MockAdaptiveEngine)(nil)
		_ optimization.ConnectionManager  = (*MockConnectionManager)(nil)
		_ optimization.PerformanceMonitor = (*MockPerformanceMonitor)(nil)
	)
	
	t.Log("All optimization interfaces are properly defined")
}

func (s *OptimizationIntegrationSuite) TestNetworkConditionsStructure() {
	t := s.T()
	
	conditions := &optimization.NetworkConditions{
		Bandwidth:      100.0,
		Latency:        50 * time.Millisecond,
		PacketLoss:     0.01,
		Jitter:         5 * time.Millisecond,
		ConnectionType: "ethernet",
		QualityScore:   0.95,
		Timestamp:      time.Now(),
	}
	
	assert.Equal(t, 100.0, conditions.Bandwidth)
	assert.Equal(t, 50*time.Millisecond, conditions.Latency)
	assert.Equal(t, 0.01, conditions.PacketLoss)
	assert.Equal(t, "ethernet", conditions.ConnectionType)
}

func (s *OptimizationIntegrationSuite) TestPerformanceMetricsStructure() {
	t := s.T()
	
	metrics := &optimization.PerformanceMetrics{
		TotalOperations:   1000,
		SuccessfulOps:     990,
		FailedOps:         10,
		AverageLatency:    25 * time.Millisecond,
		P95Latency:        50 * time.Millisecond,
		P99Latency:        100 * time.Millisecond,
		TotalBytesRead:    1024 * 1024,
		TotalBytesWritten: 512 * 1024,
		ReadThroughput:    800.0,
		WriteThroughput:   400.0,
		CacheHits:         800,
		CacheMisses:       200,
		CacheHitRatio:     0.8,
		Timestamp:         time.Now(),
	}
	
	assert.Equal(t, int64(1000), metrics.TotalOperations)
	assert.Equal(t, float64(800.0), metrics.ReadThroughput)
	assert.Equal(t, float64(0.8), metrics.CacheHitRatio)
}

func (s *OptimizationIntegrationSuite) TestObjectFSConfigStructure() {
	t := s.T()
	
	config := &optimization.ObjectFSConfig{
		TargetReadThroughput:  800.0,
		TargetWriteThroughput: 400.0,
		TargetLatency:        10 * time.Millisecond,
		CacheSize:            2 * 1024 * 1024 * 1024, // 2GB
		L1CacheSize:          512 * 1024 * 1024,      // 512MB
		L2CacheSize:          1536 * 1024 * 1024,     // 1.5GB
		MaxConcurrentReads:   100,
		MaxConcurrentWrites:  50,
		ReadAheadSize:        64 * 1024 * 1024, // 64MB
		WriteBufferSize:      32 * 1024 * 1024, // 32MB
		DirectIO:             false,
		CargoShipOptimization: true,
		SharedMetrics:        true,
	}
	
	assert.Equal(t, float64(800.0), config.TargetReadThroughput)
	assert.Equal(t, int64(2*1024*1024*1024), config.CacheSize)
	assert.True(t, config.CargoShipOptimization)
}