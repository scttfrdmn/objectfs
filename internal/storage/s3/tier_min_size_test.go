package s3

// The tier minimum-size gate, which used to refuse writes AWS accepts.
//
// AWS publishes a 128 KiB minimum billable object size for STANDARD_IA, ONEZONE_IA, and GLACIER_IR.
// It is a pricing floor: S3 stores a 1-byte object on those classes and bills it as 128 KiB. It has
// never been an API restriction. `ValidateWrite` enforced it as one, and `internal/fuse` creates
// both directory markers and empty files by PUTting zero bytes — so a mount on any of those three
// tiers could not create anything at all, and an IA-tier integration test could not even set itself
// up (#154).
//
// These tests assert the two halves of the new contract separately, because they have to be able to
// disagree: AWS's floor warns, and an operator's explicitly configured floor still refuses.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// logRecord is one slog line, decoded far enough to read the fields the warning promises.
type logRecord struct {
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	Size       *int64 `json:"size"`
	BilledSize *int64 `json:"billed_size"`
	Tier       string `json:"tier"`
	Key        string `json:"key"`
}

// captureValidator returns a validator whose log output is readable, so the warning can be asserted
// on rather than assumed. A test that only checks the error is nil cannot tell "warned about the
// billing" from "said nothing", and saying nothing is the failure mode that matters here: the write
// succeeds either way, and the operator finds out from an invoice.
func captureValidator(t *testing.T, tier string, constraints TierConstraints) (*TierValidator, func() []logRecord) {
	t.Helper()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return NewTierValidator(awsrates.DefaultRegion, tier, constraints, logger), func() []logRecord {
		t.Helper()

		var out []logRecord

		dec := json.NewDecoder(strings.NewReader(buf.String()))
		for {
			var rec logRecord
			if err := dec.Decode(&rec); err != nil {
				if err == io.EOF {
					break
				}

				t.Fatalf("decoding captured log output: %v\nraw:\n%s", err, buf.String())
			}

			out = append(out, rec)
		}

		return out
	}
}

// TestBillingMinimumDoesNotRefuseTheWrite is the defect itself: the sizes a FUSE mount actually
// PUTs, on the tiers that have a minimum.
func TestBillingMinimumDoesNotRefuseTheWrite(t *testing.T) {
	t.Parallel()

	// Every tier with a non-zero MinObjectSize, read from StorageTiers rather than listed, so a tier
	// that gains a minimum later is covered without editing this test.
	var tiers []string

	for tier, info := range StorageTiers {
		if info.MinObjectSize > 0 {
			tiers = append(tiers, tier)
		}
	}

	if len(tiers) == 0 {
		t.Fatal("no tier in StorageTiers has a MinObjectSize, so this test asserts nothing; if the " +
			"field was removed, remove this test with it rather than leaving it passing vacuously")
	}

	// Zero is the size that broke the filesystem — Mkdir and Create both PUT an empty body. The
	// others are the small-file sizes a research workload has plenty of.
	sizes := []int64{0, 1, 4096, 40 * 1024}

	for _, tier := range tiers {
		for _, size := range sizes {
			t.Run(tier, func(t *testing.T) {
				t.Parallel()

				validator, logs := captureValidator(t, tier, TierConstraints{})

				if err := validator.ValidateWrite("dir/", size); err != nil {
					t.Fatalf("ValidateWrite(%d bytes) on %s = %v, want nil.\nAWS's %d-byte minimum is a "+
						"billing floor, not an API restriction — S3 accepts this write. Refusing it means "+
						"mkdir and touch fail on this tier, since both PUT zero bytes",
						size, tier, err, StorageTiers[tier].MinObjectSize)
				}

				// The warning has to carry both numbers, because the actionable fact is the gap between
				// them. "object is small" is not a reason to change anything; "48 bytes will be billed as
				// 131072" is.
				var warned bool

				for _, rec := range logs() {
					if rec.Level != "WARN" || rec.BilledSize == nil || rec.Size == nil {
						continue
					}

					if *rec.BilledSize != StorageTiers[tier].MinObjectSize {
						continue
					}

					if *rec.Size != size {
						t.Errorf("the billing warning reported size=%d for a %d-byte write", *rec.Size, size)
					}

					if rec.Tier != tier {
						t.Errorf("the billing warning reported tier=%q, want %q", rec.Tier, tier)
					}

					if rec.Key != "dir/" {
						t.Errorf("the billing warning reported key=%q, want %q", rec.Key, "dir/")
					}

					warned = true
				}

				if !warned {
					t.Errorf("a %d-byte write to %s was accepted with no billing warning naming both the "+
						"written size and the %d bytes it is billed as.\nAccepting the write silently is "+
						"the other half of this defect: the operator pays the minimum for every small "+
						"object and the first report of it is the bill.\nCaptured records: %+v",
						size, tier, StorageTiers[tier].MinObjectSize, logs())
				}
			})
		}
	}
}

// TestConfiguredMinimumStillRefusesTheWrite is the half that must not have been relaxed. An operator
// who sets tier_constraints.min_object_size has asked for a floor that is not AWS's; that is a
// deliberate policy and the only kind worth enforcing.
func TestConfiguredMinimumStillRefusesTheWrite(t *testing.T) {
	t.Parallel()

	const configured = 256 * 1024

	// STANDARD deliberately: it has no minimum of its own, so a refusal here can only have come from
	// the constraint. Asserting this on STANDARD_IA would pass even if the code were reading the
	// tier's 128 KiB and ignoring the constraint entirely.
	validator, _ := captureValidator(t, TierStandard, TierConstraints{MinObjectSize: configured})

	err := validator.ValidateWrite("small.bin", 128*1024)
	if err == nil {
		t.Fatal("ValidateWrite accepted a 128 KiB write under a configured 256 KiB minimum on " +
			"STANDARD, a tier with no minimum of its own; the operator's floor is not being read")
	}

	// The message has to say whose rule this is. A refusal that reads as an S3 limitation sends the
	// operator to the AWS documentation, where they will find that S3 accepts the write.
	for _, want := range []string{"configured minimum", "min_object_size", "ObjectFS policy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not contain %q, so it does not distinguish itself from an S3 "+
				"limit the operator would go looking for in the AWS docs: %v", want, err)
		}
	}

	if err := validator.ValidateWrite("big.bin", configured); err != nil {
		t.Errorf("ValidateWrite at exactly the configured minimum = %v, want nil; the floor is "+
			"inclusive", err)
	}
}

// TestBillingWarningFollowsTheTierTheObjectIsStoredOn covers the other direction of the split: the
// billing warnings describe AWS's charges, so they have to follow the tier the object actually lands
// on rather than the one configured for the mount.
//
// `small_objects_on_standard` diverts an individual object to STANDARD when the configured tier would
// bill it as larger than it is, and the validator is built once per mount. Validating against the
// configured tier regardless produced the exactly-backwards message: an operator who enabled the
// setting *because of* this warning still saw "billed_size 131072" on a 16 KiB object about to be
// stored on STANDARD and billed as 16 KiB. Worse at the default log level, where the diversion's own
// Debug line is invisible and the misleading warning is all there is.
func TestBillingWarningFollowsTheTierTheObjectIsStoredOn(t *testing.T) {
	t.Parallel()

	// 16 KiB: under STANDARD_IA's 128 KiB floor, so the configured tier would warn.
	const size = 16 * 1024

	t.Run("diverted to Standard warns about nothing", func(t *testing.T) {
		t.Parallel()

		validator, logs := captureValidator(t, TierStandardIA, TierConstraints{})

		if err := validator.ValidateWriteToTier("small.bin", size, TierStandard); err != nil {
			t.Fatalf("ValidateWriteToTier(..., STANDARD) = %v, want nil", err)
		}

		for _, rec := range logs() {
			if rec.Level == "WARN" {
				t.Errorf("an object being stored on STANDARD drew the warning %q naming tier=%q: %+v.\n"+
					"STANDARD has no minimum billable size, so there is nothing to warn about — this is "+
					"the configured tier's floor being reported for a write that will not be billed "+
					"against it.", rec.Msg, rec.Tier, rec)
			}
		}
	})

	t.Run("not diverted still warns about the configured tier", func(t *testing.T) {
		t.Parallel()

		// The half that must not have been silenced. Passing the configured tier explicitly is what
		// PutObject does when no diversion happened, and it has to behave exactly like ValidateWrite.
		validator, logs := captureValidator(t, TierStandardIA, TierConstraints{})

		if err := validator.ValidateWriteToTier("small.bin", size, TierStandardIA); err != nil {
			t.Fatalf("ValidateWriteToTier(..., STANDARD_IA) = %v, want nil", err)
		}

		var warned bool

		for _, rec := range logs() {
			if rec.Level == "WARN" && rec.BilledSize != nil && *rec.BilledSize == StorageTiers[TierStandardIA].MinObjectSize {
				if rec.Tier != TierStandardIA {
					t.Errorf("the billing warning reported tier=%q, want %q", rec.Tier, TierStandardIA)
				}

				warned = true
			}
		}

		if !warned {
			t.Errorf("a %d-byte write to STANDARD_IA with no diversion drew no billing warning naming "+
				"its 128 KiB floor. Making the warning tier-aware must not silence it for the case it "+
				"was written for.\nCaptured: %+v", size, logs())
		}
	})

	t.Run("an unknown tier name falls back to the configured tier", func(t *testing.T) {
		t.Parallel()

		// Zero-valued StorageTierInfo means "no minimum, no overhead", so an unrecognized name must not
		// reach the checks as one: it would silently warn about nothing, which is indistinguishable from
		// a correct diversion to STANDARD.
		validator, logs := captureValidator(t, TierStandardIA, TierConstraints{})

		if err := validator.ValidateWriteToTier("small.bin", size, "GLACIER_DEEP_ARCHIVE_XL"); err != nil {
			t.Fatalf("ValidateWriteToTier with an unknown tier = %v, want nil", err)
		}

		var warned bool

		for _, rec := range logs() {
			if rec.Level == "WARN" && rec.Tier == TierStandardIA {
				warned = true
			}
		}

		if !warned {
			t.Errorf("an unrecognized tier name silenced the billing warning instead of falling back to "+
				"the configured STANDARD_IA.\nCaptured: %+v", logs())
		}
	})

	t.Run("the operator's floor is not relaxed by a diversion", func(t *testing.T) {
		t.Parallel()

		// tier_constraints.min_object_size is a policy for this mount, not a description of AWS billing.
		// An internal cost diversion is not a reason to stop enforcing it, and letting it be one would
		// make the floor quietly conditional on an unrelated setting.
		validator, _ := captureValidator(t, TierStandardIA, TierConstraints{MinObjectSize: 64 * 1024})

		if err := validator.ValidateWriteToTier("small.bin", size, TierStandard); err == nil {
			t.Error("a diversion to STANDARD bypassed the configured 64 KiB minimum. The operator's " +
				"floor applies to the mount; a cost optimization deciding not to enforce it is a policy " +
				"change nobody asked for")
		}
	})
}

// TestConfiguringTheTierMinimumRestoresTheOldBehavior records the sharp edge the split leaves, so
// that it is a documented consequence rather than a surprise. Setting the constraint to the tier's
// own published number is a way to reinstate exactly the gate #154 removed, zero-byte directory
// markers included.
func TestConfiguringTheTierMinimumRestoresTheOldBehavior(t *testing.T) {
	t.Parallel()

	info := StorageTiers[TierStandardIA]
	if info.MinObjectSize == 0 {
		t.Skip("STANDARD_IA no longer has a minimum billable size")
	}

	validator, _ := captureValidator(t, TierStandardIA, TierConstraints{MinObjectSize: info.MinObjectSize})

	if err := validator.ValidateWrite("dir/", 0); err == nil {
		t.Error("setting tier_constraints.min_object_size to STANDARD_IA's own 128 KiB did not refuse " +
			"a zero-byte write.\nIt should: that is an explicit operator policy and enforcing it is " +
			"the point. This test exists so that the consequence — mkdir and touch fail again under " +
			"that setting — stays a known, tested property rather than a rediscovered bug")
	}
}
