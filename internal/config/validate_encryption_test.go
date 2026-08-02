package config

// Audit finding P-7, at the loader.
//
// v0.10.0's security.encryption block held two booleans, in_transit and at_rest, both defaulting to
// true and read by nothing. No layer sent an encryption header; a grep for ServerSideEncryption,
// SSEKMS, SSECustomer or aws:kms across the tree returned zero non-test hits, while OBJECTFS.md
// documented a kms_key: ARN in that same block.
//
// v0.10.1 replaces them with mode/kms_key_id/bucket_keys and defaults the mode to "off". Removing the
// old keys rather than deprecating them is the point: the loader is strict, so a file carrying
// at_rest: true now fails and names the key. A silently-ignored key is how the claim survived three
// releases — the only reliable way to get it re-examined is to make the file stop loading.
//
// internal/storage/s3 validates the same combinations in NewBackend, and the duplication is
// deliberate: this catches them at load with the YAML path in the message, that one catches an
// s3.Config a Go caller hand-built without passing through this loader. The two entry points share no
// layer that could hold one check.

import (
	"strings"
	"testing"
)

// A syntactically valid key ARN in the documentation-reserved account 111122223333. Nothing here
// reaches KMS; only the shape is checked.
const validKMSKeyARN = "arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

// TestValidateRejectsAnEncryptionBlockThatCannotMeanWhatItSays covers the combinations that would
// otherwise be inert.
//
// Every case below produces a mount that comes up, writes objects, reads them back, and does not
// perform the encryption the operator named. That is P-7's exact shape, which is why each is a refusal
// to load rather than a warning: a warning scrolls past, and "the mount succeeded" is the evidence
// someone cites for believing encryption is on.
func TestValidateRejectsAnEncryptionBlockThatCannotMeanWhatItSays(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		enc      EncryptionConfig
		wantText string
		why      string
	}{
		{
			name:     "an unknown mode",
			enc:      EncryptionConfig{Mode: "sse-c"},
			wantText: "security.encryption.mode",
			why: "SSE-C requires the client to send and store the key itself, which ObjectFS does not " +
				"implement; accepting the word would mean the mode resolves to no header at all",
		},
		{
			name:     "a mode with the wrong case",
			enc:      EncryptionConfig{Mode: "SSE-KMS"},
			wantText: "security.encryption.mode",
			why: "the modes are compared literally, so an uppercase spelling falls through to the " +
				"default arm and sends nothing — a config that reads as encrypted and is not",
		},
		{
			name: "a key beside mode off",
			enc: EncryptionConfig{
				Mode:     EncryptionModeOff,
				KMSKeyID: validKMSKeyARN,
			},
			wantText: "kms_key_id",
			why: "the two readings — encrypt with this key, and do not encrypt — are too far apart to " +
				"pick between silently. Someone wrote that ARN intending it to be used",
		},
		{
			name:     "a key with no mode at all",
			enc:      EncryptionConfig{KMSKeyID: validKMSKeyARN},
			wantText: "kms_key_id",
			why: "the zero value must behave as off and still refuse the contradiction; an unset field " +
				"that quietly means something is how at_rest: true happened",
		},
		{
			name: "a KMS key under sse-s3",
			enc: EncryptionConfig{
				Mode:     EncryptionModeS3,
				KMSKeyID: validKMSKeyARN,
			},
			wantText: "kms_key_id",
			why: "sse-s3 encrypts with S3's own keys and cannot use a KMS key, so the key is ignored " +
				"and the object is not under the key the file names",
		},
		{
			name:     "sse-kms with no key",
			enc:      EncryptionConfig{Mode: EncryptionModeKMS},
			wantText: "kms_key_id",
			why: "S3 accepts aws:kms with no key and substitutes the AWS managed aws/s3 key, which is " +
				"shared with every other service in the account and cannot be audited or revoked " +
				"separately from the data. The write succeeds, so nothing surfaces the substitution",
		},
		{
			name: "a key ARN with trailing whitespace",
			enc: EncryptionConfig{
				Mode:     EncryptionModeKMS,
				KMSKeyID: validKMSKeyARN + " ",
			},
			wantText: "whitespace",
			why: "a quoted YAML scalar keeps the space, and KMS rejects the ARN with a message about " +
				"the ARN being malformed — which sends the operator looking at the visible characters",
		},
		{
			name: "an ARN for the wrong service",
			enc: EncryptionConfig{
				Mode:     EncryptionModeKMS,
				KMSKeyID: "arn:aws:s3:::my-bucket",
			},
			wantText: "kms",
			why: "the clipboard holds whatever was copied last. A bucket ARN or a role ARN here is a " +
				"paste error, and it is one KMS reports as a permissions problem at first write",
		},
		{
			name: "a bare key name",
			enc: EncryptionConfig{
				Mode:     EncryptionModeKMS,
				KMSKeyID: "objectfs-production",
			},
			wantText: "alias/",
			why: "this is a valid alias with the prefix added, and the omission is the most common way " +
				"to write this key wrong, so the message says what to prepend",
		},
		{
			name: "bucket keys without KMS",
			enc: EncryptionConfig{
				Mode:       EncryptionModeS3,
				BucketKeys: true,
			},
			wantText: "bucket_keys",
			why: "bucket keys cut SSE-KMS's per-object KMS requests and do nothing otherwise, so a " +
				"config reading \"we enabled the KMS cost control\" while no KMS is in use is the same " +
				"false comfort at_rest: true was",
		},
		{
			name:     "bucket keys with encryption off",
			enc:      EncryptionConfig{Mode: EncryptionModeOff, BucketKeys: true},
			wantText: "bucket_keys",
			why:      "same as above, and the more likely half-finished edit: the mode was turned off and the cost control left behind",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()
			cfg.Security.Encryption = tc.enc

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted this encryption block, so the mount comes up and the "+
					"encryption the file names does not happen. %s", tc.why)
			}

			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("Validate rejected it but the message does not mention %q, so the operator is "+
					"not told which line to edit:\n%v", tc.wantText, err)
			}
		})
	}
}

// TestValidateAcceptsEveryEncryptionBlockTheBackendCanHonour is the control.
//
// A validator that rejected everything would pass the test above completely, and the failure it would
// cause is the harsher one: a config that was working before the upgrade stops loading. The four key
// forms are here because KMS accepts all four and a validator written against ARNs alone would refuse
// the two shortest — which are what an operator actually types.
func TestValidateAcceptsEveryEncryptionBlockTheBackendCanHonour(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		enc  EncryptionConfig
		why  string
	}{
		{
			name: "the shipped default",
			enc:  NewDefault().Security.Encryption,
			why:  "whatever NewDefault produces must load; this fails if the default and the validator disagree",
		},
		{
			name: "an entirely absent block",
			enc:  EncryptionConfig{},
			why: "a config file written before these keys existed has the zero value, and it must keep " +
				"loading — the upgrade must not require an edit to mount",
		},
		{
			name: "mode off, stated explicitly",
			enc:  EncryptionConfig{Mode: EncryptionModeOff},
			why:  "off is a legitimate choice: S3 has applied SSE-S3 to all new objects since January 2023",
		},
		{
			name: "sse-s3",
			enc:  EncryptionConfig{Mode: EncryptionModeS3},
			why:  "no key, by definition",
		},
		{
			name: "sse-kms with a key ARN",
			enc:  EncryptionConfig{Mode: EncryptionModeKMS, KMSKeyID: validKMSKeyARN},
		},
		{
			name: "sse-kms with an alias ARN",
			enc: EncryptionConfig{
				Mode:     EncryptionModeKMS,
				KMSKeyID: "arn:aws:kms:us-west-2:111122223333:alias/objectfs",
			},
		},
		{
			name: "sse-kms with a bare alias",
			enc:  EncryptionConfig{Mode: EncryptionModeKMS, KMSKeyID: "alias/objectfs"},
			why:  "the short form, resolved by KMS in the caller's account and region",
		},
		{
			name: "sse-kms with the AWS managed alias, named deliberately",
			enc:  EncryptionConfig{Mode: EncryptionModeKMS, KMSKeyID: "alias/aws/s3"},
			why: "the same key S3 falls back to when none is named, but written out. Rejecting it would " +
				"refuse the one form that says the fallback was a choice rather than an omission",
		},
		{
			name: "sse-kms with a bare key UUID",
			enc: EncryptionConfig{
				Mode:     EncryptionModeKMS,
				KMSKeyID: "1234abcd-12ab-34cd-56ef-1234567890ab",
			},
			why: "valid when the key is in the caller's own account and region, which is the common case",
		},
		{
			name: "sse-kms with bucket keys",
			enc: EncryptionConfig{
				Mode:       EncryptionModeKMS,
				KMSKeyID:   validKMSKeyARN,
				BucketKeys: true,
			},
			why: "the one combination bucket_keys belongs in, and the one CargoShip's transporter cannot " +
				"express — so PutObject diverts around it rather than sending different headers",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := NewDefault()
			cfg.Security.Encryption = tc.enc

			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate rejected an encryption block the backend can honor, so this "+
					"configuration cannot be mounted. %s\n%v", tc.why, err)
			}
		})
	}
}

// TestTheDefaultEncryptionModeIsOff pins the default, which is a decision and not an omission.
//
// "Off" reads as the less safe choice and is the honest one. S3 has applied SSE-S3 to every new object
// unconditionally since January 2023, so off means "encrypted with S3's keys, not requested by us" —
// not "in the clear". Defaulting to sse-s3 would send a header that changes nothing while restoring
// exactly the reassurance at_rest: true gave, and defaulting to sse-kms cannot work at all: it needs a
// key that no default can supply, so every stock mount would fail to load.
func TestTheDefaultEncryptionModeIsOff(t *testing.T) {
	t.Parallel()

	got := NewDefault().Security.Encryption

	if got.Mode != EncryptionModeOff {
		t.Errorf("the default encryption mode is %q, want %q", got.Mode, EncryptionModeOff)
	}
	if got.KMSKeyID != "" {
		t.Errorf("the default names a KMS key (%q); a default key would be a key nobody chose", got.KMSKeyID)
	}
	if got.BucketKeys {
		t.Error("the default enables bucket keys, which only apply to SSE-KMS and so cannot be a default")
	}
}
