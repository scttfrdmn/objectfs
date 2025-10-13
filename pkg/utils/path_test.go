package utils

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidatePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		path          string
		allowAbsolute bool
		wantErr       bool
		errContains   string
	}{
		{
			name:          "valid relative path",
			path:          "config/app.yaml",
			allowAbsolute: false,
			wantErr:       false,
		},
		{
			name:          "valid absolute path when allowed",
			path:          "/etc/config.yaml",
			allowAbsolute: true,
			wantErr:       false,
		},
		{
			name:          "absolute path not allowed",
			path:          "/etc/config.yaml",
			allowAbsolute: false,
			wantErr:       true,
			errContains:   "absolute paths not allowed",
		},
		{
			name:          "directory traversal with ..",
			path:          "../../../etc/passwd",
			allowAbsolute: false,
			wantErr:       true,
			errContains:   "directory traversal",
		},
		{
			name:          "directory traversal in middle",
			path:          "config/../../../etc/passwd",
			allowAbsolute: false,
			wantErr:       true,
			errContains:   "directory traversal",
		},
		{
			name:          "empty path",
			path:          "",
			allowAbsolute: false,
			wantErr:       true,
			errContains:   "cannot be empty",
		},
		{
			name:          "valid path with dots in filename",
			path:          "config/app.config.yaml",
			allowAbsolute: false,
			wantErr:       false,
		},
		{
			name:          "current directory reference",
			path:          "./config/app.yaml",
			allowAbsolute: false,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePath(tt.path, tt.allowAbsolute)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ValidatePath() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestValidatePathWithinBase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		base        string
		path        string
		wantErr     bool
		errContains string
	}{
		{
			name:    "valid relative path within base",
			base:    "/var/cache",
			path:    "objectfs/file.dat",
			wantErr: false,
		},
		{
			name:    "valid absolute path within base",
			base:    "/var/cache",
			path:    "/var/cache/objectfs/file.dat",
			wantErr: false,
		},
		{
			name:        "path escapes base with ..",
			base:        "/var/cache",
			path:        "../../../etc/passwd",
			wantErr:     true,
			errContains: "escapes base directory",
		},
		{
			name:        "absolute path outside base",
			base:        "/var/cache",
			path:        "/etc/passwd",
			wantErr:     true,
			errContains: "outside base directory",
		},
		{
			name:        "empty base",
			base:        "",
			path:        "file.dat",
			wantErr:     true,
			errContains: "base path cannot be empty",
		},
		{
			name:        "empty path",
			base:        "/var/cache",
			path:        "",
			wantErr:     true,
			errContains: "path cannot be empty",
		},
		{
			name:    "path equals base",
			base:    "/var/cache",
			path:    "/var/cache",
			wantErr: false,
		},
		{
			name:    "complex relative path staying within base",
			base:    "/var/cache",
			path:    "a/b/../c/./file.dat",
			wantErr: false,
		},
		{
			name:        "sneaky traversal attempt",
			base:        "/var/cache",
			path:        "objectfs/../../etc/passwd",
			wantErr:     true,
			errContains: "escapes base directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Skip tests with hardcoded Unix paths on Windows
			if runtime.GOOS == "windows" && strings.HasPrefix(tt.base, "/") {
				t.Skip("Skipping Unix path test on Windows")
			}

			err := ValidatePathWithinBase(tt.base, tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePathWithinBase() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ValidatePathWithinBase() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestSecureJoin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		base        string
		elements    []string
		wantErr     bool
		errContains string
		wantPrefix  string // What the result should start with (OS-agnostic)
	}{
		{
			name:       "valid join",
			base:       "/var/cache",
			elements:   []string{"objectfs", "file.dat"},
			wantErr:    false,
			wantPrefix: "/var/cache",
		},
		{
			name:        "traversal attempt in elements",
			base:        "/var/cache",
			elements:    []string{"objectfs", "..", "..", "..", "etc", "passwd"},
			wantErr:     true,
			errContains: "escapes base directory",
		},
		{
			name:        "empty base",
			base:        "",
			elements:    []string{"file.dat"},
			wantErr:     true,
			errContains: "base path cannot be empty",
		},
		{
			name:       "single element join",
			base:       "/var/cache",
			elements:   []string{"file.dat"},
			wantErr:    false,
			wantPrefix: "/var/cache",
		},
		{
			name:       "multiple nested elements",
			base:       "/var/cache",
			elements:   []string{"a", "b", "c", "d", "file.dat"},
			wantErr:    false,
			wantPrefix: "/var/cache",
		},
		{
			name:       "elements with current directory refs",
			base:       "/var/cache",
			elements:   []string{".", "objectfs", ".", "file.dat"},
			wantErr:    false,
			wantPrefix: "/var/cache",
		},
		{
			name:        "subtle traversal with mixed elements",
			base:        "/var/cache",
			elements:    []string{"objectfs", "subdir", "..", "..", "..", "etc"},
			wantErr:     true,
			errContains: "escapes base directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Skip tests with hardcoded Unix paths on Windows
			if runtime.GOOS == "windows" && strings.HasPrefix(tt.base, "/") {
				t.Skip("Skipping Unix path test on Windows")
			}

			result, err := SecureJoin(tt.base, tt.elements...)
			if (err != nil) != tt.wantErr {
				t.Errorf("SecureJoin() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("SecureJoin() error = %v, should contain %q", err, tt.errContains)
				}
			}
			if !tt.wantErr && tt.wantPrefix != "" {
				cleanPrefix := filepath.Clean(tt.wantPrefix)
				if !strings.HasPrefix(result, cleanPrefix) {
					t.Errorf("SecureJoin() result = %v, should start with %v", result, cleanPrefix)
				}
			}
		})
	}
}

// Benchmark tests
func BenchmarkValidatePath(b *testing.B) {
	paths := []string{
		"config/app.yaml",
		"../../../etc/passwd",
		"/etc/config.yaml",
		"./config/app.yaml",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidatePath(paths[i%len(paths)], false)
	}
}

func BenchmarkValidatePathWithinBase(b *testing.B) {
	base := "/var/cache"
	paths := []string{
		"objectfs/file.dat",
		"../../../etc/passwd",
		"/var/cache/objectfs/file.dat",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidatePathWithinBase(base, paths[i%len(paths)])
	}
}

func BenchmarkSecureJoin(b *testing.B) {
	base := "/var/cache"
	elements := []string{"objectfs", "subdir", "file.dat"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = SecureJoin(base, elements...)
	}
}

// Test cross-platform behavior
func TestCrossPlatform(t *testing.T) {
	t.Parallel()

	// Test that works on both Unix and Windows
	tmpBase := t.TempDir()

	// Valid path within base
	err := ValidatePathWithinBase(tmpBase, "subdir/file.txt")
	if err != nil {
		t.Errorf("ValidatePathWithinBase() with temp dir failed: %v", err)
	}

	// Traversal attempt
	err = ValidatePathWithinBase(tmpBase, "../outside/file.txt")
	if err == nil {
		t.Error("ValidatePathWithinBase() should reject traversal attempt")
	}

	// SecureJoin
	result, err := SecureJoin(tmpBase, "subdir", "file.txt")
	if err != nil {
		t.Errorf("SecureJoin() with temp dir failed: %v", err)
	}
	if !strings.HasPrefix(result, tmpBase) {
		t.Errorf("SecureJoin() result %v doesn't start with base %v", result, tmpBase)
	}
}
