package filesystem

import (
	"context"
	"os"
	"testing"
	"time"
)

// Compile-time assertion: mockFilesystem implements FilesystemInterface.
var _ FilesystemInterface = (*mockFilesystem)(nil)

// mockFilesystem is a test-only implementation of FilesystemInterface that
// returns zero values and nil errors for every method.
type mockFilesystem struct{}

func (m *mockFilesystem) Open(_ context.Context, _ string, _ int) (FileHandle, error) {
	return &mockFileHandle{}, nil
}

func (m *mockFilesystem) Create(_ context.Context, _ string, _ os.FileMode) (FileHandle, error) {
	return &mockFileHandle{}, nil
}

func (m *mockFilesystem) Close(_ context.Context, _ FileHandle) error { return nil }

func (m *mockFilesystem) Read(_ context.Context, _ FileHandle, buf []byte, _ int64) (int, error) {
	return 0, nil
}

func (m *mockFilesystem) Write(_ context.Context, _ FileHandle, data []byte, _ int64) (int, error) {
	return len(data), nil
}

func (m *mockFilesystem) Flush(_ context.Context, _ FileHandle) error { return nil }

func (m *mockFilesystem) Sync(_ context.Context, _ FileHandle) error { return nil }

func (m *mockFilesystem) ReadDir(_ context.Context, _ string) ([]DirEntry, error) {
	return nil, nil
}

func (m *mockFilesystem) Mkdir(_ context.Context, _ string, _ os.FileMode) error { return nil }

func (m *mockFilesystem) Rmdir(_ context.Context, _ string) error { return nil }

func (m *mockFilesystem) Remove(_ context.Context, _ string) error { return nil }

func (m *mockFilesystem) Rename(_ context.Context, _, _ string) error { return nil }

func (m *mockFilesystem) Stat(_ context.Context, _ string) (FileInfo, error) {
	return FileInfo{}, nil
}

func (m *mockFilesystem) Chmod(_ context.Context, _ string, _ os.FileMode) error { return nil }

func (m *mockFilesystem) Chown(_ context.Context, _ string, _, _ int) error { return nil }

func (m *mockFilesystem) Utimes(_ context.Context, _ string, _, _ time.Time) error { return nil }

func (m *mockFilesystem) Truncate(_ context.Context, _ string, _ int64) error { return nil }

func (m *mockFilesystem) Link(_ context.Context, _, _ string) error { return nil }

func (m *mockFilesystem) Symlink(_ context.Context, _, _ string) error { return nil }

func (m *mockFilesystem) Readlink(_ context.Context, _ string) (string, error) { return "", nil }

func (m *mockFilesystem) GetXattr(_ context.Context, _, _ string) ([]byte, error) {
	return nil, nil
}

func (m *mockFilesystem) SetXattr(_ context.Context, _, _ string, _ []byte) error { return nil }

func (m *mockFilesystem) ListXattr(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockFilesystem) RemoveXattr(_ context.Context, _, _ string) error { return nil }

func (m *mockFilesystem) Statfs(_ context.Context, _ string) (StatfsInfo, error) {
	return StatfsInfo{}, nil
}

func (m *mockFilesystem) GetCostOptimization(_ context.Context, _ string) (*CostAnalysis, error) {
	return nil, nil
}

func (m *mockFilesystem) GetStorageTier(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (m *mockFilesystem) SetStorageTier(_ context.Context, _, _ string) error { return nil }

func (m *mockFilesystem) GetAccessPattern(_ context.Context, _ string) (*AccessPattern, error) {
	return nil, nil
}

// mockFileHandle is a minimal FileHandle for tests.
type mockFileHandle struct{}

func (h *mockFileHandle) Read(_ []byte) (int, error)         { return 0, nil }
func (h *mockFileHandle) Write(p []byte) (int, error)        { return len(p), nil }
func (h *mockFileHandle) Seek(_ int64, _ int) (int64, error) { return 0, nil }
func (h *mockFileHandle) Close() error                       { return nil }
func (h *mockFileHandle) ID() uint64                         { return 0 }
func (h *mockFileHandle) Path() string                       { return "" }
func (h *mockFileHandle) Flags() int                         { return 0 }
func (h *mockFileHandle) S3Key() string                      { return "" }
func (h *mockFileHandle) StorageTier() string                { return "" }
func (h *mockFileHandle) Size() int64                        { return 0 }
func (h *mockFileHandle) LastModified() time.Time            { return time.Time{} }

// ─── Tests ────────────────────────────────────────────────────────────────────

func TestMockFilesystem_InterfaceSatisfied(t *testing.T) {
	t.Parallel()

	// The compile-time assertion at the top of the file is the primary check.
	// This test provides a named entry in the test output to confirm the
	// interface constraint was satisfied at build time.
	t.Log("FilesystemInterface satisfied by mockFilesystem (compile-time assertion passed)")
}

func TestMockFilesystem_Open(t *testing.T) {
	t.Parallel()
	fs := &mockFilesystem{}
	fh, err := fs.Open(context.Background(), "/test.txt", 0)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if fh == nil {
		t.Fatal("Open: expected non-nil FileHandle")
	}
}

func TestMockFilesystem_Create(t *testing.T) {
	t.Parallel()
	fs := &mockFilesystem{}
	fh, err := fs.Create(context.Background(), "/new.txt", 0644)
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if fh == nil {
		t.Fatal("Create: expected non-nil FileHandle")
	}
}

func TestMockFilesystem_ReadWrite(t *testing.T) {
	t.Parallel()
	fs := &mockFilesystem{}
	fh, _ := fs.Open(context.Background(), "/rw.txt", 0)

	buf := make([]byte, 64)
	n, err := fs.Read(context.Background(), fh, buf, 0)
	if err != nil {
		t.Fatalf("Read: unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("Read: expected 0 bytes, got %d", n)
	}

	data := []byte("hello")
	written, err := fs.Write(context.Background(), fh, data, 0)
	if err != nil {
		t.Fatalf("Write: unexpected error: %v", err)
	}
	if written != len(data) {
		t.Errorf("Write: expected %d bytes written, got %d", len(data), written)
	}
}

func TestMockFilesystem_Stat(t *testing.T) {
	t.Parallel()
	fs := &mockFilesystem{}
	info, err := fs.Stat(context.Background(), "/file.txt")
	if err != nil {
		t.Fatalf("Stat: unexpected error: %v", err)
	}
	_ = info
}

func TestMockFilesystem_DirectoryOps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &mockFilesystem{}

	if err := fs.Mkdir(ctx, "/testdir", 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	entries, err := fs.ReadDir(ctx, "/testdir")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ReadDir: expected empty/nil, got %d entries", len(entries))
	}

	if err := fs.Rmdir(ctx, "/testdir"); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}
}

func TestMockFilesystem_LinkOps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &mockFilesystem{}

	if err := fs.Link(ctx, "/src.txt", "/dst.txt"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if err := fs.Symlink(ctx, "/target.txt", "/link.txt"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	target, err := fs.Readlink(ctx, "/link.txt")
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	_ = target
}

func TestMockFilesystem_XattrOps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &mockFilesystem{}

	if err := fs.SetXattr(ctx, "/file.txt", "user.test", []byte("value")); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	data, err := fs.GetXattr(ctx, "/file.txt", "user.test")
	if err != nil {
		t.Fatalf("GetXattr: %v", err)
	}
	_ = data

	names, err := fs.ListXattr(ctx, "/file.txt")
	if err != nil {
		t.Fatalf("ListXattr: %v", err)
	}
	_ = names

	if err := fs.RemoveXattr(ctx, "/file.txt", "user.test"); err != nil {
		t.Fatalf("RemoveXattr: %v", err)
	}
}

func TestMockFilesystem_Statfs(t *testing.T) {
	t.Parallel()
	fs := &mockFilesystem{}
	info, err := fs.Statfs(context.Background(), "/")
	if err != nil {
		t.Fatalf("Statfs: %v", err)
	}
	_ = info
}

func TestMockFilesystem_CostOps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &mockFilesystem{}

	analysis, err := fs.GetCostOptimization(ctx, "/file.txt")
	if err != nil {
		t.Fatalf("GetCostOptimization: %v", err)
	}
	_ = analysis

	tier, err := fs.GetStorageTier(ctx, "/file.txt")
	if err != nil {
		t.Fatalf("GetStorageTier: %v", err)
	}
	_ = tier

	if err := fs.SetStorageTier(ctx, "/file.txt", "STANDARD"); err != nil {
		t.Fatalf("SetStorageTier: %v", err)
	}

	pattern, err := fs.GetAccessPattern(ctx, "/file.txt")
	if err != nil {
		t.Fatalf("GetAccessPattern: %v", err)
	}
	_ = pattern
}

func TestFilesystemError_Error(t *testing.T) {
	t.Parallel()
	err := &FilesystemError{Op: "read", Path: "/test.txt", Err: os.ErrNotExist}
	s := err.Error()
	if s == "" {
		t.Fatal("FilesystemError.Error() returned empty string")
	}
	if err.Unwrap() != os.ErrNotExist {
		t.Fatal("FilesystemError.Unwrap() did not return wrapped error")
	}
}

func TestGetContextHelpers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if p := GetProtocol(ctx); p != "unknown" {
		t.Errorf("GetProtocol on empty context: want %q got %q", "unknown", p)
	}
	if ip := GetClientIP(ctx); ip != "" {
		t.Errorf("GetClientIP on empty context: want %q got %q", "", ip)
	}
	if id := GetRequestID(ctx); id != "" {
		t.Errorf("GetRequestID on empty context: want %q got %q", "", id)
	}

	ctx = context.WithValue(ctx, ContextKeyProtocol, "fuse")
	if p := GetProtocol(ctx); p != "fuse" {
		t.Errorf("GetProtocol: want %q got %q", "fuse", p)
	}
}

func TestFileInfo_Accessors(t *testing.T) {
	t.Parallel()
	now := time.Now()
	fi := FileInfo{
		Name_:    "test.txt",
		Size_:    42,
		Mode_:    0644,
		ModTime_: now,
		IsDir_:   false,
	}
	if fi.Name() != "test.txt" {
		t.Errorf("Name(): want %q got %q", "test.txt", fi.Name())
	}
	if fi.Size() != 42 {
		t.Errorf("Size(): want 42 got %d", fi.Size())
	}
	if fi.Mode() != 0644 {
		t.Errorf("Mode(): want 0644 got %v", fi.Mode())
	}
	if !fi.ModTime().Equal(now) {
		t.Errorf("ModTime() mismatch")
	}
	if fi.IsDir() {
		t.Errorf("IsDir(): expected false")
	}
	if fi.Sys() != nil {
		t.Errorf("Sys(): expected nil")
	}
}
