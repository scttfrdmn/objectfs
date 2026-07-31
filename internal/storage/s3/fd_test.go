package s3_test

import "syscall"

// fdIsOpen reports whether the given file descriptor number is open in this process.
//
// Fstat is the cheapest question that answers it and has no side effects: it succeeds for an open
// descriptor and fails with EBADF for a closed one. Verified to track opens and closes exactly on
// darwin, which matters because the obvious alternatives do not — /proc/self/fd does not exist there,
// and /dev/fd reports a constant regardless of what the process has open.
//
// It uses syscall rather than golang.org/x/sys so a test helper does not promote an indirect module
// dependency to a direct one. Fstat is not deprecated on either linux or darwin, the two platforms
// ObjectFS builds for.
func fdIsOpen(fd int) bool {
	var st syscall.Stat_t

	return syscall.Fstat(fd, &st) == nil
}
