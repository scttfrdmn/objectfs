//go:build linux

package fuse

import "syscall"

// bothXattrFlagsErrno returns 0, because linux has no answer of its own for a setxattr naming both
// XATTR_CREATE and XATTR_REPLACE.
//
// fs/xattr.c contains no combined-flag check. The flags are tested independently against whether the
// attribute exists, so the pair behaves as whichever arm fires first: ENODATA when the attribute is
// missing, because REPLACE requires it, and EEXIST when it is present, because CREATE forbids it.
// setxattr(2)'s man page says EINVAL for this case and the kernel does not implement it; the measurement
// is what this follows.
//
// Returning 0 means "no platform-specific answer, use the ordinary flag handling", which reproduces both
// of those without naming either errno here. See the darwin file for the other half, and
// xattr_oracle_test.go for the test that made the difference visible.
func bothXattrFlagsErrno() syscall.Errno { return 0 }
