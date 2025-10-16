package utils

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RotationConfig holds configuration for log rotation
type RotationConfig struct {
	// Filename is the file to write logs to
	Filename string

	// MaxSize is the maximum size in megabytes before rotation (0 = no size limit)
	MaxSize int64

	// MaxAge is the maximum age in days before rotation (0 = no age limit)
	MaxAge int

	// MaxBackups is the maximum number of old log files to retain (0 = retain all)
	MaxBackups int

	// Compress determines if rotated log files should be compressed
	Compress bool

	// LocalTime determines if the time used for formatting backup timestamps is local
	LocalTime bool
}

// LogRotator manages log file rotation
type LogRotator struct {
	mu sync.Mutex

	config   *RotationConfig
	file     *os.File
	size     int64
	openTime time.Time
}

// NewLogRotator creates a new log rotator
func NewLogRotator(config *RotationConfig) (*LogRotator, error) {
	if config == nil {
		return nil, fmt.Errorf("rotation config is required")
	}

	if config.Filename == "" {
		return nil, fmt.Errorf("filename is required")
	}

	rotator := &LogRotator{
		config: config,
	}

	// Open the initial log file
	if err := rotator.openFile(); err != nil {
		return nil, err
	}

	return rotator, nil
}

// Write implements io.Writer
func (lr *LogRotator) Write(p []byte) (n int, err error) {
	lr.mu.Lock()
	defer lr.mu.Unlock()

	writeLen := int64(len(p))

	// Check if rotation is needed
	if lr.shouldRotate(writeLen) {
		if err := lr.rotate(); err != nil {
			return 0, fmt.Errorf("failed to rotate log: %w", err)
		}
	}

	// Write to the current file
	n, err = lr.file.Write(p)
	lr.size += int64(n)

	return n, err
}

// Close closes the log file
func (lr *LogRotator) Close() error {
	lr.mu.Lock()
	defer lr.mu.Unlock()

	if lr.file != nil {
		err := lr.file.Close()
		lr.file = nil
		return err
	}
	return nil
}

// Sync flushes the log file
func (lr *LogRotator) Sync() error {
	lr.mu.Lock()
	defer lr.mu.Unlock()

	if lr.file != nil {
		return lr.file.Sync()
	}
	return nil
}

// shouldRotate checks if rotation is needed
func (lr *LogRotator) shouldRotate(writeSize int64) bool {
	// Check size-based rotation
	if lr.config.MaxSize > 0 {
		maxBytes := lr.config.MaxSize * 1024 * 1024
		if lr.size+writeSize >= maxBytes {
			return true
		}
	}

	// Check age-based rotation
	if lr.config.MaxAge > 0 {
		age := time.Since(lr.openTime)
		maxAge := time.Duration(lr.config.MaxAge) * 24 * time.Hour
		if age >= maxAge {
			return true
		}
	}

	return false
}

// rotate performs log rotation
func (lr *LogRotator) rotate() error {
	// Close current file
	if lr.file != nil {
		if err := lr.file.Close(); err != nil {
			return fmt.Errorf("failed to close current log file: %w", err)
		}
		lr.file = nil
	}

	// Generate backup filename with timestamp
	timestamp := lr.backupTimestamp()
	backupName := lr.backupFilename(timestamp)

	// Rename current log file to backup
	if err := os.Rename(lr.config.Filename, backupName); err != nil {
		// If the file doesn't exist, that's okay
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to rename log file: %w", err)
		}
	}

	// Compress the backup if configured
	if lr.config.Compress {
		if err := lr.compressFile(backupName); err != nil {
			// Log compression error but don't fail rotation
			fmt.Fprintf(os.Stderr, "Failed to compress log file %s: %v\n", backupName, err)
		}
	}

	// Clean up old backups
	if err := lr.cleanupOldBackups(); err != nil {
		// Log cleanup error but don't fail rotation
		fmt.Fprintf(os.Stderr, "Failed to cleanup old backups: %v\n", err)
	}

	// Open new log file
	return lr.openFile()
}

// openFile opens the log file for writing
func (lr *LogRotator) openFile() error {
	// Create directory if it doesn't exist
	dir := filepath.Dir(lr.config.Filename)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	// Open or create the log file
	file, err := os.OpenFile(lr.config.Filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	lr.file = file
	lr.openTime = time.Now()

	// Get current file size
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat log file: %w", err)
	}
	lr.size = info.Size()

	return nil
}

// backupTimestamp returns the timestamp to use for backup files
func (lr *LogRotator) backupTimestamp() time.Time {
	if lr.config.LocalTime {
		return time.Now()
	}
	return time.Now().UTC()
}

// backupFilename generates a backup filename with timestamp
func (lr *LogRotator) backupFilename(timestamp time.Time) string {
	dir := filepath.Dir(lr.config.Filename)
	filename := filepath.Base(lr.config.Filename)
	ext := filepath.Ext(filename)
	prefix := filename[0 : len(filename)-len(ext)]

	timestampStr := timestamp.Format("2006-01-02T15-04-05")

	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", prefix, timestampStr, ext))
}

// compressFile compresses a log file using gzip
func (lr *LogRotator) compressFile(filename string) error {
	// Open source file
	src, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	// Create compressed file
	dst, err := os.Create(filename + ".gz")
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()

	// Create gzip writer
	gzipWriter := gzip.NewWriter(dst)
	defer func() { _ = gzipWriter.Close() }()

	// Copy and compress
	if _, err := io.Copy(gzipWriter, src); err != nil {
		return err
	}

	// Close gzip writer to flush
	if err := gzipWriter.Close(); err != nil {
		return err
	}

	// Close destination file
	if err := dst.Close(); err != nil {
		return err
	}

	// Remove original file
	return os.Remove(filename)
}

// cleanupOldBackups removes old backup files based on MaxBackups and MaxAge
func (lr *LogRotator) cleanupOldBackups() error {
	// Get all backup files
	backups, err := lr.getBackupFiles()
	if err != nil {
		return err
	}

	// Sort by modification time (oldest first)
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].ModTime().Before(backups[j].ModTime())
	})

	var toDelete []string

	// Remove backups exceeding MaxBackups
	if lr.config.MaxBackups > 0 && len(backups) > lr.config.MaxBackups {
		excess := len(backups) - lr.config.MaxBackups
		for i := 0; i < excess; i++ {
			toDelete = append(toDelete, backups[i].Name())
		}
		backups = backups[excess:]
	}

	// Remove backups older than MaxAge
	if lr.config.MaxAge > 0 {
		cutoff := time.Now().Add(-time.Duration(lr.config.MaxAge) * 24 * time.Hour)
		for _, backup := range backups {
			if backup.ModTime().Before(cutoff) {
				toDelete = append(toDelete, backup.Name())
			}
		}
	}

	// Delete the files
	for _, filename := range toDelete {
		fullPath := filepath.Join(filepath.Dir(lr.config.Filename), filename)
		if err := os.Remove(fullPath); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to remove old backup %s: %v\n", fullPath, err)
		}
	}

	return nil
}

// getBackupFiles returns all backup files for this log
func (lr *LogRotator) getBackupFiles() ([]os.FileInfo, error) {
	dir := filepath.Dir(lr.config.Filename)
	filename := filepath.Base(lr.config.Filename)
	ext := filepath.Ext(filename)
	prefix := filename[0 : len(filename)-len(ext)]

	// Read directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var backups []os.FileInfo

	// Find matching backup files
	for _, entry := range entries {
		name := entry.Name()

		// Skip the current log file
		if name == filename {
			continue
		}

		// Check if it's a backup file
		if strings.HasPrefix(name, prefix+"-") {
			// Check extensions (.log or .log.gz)
			if strings.HasSuffix(name, ext) || strings.HasSuffix(name, ext+".gz") {
				info, err := entry.Info()
				if err != nil {
					continue
				}
				backups = append(backups, info)
			}
		}
	}

	return backups, nil
}

// ForceRotate forces an immediate rotation
func (lr *LogRotator) ForceRotate() error {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.rotate()
}

// Rotate is a public wrapper for rotate (for testing)
func (lr *LogRotator) Rotate() error {
	return lr.ForceRotate()
}
