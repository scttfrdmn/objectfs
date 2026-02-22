// Package archive provides virtual filesystem (VFS) support for archive
// contents exposed through the ObjectFS FUSE layer.  It enables transparent
// navigation of archive files (tar.zst, tar.gz, tar.bz2) as if they were
// ordinary directories, with correct POSIX metadata for every entry.
package archive

import "errors"

// ErrNotFound is returned when a requested path does not exist within an
// archive.
var ErrNotFound = errors.New("archive: entry not found")
