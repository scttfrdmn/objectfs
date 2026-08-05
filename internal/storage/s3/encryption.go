package s3

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/scttfrdmn/objectfs/internal/awsname"
)

// This file is audit finding P-7.
//
// v0.10.0 shipped a `security.encryption.at_rest` key that **defaulted to true** and was read by
// nothing. A grep for ServerSideEncryption, SSEKMS, SSECustomer, or aws:kms across the whole tree
// returned zero non-test hits, while OBJECTFS.md documented a `kms_key:` ARN in a security block.
// Every object went to S3 with no encryption header at all.
//
// The reason that is worth a file of its own rather than a one-line fix is the failure mode. An
// absent feature gets noticed: someone looks for encryption, does not find it, and asks. A
// configuration key that *claims* the property and sets no header ends the search — and the thing the
// search would have found is what an institutional review asks about, usually after the data is
// already there. It is the same defect as reporting a successful delete for an object still in the
// bucket, except that nothing ever surfaces to contradict it.
//
// What makes it fixable in one place is that S3 spells encryption identically on every write, so the
// write paths differ only in which input struct they hold, not in what they must send. That matters
// here: PutObject, CreateMultipartUpload, and two CopyObject call sites are four places to forget,
// and a per-path `if cfg.Mode == ...` would have been four chances to forget differently. Each path
// applies the same resolved header set, and the tests assert on the headers the endpoint received
// rather than on the struct the code just filled in.

// encryptionHeaders is the resolved wire form of an [EncryptionConfig].
//
// It exists because the SDK's write inputs spell these three fields identically but share no
// interface for them, so the alternative to one resolver plus three field-assignments is the same
// switch written three times — which is how the paths drift. Pointer fields because that is how the
// SDK distinguishes "send false" from "send nothing", and for encryption the difference between a
// header set to false and no header at all is the whole subject.
type encryptionHeaders struct {
	algorithm  s3types.ServerSideEncryption
	kmsKeyID   *string
	bucketKeys *bool
}

// headers resolves the configuration to what goes on the request.
//
// The zero value — no algorithm, no key, no bucket-key flag — is what "off" resolves to, and
// assigning it to an input leaves the input exactly as the SDK would send it unencrypted. So every
// write path can apply this unconditionally, which is the property that keeps the paths from
// diverging: there is no arm where applying the headers is skipped and therefore none to forget.
func (e EncryptionConfig) headers() encryptionHeaders {
	switch e.Mode {
	case EncryptionModeS3:
		return encryptionHeaders{algorithm: s3types.ServerSideEncryptionAes256}

	case EncryptionModeKMS:
		h := encryptionHeaders{
			algorithm: s3types.ServerSideEncryptionAwsKms,
			kmsKeyID:  aws.String(e.KMSKeyID),
		}

		// Only sent when asked for. S3 treats an absent header as "use the bucket's setting", which is
		// the right deference: an account that enabled bucket keys at the bucket level should not have
		// them turned off by a filesystem that happens not to mention them.
		if e.BucketKeys {
			h.bucketKeys = aws.Bool(true)
		}

		return h

	default:
		return encryptionHeaders{}
	}
}

// applyEncryptionPut sets the server-side encryption fields on a PutObjectInput.
func applyEncryptionPut(input *s3.PutObjectInput, cfg EncryptionConfig) {
	h := cfg.headers()
	input.ServerSideEncryption = h.algorithm
	input.SSEKMSKeyId = h.kmsKeyID
	input.BucketKeyEnabled = h.bucketKeys
}

// applyEncryptionCreateMultipart is [applyEncryptionPut] for the start of a multipart upload.
//
// The header goes on the create and not on the parts: S3 records the encryption for the upload as a
// whole, and an UploadPart that tried to restate it is rejected. So a multipart upload that omits it
// here is unencrypted no matter what the parts carry — and multipart is the path every large object
// takes, which is to say the path where the data worth encrypting is.
func applyEncryptionCreateMultipart(input *s3.CreateMultipartUploadInput, cfg EncryptionConfig) {
	h := cfg.headers()
	input.ServerSideEncryption = h.algorithm
	input.SSEKMSKeyId = h.kmsKeyID
	input.BucketKeyEnabled = h.bucketKeys
}

// applyEncryptionCopy is [applyEncryptionPut] for a CopyObject, which is how ObjectFS performs an
// attribute-only write and a storage-tier transition.
//
// A copy does not inherit the source's encryption: S3 encrypts the destination according to the
// request, and a request that says nothing gets the bucket default. So without this, a metadata
// update on an SSE-KMS object rewrites it under the bucket default and quietly moves it off the
// customer managed key — the sharpest form of this defect, because the object was encrypted correctly
// when written and stopped being so when something changed its attributes.
func applyEncryptionCopy(input *s3.CopyObjectInput, cfg EncryptionConfig) {
	h := cfg.headers()
	input.ServerSideEncryption = h.algorithm
	input.SSEKMSKeyId = h.kmsKeyID
	input.BucketKeyEnabled = h.bucketKeys
}

// validateEncryption rejects a configuration that cannot mean what it says.
//
// It is called from [NewBackend], so a bad value fails at construction rather than at the first
// write. That ordering is the point: a mount that comes up and encrypts nothing is the defect this
// whole file exists to fix, and "the mount succeeded" is exactly the evidence an operator would cite
// for believing encryption was on.
//
// Every arm below rejects a combination that is *inert* rather than wrong-on-the-wire, which is a
// deliberate choice to be strict. Ignoring a key set beside mode "off" would send no header and
// return no error, reproducing P-7 exactly — the operator has said the word "encryption" in their
// config and been told nothing is wrong.
func validateEncryption(cfg EncryptionConfig) error {
	switch cfg.Mode {
	case "", EncryptionModeOff:
		// A key named beside "off" is a contradiction, not a spare setting. Someone wrote that ARN
		// intending it to be used, and the two readings — "encrypt with this key" and "do not
		// encrypt" — are far enough apart that silently picking either is worse than refusing.
		if cfg.KMSKeyID != "" {
			return fmt.Errorf("storage.s3.encryption: kms_key_id is set but mode is %q, so no "+
				"encryption header is sent and the key is unused; set mode to %q to encrypt with the "+
				"key, or remove the key", cfg.Mode, EncryptionModeKMS)
		}

	case EncryptionModeS3:
		if cfg.KMSKeyID != "" {
			return fmt.Errorf("storage.s3.encryption: kms_key_id is set but mode is %q, which "+
				"encrypts with S3's own keys and cannot use a KMS key; set mode to %q to use the key, "+
				"or remove the key", EncryptionModeS3, EncryptionModeKMS)
		}

	case EncryptionModeKMS:
		if cfg.KMSKeyID == "" {
			return fmt.Errorf("storage.s3.encryption: mode is %q but kms_key_id is empty; S3 would "+
				"fall back to the AWS managed key aws/s3, which is shared with every other service in "+
				"the account and cannot be audited or revoked separately from the data — name a key, "+
				"or use mode %q if S3-managed keys are what you want",
				EncryptionModeKMS, EncryptionModeS3)
		}

		if err := awsname.ValidateKMSKeyID(cfg.KMSKeyID); err != nil {
			return fmt.Errorf("storage.s3.encryption: %w", err)
		}

	default:
		// Delegated so the message is the same one internal/config produces at load, and so the list of
		// valid modes has one source. A switch arm listing them itself is a second authority, which is
		// how the storage-class list drifted.
		if err := awsname.ValidateSSEMode(cfg.Mode); err != nil {
			return fmt.Errorf("storage.s3.encryption.mode is invalid: %w", err)
		}

		// Unreachable: the arms above cover every mode ValidateSSEMode accepts. It is here so that
		// adding a mode to awsname without teaching this function what header it needs fails loudly
		// instead of writing objects with no encryption — which is the exact failure P-7 was.
		return fmt.Errorf("storage.s3.encryption.mode %q is accepted by awsname but this backend does "+
			"not know what header to send for it", cfg.Mode)
	}

	// Bucket keys without SSE-KMS is inert rather than dangerous, but it is still a setting that does
	// nothing, and a config reading "we enabled the KMS cost control" while no KMS is in use is the
	// same false comfort `at_rest: true` was.
	if cfg.BucketKeys && cfg.Mode != EncryptionModeKMS {
		return fmt.Errorf("storage.s3.encryption: bucket_keys is set but mode is %q; bucket keys "+
			"reduce SSE-KMS's per-object KMS calls and do nothing without mode %q",
			cfg.Mode, EncryptionModeKMS)
	}

	return nil
}
