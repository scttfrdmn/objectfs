package awsname

import (
	"fmt"
	"strings"
	"testing"
)

// TestValidateRegion pins the contract in literals.
//
// Every rejected case here was verified against real S3 in us-west-2 or against the substrate
// emulator before the check existed, and each failed somewhere with no mention of the region: 400
// from S3 for the uppercase form, an endpoint-rule error inside the SDK resolver for the one with a
// space, and a retry exhaustion for the one with a newline. The accepted cases include every AWS
// partition whose naming is easy to leave out of a pattern.
func TestValidateRegion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		region  string
		wantErr bool
		why     string
	}{
		{
			name:   "empty resolves from the environment",
			region: "",
			why:    "the default chain — AWS_REGION, shared config, IMDS — which is correct on EC2",
		},
		{name: "commercial", region: "us-west-2"},
		{name: "europe", region: "eu-central-1"},
		{name: "a four-part region", region: "ap-southeast-4", why: "later regions add a segment"},
		{
			name:   "govcloud",
			region: "us-gov-west-1",
			why:    "a partition a pattern written only against us-east-1 would reject",
		},
		{name: "china", region: "cn-north-1"},
		{
			name:   "isolated",
			region: "us-iso-east-1",
			why:    "the air-gapped partitions are still syntactically ordinary",
		},

		{
			name:    "uppercase",
			region:  "US-WEST-2",
			wantErr: true,
			why:     "real S3 answers 400 and names nothing; the SDK does not normalize case",
		},
		{
			name:    "an embedded space",
			region:  "us west 2",
			wantErr: true,
			why:     "real S3: \"resolve auth scheme: resolve endpoint: endpoint rule error\"",
		},
		{
			name:    "a trailing space",
			region:  "us-west-2 ",
			wantErr: true,
			why:     "the shape a YAML value picks up by accident",
		},
		{
			name:    "an embedded newline",
			region:  "us-west-2\n",
			wantErr: true,
			why:     "found by FuzzConfigConstructsBackend; exhausted retries rather than failing fast",
		},
		{
			name:    "a slash",
			region:  "a/b",
			wantErr: true,
			why:     "templated into the endpoint host, injecting a path segment into a hostname",
		},
		{
			name:    "a colon",
			region:  "a:b",
			wantErr: true,
			why:     "constructs a client and reaches the wrong port",
		},
		{
			name:    "a leading hyphen",
			region:  "-us-west-2",
			wantErr: true,
			why:     "not a valid DNS label, so it can never resolve",
		},
		{
			name:    "a trailing hyphen",
			region:  "us-west-2-",
			wantErr: true,
		},
		{
			name:    "a doubled hyphen",
			region:  "us--west-2",
			wantErr: true,
			why:     "no AWS region has one, and it is a plausible typo",
		},
		{
			name:    "an underscore",
			region:  "us_west_2",
			wantErr: true,
			why:     "the most likely single-character mistake, and invalid in a DNS label",
		},
		{
			name:    "over the DNS label limit",
			region:  strings.Repeat("a", 64),
			wantErr: true,
			why:     "cannot fit in the label an S3 endpoint puts it in",
		},
		{
			name:   "exactly at the DNS label limit",
			region: strings.Repeat("a", 63),
			why:    "the boundary is inclusive; a valid label of any length must be accepted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateRegion(tc.region)

			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("ValidateRegion(%q) accepted it; it must not. %s", tc.region, tc.why)
			case !tc.wantErr && err != nil:
				t.Fatalf("ValidateRegion(%q) rejected a valid region: %v. %s", tc.region, err, tc.why)
			}

			// The message has to quote the value with %q, not just wrap it in quote marks. An
			// operator reading a log line needs to see which of several regions in a config file is
			// the bad one, and the trailing space and the newline above are visible only when
			// escaped — %q renders the newline as \n, where the raw byte would end the log line and
			// leave the message looking like it named nothing.
			if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("%q", tc.region)) {
				t.Errorf("the error does not %%q-quote the offending value, so an invisible "+
					"character in it cannot be seen: %v", err)
			}
		})
	}
}

// FuzzValidateRegion asserts the validator is total and self-consistent.
//
// Two properties, neither of which the table above can establish. It must not panic on any string —
// it runs on operator-supplied configuration, and a panic in a validator is a denial of service on
// the mount. And what it accepts must be safe to template into a hostname, which is the whole reason
// it exists: accepting something with a slash, a space, or a control character would mean the check
// passed and the request still went somewhere unintended.
func FuzzValidateRegion(f *testing.F) {
	for _, seed := range []string{
		"", "us-west-2", "US-WEST-2", "us west 2", "a/b", "a:b", "us-west-2\n",
		"-a", "a-", "a--b", "a_b", strings.Repeat("a", 64), "ünïcode", "0", "\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, region string) {
		if err := ValidateRegion(region); err != nil {
			return
		}

		// Accepted. It must now be safe in a hostname.
		if len(region) > maxRegionLength {
			t.Fatalf("accepted a %d-character region, over the %d-character DNS label limit",
				len(region), maxRegionLength)
		}

		for i, r := range region {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			default:
				t.Fatalf("accepted region %q, whose byte %d (%q) cannot appear in a DNS label — "+
					"it would be templated into an S3 endpoint host", region, i, r)
			}
		}
	})
}
