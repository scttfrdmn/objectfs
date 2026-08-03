package awsname

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestValidateBucketName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		bucket  string
		wantErr string // a substring of the message, or "" for accept
		why     string
	}{
		{name: "the shape most buckets have", bucket: "research-data"},
		{name: "the minimum length", bucket: "abc"},
		{name: "the maximum length", bucket: strings.Repeat("a", 63)},
		{name: "digits", bucket: "bucket2026"},
		{name: "begins with a digit", bucket: "2026-archive"},
		{
			name:   "dots",
			bucket: "my.bucket.with.dots",
			why: "legal, and AWS recommends against them only because the wildcard certificate for " +
				"*.s3.<region>.amazonaws.com does not match a name with a further dot — buckets " +
				"predating that recommendation exist and must stay mountable",
		},

		// Everything in this block is a name S3's *CreateBucket* rules refuse and that this validator
		// accepts anyway, because the two questions are different: those rules say what can be created
		// today, this says what can be mounted. Each was checked against real S3 in us-west-2 with
		// HeadBucket, where 400 means malformed and 404 or 403 means a well-formed name for a bucket this
		// account cannot see.
		{
			name:   "uppercase",
			bucket: "MyBucket",
			why: "HeadBucket returns 404, not 400 — S3 considers it well-formed. Uppercase names were " +
				"creatable in us-east-1 until 2018 and those buckets still exist, and DNS is " +
				"case-insensitive so virtual-hosted addressing reaches them. v0.10.3 accepted any " +
				"non-empty host, so refusing it would be a silent regression for whoever owns one",
		},
		{
			name:   "underscore",
			bucket: "my_bucket",
			why:    "HeadBucket returns 403 — same story, another legacy us-east-1 shape",
		},
		{
			name:   "leading hyphen",
			bucket: "-research-data",
			why: "not creatable, but nothing about it prevents a request from being addressed, and this " +
				"validator's job is mountability",
		},
		{name: "trailing hyphen", bucket: "research-data-"},
		{name: "two adjacent dots", bucket: "research..data"},
		{
			name:   "the ACE prefix",
			bucket: "xn--research-data",
			why: "reserved for creation because it cannot be told from a punycoded domain; an existing " +
				"bucket named this way is still addressable",
		},
		{name: "the documentation-example prefix", bucket: "amzn-s3-demo-bucket"},
		{
			name:   "not quite an IP address",
			bucket: "999.1.1.1",
			why: "net.ParseIP rejects it, so it is not IP-shaped and S3 allows it; counting dots instead " +
				"of parsing would have refused it",
		},
		{
			name:   "an IP address with a leading zero",
			bucket: "1.2.3.04",
			why:    "not a valid address either — Go's parser refuses the leading zero, so S3 allows the name",
		},
		{
			name:   "contains a reserved suffix without ending with it",
			bucket: "my-s3alias-experiments",
			why: "the rule is a suffix rule, and a substring check would refuse this legal name. The " +
				"case has to contain the suffix exactly as listed — the earlier version of it was " +
				"\"s3alias-experiments\", which does not contain \"-s3alias\" at all, so it passed " +
				"under HasSuffix and under Contains alike and tested nothing",
		},
		{
			name:   "contains the directory-bucket suffix in the middle",
			bucket: "a--x-s3-experiments",
			why:    "same property, for the suffix whose shape is most likely to appear mid-name",
		},

		// And this block is what genuinely cannot work.
		{name: "empty", bucket: "", wantErr: "is empty"},
		{
			name:    "one character",
			bucket:  "b",
			wantErr: "is 1 character; S3 requires 3 to 63",
			why: "HeadBucket returns 400 Bad Request, where a well-formed unknown name returns 404 — so " +
				"S3 itself calls this malformed. The wantErr spans the singular because `s3://b` is what " +
				"someone types while testing, so \"is 1 characters\" is a message an operator actually " +
				"reads — and a message with a grammatical error in it reads as one nobody has looked at",
		},
		{
			name:    "two characters",
			bucket:  "ab",
			wantErr: "is 2 characters; S3 requires 3 to 63",
			why:     "likewise 400, and the plural arm of the same message",
		},
		{
			name:    "too long",
			bucket:  strings.Repeat("a", 64),
			wantErr: "S3 requires 3 to 63",
			why: "63 is the DNS label limit rather than an S3 one: the name is the leftmost label of " +
				"<bucket>.s3.<region>.amazonaws.com. Verified — 63 a's returns 403 and 64 returns 404 " +
				"from the CLI's own parameter validation before the request is sent",
		},
		{
			name:    "a slash",
			bucket:  "research/data",
			wantErr: "cannot appear in a bucket name",
			why: "ends the authority in a URL, so the request would go to host \"research\" — a different " +
				"host, not a failure. Reachable through a caller that split a URI wrongly",
		},
		{
			name:    "a colon",
			bucket:  "research:9000",
			wantErr: "cannot appear in a bucket name",
			why:     "starts a port",
		},
		{
			name:    "an at-sign",
			bucket:  "user@research-data",
			wantErr: "cannot appear in a bucket name",
			why:     "starts userinfo, so the host would be everything after it",
		},
		{
			name:    "a question mark",
			bucket:  "research-data?x=1",
			wantErr: "cannot appear in a bucket name",
		},
		{
			name:    "a space",
			bucket:  "research data",
			wantErr: "cannot appear in a bucket name",
			why:     "breaks the request line; the AWS CLI refuses it in parameter validation before sending",
		},
		{
			name:    "a NUL byte",
			bucket:  "research\x00data",
			wantErr: "cannot appear in a bucket name",
		},
		{
			name:    "a non-ASCII character",
			bucket:  "reseаrch-data", // the 'а' is Cyrillic U+0430
			wantErr: "non-ASCII",
			why: "the case worth its own message: it is invisible. A Cyrillic а and a Latin a render " +
				"identically, so the operator sees a name that looks exactly right",
		},
		{
			name:    "an IP address",
			bucket:  "192.168.5.4",
			wantErr: "formatted as an IP address",
			why:     "the virtual-hosted endpoint would be ambiguous with a literal address",
		},
		{
			name:    "a directory bucket",
			bucket:  "research-data--usw2-az1--x-s3",
			wantErr: `ends with "--x-s3"`,
			why: "Express One Zone. Not a bucket ObjectFS has failed to mount — one with a different API " +
				"surface: no object annotations, a different listing model",
		},
		{
			name:    "an access-point alias",
			bucket:  "my-access-point-s3alias",
			wantErr: `ends with "-s3alias"`,
			why:     "not a bucket at all",
		},
		{
			name:    "a multi-region access point",
			bucket:  "my-mrap.mrap",
			wantErr: `ends with ".mrap"`,
		},
		{name: "an Object Lambda alias", bucket: "my-ol--ol-s3", wantErr: `ends with "--ol-s3"`},
		{name: "an S3 Tables bucket", bucket: "my-tables--table-s3", wantErr: `ends with "--table-s3"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateBucketName(tt.bucket)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateBucketName(%q) = %v, want accepted: %s", tt.bucket, err, tt.why)
				}

				return
			}

			if err == nil {
				t.Fatalf("ValidateBucketName(%q) was accepted; it cannot be mounted, so accepting it here "+
					"moves the failure to whichever API call happens to be first: %s", tt.bucket, tt.why)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ValidateBucketName(%q) = %q, want a message containing %q. %s",
					tt.bucket, err, tt.wantErr, tt.why)
			}
			// Asserted against the %q-escaped form rather than the raw string, which is the point of the
			// escaping: a name containing a NUL byte or a Cyrillic homoglyph renders as `\x00` and
			// `а`, so the operator can see the character that is wrong. Asserting the raw substring
			// would demand the opposite — that the message reproduce an invisible byte invisibly.
			if tt.bucket != "" && !strings.Contains(err.Error(), strconv.Quote(tt.bucket)) {
				t.Errorf("ValidateBucketName(%q) error does not quote the name it rejected, so an "+
					"invisible character in it cannot be seen: %v", tt.bucket, err)
			}
		})
	}
}

// FuzzValidateBucketName asserts the validator is total and that what it accepts is URL-safe.
//
// The second property is the one with teeth. An accepted name is interpolated into the hostname
// `<bucket>.s3.<region>.amazonaws.com` for virtual-hosted addressing, and into the path for path-style,
// so a name containing a slash, a colon, an at-sign or a control byte would not produce a rejected
// request — it would produce a request to a different host, or a malformed one, at a layer with no idea
// the name came from an operator's config file.
func FuzzValidateBucketName(f *testing.F) {
	for _, seed := range []string{
		"", "ab", "abc", "research-data", "my.bucket", "research..data", "-x-", "192.168.0.1",
		"xn--x", "a--x-s3", strings.Repeat("a", 64), "MyBucket", "a_b", "a/b", "a:b", "a@b",
		"a b", "ünicode", "\x00abc", "a.b.c.d", "0.0.0.0", "a%2Fb", "a?b", "a#b", "a[b]",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		if ValidateBucketName(name) != nil {
			return
		}

		if len(name) < bucketNameMin || len(name) > bucketNameMax {
			t.Fatalf("ValidateBucketName(%q) accepted a name of %d characters", name, len(name))
		}

		// A blacklist here and a blacklist in the implementation would be the same list twice, so this
		// asserts the property the list is for: the name has to survive being placed in a URL unchanged.
		// url.Parse of an endpoint built from it must yield back exactly this host.
		if u, err := urlParseHost("https://" + name + ".s3.us-west-2.amazonaws.com/"); err != nil {
			t.Fatalf("ValidateBucketName(%q) accepted a name that makes an unparseable endpoint: %v",
				name, err)
		} else if u != name+".s3.us-west-2.amazonaws.com" {
			t.Fatalf("ValidateBucketName(%q) accepted a name that changes the host it is placed in: "+
				"got %q, want %q — the request would go somewhere else rather than fail",
				name, u, name+".s3.us-west-2.amazonaws.com")
		}
	})
}

// urlParseHost returns the host url.Parse reads out of a URL, for the fuzz property above.
func urlParseHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}

	return u.Host, nil
}
