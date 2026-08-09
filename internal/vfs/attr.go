package vfs

import (
	"fmt"
	"io/fs"
	"maps"
	"strconv"
	"strings"
	"time"
)

// FileType is what kind of thing a name refers to. ObjectFS has exactly two: an object is a regular
// file, and a common prefix is a directory.
//
// There is no symlink, device, socket, or FIFO, and there will not be one without a storage
// representation to back it. Declaring a type the code cannot store is how v0.10.0's
// FilesystemInterface came to advertise Symlink and Link that no implementation provided.
type FileType uint8

const (
	// FileTypeRegular is an object.
	FileTypeRegular FileType = iota

	// FileTypeDir is a common prefix, or an explicit zero-byte marker object with a trailing slash.
	FileTypeDir
)

// String implements fmt.Stringer.
func (t FileType) String() string {
	switch t {
	case FileTypeRegular:
		return "file"
	case FileTypeDir:
		return "dir"
	default:
		return fmt.Sprintf("FileType(%d)", uint8(t))
	}
}

// Default file and directory modes, used when an object carries no stored mode.
//
// 0644 and 0755 are the modes a umask of 022 produces, which is what a user gets from a local
// filesystem and therefore what they expect here. The directory mode must have its execute bits set
// or the directory cannot be traversed: v0.10.0 reported mode 0000 for every directory, which made
// the mount unusable for any non-root user, and the reason it could was that no type owned the
// default.
const (
	DefaultFileMode = fs.FileMode(0o644)
	DefaultDirMode  = fs.FileMode(0o755)
)

// Attr is everything ObjectFS knows about one file or directory.
//
// v0.10.0 had no such type, and the absence had consequences beyond untidiness: chmod and chown
// could not persist because there was nowhere to put the result, the go-fuse mode backstop was left
// disabled because nothing owned the default, and stat answers were assembled ad hoc at each call
// site from whatever HeadObject happened to return.
//
// Fields not backed by object storage are derived, and say so. Nlink and Blocks are computed, never
// stored. Ctime tracks Mtime because S3 has no separate inode-change time.
type Attr struct {
	// Type is the kind of entry. Not stored: derived from whether the key names an object or a
	// common prefix.
	Type FileType

	// Size is the logical byte length of the file's content — after decompression, not the stored
	// object's length. Zero for directories.
	//
	// A compressed object's ContentLength is the compressed size, and reporting that as the file size
	// makes the kernel truncate every read at it. The backend records the uncompressed length in
	// objectfs-original-size for exactly this reason.
	Size int64

	// Mode is the permission bits only, without the type bits. Use [Attr.FileMode] for an
	// fs.FileMode carrying both.
	Mode fs.FileMode

	// UID and GID are the owning user and group.
	UID uint32
	GID uint32

	// Mtime is the content-modification time. For an unmodified object this is the object's
	// LastModified.
	Mtime time.Time

	// Atime is the access time. ObjectFS does not persist it — writing an object on every read would
	// be absurd — so it tracks Mtime. Callers must not rely on it to detect access.
	Atime time.Time

	// Ctime is the attribute-change time. S3 has no equivalent, so it tracks Mtime.
	Ctime time.Time

	// ETag is the stored object's entity tag, empty for a directory or an object not yet flushed.
	// It is the cache's version token: keying on it is what makes an entry invalidate on write
	// rather than persist until a TTL expires.
	ETag string

	// Checksum is the hex-encoded SHA-256 of the uncompressed content, from objectfs-sha256, or empty
	// when the object carries none.
	//
	// v0.10.0 wrote this on every upload and never once read it back. It is the only stored evidence
	// that what came out of the object store is what went in.
	Checksum string

	// Xattrs holds the file's extended attributes, keyed by their full name as the caller gave it —
	// "user.test", not a stored form. A nil value is a tombstone: the attribute has been removed and
	// the removal has not yet been overwritten by a content write. See xattr.go.
	//
	// The map is shared by every copy of an Attr, which are passed by value throughout this package, so
	// it must never be written in place. [Attr.WithXattr] and [Attr.WithoutXattr] copy it and are the
	// only way to change it; read it through [Attr.Xattr] and [Attr.XattrNames], which know what a nil
	// value means.
	Xattrs map[string][]byte
}

// blockSize is the unit stat reports Blocks in. POSIX fixes it at 512 regardless of any real block
// size, and tools including du and tar depend on that.
const blockSize = 512

// FileMode returns the mode with the type bits set, as an fs.FileMode.
func (a Attr) FileMode() fs.FileMode {
	m := a.Mode.Perm()
	if a.Type == FileTypeDir {
		m |= fs.ModeDir
	}
	return m
}

// IsDir reports whether a describes a directory.
func (a Attr) IsDir() bool { return a.Type == FileTypeDir }

// Nlink returns the link count stat should report.
//
// It is derived, never stored. A regular file is always 1: S3 has no hardlinks, so no other value is
// reachable. A directory reports 2 — itself and "." — rather than the 2+subdirectories a local
// filesystem reports, because counting subdirectories would mean a LIST on every stat. Tools that
// use nlink-2 to skip subdirectory descent (some ancient find implementations) will therefore
// descend anyway, which is correct but slower; nothing produces a wrong answer.
func (a Attr) Nlink() uint32 {
	if a.Type == FileTypeDir {
		return 2
	}
	return 1
}

// Blocks returns the number of 512-byte blocks stat should report, rounded up.
//
// It is computed from Size, so a sparse file reports blocks for its holes as though they were
// allocated. Reporting the true stored size would be more honest about billing but would make du
// disagree with ls for every sparse file, and du agreeing with ls is the property tools rely on.
func (a Attr) Blocks() int64 {
	if a.Size <= 0 {
		return 0
	}
	return (a.Size + blockSize - 1) / blockSize
}

// Validate reports whether a is internally consistent. It exists so a malformed Attr is caught where
// it is built rather than where the kernel chokes on it.
func (a Attr) Validate() error {
	switch a.Type {
	case FileTypeRegular, FileTypeDir:
	default:
		return fmt.Errorf("%w: unknown file type %d", ErrInvalid, a.Type)
	}
	if a.Size < 0 {
		return fmt.Errorf("%w: negative size %d", ErrInvalid, a.Size)
	}
	if a.Type == FileTypeDir && a.Size != 0 {
		return fmt.Errorf("%w: directory has size %d", ErrInvalid, a.Size)
	}
	if a.Mode&^fs.ModePerm != 0 {
		return fmt.Errorf("%w: mode %#o carries bits outside the permission mask", ErrInvalid, a.Mode)
	}
	return nil
}

// S3 user-metadata keys ObjectFS uses to persist POSIX attributes that object storage has no native
// field for.
//
// Object metadata, not S3 object annotations: metadata works on every S3-compatible implementation
// (MinIO, Ceph, Wasabi) and on directory buckets, and it survives the copy a tier transition
// performs. The cost is that changing a mode rewrites the object's metadata, which for S3 means a
// CopyObject onto itself.
//
// Keys are lower-case because S3 lower-cases user-metadata keys in transit; comparing them
// case-sensitively against a mixed-case constant is a bug that only shows up against real S3.
const (
	metaUID  = "objectfs-uid"
	metaGID  = "objectfs-gid"
	metaMode = "objectfs-mode"

	// metaMtime is RFC 3339 with nanoseconds. The object's own LastModified records when the object
	// was written, which is not the same as when the file's content was modified — a tier transition
	// or a metadata rewrite moves LastModified and must not move the file's mtime.
	metaMtime = "objectfs-mtime"

	// metaChecksum and metaOriginalSize are written by the storage backend rather than here; they are
	// named so that AttrFromMetadata can read them and so that the full key set is visible in one
	// place.
	metaChecksum     = "objectfs-sha256"
	metaOriginalSize = "objectfs-original-size"
)

// AttrFromMetadata builds an Attr for a regular file from an object's stored user metadata.
//
// storedSize is the object's ContentLength and etag its ETag; lastModified is used for Mtime when the
// object carries no objectfs-mtime. Defaults fill in for anything absent, so an object written by
// another tool — aws s3 cp, boto3, a bucket populated before ObjectFS existed — gets sensible
// attributes rather than zeros.
//
// Malformed metadata is ignored in favor of the default, not propagated as an error. A file must
// remain readable when someone sets objectfs-mode to "banana": the alternative is that one bad
// metadata value makes an object permanently inaccessible, which is a worse failure than a wrong
// mode. Callers wanting to know use [MetadataWarnings].
func AttrFromMetadata(meta map[string]string, storedSize int64, lastModified time.Time, etag string) Attr {
	return AttrFromMetadataWithDefaults(meta, storedSize, lastModified, etag, Attr{})
}

// AttrFromMetadataWithDefaults is [AttrFromMetadata] with caller-supplied fallbacks for the
// attributes an object does not carry.
//
// It exists because "the object has no objectfs-uid" and "the object records objectfs-uid=0" are
// different facts with different right answers, and only the caller knows the second one. A mount
// reports the mounting user as the owner of objects written by other tools, because reporting root
// makes every such file appear to belong to someone else in ls -l and makes cp -p and rsync
// complain about ownership they cannot set. An object that genuinely records uid 0 still reports 0.
//
// Only Mode, UID, and GID are taken from defaults. Size, Mtime, ETag, and Checksum are facts about
// the stored object with no sensible substitute, and Type is always [FileTypeRegular] here — a
// common prefix has no metadata to read, so directories come from [DirAttr] instead. A zero
// defaults.Mode means [DefaultFileMode].
func AttrFromMetadataWithDefaults(
	meta map[string]string, storedSize int64, lastModified time.Time, etag string, defaults Attr,
) Attr {
	mode := defaults.Mode.Perm()
	if mode == 0 {
		mode = DefaultFileMode
	}

	a := Attr{
		Type:  FileTypeRegular,
		Size:  storedSize,
		Mode:  mode,
		UID:   defaults.UID,
		GID:   defaults.GID,
		Mtime: lastModified,
		ETag:  etag,
	}

	if v, ok := lookupMeta(meta, metaOriginalSize); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			a.Size = n
		}
	}
	if v, ok := lookupMeta(meta, metaMode); ok {
		if n, err := strconv.ParseUint(v, 8, 32); err == nil {
			a.Mode = fs.FileMode(n) & fs.ModePerm
		}
	}
	if v, ok := lookupMeta(meta, metaUID); ok {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			a.UID = uint32(n)
		}
	}
	if v, ok := lookupMeta(meta, metaGID); ok {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			a.GID = uint32(n)
		}
	}
	if v, ok := lookupMeta(meta, metaMtime); ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			a.Mtime = t
		}
	}
	if v, ok := lookupMeta(meta, metaChecksum); ok {
		a.Checksum = v
	}
	a.Xattrs = xattrsFromMetadata(meta)

	a.Atime = a.Mtime
	a.Ctime = a.Mtime
	return a
}

// MetadataWarnings returns a message per metadata value that was present but unusable, in the order
// the keys are defined. It is empty when everything parsed.
//
// [AttrFromMetadata] deliberately swallows these so a bad value cannot make a file unreadable;
// this is how a caller logs them anyway. Silently discarding a value the user set is its own defect
// — a chmod that appears to work and does nothing is what this makes visible.
func MetadataWarnings(meta map[string]string) []string {
	var warns []string

	check := func(key string, parse func(string) error) {
		v, ok := lookupMeta(meta, key)
		if !ok {
			return
		}
		if err := parse(v); err != nil {
			warns = append(warns, fmt.Sprintf("%s=%q ignored: %v", key, v, err))
		}
	}

	check(metaOriginalSize, func(v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return err
		}
		if n < 0 {
			return fmt.Errorf("negative size")
		}
		return nil
	})
	check(metaMode, func(v string) error {
		_, err := strconv.ParseUint(v, 8, 32)
		return err
	})
	check(metaUID, func(v string) error {
		_, err := strconv.ParseUint(v, 10, 32)
		return err
	})
	check(metaGID, func(v string) error {
		_, err := strconv.ParseUint(v, 10, 32)
		return err
	})
	check(metaMtime, func(v string) error {
		_, err := time.Parse(time.RFC3339Nano, v)
		return err
	})

	// Extended attributes last, and every unusable one rather than the first: a caller reading this in
	// a log is asking which of its attributes did not survive.
	warns = append(warns, xattrMetadataWarnings(meta)...)

	return warns
}

// Metadata renders the attributes ObjectFS persists as S3 user metadata, to be merged into an
// object's metadata on write.
//
// Size is absent: it is the object's own length, or objectfs-original-size when compressed, and both
// are the storage layer's to write. Checksum is likewise the storage layer's, computed over the
// bytes it uploads. Duplicating either here would create a second source of truth for a value that
// integrity checking depends on.
//
// Extended attributes are included, under objectfs-xattr-… keys that cannot collide with the four
// above; see xattr.go for the encoding and for why a removal appears here as a tombstone rather than
// as an absent key.
func (a Attr) Metadata() map[string]string {
	m := map[string]string{
		metaMode: strconv.FormatUint(uint64(a.Mode.Perm()), 8),
		metaUID:  strconv.FormatUint(uint64(a.UID), 10),
		metaGID:  strconv.FormatUint(uint64(a.GID), 10),
	}
	if !a.Mtime.IsZero() {
		m[metaMtime] = a.Mtime.UTC().Format(time.RFC3339Nano)
	}
	maps.Copy(m, a.xattrMetadata())
	return m
}

// DirAttr returns the attributes for a directory, which are entirely synthetic.
//
// A common prefix is not an object: it has no metadata, no ETag, and no modification time, so there
// is nothing to read a mode from. That is exactly why the default must be right — the mode returned
// here is the only mode the directory will ever have.
func DirAttr(mode fs.FileMode, uid, gid uint32, mtime time.Time) Attr {
	if mode == 0 {
		mode = DefaultDirMode
	}
	return Attr{
		Type:  FileTypeDir,
		Mode:  mode.Perm(),
		UID:   uid,
		GID:   gid,
		Mtime: mtime,
		Atime: mtime,
		Ctime: mtime,
	}
}

// lookupMeta finds a key in object metadata case-insensitively.
//
// S3 lower-cases user-metadata keys, but the SDK's response map preserves whatever case the server
// returned, MinIO title-cases them, and a Go http.Header round-trip canonicalises to Objectfs-Mode.
// A case-sensitive lookup therefore works in unit tests and silently fails against real storage —
// which is the shape of defect this package exists to stop.
func lookupMeta(meta map[string]string, key string) (string, bool) {
	if v, ok := meta[key]; ok {
		return v, true
	}
	for k, v := range meta {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}
