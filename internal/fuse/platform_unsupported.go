//go:build !linux && !darwin

// Package fuse supports Linux and macOS only.
//
// This file exists so that building for any other GOOS fails inside this package with a message
// that says why, rather than producing a list of "undefined: fuse.PlatformFileSystem" errors in
// internal/adapter — which is what a bare set of linux||darwin constraints yields, and which reads
// like a broken build rather than an unsupported platform.
//
// Windows in particular is not supported. A `cgofuse` build tag existed through v0.10.0 and never
// compiled; it was removed in v0.10.1 along with the github.com/winfsp/cgofuse dependency. Adding
// Windows support means writing a second thin shim over internal/vfs and exercising it in CI — not
// restoring a build tag.
package fuse

const _ = ObjectFS_supports_only_linux_and_darwin__see_internal_fuse_platform_unsupported_go
