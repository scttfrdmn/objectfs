package s3_test

// The configured storage tier has to reach the stored object, and the only place that can be checked
// is the stored object.
//
// Every layer between the config and S3 agrees on the tier by construction: the minimum-size warning
// names the tier, the logs name the tier, and ConvertTierToStorageClass has a unit test asserting it
// maps STANDARD_IA to STANDARD_IA. All of that passed while the upload path stored a different class
// entirely, because the class the object was written with was chosen inside the CargoShip transporter
// from a config built alongside — not from the value those layers agree about. A tier defect is silent
// by nature: the object is readable, nothing errors, and the difference shows up on an invoice.
//
// That transporter is gone (#362) and there is one upload path, so the class now comes from the same
// value the rest of the stack agrees on. These assertions stay against the endpoint anyway: what makes
// this class of defect invisible is the absence of an assertion at the boundary, not which path was
// installed behind it.

import (
	"context"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// TestConfiguredStorageTierReachesTheStoredObject asserts the class the endpoint recorded.
//
// PutObject passes ConvertTierToStorageClass(effectiveTier) on the PutObjectInput, and this checks the
// value that arrived rather than the value passed in. The distinction is the whole point: the tier
// defect this file was written for was a correct value handed to a path that substituted its own.
func TestConfiguredStorageTierReachesTheStoredObject(t *testing.T) {
	t.Parallel()

	// Mostly STANDARD_IA and ONEZONE_IA rather than the archive tiers: a Glacier object's body is
	// unavailable, so the assertion that the bytes also survived could not be made. GLACIER_IR is
	// included with that assertion skipped, because an unreadable body is the tier working as intended
	// and the class is still worth pinning.
	tiers := []struct {
		tier string

		// readsOK is whether the body can be fetched back on this class.
		readsOK bool
	}{
		{tier: s3.TierStandard, readsOK: true},
		{tier: s3.TierStandardIA, readsOK: true},
		{tier: s3.TierOneZoneIA, readsOK: true},
		{tier: s3.TierIntelligent, readsOK: true},
		{tier: s3.TierGlacierIR, readsOK: false},
	}

	for _, tc := range tiers {
		t.Run(tc.tier, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ctx := context.Background()

			// 192 KiB clears the 128 KiB billing minimum the IA tiers warn about on the way in. Not
			// required since #154 — a smaller object is stored and billed as the minimum rather than
			// refused — but this test is about the storage class, not about the warning.
			const size = 192 * 1024

			backend := ts.Backend(func(cfg *s3.Config) {
				cfg.StorageTier = tc.tier
				// Compression off: it changes the stored length, and a tier's minimum-size check
				// against a compressed body is its own defect (M23) with its own test.
				cfg.Compression.Enabled = false
			})

			key := "tier/" + tc.tier
			want := testaws.DeterministicBytes(key, size)

			if err := backend.PutObject(ctx, key, want, nil); err != nil {
				t.Fatalf("PutObject with storage_tier %q: %v", tc.tier, err)
			}

			if got := ts.ObjectStorageClass(key); got != tc.tier {
				t.Errorf("storage_tier %q stored the object as %q, want %q. Nothing reports this: "+
					"the object is readable and the config is only visible in the bill.",
					tc.tier, got, tc.tier)
			}

			// The class is not worth much if the body did not survive the same path.
			if tc.readsOK {
				if got := ts.GetObject(key); string(got) != string(want) {
					t.Errorf("the object holds %d bytes, want %d", len(got), len(want))
				}
			}
		})
	}
}

// TestDefaultConfigStoresStandard checks the shipped default specifically, through the path the
// default actually takes.
//
// NewDefaultConfig sets StorageTier STANDARD, and with CargoShip also on it stored INTELLIGENT_TIERING
// — the combination, not either setting alone, was the defect. The transporter is gone, so the
// combination cannot recur, but the shipped default is what most users get and is worth asserting
// through the constructor they actually call rather than through a field-by-field config.
func TestDefaultConfigStoresStandard(t *testing.T) {
	t.Parallel()

	ts := testaws.Start(t)
	backend := defaultBackendAgainst(t, ts)
	ctx := context.Background()

	const key = "tier/default"

	// Incompressible, so the shipped compression setting does not change what is stored here.
	want := testaws.DeterministicBytes(key, 192*1024)
	if err := backend.PutObject(ctx, key, want, nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if got := ts.ObjectStorageClass(key); got != s3.TierStandard {
		t.Errorf("the shipped default configuration stored an object as %q, want %q. The default "+
			"names STANDARD and charges for something else.", got, s3.TierStandard)
	}
}

// TestSmallObjectsOnStandardDecidesTheStoredClass asserts the setting's effect where it is visible:
// on the object.
//
// This is the seam #203 is about. The whole cost-optimization block reached the backend through a
// config type that shared no field with the one the backend read, so the only setting in it with a
// live effect was gated by a flag named MonitorAccessPatterns — reporting, deciding storage. A test
// that checked the flag, or checked HandleStandardTierOverhead's return value, would have passed
// throughout: what was broken was whether the configuration could arrive at all. Only the stored
// class can answer that.
func TestSmallObjectsOnStandardDecidesTheStoredClass(t *testing.T) {
	t.Parallel()

	// 16 KiB is under STANDARD_IA's crossover (about 70 KiB at list prices), so it is a size where
	// STANDARD is genuinely cheaper and the setting has something to do.
	const small = 16 * 1024

	for _, tc := range []struct {
		name    string
		enabled bool
		want    string
	}{
		{
			name:    "on stores the small object on Standard",
			enabled: true,
			want:    s3.TierStandard,
		},
		{
			// The default. A mount must store objects on the class storage_tier names unless asked
			// otherwise, which is the half of this that an operator reading their bill depends on.
			name:    "off stores it on the configured tier",
			enabled: false,
			want:    s3.TierStandardIA,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			backend := ts.Backend(func(cfg *s3.Config) {
				cfg.StorageTier = s3.TierStandardIA
				cfg.CostOptimization.SmallObjectsOnStandard = tc.enabled

				// Compression off: it changes the stored length, and the length is what the decision
				// is made on.
				cfg.Compression.Enabled = false
			})

			key := "cost/small-" + tc.want
			body := testaws.DeterministicBytes(key, small)

			if err := backend.PutObject(context.Background(), key, body, nil); err != nil {
				t.Fatalf("PutObject: %v", err)
			}

			if got := ts.ObjectStorageClass(key); got != tc.want {
				t.Errorf("with small_objects_on_standard=%v and storage_tier=STANDARD_IA, a %d-byte "+
					"object was stored as %q, want %q. Nothing reports this: the object reads back "+
					"either way and the difference is only on the invoice.",
					tc.enabled, small, got, tc.want)
			}

			// The bytes have to survive the path that chose the class. A tier decision that also
			// truncated the object would pass the assertion above.
			if got := ts.GetObject(key); string(got) != string(body) {
				t.Errorf("the stored object holds %d bytes, want %d", len(got), len(body))
			}
		})
	}
}
