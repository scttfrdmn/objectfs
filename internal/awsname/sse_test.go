package awsname

import (
	"slices"
	"strings"
	"testing"
)

// TestValidateSSEMode pins the contract in literals.
//
// The rejected cases are the ones someone actually writes. `SSE-KMS` is what a hand reaching for
// AWS's own prose produces, and it is the likely typo in the opposite direction from storage classes:
// two lines above this key in the same file sits STANDARD_IA in capitals. `aws:kms` is the header
// value rather than ObjectFS's spelling of it, which is what a reader of the S3 API reference writes.
func TestValidateSSEMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mode    string
		wantErr bool
		why     string
	}{
		{
			name: "empty means off",
			mode: "",
			why: "the shipped default, and it must stay valid: a config file that omits the block " +
				"entirely leaves this field zero, and that file has to load",
		},
		{name: "off", mode: SSEModeOff},
		{name: "sse-s3", mode: SSEModeS3},
		{name: "sse-kms", mode: SSEModeKMS},

		{
			name:    "upper case",
			mode:    "SSE-KMS",
			wantErr: true,
			why:     "AWS's prose spelling, and the case the separate branch exists to explain",
		},
		{
			name:    "off in capitals",
			mode:    "OFF",
			wantErr: true,
		},
		{
			name:    "the header value",
			mode:    "aws:kms",
			wantErr: true,
			why:     "what the S3 API reference calls it; ObjectFS names modes, not header values",
		},
		{
			name:    "the other header value",
			mode:    "AES256",
			wantErr: true,
			why:     "the SSE-S3 header value, for the same reason",
		},
		{
			name:    "the mode that is not implemented",
			mode:    "sse-c",
			wantErr: true,
			why: "customer-provided keys need the key on every request including every read, which " +
				"ObjectFS has nowhere to hold; accepting the name would promise it",
		},
		{
			name:    "a trailing space",
			mode:    "sse-kms ",
			wantErr: true,
			why:     "what a YAML value picks up by accident, and invisible in the file",
		},
		{
			name:    "the boolean this replaced",
			mode:    "true",
			wantErr: true,
			why: "security.encryption.at_rest was a boolean defaulting true and read by nothing " +
				"(audit finding P-7); someone converting a file by hand writes its value here",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateSSEMode(tc.mode)

			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("ValidateSSEMode(%q) accepted it; it must not. %s", tc.mode, tc.why)
			case !tc.wantErr && err != nil:
				t.Fatalf("ValidateSSEMode(%q) rejected a mode the backend can request: %v. %s",
					tc.mode, err, tc.why)
			}

			if err == nil {
				return
			}

			// Same reasoning as ValidateStorageClass's: the operator has to be able to see which
			// value in the file is the bad one, and the trailing-space case is visible only escaped.
			if !strings.Contains(err.Error(), `"`+tc.mode+`"`) {
				t.Errorf("the error does not %%q-quote the offending value, so an invisible "+
					"character in it cannot be seen: %v", err)
			}
		})
	}
}

// TestValidateSSEModeNamesTheFixForACaseError asserts the case branch says what to write.
//
// Separate from the table because it is a property of the message, not of accept/reject. Someone who
// wrote SSE-KMS is one keystroke from correct, and an error that lists all three modes without
// pointing at the one they meant makes them read the list to find out they were already right.
func TestValidateSSEModeNamesTheFixForACaseError(t *testing.T) {
	t.Parallel()

	err := ValidateSSEMode("SSE-KMS")
	if err == nil {
		t.Fatal("ValidateSSEMode accepted an upper-case mode")
	}

	if !strings.Contains(err.Error(), `"sse-kms"`) {
		t.Errorf("a case-only error must name the lower-case spelling to write instead, got: %v", err)
	}
}

// TestSSEModesReturnsACopy asserts the accessor cannot be used to corrupt the authority.
//
// The caller this exists for is a test in internal/config or internal/storage/s3 comparing the list
// against a table, and a test that can mutate what it checks against proves nothing. Non-obvious
// because slices.Clone is easy to drop in a refactor and nothing else would notice.
func TestSSEModesReturnsACopy(t *testing.T) {
	t.Parallel()

	first := SSEModes()
	if len(first) == 0 {
		t.Fatal("SSEModes returned nothing")
	}

	first[0] = "MUTATED"

	if got := SSEModes()[0]; got == "MUTATED" {
		t.Fatal("SSEModes returns the backing array, so a caller can rewrite the canonical set")
	}

	if err := ValidateSSEMode(SSEModeOff); err != nil {
		t.Errorf("mutating the returned slice broke validation: %v", err)
	}
}

// TestSSEModesIsConsistentWithTheConstants closes the loop between the two declarations.
//
// The constants and the slice are written out separately, so a fourth mode can be added without being
// listed — and the slice is what ValidateSSEMode consults, so the new mode would be rejected by the
// loader while internal/storage/s3, reading the alias of the constant, believed it existed. That is
// audit finding C1's mechanism exactly, and it is the reason these constants live here rather than in
// the s3 package: one authority per fact, or the two spellings diverge.
func TestSSEModesIsConsistentWithTheConstants(t *testing.T) {
	t.Parallel()

	declared := []string{SSEModeOff, SSEModeS3, SSEModeKMS}
	listed := SSEModes()

	for _, mode := range declared {
		if !slices.Contains(listed, mode) {
			t.Errorf("constant %q is not in SSEModes(), so ValidateSSEMode rejects a mode the rest "+
				"of the code can name", mode)
		}
	}

	for _, mode := range listed {
		if !slices.Contains(declared, mode) {
			t.Errorf("SSEModes() admits %q, which no exported constant names — callers would have "+
				"to spell it as a literal", mode)
		}
	}

	if len(listed) != len(declared) {
		t.Errorf("SSEModes() has %d entries against %d constants; one of them is duplicated",
			len(listed), len(declared))
	}
}

// A valid key ARN in the documentation-reserved account 111122223333, used as the base for the ARN
// cases below so that each one varies exactly the field it is about.
const validKeyARN = "arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

// TestValidateKMSKeyID pins the four accepted forms and the shapes that only look like them.
//
// The rejected cases divide into two kinds. Some are malformed — whitespace, a truncated UUID — and
// S3 would refuse them on the first write. The others are well-formed ARNs for the wrong thing, and
// those are the ones worth a validator: a bucket ARN or a role ARN passes any check that counts
// colons, and S3 answers it with a key-not-found complaint that never mentions the service field, so
// the operator looks for a missing key that was never the problem.
func TestValidateKMSKeyID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		id       string
		wantText string // substring the error must contain; empty means the value must be accepted
		why      string
	}{
		{
			name: "a bare key id",
			id:   "1234abcd-12ab-34cd-56ef-1234567890ab",
			why:  "resolved in the caller's own account and region, which is the common single-account case",
		},
		{
			name: "an up-cased key id",
			id:   "1234ABCD-12AB-34CD-56EF-1234567890AB",
			why:  "AWS renders key IDs lower-case but accepts either; rejecting this invents a rule",
		},
		{name: "a key ARN", id: validKeyARN},
		{
			name: "an alias",
			id:   "alias/objectfs-research-data",
		},
		{
			name: "the AWS managed key by name",
			id:   "alias/aws/s3",
			why: "the fallback S3 would substitute for an empty key; naming it is how a caller says " +
				"they mean it, which is the whole reason an empty key is refused",
		},
		{
			name: "an alias ARN",
			id:   "arn:aws:kms:eu-central-1:111122223333:alias/objectfs-research-data",
		},
		{
			name: "an ARN in the govcloud partition",
			id:   "arn:aws-us-gov:kms:us-gov-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			why:  "a partition a check written only against arn:aws would reject",
		},

		{
			name:     "empty",
			id:       "",
			wantText: "is empty",
			why: "there is no default worth inheriting: S3 substitutes the shared aws/s3 key, which " +
				"cannot be revoked or audited separately from the data",
		},
		{
			name:     "a trailing newline",
			id:       "1234abcd-12ab-34cd-56ef-1234567890ab\n",
			wantText: "whitespace",
			why: "what a key pasted from the console arrives with; checked before the patterns so the " +
				"message names the whitespace rather than reporting a correct key as malformed",
		},
		{
			name:     "a leading space",
			id:       " alias/objectfs",
			wantText: "whitespace",
			why:      "a quoted YAML scalar keeps it",
		},
		{
			name:     "a pasted document",
			id:       strings.Repeat("a", maxKMSKeyIDLength+1),
			wantText: "over the",
			why: "the value goes into an HTTP header, so kilobytes is a paste accident — a whole " +
				"policy document or a PEM block — and saying so beats a 400 on the first write",
		},
		{
			name:     "a bucket ARN",
			id:       "arn:aws:s3:::my-research-bucket",
			wantText: `an ARN for "s3", not for kms`,
			why:      "a well-formed ARN for the wrong service; the clipboard holds what was copied last",
		},
		{
			name:     "a role ARN",
			id:       "arn:aws:iam::111122223333:role/ObjectFSMount",
			wantText: `not for kms`,
		},
		{
			name:     "an ARN with too few fields",
			id:       "arn:aws:kms:us-west-2",
			wantText: "six colon-separated",
		},
		{
			name:     "an ARN with no partition",
			id:       "arn::kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			wantText: "empty partition",
		},
		{
			name:     "an ARN with no region",
			id:       "arn:aws:kms::111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			wantText: "empty region",
			why:      "the region and account are what make an ARN worth using over a bare key ID",
		},
		{
			name:     "an ARN with an invalid region",
			id:       "arn:aws:kms:US-WEST-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			wantText: "invalid region",
			why:      "delegated to ValidateRegion so the two checks cannot disagree about what a region is",
		},
		{
			name:     "an ARN with no account",
			id:       "arn:aws:kms:us-west-2::key/1234abcd-12ab-34cd-56ef-1234567890ab",
			wantText: "empty account",
		},
		{
			name:     "a key ARN whose key is not a UUID",
			id:       "arn:aws:kms:us-west-2:111122223333:key/my-research-key",
			wantText: "is not a UUID",
			why:      "an alias written under key/, which is the resource type it is not",
		},
		{
			name:     "a grant ARN",
			id:       "arn:aws:kms:us-west-2:111122223333:grant/1234abcd",
			wantText: "neither a key nor an alias",
			why:      "a real KMS ARN for a thing that cannot encrypt an object",
		},
		{
			name:     "an alias ARN with a space in the alias",
			id:       "arn:aws:kms:us-west-2:111122223333:alias/my research key",
			wantText: "not a valid alias",
		},
		{
			name:     "an alias with a space",
			id:       "alias/my research key",
			wantText: "not a valid KMS alias",
		},
		{
			name:     "the alias prefix alone",
			id:       "alias/",
			wantText: "not a valid KMS alias",
			why:      "the pattern requires at least one character after the prefix",
		},
		{
			name:     "an alias with the prefix left off",
			id:       "objectfs-research-data",
			wantText: `it needs the prefix, as in "alias/objectfs-research-data"`,
			why: "the overwhelming case: the console displays aliases without the prefix, so the " +
				"prefix reads as boilerplate and gets dropped",
		},
		{
			name:     "a truncated key id",
			id:       "1234abcd-12ab-34cd-56ef",
			wantText: `it needs the prefix, as in "alias/1234abcd-12ab-34cd-56ef"`,
			why: "four groups instead of five, which is what a partial selection produces — but " +
				"hyphens and hex digits are legal in an alias name, so this is also a perfectly " +
				"good alias with the prefix missing and the validator cannot tell which was meant; " +
				"pinned here so that a future UUID-shaped heuristic is a deliberate change",
		},
		{
			name:     "neither a key nor an alias nor an ARN",
			id:       "my research key",
			wantText: "is not a KMS key ID",
			why: "the internal space means it cannot be an alias either, so it reaches the final " +
				"message that spells out all three forms",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateKMSKeyID(tc.id)

			if tc.wantText == "" {
				if err != nil {
					t.Fatalf("ValidateKMSKeyID(%q) rejected a key form S3 accepts: %v. %s",
						tc.id, err, tc.why)
				}

				return
			}

			if err == nil {
				t.Fatalf("ValidateKMSKeyID(%q) accepted it; it must not. %s", tc.id, tc.why)
			}

			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("the error does not say why:\n  got:  %v\n  want it to contain: %q\n  %s",
					err, tc.wantText, tc.why)
			}
		})
	}
}

// FuzzValidateSSEMode asserts the validator is total and that what it accepts is a mode that exists.
//
// The same two properties as its siblings. It runs on operator configuration so it must not panic on
// any string, and anything it accepts is turned into an x-amz-server-side-encryption header by
// internal/storage/s3 — so an accepted value outside the set would either reach S3 as a header value
// it rejects or, worse, match no arm of the resolver and send no header at all, which is P-7 restored.
func FuzzValidateSSEMode(f *testing.F) {
	for _, seed := range []string{
		"", "off", "sse-s3", "sse-kms", "SSE-KMS", "OFF", "aws:kms", "AES256", "sse-c", "true",
		"sse-kms ", "\x00", "ünïcode", strings.Repeat("a", 256),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, mode string) {
		if err := ValidateSSEMode(mode); err != nil {
			return
		}

		if mode == "" {
			return
		}

		if !slices.Contains(SSEModes(), mode) {
			t.Fatalf("accepted %q, which is not a mode ObjectFS can request — the header resolver "+
				"has no arm for it, so it would silently send no encryption header at all", mode)
		}
	})
}

// FuzzValidateKMSKeyID asserts the validator is total and that nothing it accepts is obviously unfit
// for an HTTP header.
//
// The accept side cannot be checked against a set the way the mode and storage-class fuzzers can:
// the accepted language is infinite by design, since a key ID, an alias, and two ARN forms are all
// legal. So the post-conditions are the ones that hold for every accepted value regardless of form —
// it is non-empty, it is within the bound, it carries no surrounding whitespace, and it contains no
// control character or newline. That last is the one worth fuzzing: this value is interpolated into
// a request header, and a header value containing a newline is a request-splitting shape rather than
// a merely invalid key.
func FuzzValidateKMSKeyID(f *testing.F) {
	for _, seed := range []string{
		"", "1234abcd-12ab-34cd-56ef-1234567890ab", "alias/aws/s3", "alias/", validKeyARN,
		"arn:aws:s3:::bucket", "arn:aws:kms:us-west-2:111122223333:grant/x", "objectfs-key",
		"1234abcd-12ab-34cd-56ef-1234567890ab\n", " alias/x", "\x00", "ünïcode",
		strings.Repeat("a", maxKMSKeyIDLength+1),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, id string) {
		if err := ValidateKMSKeyID(id); err != nil {
			return
		}

		if id == "" {
			t.Fatal("accepted an empty key ID; SSE-KMS with no key silently uses the shared aws/s3 key")
		}

		if len(id) > maxKMSKeyIDLength {
			t.Fatalf("accepted a %d-character value, over the %d-character bound",
				len(id), maxKMSKeyIDLength)
		}

		if strings.TrimSpace(id) != id {
			t.Fatalf("accepted %q, which S3 will not strip", id)
		}

		for _, r := range id {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("accepted %q, which contains the control character %q — this value is "+
					"interpolated into a request header", id, r)
			}
		}
	})
}
