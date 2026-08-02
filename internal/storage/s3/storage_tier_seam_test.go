package s3_test

// The configured storage tier has to reach the stored object, and the only place that can be checked
// is the stored object.
//
// Every layer between the config and S3 agrees on the tier by construction: ValidateWrite enforces the
// tier's minimum size, the logs name the tier, and ConvertTierToStorageClass has a unit test asserting
// it maps STANDARD_IA to STANDARD_IA. All of that passed while the upload path stored a different class
// entirely, because the class the object is written with is chosen inside the CargoShip transporter
// from a config built alongside — not from the value those layers agree about. A tier defect is silent
// by nature: the object is readable, nothing errors, and the difference shows up on an invoice.

import (
	"context"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/storage/s3"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// TestConfiguredStorageTierReachesTheStoredObject asserts the class the endpoint recorded, for each
// upload path independently.
//
// Both paths must be covered because they choose the class differently and only one of them was wrong.
// The direct path passes ConvertTierToStorageClass(effectiveTier) on the PutObjectInput. The CargoShip
// path hands cargoship an Archive with no AccessPattern and no RetentionDays, and
// Transporter.optimizeStorageClass falls back to its *own* config's StorageClass for exactly that
// shape — which was the constant StorageClassIntelligentTiering. Since EnableCargoShipOptimization is
// true in NewDefaultConfig, that was the live path: `storage_tier: STANDARD_IA` stored
// INTELLIGENT_TIERING.
func TestConfiguredStorageTierReachesTheStoredObject(t *testing.T) {
	t.Parallel()

	// STANDARD_IA and ONEZONE_IA rather than the archive tiers: a Glacier object's body is
	// unavailable, so the assertion that the bytes also survived could not be made. GLACIER_IR is
	// included as the case where the two paths legitimately differ — cargoship has no instant-retrieval
	// class, so it maps to GLACIER. Asserting per-path expectations rather than one shared value is
	// what keeps that from being papered over.
	tiers := []struct {
		tier string

		// direct is the class expected when the object goes out through PutObject itself.
		direct string

		// viaCargoShip is the class expected when the CargoShip transporter carries it. Different from
		// direct only where cargoship's class set is coarser.
		viaCargoShip string
	}{
		{tier: s3.TierStandard, direct: s3.TierStandard, viaCargoShip: s3.TierStandard},
		{tier: s3.TierStandardIA, direct: s3.TierStandardIA, viaCargoShip: s3.TierStandardIA},
		{tier: s3.TierOneZoneIA, direct: s3.TierOneZoneIA, viaCargoShip: s3.TierOneZoneIA},
		{tier: s3.TierIntelligent, direct: s3.TierIntelligent, viaCargoShip: s3.TierIntelligent},
		{tier: s3.TierGlacierIR, direct: s3.TierGlacierIR, viaCargoShip: s3.TierGlacier},
	}

	for _, tc := range tiers {
		t.Run(tc.tier, func(t *testing.T) {
			t.Parallel()

			ts := testaws.Start(t)
			ctx := context.Background()

			// 192 KiB clears the 128 KiB billing minimum the IA tiers enforce on the way in.
			const size = 192 * 1024

			for _, path := range []struct {
				name    string
				cargo   bool
				want    string
				readsOK bool
			}{
				{name: "direct", cargo: false, want: tc.direct, readsOK: tc.tier != s3.TierGlacierIR},
				{name: "cargoship", cargo: true, want: tc.viaCargoShip, readsOK: false},
			} {
				t.Run(path.name, func(t *testing.T) {
					backend := ts.Backend(func(cfg *s3.Config) {
						cfg.StorageTier = tc.tier
						cfg.EnableCargoShipOptimization = path.cargo
						// Compression off: it changes the stored length, and a tier's minimum-size check
						// against a compressed body is its own defect (M23) with its own test.
						cfg.Compression.Enabled = false
					})

					key := "tier/" + tc.tier + "/" + path.name
					want := testaws.DeterministicBytes(key, size)

					if err := backend.PutObject(ctx, key, want, nil); err != nil {
						t.Fatalf("PutObject with storage_tier %q: %v", tc.tier, err)
					}

					if got := ts.ObjectStorageClass(key); got != path.want {
						t.Errorf("storage_tier %q stored the object as %q, want %q. Nothing reports "+
							"this: the object is readable and the config is only visible in the bill.",
							tc.tier, got, path.want)
					}

					// The class is not worth much if the body did not survive the same path. Skipped for
					// classes whose objects cannot be read back — that is the tier working as intended.
					if path.readsOK {
						if got := ts.GetObject(key); string(got) != string(want) {
							t.Errorf("the object holds %d bytes, want %d", len(got), len(want))
						}
					}
				})
			}
		})
	}
}

// TestDefaultConfigStoresStandard checks the shipped default specifically, through the path the
// default actually takes.
//
// NewDefaultConfig sets StorageTier STANDARD and EnableCargoShipOptimization true, which is the exact
// combination that stored INTELLIGENT_TIERING. Worth its own test rather than a row above because a
// default is what most users get, and because the combination — not either setting alone — is what
// produced the defect.
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
