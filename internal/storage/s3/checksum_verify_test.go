package s3

// wholeObjectResponse decides whether the SHA-256 guard runs, which makes it the gate on the only
// end-to-end integrity check the read path has. It gets its own table test because the header it
// parses comes from S3 rather than from ObjectFS, so no other test in the tree fixes its shape: a
// change in how the header is read would otherwise surface as reads quietly going unverified, which
// is indistinguishable from reads passing.
//
// The asymmetry the cases pin down: false forgoes a check, true asserts a hash must match. So an
// unparseable header must yield false, and every case below that returns false is deliberate rather
// than a limitation to be tightened later.

import (
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/objectfs/objectfs/pkg/errors"
)

func TestWholeObjectResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		contentRange string
		bodyLen      int64
		want         bool
		why          string
	}{
		{
			name:         "no header is a 200",
			contentRange: "",
			bodyLen:      4096,
			want:         true,
			why:          "S3 omits Content-Range on an unranged GET, which returns the entire object",
		},
		{
			name:         "range covering the whole object",
			contentRange: "bytes 0-4095/4096",
			bodyLen:      4096,
			want:         true,
			why: "this is the common full-file read: the caller asked for its buffer size, S3 " +
				"clamped to the object, and every byte is here",
		},
		{
			name:         "single-byte object",
			contentRange: "bytes 0-0/1",
			bodyLen:      1,
			want:         true,
			why:          "off-by-one territory: last == total-1 == 0 and the body is complete",
		},
		{
			name:         "tail fragment",
			contentRange: "bytes 4096-8191/16384",
			bodyLen:      4096,
			want:         false,
			why:          "does not start at zero, so the whole-object hash has nothing to compare against",
		},
		{
			name:         "head fragment",
			contentRange: "bytes 0-4095/16384",
			bodyLen:      4096,
			want:         false,
			why:          "starts at zero but stops short; hashing this would report a valid object as corrupt",
		},
		{
			name:         "unsatisfied range",
			contentRange: "bytes */16384",
			bodyLen:      0,
			want:         false,
			why:          "the 416 form. There is no satisfied range, so nothing was covered",
		},
		{
			name:         "body shorter than the reported total",
			contentRange: "bytes 0-16383/16384",
			bodyLen:      4096,
			want:         false,
			why: "a truncated read. Trusting the header over the body would hash 4096 bytes against " +
				"a 16384-byte object's digest and report corruption for a transfer that merely got cut " +
				"short — the length check is what keeps this a missed verification rather than a false alarm",
		},
		{
			name:         "body longer than the reported total",
			contentRange: "bytes 0-4095/4096",
			bodyLen:      8192,
			want:         false,
			why:          "incoherent response; nothing about it should be trusted enough to gate a check",
		},
		{
			name:         "missing total",
			contentRange: "bytes 0-4095",
			bodyLen:      4096,
			want:         false,
			why:          "no slash, so the object's length is unknown and coverage is unknowable",
		},
		{
			name:         "non-numeric total",
			contentRange: "bytes 0-4095/many",
			bodyLen:      4096,
			want:         false,
			why:          "unparseable rather than absent, but the same conclusion: cannot confirm",
		},
		{
			name:         "empty total",
			contentRange: "bytes 0-4095/",
			bodyLen:      4096,
			want:         false,
			why:          "trailing slash with nothing after it must not parse as zero",
		},
		{
			name:         "garbage",
			contentRange: "not-a-range-at-all",
			bodyLen:      4096,
			want:         false,
			why:          "an unrecognized header means cannot confirm, never assume complete",
		},
		{
			name:         "empty object",
			contentRange: "bytes 0-0/0",
			bodyLen:      0,
			want:         true,
			why: "a zero-length object read in full. The digest of no bytes is a real digest and must " +
				"still be checked",
		},
		{
			name:         "interior whitespace",
			contentRange: "bytes  0-4095 / 4096",
			bodyLen:      4096,
			want:         false,
			why: "RFC 9110's grammar has no interior whitespace and no server sends it, so this is an " +
				"unrecognized header rather than a sloppy one. Erring toward false costs a verification; " +
				"tolerating shapes nobody sends adds routes to a wrong true",
		},
		{
			name:         "surrounding whitespace is trimmed",
			contentRange: " bytes 0-4095/4096 ",
			bodyLen:      4096,
			want:         true,
			why: "the one concession, and free: values arrive through http.Header, which has already " +
				"trimmed them",
		},
		{
			name:         "signed total",
			contentRange: "bytes 0-4095/+4096",
			bodyLen:      4096,
			want:         false,
			why: "complete-length is 1*DIGIT. strconv.ParseInt would accept the sign, and a signed " +
				"total has no meaning against a byte count",
		},
		{
			name:         "missing bytes unit",
			contentRange: "0-4095/4096",
			bodyLen:      4096,
			want:         false,
			why:          "the unit is not optional; a header this shape did not come from S3",
		},
		{
			name:         "total overflows int64",
			contentRange: "bytes 0-0/99999999999999999999",
			bodyLen:      0,
			want:         false,
			why:          "no real object is that long, so nothing here is worth concluding from",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := wholeObjectResponse(tt.contentRange, tt.bodyLen); got != tt.want {
				t.Errorf("wholeObjectResponse(%q, %d) = %v, want %v\n%s",
					tt.contentRange, tt.bodyLen, got, tt.want, tt.why)
			}
		})
	}
}

// TestVerifyChecksumIgnoresMetadataKeyCase covers the seam no end-to-end test can reach.
//
// S3 lower-cases user-metadata keys in transit, so a client cannot control the case the lookup sees —
// which is why the emulator-backed test cannot provoke this and why an earlier attempt to write it
// that way passed against a case-sensitive lookup, vacuously. The case that matters is the one the
// *server* returns, and the SDK preserves it: MinIO title-cases, Ceph and a Go http.Header round-trip
// canonicalize to Objectfs-Sha256. Constructing the map directly is the only way to pin this.
//
// The failure being prevented is the quietest one available. A case-sensitive lookup finds no
// checksum, verifyChecksum treats that as "written by another tool" and returns nil, and every read
// succeeds while nothing is verified. No error, no log line, no failing test — just an integrity
// guarantee that silently stopped applying.
func TestVerifyChecksumIgnoresMetadataKeyCase(t *testing.T) {
	t.Parallel()

	content := []byte("the bytes that were hashed")
	digest := sha256.Sum256(content)
	recorded := hex.EncodeToString(digest[:])
	corrupt := []byte("the bytes that came back!!")

	if len(corrupt) != len(content) {
		t.Fatalf("test setup: corrupt content must be the same length so only the hash can tell them "+
			"apart, got %d and %d", len(corrupt), len(content))
	}

	spellings := []struct {
		key    string
		server string
	}{
		{key: "objectfs-sha256", server: "AWS S3, which lower-cases in transit"},
		{key: "Objectfs-Sha256", server: "a Go http.Header round-trip, which canonicalizes"},
		{key: "OBJECTFS-SHA256", server: "a server that upper-cases"},
		{key: "ObjectFS-SHA256", server: "the spelling a human would write"},
		{key: "objectfs-Sha256", server: "mixed case, no canonical form"},
	}

	for _, sp := range spellings {
		t.Run(sp.key, func(t *testing.T) {
			t.Parallel()

			metadata := map[string]string{sp.key: recorded}

			// Matching content must verify, whatever the key's case.
			if err := verifyChecksum(metadata, content, "key"); err != nil {
				t.Errorf("verifyChecksum rejected content matching its own recorded hash, with the key "+
					"spelled %q (%s): %v", sp.key, sp.server, err)
			}

			// And corrupt content must not.
			err := verifyChecksum(metadata, corrupt, "key")
			if err == nil {
				t.Fatalf("corruption went undetected with the checksum key spelled %q (%s). The lookup "+
					"is case-sensitive, so against that server verifyChecksum finds no checksum, "+
					"concludes the object was written by another tool, and returns nil — reads keep "+
					"succeeding while nothing is verified, with no error and no log line.",
					sp.key, sp.server)
			}

			var objErr *errors.ObjectFSError
			if !stderrors.As(err, &objErr) {
				t.Fatalf("error is unstructured, so no caller can classify it: %v", err)
			}

			if objErr.Code != errors.ErrCodeDataCorruption {
				t.Errorf("error code = %q, want %q", objErr.Code, errors.ErrCodeDataCorruption)
			}
		})
	}
}

// TestVerifyChecksumSkipsUnverifiableObjects pins the two cases where returning nil is correct, so
// that the fail-closed behavior above is not mistaken for "always error when in doubt".
func TestVerifyChecksumSkipsUnverifiableObjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata map[string]string
		why      string
	}{
		{
			name:     "nil metadata",
			metadata: nil,
			why:      "an object written by aws s3 cp, boto3, or any other tool",
		},
		{
			name:     "no checksum key",
			metadata: map[string]string{"author": "someone"},
			why:      "metadata from another tool entirely",
		},
		{
			name:     "empty checksum value",
			metadata: map[string]string{metaChecksum: ""},
			why: "the key exists but records nothing. Treating an empty value as malformed would " +
				"make the object permanently unreadable for no gain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := verifyChecksum(tt.metadata, []byte("anything"), "key"); err != nil {
				t.Errorf("verifyChecksum failed for %s: %v\nRefusing these would make ObjectFS unable "+
					"to read the buckets it exists to mount.", tt.why, err)
			}
		})
	}
}

// FuzzWholeObjectResponse asserts the parser cannot panic and cannot claim whole-object coverage for
// a body whose length disagrees with the total it parsed.
//
// The property is one-directional on purpose. Returning false for a complete object costs a missed
// verification; returning true for an incomplete one turns a legitimate partial read into a
// corruption error, which is a filesystem that refuses to read valid data. Only the second is a bug
// worth failing a fuzz run over.
func FuzzWholeObjectResponse(f *testing.F) {
	f.Add("bytes 0-4095/4096", int64(4096))
	f.Add("bytes */4096", int64(0))
	f.Add("", int64(0))
	f.Add("bytes 0-0/0", int64(0))
	f.Add("bytes 0-9223372036854775807/9223372036854775807", int64(1))
	f.Add("bytes -1--1/-1", int64(-1))
	f.Add("bytes 0-4095/4096/4096", int64(4096))
	f.Add("/", int64(0))

	f.Fuzz(func(t *testing.T, contentRange string, bodyLen int64) {
		got := wholeObjectResponse(contentRange, bodyLen)
		if !got {
			return
		}

		// Claiming coverage commits to hashing bodyLen bytes as a complete object, so a negative
		// length can never be a whole object however the header reads.
		if bodyLen < 0 {
			t.Fatalf("wholeObjectResponse(%q, %d) = true for a negative body length",
				contentRange, bodyLen)
		}

		if contentRange == "" {
			return
		}

		// For a 206 the claim is only defensible if the header's own total equals what was read.
		// Re-derive it independently rather than reusing the parser's arithmetic.
		total, ok := trailingInt(contentRange)
		if !ok {
			t.Fatalf("wholeObjectResponse(%q, %d) = true but the header carries no parseable total",
				contentRange, bodyLen)
		}
		if total != bodyLen {
			t.Fatalf("wholeObjectResponse(%q, %d) = true but the header reports a total of %d; "+
				"a partial body claimed as whole becomes a corruption error on a valid read",
				contentRange, bodyLen, total)
		}
	})
}

// trailingInt parses the digits after the final slash, independently of the parser under test.
//
// Trailing whitespace is trimmed because the parser tolerates it and should: header values arrive
// through http.Header, which trims already, and a parser that rejected " 4096" would stop verifying
// on a server that pads its headers. The fuzzer found this as a disagreement between the two, and the
// oracle was the stricter one.
func trailingInt(s string) (int64, bool) {
	s = strings.TrimSpace(s)

	i := len(s) - 1
	for i >= 0 && s[i] >= '0' && s[i] <= '9' {
		i--
	}

	if i < 0 || s[i] != '/' || i == len(s)-1 {
		return 0, false
	}

	var n int64
	for _, c := range []byte(s[i+1:]) {
		n = n*10 + int64(c-'0')
	}

	return n, true
}
