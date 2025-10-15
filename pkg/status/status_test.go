package status

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/objectfs/objectfs/pkg/errors"
	"github.com/objectfs/objectfs/pkg/health"
)

func TestOperationStatus_String(t *testing.T) {
	tests := []struct {
		status   OperationStatus
		expected string
	}{
		{StatusPending, "pending"},
		{StatusInProgress, "in_progress"},
		{StatusCompleted, "completed"},
		{StatusFailed, "failed"},
		{StatusCanceled, "canceled"},
		{OperationStatus(999), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := tt.status.String()
			if result != tt.expected {
				t.Errorf("String() = %s, want %s", result, tt.expected)
			}
		})
	}
}

func TestTracker_StartOperation(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	metadata := map[string]interface{}{
		"bucket": "test-bucket",
		"key":    "test-key",
	}

	op, opCtx := tracker.StartOperation(ctx, "get-object", metadata)

	if op == nil {
		t.Fatal("StartOperation returned nil operation")
	}

	if op.ID == "" {
		t.Error("Operation ID is empty")
	}

	if op.Type != "get-object" {
		t.Errorf("Expected type='get-object', got '%s'", op.Type)
	}

	if op.Status != StatusInProgress {
		t.Errorf("Expected status=StatusInProgress, got %s", op.Status)
	}

	if opCtx == nil {
		t.Error("Operation context is nil")
	}

	if op.Metadata["bucket"] != "test-bucket" {
		t.Errorf("Expected bucket='test-bucket', got '%v'", op.Metadata["bucket"])
	}
}

func TestTracker_UpdateProgress(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	op, _ := tracker.StartOperation(ctx, "upload", nil)

	err := tracker.UpdateProgress(op.ID, 50, 100, "bytes")
	if err != nil {
		t.Fatalf("UpdateProgress failed: %v", err)
	}

	retrievedOp, err := tracker.GetOperation(op.ID)
	if err != nil {
		t.Fatalf("GetOperation failed: %v", err)
	}

	if retrievedOp.Progress == nil {
		t.Fatal("Progress is nil")
	}

	if retrievedOp.Progress.Current != 50 {
		t.Errorf("Expected current=50, got %d", retrievedOp.Progress.Current)
	}

	if retrievedOp.Progress.Total != 100 {
		t.Errorf("Expected total=100, got %d", retrievedOp.Progress.Total)
	}

	if retrievedOp.Progress.Unit != "bytes" {
		t.Errorf("Expected unit='bytes', got '%s'", retrievedOp.Progress.Unit)
	}

	if retrievedOp.Progress.Percentage != 50.0 {
		t.Errorf("Expected percentage=50.0, got %f", retrievedOp.Progress.Percentage)
	}
}

func TestTracker_UpdateProgress_NotFound(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())

	err := tracker.UpdateProgress("non-existent", 50, 100, "bytes")
	if err == nil {
		t.Error("Expected error for non-existent operation")
	}
}

func TestTracker_SetPhase(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	op, _ := tracker.StartOperation(ctx, "mount", nil)

	err := tracker.SetPhase(op.ID, "connecting")
	if err != nil {
		t.Fatalf("SetPhase failed: %v", err)
	}

	retrievedOp, _ := tracker.GetOperation(op.ID)
	if retrievedOp.Progress == nil {
		t.Fatal("Progress is nil")
	}

	if retrievedOp.Progress.Phase != "connecting" {
		t.Errorf("Expected phase='connecting', got '%s'", retrievedOp.Progress.Phase)
	}
}

func TestTracker_SetMessage(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	op, _ := tracker.StartOperation(ctx, "sync", nil)

	err := tracker.SetMessage(op.ID, "Syncing files...")
	if err != nil {
		t.Fatalf("SetMessage failed: %v", err)
	}

	retrievedOp, _ := tracker.GetOperation(op.ID)
	if retrievedOp.Progress == nil {
		t.Fatal("Progress is nil")
	}

	if retrievedOp.Progress.Message != "Syncing files..." {
		t.Errorf("Expected message='Syncing files...', got '%s'", retrievedOp.Progress.Message)
	}
}

func TestTracker_CompleteOperation(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	op, _ := tracker.StartOperation(ctx, "download", nil)

	err := tracker.CompleteOperation(op.ID)
	if err != nil {
		t.Fatalf("CompleteOperation failed: %v", err)
	}

	// Operation should be moved to history
	_, err = tracker.GetOperation(op.ID)
	if err == nil {
		t.Error("Expected error when getting completed operation")
	}

	// Check history
	history := tracker.GetHistory(10)
	if len(history) != 1 {
		t.Errorf("Expected 1 operation in history, got %d", len(history))
	}

	if history[0].Status != StatusCompleted {
		t.Errorf("Expected status=StatusCompleted, got %s", history[0].Status)
	}

	if history[0].EndTime == nil {
		t.Error("EndTime is nil for completed operation")
	}
}

func TestTracker_FailOperation(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	op, _ := tracker.StartOperation(ctx, "upload", nil)

	testErr := errors.NewError(errors.ErrCodeStorageWrite, "write failed")
	err := tracker.FailOperation(op.ID, testErr)
	if err != nil {
		t.Fatalf("FailOperation failed: %v", err)
	}

	// Check history
	history := tracker.GetHistory(10)
	if len(history) != 1 {
		t.Errorf("Expected 1 operation in history, got %d", len(history))
	}

	if history[0].Status != StatusFailed {
		t.Errorf("Expected status=StatusFailed, got %s", history[0].Status)
	}

	if history[0].Error == nil {
		t.Error("Error is nil for failed operation")
	}

	if history[0].Error.Code != errors.ErrCodeStorageWrite {
		t.Errorf("Expected error code=ErrCodeStorageWrite, got %s", history[0].Error.Code)
	}
}

func TestTracker_CancelOperation(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	op, opCtx := tracker.StartOperation(ctx, "copy", nil)

	err := tracker.CancelOperation(op.ID)
	if err != nil {
		t.Fatalf("CancelOperation failed: %v", err)
	}

	// Check that context was canceled
	select {
	case <-opCtx.Done():
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("Operation context was not canceled")
	}

	// Check history
	history := tracker.GetHistory(10)
	if len(history) != 1 {
		t.Errorf("Expected 1 operation in history, got %d", len(history))
	}

	if history[0].Status != StatusCanceled {
		t.Errorf("Expected status=StatusCanceled, got %s", history[0].Status)
	}
}

func TestTracker_GetAllOperations(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	op1, _ := tracker.StartOperation(ctx, "read", nil)
	op2, _ := tracker.StartOperation(ctx, "write", nil)
	op3, _ := tracker.StartOperation(ctx, "delete", nil)

	// Small sleep to ensure all operations are registered
	time.Sleep(10 * time.Millisecond)

	allOps := tracker.GetAllOperations()

	if len(allOps) != 3 {
		t.Errorf("Expected 3 operations, got %d", len(allOps))
		for i, op := range allOps {
			t.Logf("Operation %d: ID=%s Type=%s", i, op.ID, op.Type)
		}
	}

	// Check that all operations are present
	found := make(map[string]bool)
	for _, op := range allOps {
		found[op.ID] = true
	}

	if !found[op1.ID] || !found[op2.ID] || !found[op3.ID] {
		t.Errorf("Not all operations were returned. Found: op1=%v op2=%v op3=%v", found[op1.ID], found[op2.ID], found[op3.ID])
	}
}

func TestTracker_GetHistory(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	// Start and complete multiple operations
	for i := 0; i < 5; i++ {
		op, _ := tracker.StartOperation(ctx, fmt.Sprintf("op-%d", i), nil)
		if err := tracker.CompleteOperation(op.ID); err != nil {
			t.Fatalf("CompleteOperation failed: %v", err)
		}
	}

	// Get limited history
	history := tracker.GetHistory(3)
	if len(history) != 3 {
		t.Errorf("Expected 3 operations in history, got %d", len(history))
	}

	// Get all history
	allHistory := tracker.GetHistory(0)
	if len(allHistory) != 5 {
		t.Errorf("Expected 5 operations in full history, got %d", len(allHistory))
	}
}

func TestTracker_Subscribe(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	op, _ := tracker.StartOperation(ctx, "test", nil)

	// Subscribe to updates
	updates, err := tracker.Subscribe(op.ID)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	// Update progress and check for notification
	go func() {
		if err := tracker.UpdateProgress(op.ID, 50, 100, "bytes"); err != nil {
			t.Errorf("UpdateProgress failed: %v", err)
		}
	}()

	select {
	case update := <-updates:
		if update.Operation.ID != op.ID {
			t.Errorf("Expected operation ID=%s, got %s", op.ID, update.Operation.ID)
		}
		if update.Message != "Progress updated" {
			t.Errorf("Expected message='Progress updated', got '%s'", update.Message)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Did not receive update notification")
	}
}

func TestTracker_Subscribe_NotFound(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())

	_, err := tracker.Subscribe("non-existent")
	if err == nil {
		t.Error("Expected error for non-existent operation")
	}
}

func TestTracker_GetSystemStatus(t *testing.T) {
	config := DefaultTrackerConfig()
	healthTracker := health.NewTracker(health.DefaultConfig())
	config.HealthTracker = healthTracker

	tracker := NewTracker(config)
	ctx := context.Background()

	// Start some operations
	tracker.StartOperation(ctx, "read", nil)
	tracker.StartOperation(ctx, "write", nil)
	tracker.StartOperation(ctx, "read", nil)

	status := tracker.GetSystemStatus()

	if status == nil {
		t.Fatal("GetSystemStatus returned nil")
	}

	if status.ActiveOps != 3 {
		t.Errorf("Expected 3 active operations, got %d", status.ActiveOps)
	}

	if status.OperationsByType["read"] != 2 {
		t.Errorf("Expected 2 read operations, got %d", status.OperationsByType["read"])
	}

	if status.OperationsByType["write"] != 1 {
		t.Errorf("Expected 1 write operation, got %d", status.OperationsByType["write"])
	}

	if status.HealthState != health.StateHealthy {
		t.Errorf("Expected health state=StateHealthy, got %s", status.HealthState)
	}
}

func TestProgress_Update(t *testing.T) {
	progress := &Progress{
		Unit: "bytes",
	}

	// First update
	progress.Update(25, 100)

	if progress.Current != 25 {
		t.Errorf("Expected current=25, got %d", progress.Current)
	}

	if progress.Total != 100 {
		t.Errorf("Expected total=100, got %d", progress.Total)
	}

	if progress.Percentage != 25.0 {
		t.Errorf("Expected percentage=25.0, got %f", progress.Percentage)
	}

	// Wait a bit and make second update to test rate calculation
	time.Sleep(10 * time.Millisecond)
	progress.Update(75, 100)

	if progress.Rate <= 0 {
		t.Error("Expected positive rate")
	}

	if progress.ETA == nil {
		t.Error("Expected ETA to be calculated")
	}
}

func TestProgress_Copy(t *testing.T) {
	original := &Progress{
		Current:    50,
		Total:      100,
		Unit:       "bytes",
		Percentage: 50.0,
		Rate:       1000.0,
		Phase:      "uploading",
		Message:    "In progress",
	}

	eta := 5 * time.Second
	original.ETA = &eta

	copy := original.Copy()

	if copy.Current != original.Current {
		t.Error("Current value not copied correctly")
	}

	if copy.ETA == nil {
		t.Error("ETA not copied")
	}

	if *copy.ETA != *original.ETA {
		t.Error("ETA value not copied correctly")
	}

	// Modify copy to ensure it's independent
	copy.Current = 75
	if original.Current == 75 {
		t.Error("Copy is not independent from original")
	}
}

func TestOperation_Copy(t *testing.T) {
	now := time.Now()
	original := &Operation{
		ID:        "test-123",
		Type:      "upload",
		Status:    StatusInProgress,
		StartTime: now,
		EndTime:   &now,
		Metadata: map[string]interface{}{
			"key": "value",
		},
		Progress: &Progress{
			Current: 50,
			Total:   100,
		},
	}

	copy := original.Copy()

	if copy.ID != original.ID {
		t.Error("ID not copied correctly")
	}

	if copy.Progress == nil {
		t.Error("Progress not copied")
	}

	if copy.Progress.Current != original.Progress.Current {
		t.Error("Progress values not copied correctly")
	}

	// Modify copy to ensure it's independent
	copy.Progress.Current = 75
	if original.Progress.Current == 75 {
		t.Error("Copy is not independent from original")
	}

	copy.Metadata["key"] = "modified"
	if original.Metadata["key"] == "modified" {
		t.Error("Metadata is not independent")
	}
}

func TestTracker_MaxHistory(t *testing.T) {
	config := DefaultTrackerConfig()
	config.MaxHistorySize = 3
	tracker := NewTracker(config)
	ctx := context.Background()

	// Complete 5 operations
	for i := 0; i < 5; i++ {
		op, _ := tracker.StartOperation(ctx, fmt.Sprintf("op-%d", i), nil)
		if err := tracker.CompleteOperation(op.ID); err != nil {
			t.Fatalf("CompleteOperation failed: %v", err)
		}
	}

	history := tracker.GetHistory(0)
	if len(history) != 3 {
		t.Errorf("Expected history size=3, got %d", len(history))
	}
}

func TestTracker_ContextCancellation(t *testing.T) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx, cancel := context.WithCancel(context.Background())

	op, opCtx := tracker.StartOperation(ctx, "test", nil)

	// Cancel parent context
	cancel()

	// The operation context IS derived from parent, so it will be canceled
	// This is the expected behavior with context.WithCancel
	select {
	case <-opCtx.Done():
		// Expected - context inherits cancellation from parent
	case <-time.After(100 * time.Millisecond):
		t.Error("Operation context should be canceled when parent is canceled")
	}

	// The operation should still be tracked even after context cancellation
	_, err := tracker.GetOperation(op.ID)
	if err != nil {
		t.Error("Operation should still be tracked even after context cancellation")
	}
}

func TestGenerateOperationID(t *testing.T) {
	id1 := generateOperationID()
	time.Sleep(1 * time.Millisecond)
	id2 := generateOperationID()

	if id1 == "" {
		t.Error("Generated empty operation ID")
	}

	if id1 == id2 {
		t.Error("Generated duplicate operation IDs")
	}
}

// Benchmark tests
func BenchmarkTracker_StartOperation(b *testing.B) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tracker.StartOperation(ctx, "test", nil)
	}
}

func BenchmarkTracker_UpdateProgress(b *testing.B) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()
	op, _ := tracker.StartOperation(ctx, "test", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tracker.UpdateProgress(op.ID, int64(i), 1000000, "bytes")
	}
}

func BenchmarkTracker_GetOperation(b *testing.B) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()
	op, _ := tracker.StartOperation(ctx, "test", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = tracker.GetOperation(op.ID)
	}
}

func BenchmarkTracker_GetSystemStatus(b *testing.B) {
	tracker := NewTracker(DefaultTrackerConfig())
	ctx := context.Background()

	// Create some operations
	for i := 0; i < 10; i++ {
		tracker.StartOperation(ctx, "test", nil)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tracker.GetSystemStatus()
	}
}
