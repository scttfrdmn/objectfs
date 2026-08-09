package vfs

// Tests for the extended-attribute encoding and for the Attr accessors over it.
//
// Nothing here needs a backend: the properties under test are the encoding's — that a name survives S3
// lower-casing, that a value survives being an HTTP header, that a stored key cannot collide with a
// POSIX attribute key — and each of them is decided entirely by this package. The tests that need a
// real endpoint are the ones asserting an attribute survives to storage and back, and those live in
// internal/fuse alongside the operations that write it.

import (
	"bytes"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
)

// attrEqual compares two Attr values including their extended attributes.
//
// It exists because adding Xattrs made Attr non-comparable, so `got != want` on a whole Attr no longer
// compiles. reflect.DeepEqual is exactly as strict as == was for every other field — including
// time.Time, where it compares the wall clock, the monotonic reading, and the location just as ==
// does — and it distinguishes a nil value from an empty one, which is the tombstone/empty-attribute
// distinction this package rests on. Comparing field by field instead would go stale the next time a
// field is added, which is the failure mode that motivated this type in the first place.
func attrEqual(a, b Attr) bool {
	return reflect.DeepEqual(a, b)
}

// TestXattrKeyEncodingSurvivesCaseFolding is the case-collision property, which is the one #167 names
// and the one that cannot be checked by looking at a stored key.
//
// S3 lower-cases user-metadata keys in transit. A stored key built by concatenating the attribute name
// would therefore make user.Foo and user.foo the same key, and setting one would overwrite the other
// with no error anywhere — the second setfattr reports success and the first attribute is gone. Base32
// is what makes the two distinct after folding.
func TestXattrKeyEncodingSurvivesCaseFolding(t *testing.T) {
	t.Parallel()

	lower := encodeXattrKey("user.foo")
	upper := encodeXattrKey("user.Foo")

	if lower == upper {
		t.Fatalf("user.foo and user.Foo both encode to %q, so setting one destroys the other", lower)
	}

	// The keys as ObjectFS writes them must already be lower-case, so that what S3 stores is what was
	// sent. An encoder emitting upper-case would still decode correctly here but would make a stored
	// key differ from the one that was written, and every case-sensitive comparison anywhere downstream
	// would then be wrong in a way only real storage shows.
	for _, k := range []string{lower, upper} {
		if k != strings.ToLower(k) {
			t.Errorf("encoded key %q is not lower-case, so S3's folding changes it in transit", k)
		}
	}

	// And the round trip, through the folding S3 performs and the title-casing MinIO performs.
	for _, name := range []string{"user.foo", "user.Foo", "user.FOO", "USER.foo"} {
		key := encodeXattrKey(name)
		for _, wire := range []string{key, strings.ToUpper(key), titleCase(key)} {
			got, ok := decodeXattrKey(wire)
			if !ok {
				t.Errorf("decodeXattrKey(%q) rejected a key ObjectFS wrote for %q", wire, name)
				continue
			}
			if got != name {
				t.Errorf("%q round-tripped through %q as %q", name, wire, got)
			}
		}
	}
}

// TestXattrKeysCannotCollideWithPOSIXAttributeKeys is the security-relevant case.
//
// If a caller could choose an attribute name whose stored key is objectfs-mode, then `setfattr -n
// objectfs-mode -v 4777` would rewrite the file's permission bits — an unprivileged process editing a
// field the filesystem reports as the file's mode. The prefix is what makes that unreachable, and
// this asserts it for the names most likely to try.
func TestXattrKeysCannotCollideWithPOSIXAttributeKeys(t *testing.T) {
	t.Parallel()

	reserved := []string{metaMode, metaUID, metaGID, metaMtime, metaChecksum, metaOriginalSize}

	names := []string{
		"objectfs-mode", "objectfs-uid", "objectfs-gid", "objectfs-mtime",
		"objectfs-sha256", "objectfs-original-size",
		"user.objectfs-mode", "OBJECTFS-MODE",
		// A name that already starts with the xattr prefix must not be able to produce a key that
		// decodes as some *other* attribute either.
		metaXattrPrefix + "objectfs-mode",
	}

	for _, name := range names {
		key := encodeXattrKey(name)

		for _, r := range reserved {
			if strings.EqualFold(key, r) {
				t.Errorf("attribute %q encodes to %q, which is the stored key for %q. A caller could "+
					"rewrite a file's POSIX attributes with setfattr.", name, key, r)
			}
		}

		// The stronger statement: the key is inside the xattr namespace, so no future POSIX key can
		// collide either unless someone names one objectfs-xattr-….
		if !strings.HasPrefix(key, metaXattrPrefix) {
			t.Errorf("attribute %q encodes to %q, outside the %q namespace", name, key, metaXattrPrefix)
		}

		// And it still round-trips: namespacing must not come at the cost of the name.
		if got, ok := decodeXattrKey(key); !ok || got != name {
			t.Errorf("attribute %q encoded to %q and decoded to %q (ok=%v)", name, key, got, ok)
		}
	}

	// The reserved keys themselves must not read as extended attributes, or a chmod would appear in
	// `getfattr -d` output and a flush would rewrite the mode key as an xattr.
	for _, r := range reserved {
		if name, ok := decodeXattrKey(r); ok {
			t.Errorf("the POSIX attribute key %q decodes as extended attribute %q", r, name)
		}
	}
}

// TestXattrValueEncodingRoundTripsArbitraryBytes covers the header-safety property.
//
// A metadata value is an HTTP header value, so a raw xattr value containing a NUL, a newline, or a
// non-UTF-8 byte would either be rejected or silently mangled by something between here and S3.
// setfattr will store any of those, and `-e hex` exists precisely because people do.
func TestXattrValueEncodingRoundTripsArbitraryBytes(t *testing.T) {
	t.Parallel()

	cases := map[string][]byte{
		"empty":                     {},
		"ascii":                     []byte("hello"),
		"nul":                       {0},
		"nul in the middle":         []byte("a\x00b"),
		"crlf":                      []byte("a\r\nb"),
		"every byte":                allBytes(),
		"invalid utf-8":             {0xff, 0xfe, 0xfd},
		"header delimiters":         []byte(": ;,\"\\"),
		"plus and slash and equals": []byte("+/=+/="),
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			stored := encodeXattrValue(value)

			// Nothing in the stored form may need escaping in a header value.
			if strings.ContainsAny(stored, "\x00\r\n \t\"\\+/=,;:") {
				t.Errorf("encoded value %q carries a character that is not safe in an HTTP header value",
					stored)
			}

			got, removed, ok := decodeXattrValue(stored)
			if !ok {
				t.Fatalf("decodeXattrValue(%q) failed for a value this package encoded", stored)
			}
			if removed {
				t.Fatalf("a set value decoded as a tombstone, so the attribute would read as absent")
			}
			if !bytes.Equal(got, value) {
				t.Fatalf("value round-tripped as %v, want %v", got, value)
			}
			if got == nil {
				t.Error("an existing attribute decoded to a nil value, which is the tombstone " +
					"representation: it would read as removed")
			}
		})
	}
}

func allBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// TestXattrTombstoneIsDistinctFromAnEmptyValue pins the distinction the tag exists for.
//
// `setfattr -n user.x f` with no -v sets an attribute whose value is zero bytes, and that attribute
// exists: getfattr reports it and listxattr names it. A removed attribute does not exist. Without the
// tag both are the empty string in storage, and the two states would be indistinguishable — so either
// every empty attribute would read as removed, or every removal would leave an empty attribute behind.
func TestXattrTombstoneIsDistinctFromAnEmptyValue(t *testing.T) {
	t.Parallel()

	empty := encodeXattrValue([]byte{})
	tomb := encodeXattrValue(nil)

	if empty == tomb {
		t.Fatalf("an empty attribute and a removed one both store as %q", empty)
	}
	if empty == "" || tomb == "" {
		t.Errorf("a stored value is the empty string (empty=%q tombstone=%q); an S3 implementation that "+
			"drops empty metadata values would lose it entirely", empty, tomb)
	}

	if _, removed, ok := decodeXattrValue(empty); !ok || removed {
		t.Errorf("an empty attribute decoded as removed=%v ok=%v", removed, ok)
	}
	if v, removed, ok := decodeXattrValue(tomb); !ok || !removed || v != nil {
		t.Errorf("a tombstone decoded as value=%v removed=%v ok=%v", v, removed, ok)
	}
}

// TestAnEmptyValueDecodesToANonNilSlice pins the one property of the value encoder the tombstone rests on.
//
// nil means "removed" throughout this package, so [decodeXattrValue] returning nil for an attribute whose
// value is zero bytes would make an existing empty attribute read as removed — `getfattr` reporting ENOATTR
// for an attribute `setfattr -n user.x` just created. base64's DecodeString allocates, so this holds today
// and a guard for it in decodeXattrValue was unreachable and was removed. That makes this test the only
// thing standing between a future encoder change and the defect, which is why it asserts the nil-ness
// directly rather than only the round trip: [Attr.Xattr] maps both nil and absent to "not there", so a
// round-trip assertion would report the same answer either way and pass.
func TestAnEmptyValueDecodesToANonNilSlice(t *testing.T) {
	t.Parallel()

	value, removed, ok := decodeXattrValue(encodeXattrValue([]byte{}))

	if !ok || removed {
		t.Fatalf("an empty attribute decoded as removed=%v ok=%v", removed, ok)
	}
	if value == nil {
		t.Error("an empty attribute value decoded to nil. nil is the tombstone, so this attribute now " +
			"reads as removed: getfattr answers ENOATTR for an attribute setfattr created.")
	}
	if len(value) != 0 {
		t.Errorf("an empty attribute value decoded to %q", value)
	}
}

// TestXattrAccessorsTreatATombstoneAsAbsent covers the read side of the tombstone.
func TestXattrAccessorsTreatATombstoneAsAbsent(t *testing.T) {
	t.Parallel()

	a := Attr{Xattrs: map[string][]byte{
		"user.gone":  nil,
		"user.empty": {},
		"user.set":   []byte("x"),
	}}

	if _, ok := a.Xattr("user.gone"); ok {
		t.Error("Xattr reported a removed attribute as present, so getfattr would return a nil value " +
			"for something that was deleted")
	}
	if v, ok := a.Xattr("user.empty"); !ok || v == nil || len(v) != 0 {
		t.Errorf("Xattr on an empty attribute returned %v, %v; an empty value is a real value", v, ok)
	}
	if v, ok := a.Xattr("user.set"); !ok || string(v) != "x" {
		t.Errorf("Xattr on a set attribute returned %q, %v", v, ok)
	}
	if _, ok := a.Xattr("user.never"); ok {
		t.Error("Xattr invented an attribute that was never set")
	}

	want := []string{"user.empty", "user.set"}
	if got := a.XattrNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("XattrNames = %v, want %v (sorted, and without the tombstone)", got, want)
	}
}

// TestWithXattrDoesNotMutateTheReceiver is the copy-on-write property.
//
// Attr is a value type passed by copy everywhere in this package, and [Node.Attr] returns one under a
// lock which the caller then reads without it. A map written in place would be visible through every
// existing copy and would be a data race against any concurrent reader — so the whole reason Attr can
// carry a map at all is that these two methods never write the one they were given.
func TestWithXattrDoesNotMutateTheReceiver(t *testing.T) {
	t.Parallel()

	original := Attr{Xattrs: map[string][]byte{"user.a": []byte("1")}}
	snapshot := maps.Clone(original.Xattrs)

	next, err := original.WithXattr("user.b", []byte("2"))
	if err != nil {
		t.Fatalf("WithXattr: %v", err)
	}
	if !reflect.DeepEqual(original.Xattrs, snapshot) {
		t.Errorf("WithXattr mutated the receiver's map: %v, was %v", original.Xattrs, snapshot)
	}
	if _, ok := next.Xattr("user.b"); !ok {
		t.Error("WithXattr did not set the attribute on the returned copy")
	}

	removed, existed := next.WithoutXattr("user.a")
	if !existed {
		t.Fatal("WithoutXattr reported an attribute absent that was set")
	}
	if !reflect.DeepEqual(original.Xattrs, snapshot) {
		t.Errorf("WithoutXattr mutated the receiver's map: %v, was %v", original.Xattrs, snapshot)
	}
	if _, ok := removed.Xattr("user.a"); ok {
		t.Error("WithoutXattr left the attribute readable")
	}
	if _, ok := next.Xattr("user.a"); !ok {
		t.Error("WithoutXattr reached back into the map its receiver was sharing")
	}
}

// TestWithoutXattrReportsAbsenceWithoutWriting is what lets removexattr answer ENOATTR without
// spending an S3 round trip, and — more importantly — without leaving the node dirty.
func TestWithoutXattrReportsAbsenceWithoutWriting(t *testing.T) {
	t.Parallel()

	a := Attr{Xattrs: map[string][]byte{"user.gone": nil}}

	for _, name := range []string{"user.never", "user.gone"} {
		got, existed := a.WithoutXattr(name)
		if existed {
			t.Errorf("WithoutXattr(%q) reported an attribute that is not there", name)
		}
		if len(got.Xattrs) != len(a.Xattrs) {
			t.Errorf("WithoutXattr(%q) changed the attribute set for an attribute that is not there", name)
		}
	}
}

// TestChangingOneXattrKeepsTheOthers is the other half of copy-on-write, and the half that a
// single-attribute test cannot see.
//
// [TestWithXattrDoesNotMutateTheReceiver] checks that the receiver's map is not written; this checks that
// the returned copy still holds what the receiver held. Those are different failures with the same cause,
// and a map with one entry in it distinguishes neither: drop the copy from [Attr.WithoutXattr] and a
// one-entry map yields exactly the empty map the removal was supposed to produce.
//
// A verifying mutation is why this exists. Replacing the copy in WithoutXattr with a copy of nothing left
// every test in this package and in internal/fuse passing, because the FUSE layer's removal test reads its
// surviving attribute back after a remount, and a metadata replace *merges* — so the sibling ObjectFS had
// silently dropped from memory was still on the object and came back on the next HEAD. The defect is real
// for the caller that reads before the next flush completes: getxattr answers from the in-memory Attr,
// which would report ENOATTR for an attribute nothing ever removed.
func TestChangingOneXattrKeepsTheOthers(t *testing.T) {
	t.Parallel()

	base := Attr{Xattrs: map[string][]byte{
		"user.first":  []byte("1"),
		"user.second": []byte("2"),
		"user.third":  []byte("3"),
	}}

	t.Run("a removal", func(t *testing.T) {
		t.Parallel()

		got, existed := base.WithoutXattr("user.second")
		if !existed {
			t.Fatal("WithoutXattr reported the attribute absent")
		}

		for _, name := range []string{"user.first", "user.third"} {
			if _, ok := got.Xattr(name); !ok {
				t.Errorf("removing user.second also lost %s. Nothing removed it, so a getxattr before the "+
					"next flush would answer ENOATTR for an attribute that exists.", name)
			}
		}
		if _, ok := got.Xattr("user.second"); ok {
			t.Error("the removed attribute is still readable")
		}
	})

	t.Run("a set", func(t *testing.T) {
		t.Parallel()

		got, err := base.WithXattr("user.fourth", []byte("4"))
		if err != nil {
			t.Fatalf("WithXattr: %v", err)
		}

		for _, name := range []string{"user.first", "user.second", "user.third", "user.fourth"} {
			if _, ok := got.Xattr(name); !ok {
				t.Errorf("after setting user.fourth, %s is gone", name)
			}
		}
	})

	// And the rendered metadata carries all of them, because that is what reaches the object. A set that
	// rendered only the changed attribute would still work against a merging endpoint and would delete
	// every other attribute against one that replaces wholesale — a PutObject, which is what a content
	// write does.
	t.Run("the rendered metadata", func(t *testing.T) {
		t.Parallel()

		got, err := base.WithXattr("user.fourth", []byte("4"))
		if err != nil {
			t.Fatalf("WithXattr: %v", err)
		}

		meta := got.xattrMetadata()
		for _, name := range []string{"user.first", "user.second", "user.third", "user.fourth"} {
			if _, ok := meta[encodeXattrKey(name)]; !ok {
				t.Errorf("the rendered metadata has no entry for %s (keys: %v)", name, meta)
			}
		}
	})
}

// TestRemovalRendersATombstoneRatherThanOmittingTheKey is the property the whole tombstone mechanism
// exists for, stated where it is cheap to check.
//
// [types.Backend.SetObjectMetadata] merges the caller's metadata over the object's existing metadata, so
// **omitting a key does not delete it**. A removal that dropped the key from the map would therefore
// render metadata that reads as "change nothing about this attribute", the endpoint would leave the old
// value in place, and the attribute would stay readable forever — while removexattr returned success.
//
// A verifying mutation confirmed this needs its own test: replacing the tombstone with delete(next, name)
// left every other test in this package passing, and only the fuse-layer test that reads back through a
// fresh HEAD caught it. That is the layer where the defect is real, but it should not be the only place
// it is visible, because this is where the decision lives.
func TestRemovalRendersATombstoneRatherThanOmittingTheKey(t *testing.T) {
	t.Parallel()

	a := Attr{Xattrs: map[string][]byte{"user.doomed": []byte("value")}}

	removed, existed := a.WithoutXattr("user.doomed")
	if !existed {
		t.Fatal("WithoutXattr reported the attribute absent")
	}

	// The name must still be in the map, holding nil. An absent key would be a no-op on the wire.
	value, present := removed.Xattrs["user.doomed"]
	if !present {
		t.Fatal("WithoutXattr omitted the key from the attribute set. A metadata replace merges over the " +
			"object's existing metadata, so an omitted key changes nothing and the attribute stays " +
			"readable on the object — after removexattr returned success.")
	}
	if value != nil {
		t.Errorf("the removed attribute holds %q, want a nil tombstone", value)
	}

	// And it must render as a metadata entry, since that is what reaches S3.
	meta := removed.xattrMetadata()
	key := encodeXattrKey("user.doomed")

	stored, ok := meta[key]
	if !ok {
		t.Fatalf("the rendered metadata has no entry for the removed attribute (keys: %v). The removal "+
			"would never reach the object.", meta)
	}
	if _, isRemoved, decoded := decodeXattrValue(stored); !decoded || !isRemoved {
		t.Errorf("the rendered entry %q does not decode as a removal (removed=%v ok=%v)",
			stored, isRemoved, decoded)
	}

	// The round trip closes it: a reader must see the attribute as absent, not as an empty value.
	back := AttrFromMetadata(removed.Metadata(), 0, time.Unix(0, 0).UTC(), "")
	if _, ok := back.Xattr("user.doomed"); ok {
		t.Error("after a metadata round trip the removed attribute reads as present")
	}
}

// TestXattrBudgetIsTheS3LimitLessWhatObjectFSAlreadySpends states the arithmetic, so a change to the
// POSIX key set that quietly shrinks the xattr budget shows up here rather than as a failing PUT.
func TestXattrBudgetIsTheS3LimitLessWhatObjectFSAlreadySpends(t *testing.T) {
	t.Parallel()

	if XattrBudget+reservedMetadataBytes != s3UserMetadataLimit {
		t.Fatalf("budget %d + reserved %d != limit %d", XattrBudget, reservedMetadataBytes,
			s3UserMetadataLimit)
	}
	if XattrBudget <= 0 {
		t.Fatalf("the xattr budget is %d: ObjectFS's own metadata fills the object", XattrBudget)
	}

	// The reservation must actually cover what Metadata() renders for a file with the widest plausible
	// values, or the budget is a number that permits a PUT S3 will reject.
	widest := Attr{
		Mode:  0o777,
		UID:   1<<32 - 1,
		GID:   1<<32 - 1,
		Mtime: widestTime,
	}
	posix := widest.Metadata()
	// The two integrity keys the backend adds, at their widest.
	posix[metaChecksum] = strings.Repeat("0", 64)
	posix[metaOriginalSize] = "9223372036854775807"

	if got := metadataBytes(posix); got > reservedMetadataBytes {
		t.Errorf("ObjectFS's own metadata costs %d bytes at its widest but only %d are reserved, so a "+
			"file at the xattr budget exceeds S3's %d-byte limit", got, reservedMetadataBytes,
			s3UserMetadataLimit)
	}

	t.Logf("S3 limit %d - reserved %d = xattr budget %d bytes",
		s3UserMetadataLimit, reservedMetadataBytes, XattrBudget)
}

// TestWithXattrDistinguishesTooLargeFromNoSpace pins the two-errno split, which is what a caller acts
// on: E2BIG says shrink the value, ENOSPC says remove something else first.
func TestWithXattrDistinguishesTooLargeFromNoSpace(t *testing.T) {
	t.Parallel()

	// One attribute larger than the entire budget.
	if _, err := (Attr{}).WithXattr("user.big", make([]byte, XattrBudget)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an attribute larger than the whole budget returned %v, want ErrTooLarge; the caller "+
			"needs E2BIG to know the value is the problem", err)
	}

	// Two attributes that each fit and together do not. The sizes are derived from the budget rather
	// than written as literals, so a change to the reservation cannot make this case vacuous.
	half := make([]byte, XattrBudget/3)
	a, err := (Attr{}).WithXattr("user.one", half)
	if err != nil {
		t.Fatalf("the first half-budget attribute was refused: %v", err)
	}
	b, err := a.WithXattr("user.two", half)
	if err != nil {
		t.Fatalf("the second half-budget attribute was refused: %v", err)
	}
	if _, err := b.WithXattr("user.three", half); !errors.Is(err, ErrNoSpace) {
		t.Errorf("a third attribute past the budget returned %v, want ErrNoSpace", err)
	}

	// An attribute exactly at the budget is accepted. A boundary that is off by one turns a legal
	// setfattr into ENOSPC, which is the direction that looks like a filesystem bug to a user.
	exact := make([]byte, 0)
	for xattrMetadataBytes(map[string][]byte{"user.x": exact}) < XattrBudget {
		exact = append(exact, 'a')
	}
	if xattrMetadataBytes(map[string][]byte{"user.x": exact}) == XattrBudget {
		if _, err := (Attr{}).WithXattr("user.x", exact); err != nil {
			t.Errorf("an attribute costing exactly the budget was refused: %v", err)
		}
	}

	if _, err := (Attr{}).WithXattr("", []byte("x")); !errors.Is(err, ErrInvalid) {
		t.Errorf("an empty attribute name returned %v, want ErrInvalid", err)
	}
}

// widestTime renders as the longest RFC 3339 nanosecond stamp a metadata value can hold, which is what
// [reservedMetadataBytes] budgets for.
var widestTime = time.Date(9999, 12, 31, 23, 59, 59, 123456789, time.UTC)

// titleCase applies the mangling MinIO applies to a metadata key. strings.Title is deprecated and
// x/text/cases needs a language tag for something the S3 wire format does per ASCII byte, so it is
// spelled out.
func titleCase(s string) string {
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

// TestXattrsSurviveAMetadataRoundTrip is the property every operation above rests on: what
// [Attr.Metadata] renders is what [AttrFromMetadata] reads back.
//
// It runs through the mangling real storage applies — lower-casing, title-casing — because that is
// where a case-sensitive decode would fail and a unit test over the exact map would not.
func TestXattrsSurviveAMetadataRoundTrip(t *testing.T) {
	t.Parallel()

	in := Attr{
		Mode: 0o640,
		UID:  501,
		GID:  20,
		Xattrs: map[string][]byte{
			"user.test":          []byte("hello"),
			"user.Test":          []byte("different attribute, same folded name"),
			"user.binary":        {0, 1, 0xff},
			"user.empty":         {},
			"user.removed":       nil,
			"user.objectfs-mode": []byte("4777"),
		},
	}

	meta := in.Metadata()

	for _, mangle := range []func(string) string{
		func(s string) string { return s },
		strings.ToLower,
		strings.ToUpper,
	} {
		wire := make(map[string]string, len(meta))
		for k, v := range meta {
			wire[mangle(k)] = v
		}

		out := AttrFromMetadata(wire, 0, in.Mtime, "")

		if got, want := len(out.Xattrs), len(in.Xattrs); got != want {
			t.Fatalf("round trip produced %d attributes, want %d: %v", got, want, out.Xattrs)
		}
		for name, want := range in.Xattrs {
			got, ok := out.Xattrs[name]
			if !ok {
				t.Errorf("attribute %q did not survive the round trip", name)
				continue
			}
			if (got == nil) != (want == nil) || !bytes.Equal(got, want) {
				t.Errorf("attribute %q round-tripped as %v, want %v", name, got, want)
			}
		}

		// The POSIX attributes must be unaffected — in particular, user.objectfs-mode above must not
		// have touched the mode.
		if out.Mode != in.Mode {
			t.Errorf("mode is %#o after a round trip carrying a user.objectfs-mode attribute, want %#o. "+
				"An unprivileged setfattr just changed a file's permissions.", out.Mode, in.Mode)
		}
		if out.UID != in.UID || out.GID != in.GID {
			t.Errorf("ownership is %d:%d after the round trip, want %d:%d",
				out.UID, out.GID, in.UID, in.GID)
		}
	}
}

// TestMetadataWarningsReportsUnusableXattrEntries covers the diagnostic path for metadata someone
// wrote by hand.
//
// A malformed entry is skipped rather than failing the read, on the policy AttrFromMetadata states —
// which means the only way anyone learns is this list. An entry silently discarded is the defect
// MetadataWarnings exists to prevent.
func TestMetadataWarningsReportsUnusableXattrEntries(t *testing.T) {
	t.Parallel()

	meta := map[string]string{
		metaXattrPrefix + "not!base32":    "vaGVsbG8",
		encodeXattrKey("user.bad"):        "this is not tagged",
		encodeXattrKey("user.badbase64"):  "v!!!!",
		encodeXattrKey("user.fine"):       encodeXattrValue([]byte("ok")),
		encodeXattrKey("user.tombstoned"): encodeXattrValue(nil),
	}

	warns := MetadataWarnings(meta)
	if len(warns) != 3 {
		t.Fatalf("MetadataWarnings reported %d warnings, want 3: %v", len(warns), warns)
	}

	joined := strings.Join(warns, "\n")
	for _, want := range []string{"not!base32", "user.bad", "user.badbase64"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning mentions %q:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{"user.fine", "user.tombstoned"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("a usable entry (%s) was reported as a warning:\n%s", unwanted, joined)
		}
	}

	// And the usable ones are still read.
	a := AttrFromMetadata(meta, 0, time.Unix(0, 0).UTC(), "")
	if v, ok := a.Xattr("user.fine"); !ok || string(v) != "ok" {
		t.Errorf("a usable attribute was lost alongside the malformed ones: %q, %v", v, ok)
	}
}

// FuzzXattrEncodingRoundTrip is the round-trip property over arbitrary names and values.
//
// The encoding is the whole of this feature's correctness: a name that does not survive means an
// attribute a caller set is unreadable, and a value that does not survive means silent corruption of
// something a user asked to be stored byte-for-byte. Both are exactly the shape a fuzzer finds and a
// table does not — the table below holds the inputs a human thinks of.
func FuzzXattrEncodingRoundTrip(f *testing.F) {
	f.Add("user.test", []byte("hello"))
	f.Add("user.Test", []byte(""))
	f.Add("", []byte{0})
	f.Add("objectfs-mode", []byte("4777"))
	f.Add("user.\x00embedded", []byte{0xff, 0xfe})
	f.Add(strings.Repeat("x", 200), []byte("v"))

	f.Fuzz(func(t *testing.T, name string, value []byte) {
		key := encodeXattrKey(name)

		// The namespace guarantee has to hold for every name, not just plausible ones, because it is
		// what stops a caller reaching objectfs-mode.
		if !strings.HasPrefix(key, metaXattrPrefix) {
			t.Fatalf("name %q encoded to %q, outside the %q namespace", name, key, metaXattrPrefix)
		}
		for _, reserved := range []string{metaMode, metaUID, metaGID, metaMtime, metaChecksum, metaOriginalSize} {
			if strings.EqualFold(key, reserved) {
				t.Fatalf("name %q encoded to the POSIX attribute key %q", name, reserved)
			}
		}

		// A stored key must be a legal HTTP header field name, or the request fails or is mangled.
		for i := range len(key) {
			if c := key[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				t.Fatalf("encoded key %q holds byte %q at %d, which is not a header token character",
					key, c, i)
			}
		}

		// Through the folding S3 applies, in both directions.
		for _, wire := range []string{key, strings.ToUpper(key), strings.ToLower(key)} {
			got, ok := decodeXattrKey(wire)
			if !ok {
				t.Fatalf("decodeXattrKey(%q) rejected a key encoded from %q", wire, name)
			}
			if got != name {
				t.Fatalf("name %q round-tripped through %q as %q", name, wire, got)
			}
		}

		stored := encodeXattrValue(value)
		if stored == xattrRemovedTag && value != nil {
			t.Fatalf("value %v encoded to the tombstone, so the attribute would read as removed", value)
		}
		for i := range len(stored) {
			if c := stored[i]; c < 0x21 || c > 0x7e {
				t.Fatalf("encoded value %q holds byte %q at %d, which is not safe in a header value",
					stored, c, i)
			}
		}

		got, removed, ok := decodeXattrValue(stored)
		if !ok {
			t.Fatalf("decodeXattrValue(%q) rejected a value this package encoded", stored)
		}
		if removed != (value == nil) {
			t.Fatalf("value %v round-tripped with removed=%v", value, removed)
		}
		if !removed && !bytes.Equal(got, value) {
			t.Fatalf("value %v round-tripped as %v", value, got)
		}

		// And through the whole Attr, which is the path a flush and a stat actually take.
		if name == "" || value == nil {
			return
		}
		a, err := (Attr{}).WithXattr(name, value)
		if err != nil {
			// A refusal is a legal outcome for an oversized attribute; what must not happen is a
			// successful set that does not survive.
			return
		}
		back := AttrFromMetadata(a.Metadata(), 0, time.Unix(0, 0).UTC(), "")
		v, present := back.Xattr(name)
		if !present {
			t.Fatalf("attribute %q did not survive Metadata/AttrFromMetadata", name)
		}
		if !bytes.Equal(v, value) {
			t.Fatalf("attribute %q survived as %v, want %v", name, v, value)
		}
	})
}
