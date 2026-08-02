package awsname

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The server-side encryption modes ObjectFS accepts for storage.s3.encryption.mode.
//
// They live in this package for the reason in the package comment: the mode is read by internal/config
// and acted on by internal/storage/s3, and config cannot import s3. The s3 package's EncryptionMode*
// constants are aliases of these, so the set of modes that exist has one authority and a mode cannot
// be accepted by the loader that the backend has never heard of — which is the C1 shape this package
// was created to close.
//
// They are ObjectFS's spelling of the x-amz-server-side-encryption header rather than aliases of the
// SDK's enum, because "off" has no header and therefore no SDK value. S3 encrypts every object with
// SSE-S3 whether or not a request asks, so the meaningful distinction between these is not "encrypted
// or not" but who holds the key and who can be denied the use of it.
const (
	// SSEModeOff sends no encryption header. S3 still applies SSE-S3 — it has done so for all new
	// objects unconditionally since January 2023 — so this is not "stored in the clear". What it lacks
	// is a key of your own: no per-object access appears in CloudTrail, and nothing can be revoked
	// short of deleting the data.
	SSEModeOff = "off"

	// SSEModeS3 requests SSE-S3 (AES256) explicitly. On a modern bucket it is byte-for-byte the same
	// protection as SSEModeOff, and it exists for the case where that has to be stated on the request
	// rather than assumed of the bucket — a bucket policy denying uploads that omit
	// x-amz-server-side-encryption, which is a common compliance control.
	SSEModeS3 = "sse-s3"

	// SSEModeKMS requests SSE-KMS with a named key. This is the mode that buys what the other two do
	// not: a key auditable through CloudTrail and revocable independently of the data, which is what
	// an institutional review usually means by "encryption at rest".
	//
	// It has costs a filesystem feels more than a backup tool does. Every read and every write becomes
	// a KMS call, billed per request and subject to a per-region rate limit, so a metadata-heavy
	// traversal can be throttled by KMS while S3 is idle. S3 Bucket Keys cut those calls by up to 99%
	// and are the recommended companion.
	SSEModeKMS = "sse-kms"
)

// sseModes is the canonical set, ordered by how much key control each gives.
var sseModes = []string{SSEModeOff, SSEModeS3, SSEModeKMS}

// SSEModes returns the encryption modes ObjectFS accepts.
//
// A copy, for the same reason as [StorageClasses]: the caller that most wants this list is a test
// comparing it against a table, and a test that mutates its own authority proves nothing.
func SSEModes() []string {
	return slices.Clone(sseModes)
}

// ValidateSSEMode reports whether mode names an encryption mode ObjectFS can request.
//
// An empty mode is valid and means [SSEModeOff], matching how the other validators here treat empty
// and how S3 treats a PUT with no encryption header.
//
// The case check is separate because it is the likely typo in the other direction from storage classes:
// these modes are lower-case, and someone who has just written STANDARD_IA two lines above will write
// SSE-KMS.
func ValidateSSEMode(mode string) error {
	if mode == "" {
		return nil
	}

	if slices.Contains(sseModes, mode) {
		return nil
	}

	if lower := strings.ToLower(mode); slices.Contains(sseModes, lower) {
		return fmt.Errorf("encryption mode %q is not valid: ObjectFS spells these in lower case, "+
			"so this is %q", mode, lower)
	}

	return fmt.Errorf("encryption mode %q is not one of the modes ObjectFS can request: %s "+
		"(an empty value means %s)", mode, strings.Join(sseModes, ", "), SSEModeOff)
}

// The four forms S3 accepts for x-amz-server-side-encryption-aws-kms-key-id.
//
// Only the two ARN forms work across accounts: a bare key ID or a bare alias is resolved against the
// caller's own account, so naming another account's key without its ARN does not fail — it silently
// looks for a key of that name at home and reports the key as not found, or worse, finds an unrelated
// key that happens to share the alias. The validator cannot tell those apart, so it accepts all four
// and the doc comment says which two travel.
const (
	kmsAliasPrefix = "alias/"
	kmsARNPrefix   = "arn:"
)

// maxKMSKeyIDLength bounds the value at 2048 characters.
//
// This is a sanity bound rather than a documented AWS limit — the value is sent as an HTTP header, so
// something that runs to kilobytes is a paste accident (a whole policy document, a PEM block) and
// saying so beats a 400 from S3 on the first write. The real forms are all far shorter: a key ID is
// 36 characters, an alias is capped at 256 by KMS, and an ARN is those plus about 60.
const maxKMSKeyIDLength = 2048

// kmsKeyIDPattern is the UUID a KMS key ID is: 8-4-4-4-12 hex digits.
//
// Case-insensitive because AWS renders key IDs lower-case but accepts either, and rejecting a key ID
// somebody up-cased while retyping it would be a validator inventing a rule S3 does not have.
var kmsKeyIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// kmsAliasPattern is what KMS allows after "alias/": letters, digits, underscore, hyphen, and forward
// slash. The slash is what makes "alias/aws/s3" — the AWS managed key — a legal alias rather than a
// path.
var kmsAliasPattern = regexp.MustCompile(`^alias/[a-zA-Z0-9/_-]+$`)

// ValidateKMSKeyID reports whether id names a KMS key SSE-KMS can encrypt with.
//
// Empty is an error here, unlike [ValidateRegion] and [ValidateStorageClass] where empty means "use
// the default". There is no default worth inheriting: SSE-KMS with no key ID falls back to the AWS
// managed key aws/s3, which is shared with every other service in the account and cannot be revoked
// or its use audited separately from the data. Whether that fallback is acceptable is the caller's
// decision to make explicitly, by choosing a mode, not something to arrive at by leaving a field
// blank.
//
// Four forms are accepted, because S3 accepts four:
//
//   - a key ID — 1234abcd-12ab-34cd-56ef-1234567890ab
//   - a key ARN — arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab
//   - an alias — alias/objectfs-research-data
//   - an alias ARN — arn:aws:kms:us-west-2:111122223333:alias/objectfs-research-data
//
// The two bare forms resolve against the caller's own account and region. That is the trap this
// validator cannot close and callers should know about: naming a key that lives in another account
// without its ARN is syntactically perfect and semantically wrong, and S3 answers it as a missing key
// rather than as a configuration mistake.
//
// Like its siblings this is a syntax check, not a lookup. Whether the key exists, is enabled, is in
// the same region as the bucket, and grants this principal kms:GenerateDataKey are four questions only
// KMS can answer, and asking it at config-load would make a mount depend on a second service being
// reachable. Those failures do surface on the first write, with S3 naming the key — which is the
// difference from the case P-7 was about, where nothing surfaced at all because no header was sent.
func ValidateKMSKeyID(id string) error {
	if id == "" {
		return fmt.Errorf("kms_key_id is empty: name a key ID, key ARN, alias, or alias ARN")
	}

	// Checked before the patterns because YAML makes this easy to do and hard to see. A quoted scalar
	// keeps its trailing spaces, and a key pasted from the console can arrive with a newline folded to
	// one. Both fail every pattern below, and the resulting message — a key that looks correct
	// reported as malformed — sends the reader looking at the key rather than at the whitespace.
	if strings.TrimSpace(id) != id {
		return fmt.Errorf("kms_key_id %q has leading or trailing whitespace, which S3 will not "+
			"strip; the value itself looks like %q", id, strings.TrimSpace(id))
	}

	if len(id) > maxKMSKeyIDLength {
		return fmt.Errorf("kms_key_id is %d characters, over the %d-character bound; a key ID is 36 "+
			"characters and an ARN under 200, so this is unlikely to be a key",
			len(id), maxKMSKeyIDLength)
	}

	switch {
	case strings.HasPrefix(id, kmsARNPrefix):
		return validateKMSARN(id)

	case strings.HasPrefix(id, kmsAliasPrefix):
		if !kmsAliasPattern.MatchString(id) {
			return fmt.Errorf("kms_key_id %q is not a valid KMS alias: after %q an alias may contain "+
				"letters, digits, underscores, hyphens, and forward slashes", id, kmsAliasPrefix)
		}

		return nil

	case kmsKeyIDPattern.MatchString(id):
		return nil
	}

	// The remaining case is a bare name that is neither a UUID nor prefixed. Overwhelmingly this is an
	// alias with the prefix left off, because the prefix is the part that looks like boilerplate and
	// the console displays aliases without it.
	if kmsAliasPattern.MatchString(kmsAliasPrefix + id) {
		return fmt.Errorf("kms_key_id %q is neither a key ID nor an ARN; if it is an alias it needs "+
			"the prefix, as in %q", id, kmsAliasPrefix+id)
	}

	return fmt.Errorf("kms_key_id %q is not a KMS key ID (a UUID such as "+
		"1234abcd-12ab-34cd-56ef-1234567890ab), an alias (%q...), or an ARN (%q...)",
		id, kmsAliasPrefix, kmsARNPrefix)
}

// validateKMSARN checks the shape of an ARN and, above all, that it names KMS.
//
// The service field is the one worth checking hardest. An ARN is the form people copy, and the
// clipboard holds whatever was copied last — a bucket ARN, a role ARN, an SNS topic. All of those are
// well-formed ARNs, so a check that only counted colons would pass them through to S3, which answers
// a non-KMS ARN with a generic KMSKeyNotFound-shaped complaint that does not point at the service
// field. The resource type matters for the same reason: arn:aws:kms:...:grant/... is a real KMS ARN
// for a thing that is not a key.
func validateKMSARN(arn string) error {
	// arn:partition:service:region:account:resource — six fields, and the resource may itself contain
	// colons, so the split is bounded rather than exact.
	const arnFields = 6

	parts := strings.SplitN(arn, ":", arnFields)
	if len(parts) != arnFields {
		return fmt.Errorf("kms_key_id %q is not a well-formed ARN: expected six colon-separated "+
			"fields (arn:partition:service:region:account:resource), found %d", arn, len(parts))
	}

	if service := parts[2]; service != "kms" {
		return fmt.Errorf("kms_key_id %q is an ARN for %q, not for kms; SSE-KMS encrypts with a KMS "+
			"key, so this names the wrong resource entirely rather than the wrong key", arn, service)
	}

	if parts[1] == "" {
		return fmt.Errorf("kms_key_id %q has an empty partition field; it should be aws, or "+
			"aws-cn or aws-us-gov in those partitions", arn)
	}

	// Region and account are required on a KMS key ARN: they are what makes the ARN worth using over
	// a bare key ID, since together they are the cross-account and cross-region reference.
	if parts[3] == "" {
		return fmt.Errorf("kms_key_id %q has an empty region field; a key ARN names the key's "+
			"region, and an ARN without one is no more specific than a bare key ID", arn)
	}

	if err := ValidateRegion(parts[3]); err != nil {
		return fmt.Errorf("kms_key_id %q names an invalid region: %w", arn, err)
	}

	if parts[4] == "" {
		return fmt.Errorf("kms_key_id %q has an empty account field; a key ARN names the account "+
			"that owns the key", arn)
	}

	resource := parts[5]

	switch {
	case strings.HasPrefix(resource, "key/"):
		keyID := strings.TrimPrefix(resource, "key/")
		if !kmsKeyIDPattern.MatchString(keyID) {
			return fmt.Errorf("kms_key_id %q has resource %q, whose key ID is not a UUID such as "+
				"1234abcd-12ab-34cd-56ef-1234567890ab", arn, resource)
		}

		return nil

	case strings.HasPrefix(resource, kmsAliasPrefix):
		if !kmsAliasPattern.MatchString(resource) {
			return fmt.Errorf("kms_key_id %q has resource %q, which is not a valid alias: after %q "+
				"an alias may contain letters, digits, underscores, hyphens, and forward slashes",
				arn, resource, kmsAliasPrefix)
		}

		return nil
	}

	return fmt.Errorf("kms_key_id %q is a KMS ARN for %q, which is neither a key nor an alias; "+
		"SSE-KMS needs a key/ or alias/ resource", arn, resource)
}
