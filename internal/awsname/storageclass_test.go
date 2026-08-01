package awsname

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestValidateStorageClass pins the contract in literals.
//
// The rejected cases are the ones a person actually writes. `standard_ia` is what YAML looks like;
// `STANDARD_1A` is a digit one where a capital I belongs and is indistinguishable from it in most
// terminal fonts; `GLACIER_INSTANT_RETRIEVAL` is the name AWS uses in its own console prose, not on
// the wire. Every one of them was silently corrected to STANDARD before this check existed.
func TestValidateStorageClass(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		class   string
		wantErr bool
		why     string
	}{
		{
			name:  "empty means standard",
			class: "",
			why:   "S3 defaults a PUT with no x-amz-storage-class to STANDARD, and so does the backend",
		},
		{name: "standard", class: "STANDARD"},
		{name: "standard-ia", class: "STANDARD_IA"},
		{name: "onezone-ia", class: "ONEZONE_IA"},
		{name: "glacier instant retrieval", class: "GLACIER_IR"},
		{name: "glacier flexible retrieval", class: "GLACIER"},
		{name: "deep archive", class: "DEEP_ARCHIVE"},
		{name: "intelligent tiering", class: "INTELLIGENT_TIERING"},
		{
			name:  "reduced redundancy",
			class: "REDUCED_REDUNDANCY",
			why:   "deprecated by AWS and more expensive than STANDARD, but still accepted on a PUT",
		},

		{
			name:    "lower case",
			class:   "standard_ia",
			wantErr: true,
			why:     "the shape a YAML file has; S3 storage classes are upper-case on the wire",
		},
		{
			name:    "mixed case",
			class:   "Standard_IA",
			wantErr: true,
		},
		{
			name:    "a digit one for a capital I",
			class:   "STANDARD_1A",
			wantErr: true,
			why: "the typo this validator was written for: accepted by the loader, silently " +
				"substituted with STANDARD by the tier validator, billed as STANDARD",
		},
		{
			name:    "the console's prose name",
			class:   "GLACIER_INSTANT_RETRIEVAL",
			wantErr: true,
			why:     "AWS's own UI spells it this way; the wire name is GLACIER_IR",
		},
		{
			name:    "a hyphen for an underscore",
			class:   "STANDARD-IA",
			wantErr: true,
		},
		{
			name:    "a trailing space",
			class:   "STANDARD ",
			wantErr: true,
			why:     "what a YAML value picks up by accident",
		},
		{
			name:    "the EBS volume type",
			class:   "gp3",
			wantErr: true,
			why:     "a different service's storage vocabulary, reached for by analogy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateStorageClass(tc.class)

			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("ValidateStorageClass(%q) accepted it; it must not. %s", tc.class, tc.why)
			case !tc.wantErr && err != nil:
				t.Fatalf("ValidateStorageClass(%q) rejected a real storage class: %v. %s",
					tc.class, err, tc.why)
			}

			if err == nil {
				return
			}

			// Same reasoning as ValidateRegion's: the operator has to be able to see which value in
			// the file is the bad one, and the trailing-space case is visible only when escaped.
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", tc.class)) {
				t.Errorf("the error does not %%q-quote the offending value, so an invisible "+
					"character in it cannot be seen: %v", err)
			}
		})
	}
}

// TestValidateStorageClassNamesTheFixForACaseError asserts the case branch says what to write.
//
// Separate from the table because it is a property of the message, not of accept/reject. An operator
// who wrote `standard_ia` is one keystroke from correct, and an error that lists all eight classes
// without pointing at the one they meant makes them read the list.
func TestValidateStorageClassNamesTheFixForACaseError(t *testing.T) {
	t.Parallel()

	err := ValidateStorageClass("standard_ia")
	if err == nil {
		t.Fatal("ValidateStorageClass accepted a lower-case class")
	}

	if !strings.Contains(err.Error(), `"STANDARD_IA"`) {
		t.Errorf("a case-only error must name the upper-case spelling to write instead, got: %v", err)
	}
}

// TestStorageClassesReturnsACopy asserts the accessor cannot be used to corrupt the authority.
//
// The caller this exists for is a test comparing the list against a table in another package, and a
// test that can mutate what it checks against proves nothing. Non-obvious because slices.Clone is
// easy to drop in a refactor and nothing else would notice.
func TestStorageClassesReturnsACopy(t *testing.T) {
	t.Parallel()

	first := StorageClasses()
	if len(first) == 0 {
		t.Fatal("StorageClasses returned nothing")
	}

	first[0] = "MUTATED"

	if got := StorageClasses()[0]; got == "MUTATED" {
		t.Fatal("StorageClasses returns the backing array, so a caller can rewrite the canonical set")
	}

	if err := ValidateStorageClass(StorageClassStandard); err != nil {
		t.Errorf("mutating the returned slice broke validation: %v", err)
	}
}

// TestStorageClassesIsConsistentWithTheConstants closes the loop between the two declarations.
//
// The constants and the slice are written out separately, so a ninth constant can be added without
// being listed — and the slice is what ValidateStorageClass consults, so the new class would be
// rejected by the loader while every other layer, reading the constant, believed it existed. That is
// audit finding C1's mechanism exactly: one authority per fact, or the two spellings diverge.
func TestStorageClassesIsConsistentWithTheConstants(t *testing.T) {
	t.Parallel()

	declared := []string{
		StorageClassStandard,
		StorageClassStandardIA,
		StorageClassOneZoneIA,
		StorageClassReducedRedundancy,
		StorageClassGlacierIR,
		StorageClassGlacier,
		StorageClassDeepArchive,
		StorageClassIntelligent,
	}

	listed := StorageClasses()

	for _, class := range declared {
		if !slices.Contains(listed, class) {
			t.Errorf("constant %q is not in StorageClasses(), so ValidateStorageClass rejects a "+
				"class the rest of the code can name", class)
		}
	}

	for _, class := range listed {
		if !slices.Contains(declared, class) {
			t.Errorf("StorageClasses() admits %q, which no exported constant names — callers "+
				"would have to spell it as a literal", class)
		}
	}

	if len(listed) != len(declared) {
		t.Errorf("StorageClasses() has %d entries against %d constants; one of them is duplicated",
			len(listed), len(declared))
	}
}

// FuzzValidateStorageClass asserts the validator is total and that what it accepts is a real class.
//
// Same two properties as FuzzValidateRegion. It runs on operator configuration so it must not panic
// on any string, and anything it accepts is put in an x-amz-storage-class header and acted on by a
// billing table — so an accepted value that is not one of the eight would be a header S3 rejects, or
// worse, a tier lookup that misses and silently falls back.
func FuzzValidateStorageClass(f *testing.F) {
	for _, seed := range []string{
		"", "STANDARD", "standard", "STANDARD_1A", "STANDARD ", "GLACIER_INSTANT_RETRIEVAL",
		"gp3", "\x00", "ünïcode", strings.Repeat("A", 256),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, class string) {
		if err := ValidateStorageClass(class); err != nil {
			return
		}

		if class == "" {
			return
		}

		if !slices.Contains(StorageClasses(), class) {
			t.Fatalf("accepted %q, which is not a storage class ObjectFS can write — it would "+
				"reach S3 as an x-amz-storage-class header value", class)
		}
	})
}
