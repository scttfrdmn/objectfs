//go:build darwin

package fuse

import "syscall"

// bothXattrFlagsErrno returns EINVAL, which is what darwin's kernel returns for a setxattr naming both
// XATTR_CREATE and XATTR_REPLACE.
//
// Measured rather than read: setxattr with 0x6 (CREATE|REPLACE on this platform) returns EINVAL both when
// the attribute is absent and when it is present, so the answer does not depend on existence the way
// linux's does. See the linux file for the other half.
func bothXattrFlagsErrno() syscall.Errno { return syscall.EINVAL }
