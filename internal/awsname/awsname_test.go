package awsname

import (
	"fmt"
	"os"
	"path/filepath"
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
		// Long inputs are discarded rather than validated, and that is about the fuzzer rather than
		// about ValidateRegion.
		//
		// This target runs at roughly 700,000 executions per second, because the function under test is
		// a length check and one RE2 match. At that rate a 60-second run in CI mutates its way to a
		// corpus of a couple of hundred entries, almost all of them long strings that differ only past
		// the length limit and so tell the validator nothing it did not already know at byte 64 — and
		// every one has to be minimized and written out when -fuzztime expires. On a shared runner that
		// shutdown overran its grace period and the job failed with "context deadline exceeded" and no
		// counterexample, which reads exactly like a hang in the code and is not one.
		//
		// t.Skip keeps the input out of the corpus instead of counting it as interesting. The length
		// path stays covered by the seed corpus, which carries a 64-character region and is replayed by
		// every ordinary `go test` run — a bound the fuzzer cannot erode.
		if len(region) > maxRegionLength*2 {
			t.Skip("beyond the length limit by a margin; see the comment above")
		}

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

// TestRegionIsResolvable pins the check that closed the seam FuzzConfigConstructsBackend found.
//
// It uses t.Setenv rather than a table of pure inputs because the function's answer depends on the
// environment by design — that is what it is for. t.Setenv also makes these cases un-parallelizable,
// which is why this test does not call t.Parallel: the environment is process-global, so two of these
// running concurrently would read each other's variables.
//
// AWS_CONFIG_FILE is pointed at a real file for the shared-config case and at a path that does not
// exist for the negative cases. Without that, the result would depend on whether the machine running
// the test happens to have ~/.aws/config — which is exactly the environment-dependence being fixed,
// reintroduced in the test for it.
func TestRegionIsResolvable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-config")

	t.Run("an explicit region needs nothing from the environment", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")
		t.Setenv("AWS_CONFIG_FILE", missing)

		if !RegionIsResolvable("us-west-2") {
			t.Error("an explicitly configured region must be resolvable regardless of environment")
		}
	})

	t.Run("no region and nothing to resolve it from", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")
		t.Setenv("AWS_CONFIG_FILE", missing)

		if RegionIsResolvable("") {
			t.Error("an empty region with no environment source must not be reported resolvable: " +
				"this is the case that reached a HeadBucket health check and failed there")
		}
	})

	for _, env := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
		t.Run("resolved from "+env, func(t *testing.T) {
			t.Setenv("AWS_REGION", "")
			t.Setenv("AWS_DEFAULT_REGION", "")
			t.Setenv("AWS_CONFIG_FILE", missing)
			t.Setenv(env, "eu-central-1")

			if !RegionIsResolvable("") {
				t.Errorf("an empty region must be resolvable when %s is set", env)
			}
		})
	}

	t.Run("a non-empty shared config file is taken at its word", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(path, []byte("[default]\nregion = us-west-2\n"), 0o600); err != nil {
			t.Fatalf("writing the config fixture: %v", err)
		}

		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")
		t.Setenv("AWS_CONFIG_FILE", path)

		if !RegionIsResolvable("") {
			t.Error("a shared config file that exists must be treated as a source, since the SDK " +
				"reads it and this package deliberately does not parse it")
		}
	})

	t.Run("an empty shared config file is not a source", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("writing the empty config fixture: %v", err)
		}

		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")
		t.Setenv("AWS_CONFIG_FILE", path)

		if RegionIsResolvable("") {
			t.Error("a zero-length config file supplies no region, so it must not count as a source")
		}
	})
}
