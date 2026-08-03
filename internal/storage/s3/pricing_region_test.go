package s3

// The region half of cost reporting, which was decorative until #161.
//
// `pricing_config.region` was read at exactly one line in this package — a summary field — while
// every rate came from a map filled at package init. Package init cannot see a configuration, so the
// numbers were unconditionally us-east-1's, and the summary labeled them with whatever region the
// operator had written. That is worse than an unlabeled figure: `region: sa-east-1` above us-east-1
// prices reads as correct, and sa-east-1 storage is 76% more expensive.
//
// These tests assert the three things that had no coverage: a known region yields its own rates, an
// unknown one falls back *and says so*, and the summary distinguishes the region used from the region
// asked for. The fallback is asserted on captured log output rather than on the returned number,
// because a silent fallback and a warned fallback return the same number — and the whole acceptance
// criterion is about which of the two happened.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// capturePricingManager returns a manager whose log output can be read back, so a warning the
// contract promises can be asserted rather than assumed.
func capturePricingManager(t *testing.T, config PricingConfig) (*PricingManager, func() []pricingLogRecord) {
	t.Helper()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return NewPricingManager(config, logger), func() []pricingLogRecord {
		t.Helper()

		var out []pricingLogRecord

		dec := json.NewDecoder(strings.NewReader(buf.String()))
		for {
			var rec pricingLogRecord
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

// pricingLogRecord is one slog line, decoded far enough to read the fields the warning promises.
//
// ConfiguredRegion carries slog's rendering of the "pricing_config.region" key, which is the field
// that makes the warning actionable: a message that says "unknown region" without naming which one
// sends the reader back to the config file to guess.
type pricingLogRecord struct {
	Level            string `json:"level"`
	Msg              string `json:"msg"`
	ConfiguredRegion string `json:"pricing_config.region"`
	Using            string `json:"using"`
	Hint             string `json:"hint"`
}

// TestPricingRegionSelectsTheRates is the test the removed StorageTierInfo.CostPerGBMonth field made
// impossible to write.
//
// The regions are chosen as the extremes of the published range plus one middle case, so the
// assertion fails if the table is region-blind in either direction — a fallback to us-east-1 and a
// stuck-on-one-region bug both show up here. Values are AWS list prices for S3 Standard, first volume
// band; regenerate the table with `go generate ./internal/awsrates/...` if AWS moves them.
func TestPricingRegionSelectsTheRates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		region string
		want   float64
		note   string
	}{
		{"us-east-1", 0.023, "the default, and the price every caller used to get regardless of config"},
		{"us-west-2", 0.023, "same as us-east-1 for Standard — included so a test that only compared " +
			"two same-priced regions cannot be mistaken for coverage"},
		{"eu-central-1", 0.0245, "6.5% above us-east-1"},
		{"sa-east-1", 0.0405, "76% above us-east-1 — the widest commercial gap, and the reason a " +
			"region-blind rate table is a material error rather than a rounding one"},
		{"ap-east-2", 0.0225, "below us-east-1, so a fallback-to-default bug cannot hide behind " +
			"'the number looks plausible'"},
	}

	for _, tc := range cases {
		t.Run(tc.region, func(t *testing.T) {
			t.Parallel()

			pm := NewPricingManager(PricingConfig{Region: tc.region}, discardLogger())

			if got := pm.Region(); got != tc.region {
				t.Fatalf("Region() = %q, want %q — the configured region was not resolved to itself, "+
					"so awsrates has no table for it and every assertion below is about the fallback",
					got, tc.region)
			}

			if got := pm.StorageRate(TierStandard); got != tc.want {
				t.Errorf("StorageRate(STANDARD) in %s = %v, want %v (%s)",
					tc.region, got, tc.want, tc.note)
			}

			// Through the full pricing path as well, not just the accessor: GetTierPricing is what
			// callers on the cost path actually use, and it is where the old package-level map was
			// read.
			pricing, err := pm.GetTierPricing(TierStandard)
			if err != nil {
				t.Fatalf("GetTierPricing: %v", err)
			}

			if pricing.StorageCostPerGBMonth != tc.want {
				t.Errorf("GetTierPricing(STANDARD).StorageCostPerGBMonth in %s = %v, want %v",
					tc.region, pricing.StorageCostPerGBMonth, tc.want)
			}
		})
	}
}

// TestPricingRegionsDisagreeAcrossEveryTier guards the direction the previous test cannot: that the
// region reaches *all* the rates on a tier, not only storage.
//
// A plausible partial implementation reads storage from the regional table and leaves requests,
// retrieval, and egress on the old us-east-1 constants. Every assertion about storage still passes.
// So this compares two regions field by field and requires them to differ where AWS publishes
// different numbers.
func TestPricingRegionsDisagreeAcrossEveryTier(t *testing.T) {
	t.Parallel()

	east, ok := awsrates.ForRegion("us-east-1", TierStandard)
	if !ok {
		t.Fatal("no us-east-1 Standard rate")
	}

	saoPaulo, ok := awsrates.ForRegion("sa-east-1", TierStandard)
	if !ok {
		t.Fatal("no sa-east-1 Standard rate")
	}

	fields := []struct {
		name       string
		east, sao  float64
		mustDiffer bool
	}{
		{"StoragePerGBMonth", east.StoragePerGBMonth, saoPaulo.StoragePerGBMonth, true},
		{"PutRequest", east.PutRequest, saoPaulo.PutRequest, true},
		{"GetRequest", east.GetRequest, saoPaulo.GetRequest, true},
		{"ListRequest", east.ListRequest, saoPaulo.ListRequest, true},
		{"EgressPerGB", east.EgressPerGB, saoPaulo.EgressPerGB, true},
	}

	for _, f := range fields {
		if f.east <= 0 {
			t.Errorf("us-east-1 Standard %s is %v; a zero rate prices the operation at nothing",
				f.name, f.east)
		}

		if f.mustDiffer && f.east == f.sao {
			t.Errorf("Standard %s is %v in both us-east-1 and sa-east-1; AWS publishes different "+
				"numbers, so this field is being served from one region for both", f.name, f.east)
		}
	}
}

// TestPricingUnknownRegionWarnsAndFallsBack is #161's acceptance criterion stated as a test: "an
// unknown region falls back to us-east-1 with a warning rather than returning zero".
//
// All three clauses are asserted, because each has a distinct failure mode. Returning zero prices
// every byte at nothing; falling back silently means an operator who typed the region wrong never
// finds out; and falling back without recording what they asked for makes the report unable to
// explain itself later.
func TestPricingUnknownRegionWarnsAndFallsBack(t *testing.T) {
	t.Parallel()

	const bogus = "mars-1"

	if awsrates.HasRegion(bogus) {
		t.Fatalf("%q is a real region in the rate table; this test needs one that is not", bogus)
	}

	pm, records := capturePricingManager(t, PricingConfig{Region: bogus})

	// Falls back, rather than returning zero.
	if got := pm.Region(); got != awsrates.DefaultRegion {
		t.Errorf("Region() = %q, want the fallback %q", got, awsrates.DefaultRegion)
	}

	east, _ := awsrates.ForRegion(awsrates.DefaultRegion, TierStandard)

	if got := pm.StorageRate(TierStandard); got != east.StoragePerGBMonth {
		t.Errorf("StorageRate(STANDARD) with an unknown region = %v, want the us-east-1 rate %v; "+
			"a zero here would price every stored byte at nothing and read as a free filesystem",
			got, east.StoragePerGBMonth)
	}

	// Warns, and names both regions. Asserted on the structured fields rather than on the message
	// text, so rewording the sentence does not break the test but dropping a field does.
	var warned bool

	for _, rec := range records() {
		if rec.Level != slog.LevelWarn.String() || rec.ConfiguredRegion == "" {
			continue
		}

		warned = true

		if rec.ConfiguredRegion != bogus {
			t.Errorf("warning names pricing_config.region %q, want %q; an operator cannot fix a "+
				"typo the warning does not quote", rec.ConfiguredRegion, bogus)
		}

		if rec.Using != awsrates.DefaultRegion {
			t.Errorf("warning says using %q, want %q; without this the reader knows something is "+
				"wrong but not what the numbers now mean", rec.Using, awsrates.DefaultRegion)
		}

		if rec.Hint == "" {
			t.Error("warning carries no hint; the two real causes — AWS added a region since this " +
				"build, or the region is misspelled — have different fixes, and the hint is where " +
				"that is said")
		}
	}

	if !warned {
		t.Errorf("configuring the unknown region %q produced no warning naming it. The rates "+
			"silently became another region's, which is the failure #161 exists to close: it looks "+
			"exactly like success.", bogus)
	}
}

// TestPricingUnsetRegionIsQuiet is the other half of the fallback contract: an unset region is the
// documented default, not a mistake, so it must not warn.
//
// Without this, the obvious way to satisfy the previous test — warn whenever the resolved region is
// not the configured one — makes every default mount log a warning at startup, and a warning that
// fires on the default configuration is one operators learn to ignore.
func TestPricingUnsetRegionIsQuiet(t *testing.T) {
	t.Parallel()

	pm, records := capturePricingManager(t, PricingConfig{})

	if got := pm.Region(); got != awsrates.DefaultRegion {
		t.Errorf("Region() with no configured region = %q, want %q", got, awsrates.DefaultRegion)
	}

	for _, rec := range records() {
		if rec.Level == slog.LevelWarn.String() {
			t.Errorf("an unset pricing region warned: %q. It is the documented default; warning "+
				"here trains operators to ignore the warning that matters.", rec.Msg)
		}
	}
}

// TestPricingSummaryReportsBothRegions covers the one place in the repo that used to actively assert
// something false.
//
// PricingSummary.Region held pricing_config.region while the rates in the same struct were
// us-east-1's. A reader had no way to detect it: the field and the numbers were individually
// plausible and only jointly wrong.
func TestPricingSummaryReportsBothRegions(t *testing.T) {
	t.Parallel()

	t.Run("known region", func(t *testing.T) {
		t.Parallel()

		summary := NewPricingManager(PricingConfig{Region: "sa-east-1"}, discardLogger()).
			GetPricingSummary()

		if summary.Region != "sa-east-1" {
			t.Errorf("Region = %q, want sa-east-1", summary.Region)
		}

		if summary.ConfiguredRegion != "sa-east-1" {
			t.Errorf("ConfiguredRegion = %q, want sa-east-1", summary.ConfiguredRegion)
		}

		// And the numbers in the same struct agree with the label on it, which is the property that
		// was missing.
		want, _ := awsrates.ForRegion("sa-east-1", TierStandard)

		got := summary.TierPricing[TierStandard]
		if got.StorageCostPerGBMonth != want.StoragePerGBMonth {
			t.Errorf("summary labeled sa-east-1 carries StorageCostPerGBMonth %v; sa-east-1's rate "+
				"is %v. A label that disagrees with its own numbers is worse than no label.",
				got.StorageCostPerGBMonth, want.StoragePerGBMonth)
		}
	})

	t.Run("unknown region", func(t *testing.T) {
		t.Parallel()

		summary := NewPricingManager(PricingConfig{Region: "mars-1"}, discardLogger()).
			GetPricingSummary()

		if summary.Region != awsrates.DefaultRegion {
			t.Errorf("Region = %q, want the region the rates are actually from (%q)",
				summary.Region, awsrates.DefaultRegion)
		}

		if summary.ConfiguredRegion != "mars-1" {
			t.Errorf("ConfiguredRegion = %q, want the operator's value mars-1; carrying it is how "+
				"the fallback stays visible in a report written long after the startup warning "+
				"scrolled away", summary.ConfiguredRegion)
		}
	})
}

// TestPricingNeedsNoCredentialsOrNetwork is the second acceptance criterion: "keep the static path
// working with no credentials and no network — cost estimation must not become a hard dependency on
// another AWS service."
//
// The environment is cleared rather than merely unset, so the test fails if any part of the pricing
// path acquires an AWS SDK client — those resolve credentials at construction and would error, and a
// config file at ~/.aws/config would otherwise mask it. RefreshPricing is included because it is the
// method most likely to grow a network call back.
func TestPricingNeedsNoCredentialsOrNetwork(t *testing.T) {
	// Not parallel: t.Setenv forbids it.
	for _, key := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
		"AWS_EC2_METADATA_DISABLED", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	} {
		t.Setenv(key, "")
	}

	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	pm := NewPricingManager(PricingConfig{Region: "eu-west-1"}, discardLogger())

	if err := pm.RefreshPricing(t.Context()); err != nil {
		t.Errorf("RefreshPricing with no credentials: %v. It is documented as a no-op over a static "+
			"table; an error here means pricing became a hard dependency on a second AWS service, "+
			"and a filesystem that will not mount without the Pricing API is a worse filesystem.",
			err)
	}

	for _, tier := range awsrates.StorageClasses() {
		pricing, err := pm.GetTierPricing(tier)
		if err != nil {
			t.Fatalf("GetTierPricing(%s) with no credentials: %v", tier, err)
		}

		if pricing.StorageCostPerGBMonth <= 0 {
			t.Errorf("GetTierPricing(%s).StorageCostPerGBMonth = %v with no credentials; the static "+
				"table is supposed to answer without them", tier, pricing.StorageCostPerGBMonth)
		}
	}
}

// TestPricingManagerRegionCoversEveryPublishedRegion asserts the manager can resolve every region
// awsrates publishes, with no tier priced at zero in any of them.
//
// 36 regions × 8 classes is a wide enough surface that a generator bug affecting one region — a
// missing SKU silently emitted as 0 — would otherwise reach a release. The generator treats an
// ambiguous lookup as an error, so the gap this catches is a rate that resolved to nothing at all.
func TestPricingManagerRegionCoversEveryPublishedRegion(t *testing.T) {
	t.Parallel()

	regions := awsrates.Regions()
	if len(regions) < 20 {
		t.Fatalf("awsrates publishes only %d regions; the generated table looks truncated",
			len(regions))
	}

	for _, region := range regions {
		t.Run(region, func(t *testing.T) {
			t.Parallel()

			pm, records := capturePricingManager(t, PricingConfig{Region: region})

			if got := pm.Region(); got != region {
				t.Fatalf("Region() = %q, want %q", got, region)
			}

			for _, rec := range records() {
				if rec.Level == slog.LevelWarn.String() && rec.ConfiguredRegion != "" {
					t.Errorf("published region %q warned as unknown: %q", region, rec.Msg)
				}
			}

			for tier := range StorageTiers {
				if got := pm.StorageRate(tier); got <= 0 {
					t.Errorf("StorageRate(%s) in %s = %v; no S3 class is free anywhere",
						tier, region, got)
				}
			}
		})
	}
}
