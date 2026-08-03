package awsname

import (
	"strings"
	"testing"
)

func TestParseStorageURI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		uri        string
		wantBucket string
		wantPrefix string
		wantErr    string // a substring of the message, or "" for accept
		why        string
	}{
		{
			name:       "a bucket",
			uri:        "s3://research-data",
			wantBucket: "research-data",
		},
		{
			name:       "a bucket and a prefix",
			uri:        "s3://research-data/lab/2026",
			wantBucket: "research-data",
			wantPrefix: "lab/2026",
		},
		{
			name:       "a trailing slash is not a prefix",
			uri:        "s3://research-data/",
			wantBucket: "research-data",
			why: "the most common way the URI is written by hand, and it must mount the same thing as " +
				"the form without it rather than a prefix of \"\" that is not empty",
		},
		{
			name:       "doubled slashes collapse",
			uri:        "s3://research-data//lab//",
			wantBucket: "research-data",
			wantPrefix: "lab",
			why: "a prefix is concatenated with object keys, so a leading or trailing slash produces a " +
				"key with an empty path component that no other S3 tool writes — the same mount " +
				"addressed two ways would be two namespaces",
		},
		{
			name:       "dots in the bucket name are legal",
			uri:        "s3://my.bucket.with.dots/x",
			wantBucket: "my.bucket.with.dots",
			wantPrefix: "x",
			why: "AWS recommends against them because the wildcard certificate does not match, but S3 " +
				"accepts them and buckets predating the recommendation exist",
		},
		{
			name:       "a prefix may contain a space",
			uri:        "s3://research-data/my data",
			wantBucket: "research-data",
			wantPrefix: "my data",
			why:        "object keys may contain spaces, so a prefix may too",
		},

		{
			name:    "no scheme",
			uri:     "research-data",
			wantErr: "has no scheme",
			why: "what someone writes who has not seen the URI form; it parses cleanly as a relative " +
				"path, so without an explicit arm it would be reported as an unsupported scheme of \"\"",
		},
		{
			name:    "the suggestion for a bare name is the name with a scheme",
			uri:     "research-data",
			wantErr: "s3://research-data",
			why:     "the message has to contain the line to write, not a description of it",
		},
		{
			name:    "empty",
			uri:     "",
			wantErr: "no storage URI given",
		},
		{
			name:    "whitespace only",
			uri:     "   ",
			wantErr: "no storage URI given",
			why:     "a config file's `uri: \" \"` is an unset URI, not a bucket named with a space",
		},
		{
			name:    "gs",
			uri:     "gs://research-data",
			wantErr: `uses scheme "gs"`,
		},
		{
			name:    "azure",
			uri:     "azure://container-name",
			wantErr: `uses scheme "azure"`,
			why: "cmd/objectfs/doc.go lists this under Future Support; nothing in this build can mount " +
				"it, and accepting it would produce a bucket called \"container-name\"",
		},
		{
			name:    "https",
			uri:     "https://bucket.s3.amazonaws.com",
			wantErr: `uses scheme "https"`,
			why:     "the console URL, which is the other thing people paste",
		},
		{
			name:    "unparseable",
			uri:     "://invalid",
			wantErr: "cannot be parsed",
		},
		{
			name:    "three slashes",
			uri:     "s3:///research-data",
			wantErr: "names no bucket",
			why: "an empty authority to every URI consumer, so it is a missing bucket rather than a " +
				"bucket named research-data",
		},
		{
			name:    "scheme only",
			uri:     "s3://",
			wantErr: "names no bucket",
		},

		// AKIAIOSFODNN7EXAMPLE is AWS's own documentation-example key ID, chosen so that this case looks
		// to a reader exactly like the thing it is guarding against. gosec's G101 flags the shape rather
		// than the value, which is the right default — the exemption is here and not in .golangci.yml so
		// it covers this one literal.
		{ //nolint:gosec // AWS's published example key ID, in a test asserting such a URI is refused
			name:    "credentials in the URI",
			uri:     "s3://AKIAIOSFODNN7EXAMPLE:secret@research-data",
			wantErr: "carries credentials in the URI",
			why: "url.Parse puts these in User, which nothing downstream reads, so accepting it would " +
				"mount the bucket while silently ignoring the credentials the operator believed they " +
				"had supplied — and would have put a secret key in a config file and a journal",
		},
		{
			name:    "a port",
			uri:     "s3://research-data:9000/x",
			wantErr: "specifies port",
			why: "the MinIO habit. url.Parse leaves the port in Host, so without this arm the bucket " +
				"would be \"research-data:9000\" — which ValidateBucketName rejects, but for the wrong " +
				"reason and with a message that does not mention storage.s3.endpoint",
		},
		{
			name:    "a query string",
			uri:     "s3://research-data?versionId=abc123",
			wantErr: "has a query string",
			why:     "silently dropped otherwise, and a versioned read is not a thing a mount can express",
		},
		{
			name:    "a fragment",
			uri:     "s3://research-data#lab",
			wantErr: "has a fragment",
			why:     "someone reaching for a prefix and writing an anchor; dropped silently otherwise",
		},

		// The two legacy shapes, accepted. S3's CreateBucket rules refuse both, and applying those rules
		// here would refuse to mount a bucket that exists: verified against real S3 in us-west-2, where
		// HeadBucket on `MyBucket` returns 404 and on `my_bucket` returns 403 — well-formed names for
		// buckets this account cannot see — while a malformed name returns 400. Both were creatable in
		// us-east-1 until 2018. See ValidateBucketName.
		{
			name:       "uppercase",
			uri:        "s3://Research-Data",
			wantBucket: "Research-Data",
			why: "url.Parse preserves host case, so the name arrives exactly as written and addresses " +
				"the bucket the operator named; DNS is case-insensitive, so virtual-hosted addressing " +
				"reaches it. Lowercasing it here would mount a different bucket",
		},
		{
			name:       "underscore",
			uri:        "s3://research_data",
			wantBucket: "research_data",
			why:        "HeadBucket returns 403, so S3 considers the name well-formed",
		},
		{
			name:    "too short",
			uri:     "s3://ab",
			wantErr: "S3 requires 3 to 63",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseStorageURI(tt.uri)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseStorageURI(%q) was accepted, bucket=%q prefix=%q; it must be "+
						"rejected: %s", tt.uri, got.Bucket, got.Prefix, tt.why)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("ParseStorageURI(%q) = %q, want a message containing %q. %s",
						tt.uri, err, tt.wantErr, tt.why)
				}
				// Every rejection names the value, so an operator editing a YAML line can see which one
				// it was — including when the problem is an invisible character.
				if tt.uri != "" && strings.TrimSpace(tt.uri) != "" &&
					!strings.Contains(err.Error(), tt.uri) {
					t.Errorf("ParseStorageURI(%q) error does not quote the URI it rejected: %v",
						tt.uri, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseStorageURI(%q) = %v, want accepted: %s", tt.uri, err, tt.why)
			}
			if got.Bucket != tt.wantBucket {
				t.Errorf("ParseStorageURI(%q).Bucket = %q, want %q: this is the bucket every object "+
					"key is addressed to. %s", tt.uri, got.Bucket, tt.wantBucket, tt.why)
			}
			if got.Prefix != tt.wantPrefix {
				t.Errorf("ParseStorageURI(%q).Prefix = %q, want %q: this is prepended to every key, "+
					"so a wrong one is a mount of a namespace that does not exist. %s",
					tt.uri, got.Prefix, tt.wantPrefix, tt.why)
			}
		})
	}
}

// TestValidateStorageURIAgreesWithParseStorageURI is the delegation property.
//
// ValidateStorageURI exists so internal/config can ask the yes-or-no question without wanting the
// bucket. If it ever answered differently from the parse, a config file would load with a URI the
// mount then refuses — which is the two-validators shape this package exists to prevent.
func TestValidateStorageURIAgreesWithParseStorageURI(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{
		"s3://research-data", "s3://research-data/lab", "s3://research-data/", "", "   ",
		"research-data", "gs://b", "s3://", "s3:///b", "s3://Research-Data", "s3://b?x=1",
		"s3://b#f", "s3://u:p@b", "s3://b:9000", "://invalid", "s3://ab", "s3://research_data",
	} {
		vErr := ValidateStorageURI(uri)
		_, pErr := ParseStorageURI(uri)

		if (vErr != nil) != (pErr != nil) {
			t.Errorf("ValidateStorageURI(%q) err=%v but ParseStorageURI err=%v", uri, vErr, pErr)
		}
	}
}

// FuzzParseStorageURI asserts the parser is total and that what it accepts, it accepts consistently.
//
// It reads operator configuration and a command-line argument, so a panic here is a mount that dies
// during startup with a stack trace instead of a message naming the setting. The second property is
// the one worth fuzzing for: an accepted URI must yield a bucket that ValidateBucketName itself
// accepts, and a prefix with no leading or trailing slash. Both are relied on downstream — the bucket
// goes into an endpoint hostname, the prefix is concatenated with object keys — and neither is
// re-checked there.
func FuzzParseStorageURI(f *testing.F) {
	for _, seed := range []string{
		"s3://bucket", "s3://bucket/prefix", "s3://bucket//p//", "s3://", "s3:///b", "",
		"s3://u:p@b/x", "s3://b:9000", "s3://b?q=1", "s3://b#f", "s3://B", "s3://b_c",
		"s3://192.168.0.1", "s3://xn--bucket", "s3://bucket--x-s3", "gs://b", "://x",
		"s3://b/\x00", "s3://ünicode", "s3://b/%2F%2F", "s3://b/.././x", "s3://" + strings.Repeat("b", 64),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseStorageURI(s)
		if err != nil {
			if got.Bucket != "" || got.Prefix != "" {
				t.Errorf("ParseStorageURI(%q) returned bucket=%q prefix=%q alongside an error; a "+
					"caller that logs the error and carries on would mount them", s, got.Bucket, got.Prefix)
			}

			return
		}

		if err := ValidateBucketName(got.Bucket); err != nil {
			t.Errorf("ParseStorageURI(%q) accepted, yielding bucket %q that ValidateBucketName "+
				"rejects: %v — the bucket becomes part of an endpoint hostname", s, got.Bucket, err)
		}

		if strings.HasPrefix(got.Prefix, "/") || strings.HasSuffix(got.Prefix, "/") {
			t.Errorf("ParseStorageURI(%q).Prefix = %q has a boundary slash; it is concatenated with "+
				"object keys, so this produces keys with an empty path component", s, got.Prefix)
		}
	})
}
