// Copyright 2025-2026 Scott Friedman. Licensed under the Apache License 2.0.

package main

// No `import "C"` in this file, deliberately. `go test` rejects it —
// "use of cgo in test main_test.go not supported" — so a test in a cgo package cannot name C.int or
// call C.malloc, even though the package under test is built with cgo. The two places that would have
// wanted it are handled without it: the return codes are pinned by parsing objectfs_types.h (which is
// a stronger check than C.OBJECTFS_OK anyway — it catches a macro whose value changed, whereas
// comparing C.OBJECTFS_OK to C.OBJECTFS_OK cannot), and fillCStr is given a Go byte array, which cgo
// permits passing to C for the duration of a call because it contains no Go pointers.

import (
	"errors"
	"fmt"
	"math"

	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/scttfrdmn/objectfs/pkg/utils"
	objectfssdk "github.com/scttfrdmn/objectfs/sdks/go/objectfs"
)

// The Go side of this SDK had no Go tests at all, only tests/test_basic.c and tests/test_smoke.py,
// which reach it through the shared library and so cannot see a helper that never crosses the C
// boundary. bytesToCacheString was one such helper, and it was silently discarding up to half of the
// requested cache size.
//
// These are in package main so they can call the unexported helpers directly, and they run under
// `go test -race ./...` — which the `test` job already invokes across the whole module, so unlike
// `make test` in this directory, this file is gated today.

// The return codes objectfs_types.h defines. Written out here rather than read from the header, so
// that TestReturnCodesMatchTheHeader below has two independent statements to compare; a constant that
// derives from the header cannot detect the header changing.
const (
	wantOK          = 0
	wantErrInvalid  = -1
	wantErrNotFound = -2
	wantErrAccess   = -3
	wantErrIO       = -4
	wantErrNotMnt   = -5
	wantErrMounted  = -6
	wantErrInternal = -9
)

// TestCacheSizeSurvivesTheRoundTrip is the one that catches the truncation.
//
// objectfs.h documents cache_bytes as "memory cache size in bytes". objectfs_new renders it with
// bytesToCacheString and hands the string to WithCacheSize, which lands in Performance.CacheSize and
// is read back by utils.ParseBytes. So the contract is: whatever number of bytes C passed in must be
// the number of bytes that comes out the far end.
func TestCacheSizeSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)

	cases := []struct {
		name  string
		bytes int64
	}{
		// The sizes the old integer-division form lost bytes on. 1.5 GiB became "1GB" and lost
		// 536870912 bytes; 2047 MiB became "1GB" and lost 1072693248.
		{"one and a half GiB", 3 * gib / 2},
		{"just under two GiB", 2047 * mib},
		{"one MiB plus one byte", mib + 1},

		// Exact multiples, which the old form happened to get right. Kept so a future change that
		// breaks them is caught too.
		{"one hundred MiB", 100 * mib},
		{"one GiB", gib},

		// Below the old form's smallest unit, where it already emitted a bare count.
		{"just under one MiB", 1023 * kib},
		{"one page", 4096},
		{"one byte", 1},

		// Zero never reaches here — objectfs_new only calls this when cacheBytes > 0 — but the
		// helper should not invent a size if that guard is ever relaxed.
		{"zero", 0},

		// The top of the exactly-representable range. utils.ParseBytes parses through float64, so a
		// byte count above 2^53 can land on a neighboring value — measured: 2^53+1 comes back as
		// 2^53, and math.MaxInt64/2 comes back one byte high. That imprecision is in ParseBytes and
		// applies to every caller of it, not to this SDK; it is out of reach here because 2^53 bytes
		// is 8 PiB of memory cache. Asserting exactness up to the boundary and not past it says which
		// range is guaranteed, rather than pretending a limit that exists does not.
		{"one PiB", 1 << 50},
		{"two to the fifty-three, the exact-integer limit of the float64 ParseBytes uses", 1 << 53},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spelling := bytesToCacheString(tc.bytes)
			got, err := utils.ParseBytes(spelling)
			if err != nil {
				t.Fatalf("bytesToCacheString(%d) = %q, which ParseBytes rejects: %v",
					tc.bytes, spelling, err)
			}
			if got != tc.bytes {
				t.Errorf("cache size does not survive the round trip: asked for %d bytes, "+
					"rendered as %q, parsed back as %d (lost %d)",
					tc.bytes, spelling, got, tc.bytes-got)
			}
		})
	}
}

// TestCodeFromErrMapsEverySentinel pins the mapping objectfs_types.h documents.
//
// Each constant there states which condition it means, and a caller switching on the return value has
// nothing else to go on. The sentinels are *ObjectFSError values whose Is compares Code, so a wrapped
// error still matches — the wrapped cases assert that, since every real error from the Go SDK arrives
// wrapped.
func TestCodeFromErrMapsEverySentinel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is OK", nil, wantOK},
		{"not found", objectfssdk.ErrNotFound, wantErrNotFound},
		{"access denied", objectfssdk.ErrAccessDenied, wantErrAccess},
		{"not mounted", objectfssdk.ErrNotMounted, wantErrNotMnt},
		{"already mounted", objectfssdk.ErrAlreadyMounted, wantErrMounted},
		{"invalid config", objectfssdk.ErrInvalidConfig, wantErrInvalid},

		// Anything unrecognized is I/O, which is the documented catch-all.
		{"an unrecognized error is I/O", errors.New("something else"), wantErrIO},

		// Wrapped, because that is the shape that actually arrives: the Go SDK's Unmount wraps with
		// fmt.Errorf("failed to stop adapter: %w", err), and the backend wraps throughout. If these
		// stopped matching, every error C sees would collapse to OBJECTFS_ERR_IO.
		{"wrapped not found", wrap(objectfssdk.ErrNotFound), wantErrNotFound},
		{"wrapped access denied", wrap(objectfssdk.ErrAccessDenied), wantErrAccess},
		{"twice-wrapped not mounted", wrap(wrap(objectfssdk.ErrNotMounted)), wantErrNotMnt},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := int(codeFromErr(tc.err)); got != tc.want {
				t.Errorf("codeFromErr(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func wrap(err error) error {
	return fmt.Errorf("outer context: %w", err)
}

// TestReturnCodesMatchTheHeader reads objectfs_types.h and checks the macros against the constants
// this file asserts against.
//
// The header is the API: a C caller compares against OBJECTFS_ERR_NOT_FOUND, and if that macro's
// value ever diverges from what codeFromErr returns, every test above still passes while every C
// program mis-reads every error. Nothing else in the repository compares the two — the C test binary
// uses the macros on both sides of its assertions, so it agrees with the header by construction.
func TestReturnCodesMatchTheHeader(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("objectfs_types.h")
	if err != nil {
		t.Fatalf("reading the header: %v", err)
	}

	re := regexp.MustCompile(`(?m)^#define\s+(OBJECTFS_\w+)\s+(-?\d+)`)
	found := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("macro %s has a non-numeric value %q", m[1], m[2])
		}
		found[m[1]] = n
	}

	want := map[string]int{
		"OBJECTFS_OK":              wantOK,
		"OBJECTFS_ERR_INVALID":     wantErrInvalid,
		"OBJECTFS_ERR_NOT_FOUND":   wantErrNotFound,
		"OBJECTFS_ERR_ACCESS":      wantErrAccess,
		"OBJECTFS_ERR_IO":          wantErrIO,
		"OBJECTFS_ERR_NOT_MOUNTED": wantErrNotMnt,
		"OBJECTFS_ERR_MOUNTED":     wantErrMounted,
		"OBJECTFS_ERR_INTERNAL":    wantErrInternal,
	}

	for name, wantVal := range want {
		gotVal, ok := found[name]
		if !ok {
			t.Errorf("objectfs_types.h no longer defines %s", name)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("%s is %d in objectfs_types.h but %d in the Go tests; a C caller and this "+
				"library now disagree about what that code means", name, gotVal, wantVal)
		}
	}

	// The other direction: a code added to the header that nothing in Go returns is a code no
	// caller can ever see, and one that collides with an existing value is worse.
	for name := range found {
		if _, ok := want[name]; !ok {
			t.Errorf("objectfs_types.h defines %s = %d, which no Go code returns and no test "+
				"covers", name, found[name])
		}
	}
}

// TestInfoFieldsHoldTheirLongestLegalValue checks the char array widths in objectfs_types.h against
// the longest value each field can actually receive.
//
// A NUL-terminated string in a char[N] carries N-1 bytes, so a field sized to exactly the protocol
// maximum silently truncates a maximum-length value. key was char[1024] against an S3 key limit of
// 1024 bytes, so a legal 1024-byte key came back to the caller one byte short — and in
// objectfs_list, where the key is the result rather than an input, a truncated key names a different
// object or none at all, with nothing to signal it. Passing it back to objectfs_get or
// objectfs_delete would then act on the wrong key.
//
// Asserted against the header text rather than unsafe.Sizeof because this file cannot use cgo (see
// the note at the top of this file), and because the header is what a C caller compiles against.
func TestInfoFieldsHoldTheirLongestLegalValue(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("objectfs_types.h")
	if err != nil {
		t.Fatalf("reading the header: %v", err)
	}

	fields := map[string]int{}
	re := regexp.MustCompile(`(?m)^\s*char\s+(\w+)\[(\d+)\];`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("field %s has a non-numeric width %q", m[1], m[2])
		}
		fields[m[1]] = n
	}

	cases := []struct {
		field string
		// longest is the longest value the field must hold, in bytes, not counting a terminator.
		longest int
		why     string
	}{
		{"key", 1024, "an S3 object key may be up to 1024 bytes when UTF-8 encoded"},
		{"etag", 126, "an ETag is a 32-char hex digest, or 32+1+5 for a multipart one, plus the two " +
			"quote characters S3 includes; 126 leaves room for a long multipart suffix"},
		{"content_type", 127, "a MIME type with parameters; no hard protocol limit, so this is a " +
			"chosen bound and the number here records it"},
	}

	for _, tc := range cases {
		gotWidth, ok := fields[tc.field]
		if !ok {
			t.Errorf("objectfs_types.h no longer declares a char array named %s", tc.field)
			continue
		}
		if capacity := gotWidth - 1; capacity < tc.longest {
			t.Errorf("objectfs_info_t.%s is char[%d], which holds %d bytes plus a terminator, but "+
				"must hold %d: %s. A maximum-length value is truncated with no error returned.",
				tc.field, gotWidth, capacity, tc.longest, tc.why)
		}
	}
}

// TestFillCStrTruncatesAtCapacity covers the bound that keeps a long key from running off the end of
// objectfs_info_t.key.
//
// A 1024-byte array holding a NUL-terminated string can carry 1023 bytes. S3 keys go to 1024
// characters, so the boundary is reachable with a legal key, not only a hostile one — and the
// consequence of getting it wrong is a write one byte past a struct field, which is the kind of bug
// that surfaces as corruption somewhere else entirely.
func TestFillCStrTruncatesAtCapacity(t *testing.T) {
	t.Parallel()

	const (
		capacity = 16
		guard    = 8
	)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"shorter than capacity", "abc", "abc"},
		{"exactly capacity minus one", "123456789012345", "123456789012345"},
		{"exactly capacity", "1234567890123456", "123456789012345"},
		{"longer than capacity", "1234567890123456789", "123456789012345"},

		// Truncation is by byte, not by rune, so an over-long multibyte key is cut mid-character and
		// the last byte of the result is half of one. That is correct for this boundary — S3 counts a
		// key's length in UTF-8 bytes, and the array is bytes — and truncating to the preceding rune
		// would produce a key that is neither the original nor a valid prefix of it in bytes. Pinned
		// so it is a decision rather than an accident: 9 two-byte runes is 18 bytes, cut at 15.
		{"multibyte, cut mid-rune at the byte bound", "ααααααααα", "ααααααα\xce"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			buf := make([]byte, capacity+guard)
			for i := range buf {
				buf[i] = 0xAA
			}

			// A Go byte slice, not C.malloc: cgo is not available in this file (see the note at the
			// top), and passing a pointer to Go memory containing no Go pointers into a C call is
			// permitted for the duration of that call, which is all fillCStr needs.
			//
			//nolint:gosec // G103: the unsafe.Pointer is the parameter type of the function under
			// test, whose whole job is to write into a C char array. A test for a bound on an
			// unsafe write cannot avoid taking the pointer; the guard bytes checked below are what
			// make the write safe to perform here.
			fillCStr(unsafe.Pointer(&buf[0]), tc.in, capacity)

			if got := goString(buf[:capacity]); got != tc.want {
				t.Errorf("fillCStr(%q, capacity=%d) = %q, want %q", tc.in, capacity, got, tc.want)
			}
			for i := capacity; i < len(buf); i++ {
				if buf[i] != 0xAA {
					t.Fatalf("fillCStr wrote past its capacity: byte %d of the guard is %#x, "+
						"not 0xAA", i-capacity, buf[i])
				}
			}
		})
	}
}

// TestFillCStrRejectsNonPositiveCapacity holds the guard that stops a zero-sized field from being
// NUL-terminated at offset zero of memory that does not belong to it.
func TestFillCStrRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int{0, -1} {
		buf := []byte{0xAA, 0xAA, 0xAA, 0xAA}
		//nolint:gosec // G103: see the note in TestFillCStrTruncatesAtCapacity. This case asserts
		// that fillCStr writes nothing at all, so every byte of buf is a guard byte.
		fillCStr(unsafe.Pointer(&buf[0]), "data", capacity)
		for i, b := range buf {
			if b != 0xAA {
				t.Errorf("fillCStr with capacity=%d wrote %#x at byte %d; it must write nothing",
					capacity, b, i)
			}
		}
	}
}

// goString reads a NUL-terminated string out of a byte slice.
func goString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// TestPutLengthErrorAtTheBoundary covers the guard added for #200, which is the one real defect the
// G115 audit of this file found.
//
// objectfs_put took a size_t and handed C.int(length) to C.GoBytes, narrowing 64 bits to 32. The
// consequences were measured in a standalone cgo probe rather than reasoned about, because they are
// not uniform — the same narrowing fails three different ways:
//
//	length = 1<<32     (4 GiB)  -> C.int 0            -> len(goData) == 0, OBJECTFS_OK, empty object
//	length = (1<<32)+100        -> C.int 100          -> len(goData) == 100, a 100-byte object
//	length = 1<<31     (2 GiB)  -> C.int -2147483648  -> panic: gobytes: length out of range
//
// The first is the worst and is exactly what the issue was concerned about: the caller gets a success
// code for an object S3 holds as empty, and nothing anywhere reports a short write, because from Go's
// side the length arrived as zero and there was no short write. The third is worse in a shared
// library than it would be in a program: a panic in a c-shared build tears down the host process, so
// a C caller loses unrelated state of its own to one bad argument.
//
// This test asserts the boundary rather than the narrowing itself. The narrowing is unreachable now
// and cannot be provoked from Go — see the header comment on why this package's tests cannot
// `import "C"` — so tests/test_basic.c carries the other half: that a C caller passing an oversized
// length gets OBJECTFS_ERR_INVALID and a message, not OBJECTFS_OK.
func TestPutLengthErrorAtTheBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		length  uint64
		wantErr bool
		why     string
	}{
		{name: "zero", length: 0},
		{name: "one", length: 1},
		{name: "a typical object", length: 4 << 20},
		{
			name: "exactly maxPutLength", length: maxPutLength,
			why: "the boundary itself is representable as a C.int and must stay usable; the guard is " +
				"`>` and not `>=` for this case",
		},
		{
			name: "maxPutLength plus one", length: maxPutLength + 1, wantErr: true,
			why: "one past what C.int can hold; C.GoBytes would see a negative length and panic",
		},
		{
			name: "2 GiB", length: 1 << 31, wantErr: true,
			why: "measured: C.int -2147483648, panic: gobytes: length out of range",
		},
		{
			name: "4 GiB", length: 1 << 32, wantErr: true,
			why: "measured: C.int 0 — the silent-empty-object case, the reason this guard exists",
		},
		{
			name: "4 GiB plus 100", length: (1 << 32) + 100, wantErr: true,
			why: "measured: C.int 100 — a partial write reported as complete",
		},
		{
			name: "MaxUint64", length: math.MaxUint64, wantErr: true,
			why: "size_t's own maximum, which is what an underflowed length subtraction produces",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := putLengthError(tt.length)

			if tt.wantErr && err == nil {
				t.Fatalf("putLengthError(%d) = nil, want an error%s", tt.length, because(tt.why))
			}

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("putLengthError(%d) = %v, want nil%s", tt.length, err, because(tt.why))
				}

				return
			}

			// The message has to name the offending length and the limit. A C caller's only channel for
			// this is objectfs_last_error, so a message that says merely "invalid length" tells it
			// nothing it can act on — it cannot see either number otherwise.
			msg := err.Error()

			for _, want := range []string{
				strconv.FormatUint(tt.length, 10),
				strconv.FormatUint(uint64(maxPutLength), 10),
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("putLengthError(%d) message %q does not contain %q; objectfs_last_error is "+
						"the caller's only view of this, so both the value and the limit have to appear "+
						"in it", tt.length, msg, want)
				}
			}
		})
	}
}

// TestListLimitErrorRejectsNegative covers objectfs_list's limit.
//
// The conversion here was never unsafe — C.int is 32 bits and Go's int is at least that on every
// target this library builds for, so int(limit) widens or matches. The *value* was: objectfs.h
// documents limit as "max results (0 = no limit, up to S3 page maximum)" and says nothing about a
// negative one, and downstream ListObjects gates every use of limit on `limit > 0`. So -1 meant "no
// limit" — which is what -1 conventionally means in a good deal of C API design, and would therefore
// have looked like it worked, but by accident rather than by contract. Rejecting it says which it is.
func TestListLimitErrorRejectsNegative(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		limit   int64
		wantErr bool
		why     string
	}{
		{name: "zero means no limit", limit: 0, why: "documented in objectfs.h as 0 = no limit"},
		{name: "one", limit: 1},
		{name: "a page", limit: 1000},
		{name: "MaxInt32", limit: math.MaxInt32, why: "the largest C.int, so the largest reachable limit"},
		{
			name: "negative one", limit: -1, wantErr: true,
			why: "the case that silently meant 'no limit': ListObjects gates on limit > 0",
		},
		{name: "MinInt32", limit: math.MinInt32, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := listLimitError(tt.limit)

			switch {
			case tt.wantErr && err == nil:
				t.Fatalf("listLimitError(%d) = nil, want an error%s", tt.limit, because(tt.why))
			case !tt.wantErr && err != nil:
				t.Fatalf("listLimitError(%d) = %v, want nil%s", tt.limit, err, because(tt.why))
			case tt.wantErr && !strings.Contains(err.Error(), strconv.FormatInt(tt.limit, 10)):
				t.Errorf("listLimitError(%d) message %q does not name the limit it rejected",
					tt.limit, err.Error())
			}
		})
	}
}

// because renders a case's rationale into a failure message, or nothing when there is none.
func because(why string) string {
	if why == "" {
		return ""
	}

	return " — " + why
}
