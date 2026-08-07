package utils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewLogRotator(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	config := &RotationConfig{
		Filename:   logFile,
		MaxSize:    1, // 1 MB
		MaxAge:     7,
		MaxBackups: 3,
		Compress:   false,
	}

	rotator, err := NewLogRotator(config)
	if err != nil {
		t.Fatalf("Failed to create rotator: %v", err)
	}
	defer func() { _ = rotator.Close() }()

	// Check file was created
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Error("Log file was not created")
	}
}

func TestLogRotator_Write(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	config := &RotationConfig{
		Filename:   logFile,
		MaxSize:    1, // 1 MB
		MaxAge:     7,
		MaxBackups: 3,
		Compress:   false,
	}

	rotator, err := NewLogRotator(config)
	if err != nil {
		t.Fatalf("Failed to create rotator: %v", err)
	}
	defer func() { _ = rotator.Close() }()

	// Write some data
	message := "Test log message\n"
	n, err := rotator.Write([]byte(message))
	if err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	if n != len(message) {
		t.Errorf("Expected to write %d bytes, wrote %d", len(message), n)
	}

	// Sync to ensure it's written
	if err := rotator.Sync(); err != nil {
		t.Fatalf("Failed to sync: %v", err)
	}

	// Read the file and verify content
	content := readLog(t, logFile)

	if string(content) != message {
		t.Errorf("Expected content %q, got %q", message, string(content))
	}
}

func TestLogRotator_SizeBasedRotation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	// Set very small max size to trigger rotation
	config := &RotationConfig{
		Filename:   logFile,
		MaxSize:    0, // Will set manually below
		MaxAge:     0,
		MaxBackups: 3,
		Compress:   false,
	}

	rotator, err := NewLogRotator(config)
	if err != nil {
		t.Fatalf("Failed to create rotator: %v", err)
	}
	defer func() { _ = rotator.Close() }()

	// Write some data
	message := strings.Repeat("Test log message\n", 100)
	_, _ = rotator.Write([]byte(message))

	// Manually set small size and trigger rotation
	rotator.config.MaxSize = 1     // 1 MB
	rotator.size = 2 * 1024 * 1024 // Pretend we've written 2MB

	// Write more data to trigger rotation
	_, _ = rotator.Write([]byte("trigger rotation\n"))

	// Check that backup file was created
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("Failed to read dir: %v", err)
	}

	backupFound := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "test-") && strings.HasSuffix(entry.Name(), ".log") {
			backupFound = true
			break
		}
	}

	if !backupFound {
		t.Error("Backup file was not created after rotation")
	}
}

func TestLogRotator_ForceRotate(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	config := &RotationConfig{
		Filename:   logFile,
		MaxSize:    10,
		MaxAge:     7,
		MaxBackups: 3,
		Compress:   false,
	}

	rotator, err := NewLogRotator(config)
	if err != nil {
		t.Fatalf("Failed to create rotator: %v", err)
	}
	defer func() { _ = rotator.Close() }()

	// Write some data
	message := "Test log message before rotation\n"
	_, _ = rotator.Write([]byte(message))
	_ = rotator.Sync()

	// Force rotation
	if err := rotator.ForceRotate(); err != nil {
		t.Fatalf("Failed to force rotate: %v", err)
	}

	// Check that backup file was created
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("Failed to read dir: %v", err)
	}

	backupFound := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "test-") && strings.HasSuffix(entry.Name(), ".log") {
			backupFound = true
			break
		}
	}

	if !backupFound {
		t.Error("Backup file was not created after forced rotation")
	}

	// Write to new file
	newMessage := "Test log message after rotation\n"
	if _, err := rotator.Write([]byte(newMessage)); err != nil {
		t.Fatalf("Failed to write after rotation: %v", err)
	}
	_ = rotator.Sync()

	// Check new file contains only new message
	content := readLog(t, logFile)

	if string(content) != newMessage {
		t.Errorf("Expected new file to contain %q, got %q", newMessage, string(content))
	}
}

func TestLogRotator_Compression(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	config := &RotationConfig{
		Filename:   logFile,
		MaxSize:    10,
		MaxAge:     7,
		MaxBackups: 3,
		Compress:   true, // Enable compression
	}

	rotator, err := NewLogRotator(config)
	if err != nil {
		t.Fatalf("Failed to create rotator: %v", err)
	}
	defer func() { _ = rotator.Close() }()

	// Write some data
	message := "Test log message for compression\n"
	if _, err := rotator.Write([]byte(message)); err != nil {
		t.Fatalf("Failed to write: %v", err)
	}
	_ = rotator.Sync()

	// Force rotation
	if err := rotator.ForceRotate(); err != nil {
		t.Fatalf("Failed to force rotate: %v", err)
	}

	// Give compression time to complete
	time.Sleep(100 * time.Millisecond)

	// Check that compressed backup file was created
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("Failed to read dir: %v", err)
	}

	compressedFound := false
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".log.gz") {
			compressedFound = true
			break
		}
	}

	if !compressedFound {
		t.Error("Compressed backup file (.log.gz) was not created")
	}
}

func TestLogRotator_MaxBackups(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	config := &RotationConfig{
		Filename:   logFile,
		MaxSize:    10,
		MaxAge:     0,
		MaxBackups: 2, // Keep only 2 backups
		Compress:   false,
	}

	rotator, err := NewLogRotator(config)
	if err != nil {
		t.Fatalf("Failed to create rotator: %v", err)
	}
	defer func() { _ = rotator.Close() }()

	// Create multiple rotations
	for range 5 {
		if _, err := rotator.Write([]byte("Test message\n")); err != nil {
			t.Fatalf("Failed to write: %v", err)
		}
		_ = rotator.Sync()
		if err := rotator.ForceRotate(); err != nil {
			t.Fatalf("Failed to rotate: %v", err)
		}
		time.Sleep(10 * time.Millisecond) // Ensure different timestamps
	}

	// Count backup files
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("Failed to read dir: %v", err)
	}

	backupCount := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "test-") && strings.HasSuffix(entry.Name(), ".log") {
			backupCount++
		}
	}

	// Should have at most MaxBackups files
	if backupCount > config.MaxBackups {
		t.Errorf("Expected at most %d backup files, found %d", config.MaxBackups, backupCount)
	}
}

func TestLogRotator_DirectoryCreation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "logs", "app")
	logFile := filepath.Join(logDir, "test.log")

	config := &RotationConfig{
		Filename:   logFile,
		MaxSize:    10,
		MaxAge:     7,
		MaxBackups: 3,
		Compress:   false,
	}

	rotator, err := NewLogRotator(config)
	if err != nil {
		t.Fatalf("Failed to create rotator: %v", err)
	}
	defer func() { _ = rotator.Close() }()

	// Check that directory was created
	if _, err := os.Stat(logDir); os.IsNotExist(err) {
		t.Error("Log directory was not created")
	}

	// Check that file was created
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Error("Log file was not created")
	}
}

func TestLogRotator_Close(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	config := &RotationConfig{
		Filename:   logFile,
		MaxSize:    10,
		MaxAge:     7,
		MaxBackups: 3,
		Compress:   false,
	}

	rotator, err := NewLogRotator(config)
	if err != nil {
		t.Fatalf("Failed to create rotator: %v", err)
	}

	if _, err := rotator.Write([]byte("Test message\n")); err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	if err := rotator.Close(); err != nil {
		t.Fatalf("Failed to close rotator: %v", err)
	}

	// Writing after close should fail
	_, err = rotator.Write([]byte("Should fail\n"))
	if err == nil {
		t.Error("Expected write after close to fail")
	}
}

func TestRotationConfig_Validation(t *testing.T) {
	t.Parallel()

	// Test with nil config
	_, err := NewLogRotator(nil)
	if err == nil {
		t.Error("Expected error with nil config")
	}

	// Test with empty filename
	config := &RotationConfig{
		Filename: "",
	}
	_, err = NewLogRotator(config)
	if err == nil {
		t.Error("Expected error with empty filename")
	}
}

func TestLogRotator_Sync(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	config := &RotationConfig{
		Filename:   logFile,
		MaxSize:    10,
		MaxAge:     7,
		MaxBackups: 3,
		Compress:   false,
	}

	rotator, err := NewLogRotator(config)
	if err != nil {
		t.Fatalf("Failed to create rotator: %v", err)
	}
	defer func() { _ = rotator.Close() }()

	// Write and sync
	if _, err := rotator.Write([]byte("Test message\n")); err != nil {
		t.Fatalf("Failed to write: %v", err)
	}
	if err := rotator.Sync(); err != nil {
		t.Fatalf("Failed to sync: %v", err)
	}

	// File should contain the message
	content := readLog(t, logFile)

	if !strings.Contains(string(content), "Test message") {
		t.Error("Synced content not found in file")
	}
}

func TestBackupFilename(t *testing.T) {
	t.Parallel()

	config := &RotationConfig{
		Filename:  "/var/log/app/test.log",
		LocalTime: false,
	}

	rotator := &LogRotator{
		config: config,
	}

	timestamp := time.Date(2023, 10, 15, 14, 30, 45, 0, time.UTC)
	filename := rotator.backupFilename(timestamp)

	expected := "/var/log/app/test-2023-10-15T14-30-45.log"
	if filename != expected {
		t.Errorf("Expected filename %s, got %s", expected, filename)
	}
}

func TestGetBackupFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.log")

	config := &RotationConfig{
		Filename:   logFile,
		MaxSize:    10,
		MaxAge:     7,
		MaxBackups: 3,
		Compress:   false,
	}

	rotator, err := NewLogRotator(config)
	if err != nil {
		t.Fatalf("Failed to create rotator: %v", err)
	}
	defer func() { _ = rotator.Close() }()

	// Create some backup files manually
	backupFiles := []string{
		"test-2023-10-01T10-00-00.log",
		"test-2023-10-02T10-00-00.log",
		"test-2023-10-03T10-00-00.log.gz",
	}

	for _, name := range backupFiles {
		path := filepath.Join(tmpDir, name)
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatalf("Failed to create backup file: %v", err)
		}
	}

	// Get backup files
	backups, err := rotator.getBackupFiles()
	if err != nil {
		t.Fatalf("Failed to get backup files: %v", err)
	}

	if len(backups) != 3 {
		t.Errorf("Expected 3 backup files, found %d", len(backups))
	}
}

// readLog reads the rotator's log file, which every content assertion in this file needs.
//
// A helper rather than three copies of the same five lines, and one #nosec rather than three: the path
// is always filepath.Join(t.TempDir(), ...), so G304's "file inclusion via variable" has no untrusted
// variable to include. Three suppressions on three identical reads is three places for the reasoning
// to drift.
func readLog(t *testing.T, path string) []byte {
	t.Helper()

	content, err := os.ReadFile(path) // #nosec G304 -- a path under this test's own t.TempDir
	if err != nil {
		t.Fatalf("reading the log file %s: %v", path, err)
	}

	return content
}

// TestLogRotator_FilePermissions_AreNotWorldReadable pins the modes openFile creates.
//
// They were 0755 on the directory and 0644 on the file. What ObjectFS writes to a log is not public:
// an object key is the path of a user's file, and the error strings carry the bucket and key. On the
// multi-user systems this filesystem is built for, a 0644 log hands every account on the host a
// listing of what the mount owner has been reading — the same mistake as #211, which published
// component names and error strings on the wildcard address by default.
//
// Asserted rather than left to review, and asserted on the bits rather than on equality with a
// literal, so that a later change widening only the world bits fails here with the mode named.
func TestLogRotator_FilePermissions_AreNotWorldReadable(t *testing.T) {
	t.Parallel()

	// A subdirectory that does not exist yet, so MkdirAll's mode is what gets tested rather than
	// t.TempDir's own 0700.
	dir := filepath.Join(t.TempDir(), "logs")
	logFile := filepath.Join(dir, "test.log")

	rotator, err := NewLogRotator(&RotationConfig{Filename: logFile, MaxSize: 10})
	if err != nil {
		t.Fatalf("NewLogRotator: %v", err)
	}
	defer func() { _ = rotator.Close() }()

	for _, tc := range []struct {
		what string
		path string
		want os.FileMode
	}{
		{"log file", logFile, 0o640},
		{"log directory", dir, 0o750},
	} {
		info, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.what, err)
		}

		perm := info.Mode().Perm()
		if perm&0o007 != 0 {
			t.Errorf("%s mode = %#o, world bits must be clear: a log holds object keys and bucket names",
				tc.what, perm)
		}
		if perm != tc.want {
			t.Errorf("%s mode = %#o, want %#o", tc.what, perm, tc.want)
		}
	}
}
