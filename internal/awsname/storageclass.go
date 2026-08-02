package awsname

import (
	"fmt"
	"slices"
	"strings"
)

// The S3 storage class names, spelled as S3 spells them on the wire.
//
// They live in this package rather than in internal/storage/s3 for the reason in the package
// comment: storage_tier is read by internal/config and acted on by internal/storage/s3, and config
// cannot import s3. The s3 package's Tier* constants are aliases of these, and a test there asserts
// its billing table covers exactly this set — so the set of names that exist has one authority, and
// the table cannot quietly grow a tier the validator has never heard of.
const (
	StorageClassStandard          = "STANDARD"
	StorageClassStandardIA        = "STANDARD_IA"
	StorageClassOneZoneIA         = "ONEZONE_IA"
	StorageClassReducedRedundancy = "REDUCED_REDUNDANCY"
	StorageClassGlacierIR         = "GLACIER_IR"
	StorageClassGlacier           = "GLACIER"
	StorageClassDeepArchive       = "DEEP_ARCHIVE"
	StorageClassIntelligent       = "INTELLIGENT_TIERING"
)

// storageClasses is the canonical set, listed warmest-first because that is the order someone
// choosing a tier reads them in.
var storageClasses = []string{
	StorageClassStandard,
	StorageClassIntelligent,
	StorageClassStandardIA,
	StorageClassOneZoneIA,
	StorageClassGlacierIR,
	StorageClassGlacier,
	StorageClassDeepArchive,
	StorageClassReducedRedundancy,
}

// StorageClasses returns the storage classes ObjectFS accepts for storage_tier.
//
// A copy, because the caller that most wants this list is a test comparing it against a table, and a
// test that mutates the authority it is checking against proves nothing.
func StorageClasses() []string {
	return slices.Clone(storageClasses)
}

// ValidateStorageClass reports whether class is a storage class ObjectFS can write objects with.
//
// An empty class is valid and means STANDARD, which is both what S3 does for a PUT with no
// x-amz-storage-class header and what the backend fills in.
//
// Anything else non-empty must name a real class, because nothing downstream will say so. The
// backend's tier validator looked the name up in its billing table and, on a miss, silently
// substituted STANDARD and carried on — so `storage_tier: STANDARD_1A` (digit one for letter I) was
// accepted by the loader, accepted by the validator, logged as STANDARD, and billed as STANDARD,
// while the operator believed they had configured infrequent-access storage. That is audit finding
// C1's shape with the failure moved one step further out: not even the acting layer complained.
//
// The case check is separate because it is the likely typo. S3 storage classes are upper-case, YAML
// is not, and "standard_ia" is what a person writes.
func ValidateStorageClass(class string) error {
	if class == "" {
		return nil
	}

	if slices.Contains(storageClasses, class) {
		return nil
	}

	if upper := strings.ToUpper(class); slices.Contains(storageClasses, upper) {
		return fmt.Errorf("storage class %q is not valid: S3 spells storage classes in upper case, "+
			"so this is %q", class, upper)
	}

	return fmt.Errorf("storage class %q is not one of the classes ObjectFS can write: %s "+
		"(an empty value means %s)",
		class, strings.Join(storageClasses, ", "), StorageClassStandard)
}
