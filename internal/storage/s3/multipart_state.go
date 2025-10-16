package s3

import (
	"sync"
	"time"
)

// UploadPart represents a single part of a multipart upload
type UploadPart struct {
	PartNumber   int       `json:"part_number"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	Completed    bool      `json:"completed"`
	LastModified time.Time `json:"last_modified"`
	Offset       int64     `json:"offset"`          // Byte offset in the file
	RetryCount   int       `json:"retry_count"`     // Number of retry attempts
	Error        string    `json:"error,omitempty"` // Last error if any
}

// MultipartUploadState tracks the state of an in-progress multipart upload
type MultipartUploadState struct {
	UploadID       string                `json:"upload_id"`
	Bucket         string                `json:"bucket"`
	Key            string                `json:"key"`
	TotalSize      int64                 `json:"total_size"`
	ChunkSize      int64                 `json:"chunk_size"`
	Parts          map[int]*UploadPart   `json:"parts"` // Key is part number
	StartedAt      time.Time             `json:"started_at"`
	LastUpdatedAt  time.Time             `json:"last_updated_at"`
	CompletedParts int                   `json:"completed_parts"`
	TotalParts     int                   `json:"total_parts"`
	BytesUploaded  int64                 `json:"bytes_uploaded"`
	Status         MultipartUploadStatus `json:"status"`
	Metadata       map[string]string     `json:"metadata,omitempty"`
}

// MultipartUploadStatus represents the status of a multipart upload
type MultipartUploadStatus string

const (
	UploadStatusInitiated  MultipartUploadStatus = "initiated"
	UploadStatusInProgress MultipartUploadStatus = "in_progress"
	UploadStatusCompleted  MultipartUploadStatus = "completed"
	UploadStatusFailed     MultipartUploadStatus = "failed"
	UploadStatusAborted    MultipartUploadStatus = "aborted"
)

// IsCompleted returns true if the upload is in a terminal state
func (s MultipartUploadStatus) IsCompleted() bool {
	return s == UploadStatusCompleted || s == UploadStatusFailed || s == UploadStatusAborted
}

// NewMultipartUploadState creates a new multipart upload state tracker
func NewMultipartUploadState(uploadID, bucket, key string, totalSize, chunkSize int64) *MultipartUploadState {
	totalParts := CalculatePartCount(totalSize, chunkSize)

	return &MultipartUploadState{
		UploadID:      uploadID,
		Bucket:        bucket,
		Key:           key,
		TotalSize:     totalSize,
		ChunkSize:     chunkSize,
		Parts:         make(map[int]*UploadPart),
		StartedAt:     time.Now(),
		LastUpdatedAt: time.Now(),
		TotalParts:    totalParts,
		Status:        UploadStatusInitiated,
		Metadata:      make(map[string]string),
	}
}

// MarkPartCompleted marks a part as successfully uploaded
func (s *MultipartUploadState) MarkPartCompleted(partNumber int, size int64, etag string) {
	if s.Parts[partNumber] == nil {
		s.Parts[partNumber] = &UploadPart{
			PartNumber: partNumber,
		}
	}

	part := s.Parts[partNumber]
	part.Size = size
	part.ETag = etag
	part.Completed = true
	part.LastModified = time.Now()
	part.Error = ""

	s.CompletedParts++
	s.BytesUploaded += size
	s.LastUpdatedAt = time.Now()
	s.Status = UploadStatusInProgress
}

// MarkPartFailed marks a part as failed
func (s *MultipartUploadState) MarkPartFailed(partNumber int, err error) {
	if s.Parts[partNumber] == nil {
		s.Parts[partNumber] = &UploadPart{
			PartNumber: partNumber,
		}
	}

	part := s.Parts[partNumber]
	part.Completed = false
	part.RetryCount++
	part.LastModified = time.Now()
	part.Error = err.Error()

	s.LastUpdatedAt = time.Now()
}

// IsComplete returns true if all parts have been uploaded
func (s *MultipartUploadState) IsComplete() bool {
	return s.CompletedParts == s.TotalParts
}

// GetProgress returns the upload progress as a percentage (0-100)
func (s *MultipartUploadState) GetProgress() float64 {
	if s.TotalParts == 0 {
		return 0
	}
	return (float64(s.CompletedParts) / float64(s.TotalParts)) * 100
}

// GetRemainingParts returns a list of part numbers that still need to be uploaded
func (s *MultipartUploadState) GetRemainingParts() []int {
	remaining := make([]int, 0)
	for i := 1; i <= s.TotalParts; i++ {
		part, exists := s.Parts[i]
		if !exists || !part.Completed {
			remaining = append(remaining, i)
		}
	}
	return remaining
}

// GetCompletedParts returns a list of successfully uploaded parts
func (s *MultipartUploadState) GetCompletedParts() []*UploadPart {
	completed := make([]*UploadPart, 0, s.CompletedParts)
	for i := 1; i <= s.TotalParts; i++ {
		if part, exists := s.Parts[i]; exists && part.Completed {
			completed = append(completed, part)
		}
	}
	return completed
}

// MultipartStateManager manages the state of multiple concurrent multipart uploads
type MultipartStateManager struct {
	mu      sync.RWMutex
	uploads map[string]*MultipartUploadState // Key is upload ID
}

// NewMultipartStateManager creates a new multipart state manager
func NewMultipartStateManager() *MultipartStateManager {
	return &MultipartStateManager{
		uploads: make(map[string]*MultipartUploadState),
	}
}

// TrackUpload starts tracking a new multipart upload
func (m *MultipartStateManager) TrackUpload(state *MultipartUploadState) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.uploads[state.UploadID] = state
}

// GetUploadState retrieves the state of a tracked upload
func (m *MultipartStateManager) GetUploadState(uploadID string) (*MultipartUploadState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	state, exists := m.uploads[uploadID]
	return state, exists
}

// UpdatePartStatus updates the status of a specific part
func (m *MultipartStateManager) UpdatePartStatus(uploadID string, partNumber int, size int64, etag string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, exists := m.uploads[uploadID]
	if !exists {
		return
	}

	if err != nil {
		state.MarkPartFailed(partNumber, err)
	} else {
		state.MarkPartCompleted(partNumber, size, etag)
	}
}

// MarkUploadCompleted marks an upload as completed
func (m *MultipartStateManager) MarkUploadCompleted(uploadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if state, exists := m.uploads[uploadID]; exists {
		state.Status = UploadStatusCompleted
		state.LastUpdatedAt = time.Now()
	}
}

// MarkUploadFailed marks an upload as failed
func (m *MultipartStateManager) MarkUploadFailed(uploadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if state, exists := m.uploads[uploadID]; exists {
		state.Status = UploadStatusFailed
		state.LastUpdatedAt = time.Now()
	}
}

// RemoveUpload removes a tracked upload from the manager
func (m *MultipartStateManager) RemoveUpload(uploadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.uploads, uploadID)
}

// GetAllUploads returns all tracked uploads
func (m *MultipartStateManager) GetAllUploads() []*MultipartUploadState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	uploads := make([]*MultipartUploadState, 0, len(m.uploads))
	for _, state := range m.uploads {
		uploads = append(uploads, state)
	}
	return uploads
}

// GetInProgressUploads returns all uploads that are currently in progress
func (m *MultipartStateManager) GetInProgressUploads() []*MultipartUploadState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	uploads := make([]*MultipartUploadState, 0)
	for _, state := range m.uploads {
		if state.Status == UploadStatusInProgress || state.Status == UploadStatusInitiated {
			uploads = append(uploads, state)
		}
	}
	return uploads
}

// CleanupOldUploads removes uploads that have been in a terminal state for longer than the specified duration
func (m *MultipartStateManager) CleanupOldUploads(maxAge time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	removed := 0
	cutoff := time.Now().Add(-maxAge)

	for uploadID, state := range m.uploads {
		if state.Status.IsCompleted() && state.LastUpdatedAt.Before(cutoff) {
			delete(m.uploads, uploadID)
			removed++
		}
	}

	return removed
}

// GetUploadCount returns the total number of tracked uploads
func (m *MultipartStateManager) GetUploadCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.uploads)
}
