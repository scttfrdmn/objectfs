package s3

import (
	"errors"
	"testing"
	"time"
)

// Test multipart configuration
func TestConfig_MultipartConfiguration(t *testing.T) {
	cfg := NewDefaultConfig()

	// Test default values
	if cfg.MultipartThreshold != 32*1024*1024 {
		t.Errorf("Expected default multipart threshold of 32MB, got %d", cfg.MultipartThreshold)
	}

	if cfg.MultipartChunkSize != 16*1024*1024 {
		t.Errorf("Expected default chunk size of 16MB, got %d", cfg.MultipartChunkSize)
	}

	if cfg.MultipartConcurrency != 8 {
		t.Errorf("Expected default concurrency of 8, got %d", cfg.MultipartConcurrency)
	}
}

func TestConfig_ShouldUseMultipart(t *testing.T) {
	cfg := NewDefaultConfig()

	tests := []struct {
		name     string
		fileSize int64
		expected bool
	}{
		{
			name:     "small file below threshold",
			fileSize: 10 * 1024 * 1024, // 10MB
			expected: false,
		},
		{
			name:     "file exactly at threshold",
			fileSize: 32 * 1024 * 1024, // 32MB
			expected: false,
		},
		{
			name:     "file just above threshold",
			fileSize: 33 * 1024 * 1024, // 33MB
			expected: true,
		},
		{
			name:     "large file",
			fileSize: 500 * 1024 * 1024, // 500MB
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cfg.ShouldUseMultipart(tt.fileSize)
			if result != tt.expected {
				t.Errorf("ShouldUseMultipart(%d) = %v, want %v", tt.fileSize, result, tt.expected)
			}
		})
	}
}

func TestCalculateOptimalChunkSize(t *testing.T) {
	threshold := int64(32 * 1024 * 1024)     // 32MB
	baseChunkSize := int64(16 * 1024 * 1024) // 16MB

	tests := []struct {
		name          string
		fileSize      int64
		expectedChunk int64
	}{
		{
			name:          "file below threshold",
			fileSize:      20 * 1024 * 1024, // 20MB
			expectedChunk: 20 * 1024 * 1024, // Same as file size
		},
		{
			name:          "file just over threshold",
			fileSize:      40 * 1024 * 1024, // 40MB
			expectedChunk: 8 * 1024 * 1024,  // 8MB (half of base)
		},
		{
			name:          "medium file (500MB)",
			fileSize:      500 * 1024 * 1024,
			expectedChunk: 16 * 1024 * 1024, // 16MB (base)
		},
		{
			name:          "large file (5GB)",
			fileSize:      5 * 1024 * 1024 * 1024,
			expectedChunk: 32 * 1024 * 1024, // 32MB (2x base)
		},
		{
			name:          "very large file (50GB)",
			fileSize:      50 * 1024 * 1024 * 1024,
			expectedChunk: 64 * 1024 * 1024, // 64MB (4x base)
		},
		{
			name:          "massive file (500GB)",
			fileSize:      500 * 1024 * 1024 * 1024,
			expectedChunk: 128 * 1024 * 1024, // 128MB (8x base)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CalculateOptimalChunkSize(tt.fileSize, threshold, baseChunkSize)
			if result != tt.expectedChunk {
				t.Errorf("CalculateOptimalChunkSize(%d) = %d, want %d",
					tt.fileSize, result, tt.expectedChunk)
			}
		})
	}
}

func TestCalculatePartCount(t *testing.T) {
	tests := []struct {
		name          string
		fileSize      int64
		chunkSize     int64
		expectedParts int
	}{
		{
			name:          "exact division",
			fileSize:      64 * 1024 * 1024,
			chunkSize:     16 * 1024 * 1024,
			expectedParts: 4,
		},
		{
			name:          "with remainder",
			fileSize:      70 * 1024 * 1024,
			chunkSize:     16 * 1024 * 1024,
			expectedParts: 5, // 4 full parts + 1 partial
		},
		{
			name:          "single part",
			fileSize:      10 * 1024 * 1024,
			chunkSize:     16 * 1024 * 1024,
			expectedParts: 1,
		},
		{
			name:          "zero chunk size",
			fileSize:      100 * 1024 * 1024,
			chunkSize:     0,
			expectedParts: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CalculatePartCount(tt.fileSize, tt.chunkSize)
			if result != tt.expectedParts {
				t.Errorf("CalculatePartCount(%d, %d) = %d, want %d",
					tt.fileSize, tt.chunkSize, result, tt.expectedParts)
			}
		})
	}
}

func TestConfig_GetOptimalChunkSize(t *testing.T) {
	cfg := NewDefaultConfig()

	fileSize := int64(500 * 1024 * 1024) // 500MB
	chunkSize := cfg.GetOptimalChunkSize(fileSize)

	expectedChunkSize := int64(16 * 1024 * 1024) // 16MB for 500MB file
	if chunkSize != expectedChunkSize {
		t.Errorf("GetOptimalChunkSize(%d) = %d, want %d", fileSize, chunkSize, expectedChunkSize)
	}
}

// Test multipart metrics
func TestMetricsCollector_MultipartMetrics(t *testing.T) {
	mc := NewMetricsCollector()

	// Record multipart upload start
	mc.RecordMultipartUploadStart()
	metrics := mc.GetMetrics()
	if metrics.MultipartUploads != 1 {
		t.Errorf("Expected 1 multipart upload, got %d", metrics.MultipartUploads)
	}

	// Record multiple parts
	mc.RecordMultipartUploadPart(16 * 1024 * 1024)
	mc.RecordMultipartUploadPart(16 * 1024 * 1024)
	mc.RecordMultipartUploadPart(8 * 1024 * 1024)

	metrics = mc.GetMetrics()
	if metrics.MultipartUploadsParts != 3 {
		t.Errorf("Expected 3 parts, got %d", metrics.MultipartUploadsParts)
	}

	expectedBytes := int64((16 + 16 + 8) * 1024 * 1024)
	if metrics.MultipartBytes != expectedBytes {
		t.Errorf("Expected %d bytes, got %d", expectedBytes, metrics.MultipartBytes)
	}

	// Record completion
	mc.RecordMultipartUploadComplete(expectedBytes, 5*time.Second)
	metrics = mc.GetMetrics()
	if metrics.MultipartUploadsCompleted != 1 {
		t.Errorf("Expected 1 completed upload, got %d", metrics.MultipartUploadsCompleted)
	}

	// Record failure
	mc.RecordMultipartUploadStart()
	mc.RecordMultipartUploadFailed()
	metrics = mc.GetMetrics()
	if metrics.MultipartUploadsFailed != 1 {
		t.Errorf("Expected 1 failed upload, got %d", metrics.MultipartUploadsFailed)
	}
}

func TestMetricsCollector_MultipartRates(t *testing.T) {
	mc := NewMetricsCollector()

	// Set up some requests for rate calculation
	mc.metrics.Requests = 10

	// Record 3 multipart uploads (30% of total requests)
	mc.RecordMultipartUploadStart()
	mc.RecordMultipartUploadStart()
	mc.RecordMultipartUploadStart()

	usageRate := mc.GetMultipartUsageRate()
	expectedUsageRate := 30.0
	if usageRate != expectedUsageRate {
		t.Errorf("Expected usage rate %.2f%%, got %.2f%%", expectedUsageRate, usageRate)
	}

	// Complete 2, fail 1 (66.67% success rate)
	mc.RecordMultipartUploadComplete(100, 1*time.Second)
	mc.RecordMultipartUploadComplete(100, 1*time.Second)
	mc.RecordMultipartUploadFailed()

	successRate := mc.GetMultipartSuccessRate()
	expectedSuccessRate := 66.66666666666666
	if successRate != expectedSuccessRate {
		t.Errorf("Expected success rate %.2f%%, got %.2f%%", expectedSuccessRate, successRate)
	}
}

func TestMetricsCollector_AveragePartsPerUpload(t *testing.T) {
	mc := NewMetricsCollector()

	// Upload 1: 4 parts
	mc.RecordMultipartUploadStart()
	for i := 0; i < 4; i++ {
		mc.RecordMultipartUploadPart(16 * 1024 * 1024)
	}

	// Upload 2: 2 parts
	mc.RecordMultipartUploadStart()
	for i := 0; i < 2; i++ {
		mc.RecordMultipartUploadPart(16 * 1024 * 1024)
	}

	avgParts := mc.GetAveragePartsPerUpload()
	expectedAvgParts := 3.0 // (4 + 2) / 2 = 3
	if avgParts != expectedAvgParts {
		t.Errorf("Expected average parts %.2f, got %.2f", expectedAvgParts, avgParts)
	}
}

// Test multipart upload state
func TestMultipartUploadState(t *testing.T) {
	uploadID := "test-upload-123"
	bucket := "test-bucket"
	key := "test-key"
	totalSize := int64(100 * 1024 * 1024) // 100MB
	chunkSize := int64(16 * 1024 * 1024)  // 16MB

	state := NewMultipartUploadState(uploadID, bucket, key, totalSize, chunkSize)

	// Verify initial state
	if state.UploadID != uploadID {
		t.Errorf("Expected upload ID %s, got %s", uploadID, state.UploadID)
	}

	if state.TotalParts != 7 { // 100MB / 16MB = 6.25, rounded up to 7
		t.Errorf("Expected 7 parts, got %d", state.TotalParts)
	}

	if state.Status != UploadStatusInitiated {
		t.Errorf("Expected status %s, got %s", UploadStatusInitiated, state.Status)
	}

	// Mark parts as completed
	state.MarkPartCompleted(1, 16*1024*1024, "etag-1")
	state.MarkPartCompleted(2, 16*1024*1024, "etag-2")

	if state.CompletedParts != 2 {
		t.Errorf("Expected 2 completed parts, got %d", state.CompletedParts)
	}

	if state.Status != UploadStatusInProgress {
		t.Errorf("Expected status %s, got %s", UploadStatusInProgress, state.Status)
	}

	// Test progress calculation
	progress := state.GetProgress()
	expectedProgress := (2.0 / 7.0) * 100
	// Use tolerance for floating point comparison
	tolerance := 0.01
	if progress < expectedProgress-tolerance || progress > expectedProgress+tolerance {
		t.Errorf("Expected progress %.2f%%, got %.2f%%", expectedProgress, progress)
	}

	// Mark part as failed
	state.MarkPartFailed(3, errors.New("upload failed"))
	part := state.Parts[3]
	if part.Completed {
		t.Error("Expected part 3 to not be completed")
	}
	if part.RetryCount != 1 {
		t.Errorf("Expected retry count 1, got %d", part.RetryCount)
	}

	// Test remaining parts
	remaining := state.GetRemainingParts()
	expectedRemaining := 5 // Parts 3, 4, 5, 6, 7
	if len(remaining) != expectedRemaining {
		t.Errorf("Expected %d remaining parts, got %d", expectedRemaining, len(remaining))
	}

	// Test completed parts
	completed := state.GetCompletedParts()
	if len(completed) != 2 {
		t.Errorf("Expected 2 completed parts, got %d", len(completed))
	}

	// Test IsComplete
	if state.IsComplete() {
		t.Error("Expected upload to not be complete")
	}

	// Complete all remaining parts
	for i := 3; i <= 7; i++ {
		state.MarkPartCompleted(i, 16*1024*1024, "etag")
	}

	if !state.IsComplete() {
		t.Error("Expected upload to be complete")
	}
}

func TestMultipartStateManager(t *testing.T) {
	manager := NewMultipartStateManager()

	// Create and track multiple uploads
	state1 := NewMultipartUploadState("upload-1", "bucket", "key1", 100*1024*1024, 16*1024*1024)
	state2 := NewMultipartUploadState("upload-2", "bucket", "key2", 200*1024*1024, 16*1024*1024)

	manager.TrackUpload(state1)
	manager.TrackUpload(state2)

	// Test GetUploadState
	retrieved, exists := manager.GetUploadState("upload-1")
	if !exists {
		t.Error("Expected upload-1 to exist")
	}
	if retrieved.UploadID != "upload-1" {
		t.Errorf("Expected upload ID upload-1, got %s", retrieved.UploadID)
	}

	// Test UpdatePartStatus
	manager.UpdatePartStatus("upload-1", 1, 16*1024*1024, "etag-1", nil)
	state, _ := manager.GetUploadState("upload-1")
	if state.CompletedParts != 1 {
		t.Errorf("Expected 1 completed part, got %d", state.CompletedParts)
	}

	// Test failed update
	testErr := errors.New("upload failed")
	manager.UpdatePartStatus("upload-1", 2, 0, "", testErr)
	state, _ = manager.GetUploadState("upload-1")
	if state.Parts[2].Error != testErr.Error() {
		t.Errorf("Expected error %s, got %s", testErr.Error(), state.Parts[2].Error)
	}

	// Test MarkUploadCompleted
	manager.MarkUploadCompleted("upload-1")
	state, _ = manager.GetUploadState("upload-1")
	if state.Status != UploadStatusCompleted {
		t.Errorf("Expected status %s, got %s", UploadStatusCompleted, state.Status)
	}

	// Test MarkUploadFailed
	manager.MarkUploadFailed("upload-2")
	state, _ = manager.GetUploadState("upload-2")
	if state.Status != UploadStatusFailed {
		t.Errorf("Expected status %s, got %s", UploadStatusFailed, state.Status)
	}

	// Test GetAllUploads
	allUploads := manager.GetAllUploads()
	if len(allUploads) != 2 {
		t.Errorf("Expected 2 uploads, got %d", len(allUploads))
	}

	// Test GetInProgressUploads (should be 0, both completed or failed)
	inProgress := manager.GetInProgressUploads()
	if len(inProgress) != 0 {
		t.Errorf("Expected 0 in-progress uploads, got %d", len(inProgress))
	}

	// Test GetUploadCount
	count := manager.GetUploadCount()
	if count != 2 {
		t.Errorf("Expected 2 uploads, got %d", count)
	}

	// Test RemoveUpload
	manager.RemoveUpload("upload-1")
	count = manager.GetUploadCount()
	if count != 1 {
		t.Errorf("Expected 1 upload after removal, got %d", count)
	}

	// Test CleanupOldUploads
	state2.LastUpdatedAt = time.Now().Add(-2 * time.Hour)
	removed := manager.CleanupOldUploads(1 * time.Hour)
	if removed != 1 {
		t.Errorf("Expected 1 upload to be removed, got %d", removed)
	}
}

func TestMultipartUploadStatus(t *testing.T) {
	tests := []struct {
		status      MultipartUploadStatus
		isCompleted bool
	}{
		{UploadStatusInitiated, false},
		{UploadStatusInProgress, false},
		{UploadStatusCompleted, true},
		{UploadStatusFailed, true},
		{UploadStatusAborted, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			result := tt.status.IsCompleted()
			if result != tt.isCompleted {
				t.Errorf("Expected IsCompleted()=%v for status %s, got %v",
					tt.isCompleted, tt.status, result)
			}
		})
	}
}
