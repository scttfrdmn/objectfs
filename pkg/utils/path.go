package utils

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidatePath validates that a file path is safe and does not contain directory traversal attempts.
// It checks for common directory traversal patterns and ensures the cleaned path doesn't escape
// the intended directory structure.
//
// Returns an error if the path contains:
//   - ".." directory traversal sequences
//   - Absolute paths when not expected
//   - Other potentially unsafe patterns
//
// Example usage:
//
//	if err := ValidatePath(userProvidedPath, false); err != nil {
//		return fmt.Errorf("invalid path: %w", err)
//	}
func ValidatePath(path string, allowAbsolute bool) error {
	if path == "" {
		return fmt.Errorf("path cannot be empty")
	}

	// Clean the path to resolve any . or .. elements
	cleanPath := filepath.Clean(path)

	// Check for directory traversal attempts
	if strings.Contains(cleanPath, "..") {
		return fmt.Errorf("path contains directory traversal: %s", path)
	}

	// Check if path is absolute when not allowed
	if !allowAbsolute && filepath.IsAbs(cleanPath) {
		return fmt.Errorf("absolute paths not allowed: %s", path)
	}

	return nil
}

// ValidatePathWithinBase validates that a file path is within a specified base directory.
// This is useful for ensuring that user-provided paths don't escape a designated directory.
//
// The function:
//  1. Cleans both the base and target paths
//  2. Joins them together
//  3. Verifies the result stays within the base directory
//
// Example usage:
//
//	if err := ValidatePathWithinBase("/var/cache", userPath); err != nil {
//		return fmt.Errorf("path outside allowed directory: %w", err)
//	}
func ValidatePathWithinBase(base, path string) error {
	if base == "" {
		return fmt.Errorf("base path cannot be empty")
	}
	if path == "" {
		return fmt.Errorf("path cannot be empty")
	}

	// Clean both paths
	cleanBase := filepath.Clean(base)
	cleanPath := filepath.Clean(path)

	// If path is absolute, it must be within base
	if filepath.IsAbs(cleanPath) {
		if !strings.HasPrefix(cleanPath, cleanBase+string(filepath.Separator)) &&
			cleanPath != cleanBase {
			return fmt.Errorf("path %s is outside base directory %s", path, base)
		}
		return nil
	}

	// For relative paths, join and validate
	fullPath := filepath.Join(cleanBase, cleanPath)

	// Verify the joined path is still within base
	if !strings.HasPrefix(fullPath, cleanBase+string(filepath.Separator)) &&
		fullPath != cleanBase {
		return fmt.Errorf("path %s escapes base directory %s", path, base)
	}

	return nil
}

// SecureJoin safely joins path elements and ensures the result stays within the base directory.
// Unlike filepath.Join, this function validates that the result doesn't escape the base through
// directory traversal.
//
// Example usage:
//
//	safePath, err := SecureJoin("/var/cache", "user", filename)
//	if err != nil {
//		return fmt.Errorf("invalid path combination: %w", err)
//	}
func SecureJoin(base string, elements ...string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("base path cannot be empty")
	}

	cleanBase := filepath.Clean(base)

	// Join all elements
	fullPath := filepath.Join(append([]string{cleanBase}, elements...)...)

	// Validate the result is within base
	if !strings.HasPrefix(fullPath, cleanBase+string(filepath.Separator)) &&
		fullPath != cleanBase {
		return "", fmt.Errorf("path escapes base directory")
	}

	return fullPath, nil
}
