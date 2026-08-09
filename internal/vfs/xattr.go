package vfs

import (
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Extended attributes, stored in S3 user metadata.
//
// # Why metadata and not object annotations
//
// #167 specifies "object annotations where available, x-amz-meta- keys otherwise". Its premise is not
// that annotations are unavailable — PutObjectAnnotation, GetObjectAnnotation,
// ListObjectAnnotations, and DeleteObjectAnnotation are all present in the pinned SDK
// (service/s3 v1.106.3), and #165's note that they were absent was true of v1.88.4 and is now stale.
// The premise that fails is "where available": it is a per-endpoint fork, and this project's thesis
// says a capability is established by probing rather than by asking. Annotations offer far more room
// than metadata — 1 MiB per annotation, 1,000 per object, against 2 KB total here — so the tradeoff
// is real and the fork is the cost of it.
//
// Metadata is chosen for two reasons, and only the second is about portability:
//
//  1. **A fork would be two storage formats for one attribute set, with no way to migrate between
//     them.** An attribute written as an annotation is invisible to a mount that reads metadata, and a
//     bucket does not gain or lose annotation support atomically — a tier transition to a directory
//     bucket loses annotations, and CopyObject's default AnnotationDirective for a multipart copy does
//     not carry them. Two of those are the operations ObjectFS performs *itself* to persist an
//     attribute change, so a fork would drop attributes during ObjectFS's own writes. The decision
//     recorded on the metadata key block in attr.go went the same way for the same reason.
//  2. Metadata works on every S3-compatible implementation and on directory buckets, and it needs no
//     capability probe — it is the mechanism every write on this filesystem already depends on.
//
// So the honest statement of the limitation is not "annotations are unavailable" but "extended
// attributes on ObjectFS share a 2 KB budget, and lifting it means a migration". If a caller needs
// megabyte-scale attributes, an annotations backend is the way to get them and it should be a
// deliberate, probed, migrating change rather than a silent fallback — a correctness capability by the
// thesis's classification, since the failure mode is an attribute that reads as absent.
//
// # The three encoding problems, and why each rule exists
//
// An xattr name and value are arbitrary bytes from the caller. An S3 user-metadata entry is an HTTP
// header, and three separate properties of that make a direct mapping wrong:
//
//  1. **Case.** S3 lower-cases user-metadata keys in transit, which attr.go records as "a bug that
//     only shows up against real S3". So `user.Foo` and `user.foo` would name the same stored key and
//     silently overwrite each other. The name is therefore base32-encoded, whose alphabet survives
//     case folding without loss.
//  2. **Character set.** An xattr name may hold any byte but NUL, and a header field name may hold
//     only token characters. Base32 solves this at the same time, with one rule rather than an escape
//     scheme whose edge cases are the whole risk.
//  3. **Value bytes.** An xattr value is binary — `setfattr` will happily store a NUL or a newline —
//     and a header value may not carry either. Values are base64-encoded, in the URL alphabet
//     specifically: it avoids `+`, `/`, and `=`, so nothing downstream that treats a header value as a
//     URL fragment or strips padding can corrupt one. That is not hypothetical caution about S3
//     specifically — [Backend.copySource] documents a real case where S3 read `+` in a header as a
//     space.
//
// The cost of base32 is 1.6 bytes stored per byte of name, and that a stored key is no longer legible
// in the AWS console. Both were accepted deliberately over a partial escaping scheme: one rule that is
// always right beats a shorter rule with a class of inputs that breaks it, on a filesystem whose first
// priority is integrity.
const (
	// metaXattrPrefix begins every stored extended attribute's key.
	//
	// It is what makes an xattr unable to collide with a POSIX attribute. No user-supplied name can
	// produce a stored key outside this prefix, and none of objectfs-mode, objectfs-uid, objectfs-gid,
	// objectfs-mtime, objectfs-sha256, or objectfs-original-size begins with it — so a caller setting
	// `user.objectfs-mode` gets a key of objectfs-xattr-<base32 of that name> and cannot reach the
	// file's actual mode. That is a security property rather than a tidiness one: the alternative is a
	// filesystem where an unprivileged setfattr rewrites a file's permission bits.
	metaXattrPrefix = "objectfs-xattr-"

	// xattrPresentTag and xattrRemovedTag are the first byte of a stored value, distinguishing an
	// attribute that exists from one that has been removed.
	//
	// A tag is needed because both states are otherwise the empty string, and both are real: an
	// attribute set to an empty value is legal (`setfattr -n user.x` with no -v), and a removal has to
	// be expressible. With the tag, every stored value is non-empty, which also removes a dependency on
	// whether a given S3 implementation preserves an empty metadata value at all.
	xattrPresentTag = "v"
	xattrRemovedTag = "-"
)

// Why a removal is a tombstone rather than a deletion.
//
// [types.Backend.SetObjectMetadata] is a self-copy with MetadataDirective=REPLACE, and the S3 backend
// merges the object's existing metadata underneath the caller's so that the integrity keys — which
// only the backend has seen the bytes to compute — survive a chmod. A consequence, verified against a
// real endpoint rather than reasoned about: a key omitted from the caller's map is *not* removed from
// the object. So "remove this metadata key" is not an operation the write path has, and a removexattr
// that simply stopped rendering the key would report success while the attribute stayed readable
// forever — the same defect shape as an `rm` that returns success while the object survives.
//
// The alternative to a tombstone is to force a full-content PutObject on every removexattr, since a
// PUT does write metadata wholesale. That is correct and costs a read plus a write of the entire
// object to delete a few bytes: 20 GiB of transfer to remove an attribute from a 10 GiB file. A
// tombstone costs one metadata entry that stays until the object's next content write, which is the
// cheaper wrong-in-a-visible-way, and it is loud in the one place it shows: the entry is visible in
// `head-object` output.
//
// A removal of an attribute that was never set writes nothing at all, so tombstones accumulate only
// for attributes a caller actually used.

// s3UserMetadataLimit is the total size S3 allows for one object's user metadata, in bytes.
//
// AWS measures it as the sum, over every user-metadata entry, of the UTF-8 byte length of the key plus
// the value. Exceeding it fails the request rather than truncating, so this has to be enforced before
// the write is accepted — a setfattr that succeeds and makes the next flush fail moves the error to a
// caller that cannot act on it.
const s3UserMetadataLimit = 2048

// metaHeaderPrefix is the header-name prefix S3 puts in front of every user-metadata key on the wire.
//
// It is counted against the limit here. AWS's documentation says the measurement is over "each key and
// value" without stating whether the key includes this prefix, and the two readings differ by 11 bytes
// per entry. Counting it is the conservative reading: if AWS does not count it, ObjectFS leaves a
// little of the budget unused, and if it does, nothing fails. The other choice fails PUTs.
const metaHeaderPrefix = "x-amz-meta-"

// metadataBytes returns what m costs against [s3UserMetadataLimit].
func metadataBytes(m map[string]string) int {
	n := 0
	for k, v := range m {
		n += len(metaHeaderPrefix) + len(k) + len(v)
	}
	return n
}

// reservedMetadataBytes is the worst-case cost of the metadata ObjectFS writes for itself: the four
// POSIX attribute keys this package renders, plus the two integrity keys the storage backend adds.
//
// Computed from the key constants rather than written as a number, so that renaming a key or adding
// one cannot leave the figure stale — which is the failure this repository has hit repeatedly with
// numbers transcribed into prose. Each value is the longest that key can hold.
var reservedMetadataBytes = metadataBytes(map[string]string{
	// The widest permission rendering, the widest uint32, and the widest RFC 3339 nanosecond stamp.
	metaMode:  strconv.FormatUint(0o777, 8),
	metaUID:   strconv.FormatUint(1<<32-1, 10),
	metaGID:   strconv.FormatUint(1<<32-1, 10),
	metaMtime: time.Date(9999, 12, 31, 23, 59, 59, 123456789, time.UTC).Format(time.RFC3339Nano),

	// Written by the storage backend, not here, and reserved anyway: they are on the object and they
	// count against the same limit. objectfs-original-size appears only on compressed objects, so
	// reserving it always is deliberately pessimistic — a budget that is right only for uncompressed
	// files is a budget that fails when compression is enabled.
	metaChecksum:     strings.Repeat("0", 64),
	metaOriginalSize: strconv.FormatInt(1<<63-1, 10),
})

// XattrBudget is the number of metadata bytes extended attributes may occupy on one object.
//
// It is the S3 limit less what ObjectFS already spends on POSIX attributes and integrity, and it is a
// budget for the *encoded* form: a name costs 11 bytes of header prefix plus 15 of key prefix plus
// ceil(len(name)*8/5), and a value costs 1 plus ceil(len(value)*4/3). Roughly 35 attributes of the
// shape `user.test`=`hello` fit.
//
// It does not account for user metadata some other tool put on the object, which the metadata replace
// preserves and which counts against the same limit. That metadata is not visible to this package —
// [AttrFromMetadata] keeps only the keys it understands — and threading the whole stored map through
// the flush protocol to count it would be a larger change than the accuracy is worth. The consequence
// is bounded and honest: with enough foreign metadata a setfattr inside this budget is refused by S3
// instead, and because the FUSE layer flushes an xattr synchronously the caller sees that refusal on
// its own setxattr call rather than discovering it later.
var XattrBudget = s3UserMetadataLimit - reservedMetadataBytes

// xattrNameEncoding is base32 without padding. Padding is dropped because "=" is not a header-name
// character; case is folded by the accessors, since S3 lower-cases the key in transit.
var xattrNameEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// xattrValueEncoding is unpadded base64 in the URL alphabet. See the note on this file's constants for
// why the URL alphabet and not the standard one.
var xattrValueEncoding = base64.RawURLEncoding

// encodeXattrKey returns the S3 user-metadata key that stores the attribute named name.
func encodeXattrKey(name string) string {
	return metaXattrPrefix + strings.ToLower(xattrNameEncoding.EncodeToString([]byte(name)))
}

// decodeXattrKey returns the attribute name a stored metadata key names, and whether the key is one of
// ObjectFS's extended attributes at all.
//
// Both the prefix match and the base32 decode are case-insensitive, for the reason [lookupMeta]
// documents: S3 lower-cases user-metadata keys, MinIO title-cases them, and a Go http.Header round
// trip canonicalises them to Objectfs-Xattr-…. A case-sensitive decode here would work in a unit test
// and lose every attribute against real storage.
func decodeXattrKey(key string) (string, bool) {
	if len(key) < len(metaXattrPrefix) || !strings.EqualFold(key[:len(metaXattrPrefix)], metaXattrPrefix) {
		return "", false
	}

	raw, err := xattrNameEncoding.DecodeString(strings.ToUpper(key[len(metaXattrPrefix):]))
	if err != nil {
		return "", false
	}

	return string(raw), true
}

// encodeXattrValue renders an attribute value for storage. A nil value is a tombstone.
func encodeXattrValue(value []byte) string {
	if value == nil {
		return xattrRemovedTag
	}

	return xattrPresentTag + xattrValueEncoding.EncodeToString(value)
}

// decodeXattrValue returns the attribute value a stored metadata value holds, whether it is a
// tombstone, and whether it could be decoded at all.
func decodeXattrValue(stored string) (value []byte, removed, ok bool) {
	switch {
	case stored == xattrRemovedTag:
		return nil, true, true
	case strings.HasPrefix(stored, xattrPresentTag):
		raw, err := xattrValueEncoding.DecodeString(stored[len(xattrPresentTag):])
		if err != nil {
			return nil, false, false
		}
		// raw is non-nil even for an empty value, which is load-bearing rather than incidental: nil is the
		// tombstone here, so a nil returned for an attribute set to zero bytes would make an existing
		// attribute read as removed. base64's DecodeString allocates its result, so a zero-length decode is
		// an empty non-nil slice. A guard normalising it stood here and was removed as unreachable — code no
		// test can enter is code no test can check. [TestAnEmptyValueDecodesToANonNilSlice] pins the property
		// instead, so a change of encoder that broke it fails a test rather than being caught by a guard
		// nobody could confirm still worked.
		return raw, false, true
	default:
		return nil, false, false
	}
}

// Xattr returns the value of the extended attribute named name, and whether the file has one.
//
// A tombstoned attribute reports absent, which is what it is. Callers must not read [Attr.Xattrs]
// directly to answer this: a nil value there means removed, not empty.
func (a Attr) Xattr(name string) ([]byte, bool) {
	v, ok := a.Xattrs[name]
	if !ok || v == nil {
		return nil, false
	}

	return v, true
}

// XattrNames returns the names of the file's extended attributes, sorted, excluding tombstones.
//
// Sorted so that listxattr is stable across calls. The kernel does not require an order, but a
// listing that changes between two identical calls makes every test of it flaky and makes `getfattr
// -d` output diff noisily for no reason.
func (a Attr) XattrNames() []string {
	names := make([]string, 0, len(a.Xattrs))
	for name, v := range a.Xattrs {
		if v == nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// WithXattr returns a copy of a with the extended attribute name set to value.
//
// The returned Attr holds a new map: nothing mutates the receiver's, so an [Attr] already handed to a
// caller — [Node.Attr] returns one under a lock and the caller reads it without — cannot observe a
// change or race one. Copy-on-write is what makes the map in a value type safe to share.
//
// A value of nil sets a tombstone; use [Attr.WithoutXattr], which says so.
//
// The budget is enforced here rather than at the FUSE boundary, because here is where the encoded form
// exists. Two distinct errors come out of it, matching setxattr(2): [ErrTooLarge] when this one
// attribute could not fit on an object with no others, and [ErrNoSpace] when it cannot fit alongside
// the ones already stored. The distinction is what a caller acts on — the first says the value is too
// big, the second says the file is full — and collapsing them would make a 3 KB value and a full file
// indistinguishable.
func (a Attr) WithXattr(name string, value []byte) (Attr, error) {
	if name == "" {
		return a, fmt.Errorf("%w: empty extended attribute name", ErrInvalid)
	}

	// The one-attribute check first, so the E2BIG case is decided without allocating a map it is going
	// to reject, and so the two errors cannot be reported in the wrong order: an attribute that is too
	// large on its own is E2BIG whatever else the file holds.
	alone := xattrMetadataBytes(map[string][]byte{name: value})
	if alone > XattrBudget {
		return a, fmt.Errorf("%w: extended attribute %q needs %d metadata bytes and the whole budget is "+
			"%d", ErrTooLarge, name, alone, XattrBudget)
	}

	next := make(map[string][]byte, len(a.Xattrs)+1)
	maps.Copy(next, a.Xattrs)
	next[name] = value

	if total := xattrMetadataBytes(next); total > XattrBudget {
		return a, fmt.Errorf("%w: extended attributes would need %d metadata bytes of the %d available",
			ErrNoSpace, total, XattrBudget)
	}

	a.Xattrs = next

	return a, nil
}

// WithoutXattr returns a copy of a with the extended attribute name removed, and whether it was there
// to remove.
//
// A false second return means nothing changed and the receiver's copy is returned unmodified, so a
// caller can report ENOATTR without having written anything. That matters beyond the errno: writing a
// tombstone for an attribute that never existed would spend budget, and would make `setfattr -x` on an
// absent attribute cost an S3 round trip.
func (a Attr) WithoutXattr(name string) (Attr, bool) {
	if _, ok := a.Xattr(name); !ok {
		return a, false
	}

	next := make(map[string][]byte, len(a.Xattrs))
	maps.Copy(next, a.Xattrs)
	// nil, not delete: see the tombstone note above. Omitting the key would leave the attribute
	// readable on the object forever, because a metadata replace merges rather than replaces.
	next[name] = nil

	a.Xattrs = next

	return a, true
}

// xattrMetadataBytes returns what a set of extended attributes costs against [XattrBudget].
func xattrMetadataBytes(xattrs map[string][]byte) int {
	m := make(map[string]string, len(xattrs))
	for name, value := range xattrs {
		m[encodeXattrKey(name)] = encodeXattrValue(value)
	}

	return metadataBytes(m)
}

// xattrsFromMetadata decodes every extended attribute an object's user metadata carries, returning nil
// when it carries none.
//
// Undecodable entries are skipped rather than reported, on the policy [AttrFromMetadata] states: a
// file must stay readable when someone hand-writes a malformed objectfs-xattr-… key with the AWS CLI.
// [MetadataWarnings] is how a caller learns what was ignored.
func xattrsFromMetadata(meta map[string]string) map[string][]byte {
	var xattrs map[string][]byte

	for k, v := range meta {
		name, ok := decodeXattrKey(k)
		if !ok {
			continue
		}
		value, removed, ok := decodeXattrValue(v)
		if !ok {
			continue
		}
		if xattrs == nil {
			xattrs = make(map[string][]byte)
		}
		if removed {
			xattrs[name] = nil
			continue
		}
		xattrs[name] = value
	}

	return xattrs
}

// xattrMetadata renders a's extended attributes as S3 user metadata, to be merged with the POSIX
// attribute keys by [Attr.Metadata].
//
// Tombstones are rendered, not skipped. They are the removal, and a flush that dropped them would
// leave every removed attribute readable on the object.
func (a Attr) xattrMetadata() map[string]string {
	if len(a.Xattrs) == 0 {
		return nil
	}

	m := make(map[string]string, len(a.Xattrs))
	for name, value := range a.Xattrs {
		m[encodeXattrKey(name)] = encodeXattrValue(value)
	}

	return m
}

// xattrMetadataWarnings describes every objectfs-xattr- entry that was present and unusable.
func xattrMetadataWarnings(meta map[string]string) []string {
	var keys []string
	for k := range meta {
		if len(k) >= len(metaXattrPrefix) && strings.EqualFold(k[:len(metaXattrPrefix)], metaXattrPrefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var warns []string
	for _, k := range keys {
		name, ok := decodeXattrKey(k)
		if !ok {
			warns = append(warns, fmt.Sprintf("%s ignored: the name after %q is not base32",
				k, metaXattrPrefix))
			continue
		}
		if _, _, ok := decodeXattrValue(meta[k]); !ok {
			warns = append(warns, fmt.Sprintf("%s (extended attribute %q) ignored: value %q is neither a "+
				"%q tombstone nor %q followed by base64",
				k, name, meta[k], xattrRemovedTag, xattrPresentTag))
		}
	}

	return warns
}
