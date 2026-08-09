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

		// The #384 cases. Every one of these is an ordinary filename that happens to contain an
		// adjacent pair of dots, and every one was rejected as "directory traversal" before the fix.
		//
		// The case above — "config/app.config.yaml" — is why that went unnoticed for as long as it did:
		// it was the table's only "dots in filename" entry and its dots are not adjacent, so it passed
		// under the broken check and under the correct one alike. A regression case has to be able to
		// tell the two apart.
		{
			name:          "adjacent dots inside a filename are not traversal",
			path:          "a..b.log",
			allowAbsolute: true,
			wantErr:       false,
		},
		{
			name:          "adjacent dots with no extension are not traversal",
			path:          "my..file",
			allowAbsolute: true,
			wantErr:       false,
		},
		{
			name:          "adjacent dots in a directory component are not traversal",
			path:          "v1..2/x",
			allowAbsolute: true,
			wantErr:       false,
		},
		{
			name:          "the concrete case from the issue",
			path:          "./objectfs..staging.yaml",
			allowAbsolute: true,
			wantErr:       false,
		},
		{
			name:          "leading dots that are not a pair",
			path:          "..foo/bar",
			allowAbsolute: true,
			wantErr:       false,
		},
		{
			name:          "three dots is a legal directory name",
			path:          ".../x",
			allowAbsolute: true,
			wantErr:       false,
		},

		// The other direction: traversal that must still be refused, including the two forms Clean
		// produces that a naive prefix check would miss. Without these the fix could have been "delete
		// the check", which passes every false-positive case above.
		{
			name:          "traversal that Clean reduces to exactly ..",
			path:          "a/../..",
			allowAbsolute: true,
			wantErr:       true,
			errContains:   "directory traversal",
		},
		{
			name:          "traversal surviving Clean as a leading ..",
			path:          "sub/../../x",
			allowAbsolute: true,
			wantErr:       true,
			errContains:   "directory traversal",
		},
		{
			name:          "traversal through a current-directory reference",
			path:          "./x/../../y",
			allowAbsolute: true,
			wantErr:       true,
			errContains:   "directory traversal",
		},
		{
			name:          "relative traversal is refused even when absolute paths are allowed",
			path:          "../../../etc/passwd",
			allowAbsolute: true,
			wantErr:       true,
			errContains:   "directory traversal",
		},

		// Absolute paths with .. in them, asserted so the doc comment's claim is checked rather than
		// stated. Clean treats the root as its own parent, so it resolves these to /etc/passwd and no
		// ".." reaches the traversal check — they are accepted under allowAbsolute, as any other
		// absolute path is. That is the honest boundary of this function, and if Clean's behavior ever
		// changed these would fail rather than the comment quietly becoming wrong.
		{
			name:          "absolute path whose traversal Clean resolves away",
			path:          "/var/../etc/passwd",
			allowAbsolute: true,
			wantErr:       false,
		},
		{
			name:          "absolute path climbing above the root",
			path:          "/../etc/passwd",
			allowAbsolute: true,
			wantErr:       false,
		},
		{
			name:          "the same absolute path is refused for being absolute, not for traversal",
			path:          "/../etc/passwd",
			allowAbsolute: false,
			wantErr:       true,
			errContains:   "absolute paths not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

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
			t.Parallel()

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
			t.Parallel()

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
	for i := range b.N {
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
	for i := range b.N {
		_ = ValidatePathWithinBase(base, paths[i%len(paths)])
	}
}

func BenchmarkSecureJoin(b *testing.B) {
	base := "/var/cache"
	elements := []string{"objectfs", "subdir", "file.dat"}

	b.ResetTimer()
	for range b.N {
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
