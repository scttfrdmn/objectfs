package offerfile

// One test per extraction rule, each named for the case that forced the rule.
//
// The package comment records the rules and the observed AWS shapes that make them necessary; these
// tests are that document made executable. A rule without a test here is a rule someone can delete
// during a cleanup and see everything still pass — which is how the previous rate table came to hold a
// restore price in a PUT field.

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates/offerfile/offertest"
)

// TestExtractReadsEveryRate is the baseline: a well-formed file yields every rate for every class.
//
// Both a prefixed region and us-east-1, from the same fixture builder, because the empty prefix is a
// case in its own right — see TestEmptyPrefixIsUsEast1.
func TestExtractReadsEveryRate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		region, prefix, location string
	}{
		{"us-east-1", "", "US East (N. Virginia)"},
		{"us-west-2", "USW2-", "US West (Oregon)"},
		{"eu-west-1", "EU-", "EU (Ireland)"},
	}

	for _, tc := range cases {
		t.Run(tc.region, func(t *testing.T) {
			t.Parallel()

			region, err := Extract(tc.region,
				offertest.CompleteS3Offer(tc.prefix, tc.location).JSON(t),
				offertest.CompleteDataTransfer(tc.location).JSON(t))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}

			if region.Code != tc.region {
				t.Errorf("Code = %q, want %q", region.Code, tc.region)
			}

			if region.Prefix != tc.prefix {
				t.Errorf("Prefix = %q, want %q", region.Prefix, tc.prefix)
			}

			if region.Location != tc.location {
				t.Errorf("Location = %q, want %q; the AWSDataTransfer file can only be matched on "+
					"it", region.Location, tc.location)
			}

			// Every class awsname admits, not just the ones with obvious rates: a class missing from
			// the result is priced at zero by the consumer.
			for _, class := range awsname.StorageClasses() {
				want, ok := offertest.WantRates[class]
				if !ok {
					t.Fatalf("this test has no expectation for storage class %q; awsname admits it, "+
						"so it needs one", class)
				}

				got, ok := region.Rates[class]
				if !ok {
					t.Errorf("no rate extracted for %s", class)
					continue
				}

				for _, f := range []struct {
					name      string
					got, want float64
				}{
					{"StoragePerGBMonth", got.StoragePerGBMonth, want.Storage},
					{"PutRequest", got.PutRequest, want.Put},
					{"GetRequest", got.GetRequest, want.Get},
					{"ListRequest", got.ListRequest, want.List},
					{"RetrievalPerGB", got.RetrievalPerGB, want.Retrieval},
					{"EgressPerGB", got.EgressPerGB, offertest.WantEgress},
				} {
					if f.got != f.want {
						t.Errorf("%s.%s = %v, want %v", class, f.name, f.got, f.want)
					}
				}
			}
		})
	}
}

// TestEmptyPrefixIsUsEast1 pins the case that broke the first probe written against these files.
//
// us-east-1's usagetypes carry no region prefix at all — the Standard storage product is
// "TimedStorage-ByteHrs", not "USE1-TimedStorage-ByteHrs". The first version of this extraction
// filtered on a "USE1-" prefix and returned two rates out of twenty-four. The reason the case needs
// its own test is the inverse: a prefix-derivation bug that returns "" for every region looks correct
// in us-east-1, so only a prefixed region catches it, and only us-east-1 catches a bug that assumes
// there is always a prefix.
func TestEmptyPrefixIsUsEast1(t *testing.T) {
	t.Parallel()

	region, err := Extract("us-east-1",
		offertest.CompleteS3Offer("", "US East (N. Virginia)").JSON(t),
		offertest.CompleteDataTransfer("US East (N. Virginia)").JSON(t))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if region.Prefix != "" {
		t.Errorf("Prefix = %q, want the empty string", region.Prefix)
	}

	if got := region.Rates[awsname.StorageClassStandard].StoragePerGBMonth; got != 0.023 {
		t.Errorf("Standard storage = %v, want 0.023", got)
	}
}

// TestSuffixCollisionsAreNotStandardStorage is the strings.HasSuffix rule.
//
// The real us-west-2 file publishes USW2-Tables-TimedStorage-ByteHrs, -Annotation-, -Files- and
// -Vectors- variants, all of which end in TimedStorage-ByteHrs. S3 Tables storage is priced well above
// Standard, so a suffix match reports Standard storage at whichever of them map iteration reaches
// first — a wrong number that changes between runs.
// It is run for us-east-1 as well as us-west-2, and that is not symmetry for its own sake. The two
// exercise different code:
//
//   - With a prefix, lookup compares against "USW2-TimedStorage-ByteHrs", and
//     "USW2-Tables-TimedStorage-ByteHrs" is not a suffix of it — the collision is an infix. So a
//     suffix-matching lookup is still correct there, and only derivePrefix is at risk.
//   - With no prefix, lookup compares against "TimedStorage-ByteHrs", of which
//     "Tables-TimedStorage-ByteHrs" *is* a suffix. So us-east-1 is the only region where the exact
//     match in lookup is load-bearing — and us-east-1 is the default region and the fallback for
//     every unknown one.
//
// Mutation-testing found this: replacing lookup's exact comparison with strings.HasSuffix passed the
// us-west-2 case and every other test in the package.
func TestSuffixCollisionsAreNotStandardStorage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ region, prefix, location string }{
		{"us-east-1", "", "US East (N. Virginia)"},
		{"us-west-2", "USW2-", "US West (Oregon)"},
	} {
		t.Run(tc.region, func(t *testing.T) {
			t.Parallel()
			assertSuffixCollisionsExcluded(t, tc.region, tc.prefix, tc.location)
		})
	}
}

func assertSuffixCollisionsExcluded(t *testing.T, region, prefix, location string) {
	t.Helper()

	f := offertest.CompleteS3Offer(prefix, location)

	// The four variants as the live us-west-2 file publishes them, attribute for attribute. Two omit
	// productFamily entirely, which is the common case — 315 of us-east-1's 381 products do. The
	// Annotation- one is the interesting member of this group: its storageClass is "General Purpose",
	// the same as the Standard anchor's, so only volumeType tells them apart. Substituting a guessed
	// set of attributes here would make the test easier to pass and would stop testing the collision.
	for _, variant := range []struct {
		infix, family, volumeType, storageClass, usd string
	}{
		{"Tables-", "", "Tables", "Analytics", "0.0265000000"},
		{"Annotation-", "Storage", "Annotations", "General Purpose", "0.0300000000"},
		{"Files-", "Storage", "Files", "Files", "0.0400000000"},
		{"Vectors-", "", "Vectors", "Vectors", "0.0600000000"},
	} {
		f.AddProduct("SKU-"+variant.infix, variant.family, map[string]string{
			"usagetype":    prefix + variant.infix + "TimedStorage-ByteHrs",
			"volumeType":   variant.volumeType,
			"storageClass": variant.storageClass,
			"location":     location,
		}, variant.usd)
	}

	// Tables-TimedStorage-INT-FA-ByteHrs, which is the collision that actually depends on the exact
	// match. The Standard-storage colliders above all carry a distinct volumeType, and the Standard
	// query checks volumeType, so they are excluded twice over — replacing lookup's exact comparison
	// with strings.HasSuffix left every assertion above passing. This one is different: the
	// Intelligent-Tiering query has no volumeType clause to fall back on, and neither product carries
	// an operation, so the exact match is the only thing separating them. In the live us-east-1 file it
	// is priced at $0.0265 against Intelligent-Tiering's $0.023, and it is the single query out of the
	// 27 in byClass that a suffix match would get wrong.
	f.AddProduct("SKU-Tables-INT-FA", "", map[string]string{
		"usagetype":    prefix + "Tables-TimedStorage-INT-FA-ByteHrs",
		"volumeType":   "Tables",
		"storageClass": "Analytics",
		"location":     location,
	}, "0.0265000000")

	extracted, err := Extract(region, f.JSON(t), offertest.CompleteDataTransfer(location).JSON(t))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if got := extracted.Rates[awsname.StorageClassStandard].StoragePerGBMonth; got != 0.023 {
		t.Errorf("Standard storage = %v, want 0.023. A colliding usagetype was matched: S3 Tables, "+
			"Annotation, Files and Vectors storage all end in TimedStorage-ByteHrs, and each is "+
			"priced above Standard.", got)
	}

	if got := extracted.Rates[awsname.StorageClassIntelligent].StoragePerGBMonth; got != 0.023 {
		t.Errorf("Intelligent-Tiering storage = %v, want 0.023. Tables-TimedStorage-INT-FA-ByteHrs "+
			"was matched, at $0.0265. This is the one query of the 27 in byClass where lookup's "+
			"exact usagetype comparison is the only guard: the query carries no volumeType clause, "+
			"and neither product carries an operation.", got)
	}

	// And the prefix derivation, which uses the only HasSuffix in the package, must not have picked
	// one of them either — that would shift every other lookup in the file.
	if extracted.Prefix != prefix {
		t.Errorf("Prefix = %q, want %q; a colliding product was used as the derivation anchor, "+
			"which moves every usagetype lookup in the file", extracted.Prefix, prefix)
	}
}

// TestOperationSeparatesAmbiguousUsagetypes is the rule the Glacier PUT defect came from.
//
// Requests-Tier3 resolves to three SKUs at two prices, and Standard-Retrieval-Bytes to two SKUs at
// two prices. The fixture carries both, so a query stripped of its operation attribute becomes
// ambiguous rather than merely wrong — and ambiguity is an error here, which is the property that
// makes the failure loud instead of order-dependent.
func TestOperationSeparatesAmbiguousUsagetypes(t *testing.T) {
	t.Parallel()

	body := offertest.CompleteS3Offer("", "US East (N. Virginia)").JSON(t)

	cases := []struct {
		name string
		q    query
		want float64
		why  string
	}{
		{
			name: "glacier PUT is not a restore",
			q:    query{usagetype: "Requests-GLACIER-Tier1", operation: "PutObject"},
			want: 0.00003,
			why: "the rate this replaced was 0.00005 from Requests-Tier3, which is the price of " +
				"thawing an object — 67% above the PUT",
		},
		{
			name: "deep archive transition, not glacier transition",
			q:    query{usagetype: "Requests-Tier3", operation: "S3-GDATransition"},
			want: 0.00005,
			why:  "the same usagetype carries S3-GlacierTransition at 0.00003",
		},
		{
			name: "glacier transition, not deep archive",
			q:    query{usagetype: "Requests-Tier3", operation: "S3-GlacierTransition"},
			want: 0.00003,
			why:  "the opposite direction of the previous case, so neither can pass by accident",
		},
		{
			name: "glacier retrieval",
			q:    query{usagetype: "Standard-Retrieval-Bytes", operation: "RestoreObject"},
			want: 0.01,
			why:  "DeepArchiveRestoreObject on the same usagetype is 0.02, twice this",
		},
		{
			name: "deep archive retrieval",
			q:    query{usagetype: "Standard-Retrieval-Bytes", operation: "DeepArchiveRestoreObject"},
			want: 0.02,
			why:  "and RestoreObject is 0.01, half this",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			o := parse(t, body)

			got, err := lookup(o, "", tc.q)
			if err != nil {
				t.Fatalf("lookup(%s): %v", tc.q, err)
			}

			if got != tc.want {
				t.Errorf("lookup(%s) = %v, want %v — %s", tc.q, got, tc.want, tc.why)
			}

			// The same query with the operation removed must fail rather than return one of the
			// prices. This is what makes the operation field load-bearing: without it, dropping the
			// field from byClass would still produce a table, of numbers from whichever SKU map
			// iteration reached.
			//
			// It fails as "not found" rather than "ambiguous", because an empty operation means the
			// product's operation must also be empty and none of these SKUs has one — checked
			// against the live us-east-1 file, where every SKU on all three of these usagetypes
			// carries an operation. Either error is acceptable here; returning a price is not.
			bare := query{usagetype: tc.q.usagetype}

			if _, err := lookup(o, "", bare); err == nil {
				t.Errorf("lookup(%s) with no operation succeeded; the SKUs on this usagetype "+
					"publish two different prices, so a query that cannot tell them apart must "+
					"fail rather than return whichever one Go's map iteration reached first", bare)
			}
		})
	}
}

// TestAmbiguousSKUsAcrossOneQueryAreAnError covers lookup's disagreement branch directly.
//
// No live region has two SKUs agreeing on usagetype, operation and volumeType while disagreeing on
// price — verified across all 36 rate-bearing regions, where every such group holds exactly one
// price. So this shape has to be constructed, and it has to be constructed rather than skipped: the
// branch is the reason a wrong rate cannot be introduced quietly, and an untested error branch is one
// a later refactor can turn into a silent pick without any test noticing.
func TestAmbiguousSKUsAcrossOneQueryAreAnError(t *testing.T) {
	t.Parallel()

	f := &offertest.Fixture{}

	for i, usd := range []string{"0.0000050000", "0.0000090000"} {
		f.AddProduct("SKU-DUP-"+string(rune('A'+i)), "API Request", map[string]string{
			"usagetype": "Requests-Tier1",
		}, usd)
	}

	_, err := lookup(parse(t, f.JSON(t)), "", query{usagetype: "Requests-Tier1"})
	if err == nil {
		t.Fatal("two SKUs on the same usagetype at different prices resolved to one of them; which " +
			"one depends on map iteration order, so the rate would differ between generation runs")
	}

	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error is %q; want it to name the ambiguity and the SKUs, since the fix is to add "+
			"an attribute to the query and the reader needs to know which SKUs to compare", err)
	}

	// Both SKU names, so the error is actionable rather than merely correct.
	for _, sku := range []string{"SKU-DUP-A", "SKU-DUP-B"} {
		if !strings.Contains(err.Error(), sku) {
			t.Errorf("error %q does not name SKU %s", err, sku)
		}
	}
}

// TestEmptyOperationMeansAbsentNotAny is the other half of the operation rule.
//
// A query with no operation must match only products that publish no operation. Treating empty as a
// wildcard would make Requests-Tier1 — the Standard PUT and the LIST rate for every class — match the
// operation-bearing SKUs on the same usagetype, and the rate would become order-dependent again.
func TestEmptyOperationMeansAbsentNotAny(t *testing.T) {
	t.Parallel()

	f := offertest.CompleteS3Offer("", "US East (N. Virginia)")

	// A Requests-Tier1 SKU with an operation, priced differently. The real files have these.
	f.AddProduct("SKU-TIER1-COPY", "API Request", map[string]string{
		"usagetype": "Requests-Tier1",
		"operation": "CopyObject",
		"location":  "US East (N. Virginia)",
	}, "0.0000090000")

	o := parse(t, f.JSON(t))

	got, err := lookup(o, "", query{usagetype: "Requests-Tier1"})
	if err != nil {
		t.Fatalf("lookup(Requests-Tier1): %v", err)
	}

	if got != 0.000005 {
		t.Errorf("lookup(Requests-Tier1) = %v, want 0.000005. An operation-bearing SKU on the same "+
			"usagetype was matched, which means an empty operation is being treated as a wildcard.",
			got)
	}
}

// TestApSoutheast6OverheadSKUsAreExcludedByOperation is the ap-southeast-6 case as the live file
// actually publishes it.
//
// That region has three SKUs on APS8-TimedStorage-ByteHrs: Standard at $0.02625 and two
// Intelligent-Tiering object-overhead SKUs at $0.02415. The attributes below are copied from the live
// file, including the detail that makes this a weaker case than it first looked: the overhead SKUs
// carry an operation, so the empty-operation rule excludes them and volumeType is not what saves the
// query. See the package comment.
func TestApSoutheast6OverheadSKUsAreExcludedByOperation(t *testing.T) {
	t.Parallel()

	f := offertest.CompleteS3Offer("APS8-", "Asia Pacific (New Zealand)")

	for _, overhead := range []struct {
		sku, volumeType, operation string
	}{
		{"SKU-INTAA-OVERHEAD", "INTAAS3ObjectOverhead", "INTAAS3ObjectOverhead"},
		{"SKU-INTDAA-OVERHEAD", "INTDAAObjectOverhead", "INTDAAS3ObjectOverhead"},
	} {
		f.AddProduct(overhead.sku, "Storage", map[string]string{
			"usagetype":    "APS8-TimedStorage-ByteHrs",
			"volumeType":   overhead.volumeType,
			"storageClass": "Intelligent-Tiering",
			"operation":    overhead.operation,
			"location":     "Asia Pacific (New Zealand)",
		}, "0.0241500000")
	}

	o := parse(t, f.JSON(t))

	// The query as byClass has it.
	got, err := lookup(o, "APS8-", query{usagetype: "TimedStorage-ByteHrs", volumeType: "Standard"})
	if err != nil {
		t.Fatalf("lookup with volumeType: %v", err)
	}

	if got != 0.023 {
		t.Errorf("Standard storage = %v, want 0.023 (the fixture's Standard band)", got)
	}

	// And without volumeType, which is what the 36-region probe found: still unambiguous, because the
	// operation attribute already excluded the overhead SKUs. Asserted rather than left implicit, so
	// the redundancy is a recorded fact instead of something a later reader has to re-derive.
	bare, err := lookup(o, "APS8-", query{usagetype: "TimedStorage-ByteHrs"})
	if err != nil {
		t.Fatalf("lookup without volumeType: %v; the live overhead SKUs carry an operation, so the "+
			"empty-operation rule should already have excluded them", err)
	}

	if bare != 0.023 {
		t.Errorf("Standard storage without the volumeType clause = %v, want 0.023", bare)
	}
}

// TestVolumeTypeSeparatesAnOperationlessOverheadSKU is what the redundant volumeType clause would
// catch if AWS added an overhead SKU with no operation.
//
// Not a shape any live file has. It is the reason the clause is kept: AWS has added SKUs to this
// usagetype repeatedly — Tables, Annotation, Files, Vectors — and an operationless one would otherwise
// make Standard storage ambiguous in that region and fail the whole generation run for a region that
// had been fine.
func TestVolumeTypeSeparatesAnOperationlessOverheadSKU(t *testing.T) {
	t.Parallel()

	f := offertest.CompleteS3Offer("APS8-", "Asia Pacific (New Zealand)")
	f.AddProduct("SKU-HYPOTHETICAL-OVERHEAD", "Storage", map[string]string{
		"usagetype":    "APS8-TimedStorage-ByteHrs",
		"volumeType":   "SomeFutureOverhead",
		"storageClass": "Intelligent-Tiering",
		"location":     "Asia Pacific (New Zealand)",
	}, "0.0241500000")

	o := parse(t, f.JSON(t))

	got, err := lookup(o, "APS8-", query{usagetype: "TimedStorage-ByteHrs", volumeType: "Standard"})
	if err != nil {
		t.Fatalf("lookup with volumeType: %v", err)
	}

	if got != 0.023 {
		t.Errorf("Standard storage = %v, want 0.023", got)
	}

	// The same lookup without the clause is ambiguous, which is the whole value of keeping it.
	if _, err := lookup(o, "APS8-", query{usagetype: "TimedStorage-ByteHrs"}); err == nil {
		t.Error("lookup without volumeType succeeded against an operationless overhead SKU; that is " +
			"the case the volumeType clause exists for, so if this passes the clause can go")
	}
}

// TestBandSelectionTakesBeginRangeZero pins the volume-band rule.
//
// Standard storage is three price dimensions in one SKU. Taking the first entry of the priceDimensions
// map gives a different band on different runs, because Go randomizes map iteration order — so the
// bug this prevents is not a wrong number but an inconsistent one, which is harder to notice and
// harder to reproduce.
func TestBandSelectionTakesBeginRangeZero(t *testing.T) {
	t.Parallel()

	body := offertest.CompleteS3Offer("", "US East (N. Virginia)").JSON(t)

	// Run it enough times that a map-order-dependent implementation would be very unlikely to return
	// the same band every time. Cheap: the fixture is small and already parsed per iteration.
	for i := range 50 {
		o := parse(t, body)

		got, err := lookup(o, "", query{usagetype: "TimedStorage-ByteHrs", volumeType: "Standard"})
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}

		if got != 0.023 {
			t.Fatalf("iteration %d: Standard storage = %v, want the beginRange-0 band 0.023; the "+
				"other bands in this SKU are 0.022 and 0.021", i, got)
		}
	}
}

// TestAbsentBeginRangeCountsAsTheFirstBand covers the single-band shape that omits the field.
//
// Some products publish one dimension with no beginRange at all. Requiring the literal string "0"
// would report those as not found, and "not found" on a request rate fails the whole region.
func TestAbsentBeginRangeCountsAsTheFirstBand(t *testing.T) {
	t.Parallel()

	f := &offertest.Fixture{}
	f.AddBanded("SKU-NOBAND", "API Request", map[string]string{
		"usagetype": "Requests-Tier1",
	}, []offertest.Band{{USD: "0.0000050000"}}) // no beginRange

	got, err := lookup(parse(t, f.JSON(t)), "", query{usagetype: "Requests-Tier1"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	if got != 0.000005 {
		t.Errorf("got %v, want 0.000005", got)
	}
}

// TestNoS3RatesIsDistinguishable is the local-zone case.
//
// Three entries in the S3 region index — ap-southeast-1-han-1, eu-central-1-ath-1,
// eu-central-1-ist-1 — are well-formed files with no S3 storage product at all. The generator skips
// them by name in its output, and it can only do that safely if the error is distinct from a real
// parse failure: a sentinel-matched skip cannot swallow a malformed file.
func TestNoS3RatesIsDistinguishable(t *testing.T) {
	t.Parallel()

	t.Run("no storage product", func(t *testing.T) {
		t.Parallel()

		f := &offertest.Fixture{}
		f.AddProduct("SKU-REQ", "API Request", map[string]string{
			"usagetype": "APS1-HAN1-Requests-Tier1",
		}, "0.0000050000")

		_, err := Extract("ap-southeast-1-han-1", f.JSON(t), nil)
		if !errors.Is(err, ErrNoS3Rates) {
			t.Errorf("Extract on a local-zone-shaped file: %v, want ErrNoS3Rates", err)
		}
	})

	t.Run("malformed JSON is not ErrNoS3Rates", func(t *testing.T) {
		t.Parallel()

		_, err := Extract("us-east-1", []byte("{not json"), nil)
		if err == nil {
			t.Fatal("Extract on malformed JSON succeeded")
		}

		if errors.Is(err, ErrNoS3Rates) {
			t.Error("malformed JSON reported as ErrNoS3Rates; the generator skips that sentinel, so " +
				"a truncated download would be silently omitted from the table rather than failing " +
				"the build")
		}
	})

	t.Run("missing rate is not ErrNoS3Rates", func(t *testing.T) {
		t.Parallel()

		// A file with the anchor product and nothing else: the prefix derives, and then every
		// request lookup fails.
		f := &offertest.Fixture{}
		f.AddProduct("SKU-STD", "Storage", map[string]string{
			"usagetype":    "TimedStorage-ByteHrs",
			"volumeType":   "Standard",
			"storageClass": "General Purpose",
			"location":     "US East (N. Virginia)",
		}, "0.0230000000")

		_, err := Extract("us-east-1", f.JSON(t), nil)
		if err == nil {
			t.Fatal("Extract on a file missing every request rate succeeded; those rates would be " +
				"zero, pricing every request at nothing")
		}

		if errors.Is(err, ErrNoS3Rates) {
			t.Errorf("a missing rate reported as ErrNoS3Rates: %v", err)
		}
	})
}

// TestEgressComesFromTheDataTransferFile pins the two-file rule and the three filters that separate
// internet egress from everything else AWSDataTransfer prices.
func TestEgressComesFromTheDataTransferFile(t *testing.T) {
	t.Parallel()

	const location = "US West (Oregon)"

	t.Run("reads the outbound rate", func(t *testing.T) {
		t.Parallel()

		got, err := extractEgress(location, offertest.CompleteDataTransfer(location).JSON(t))
		if err != nil {
			t.Fatalf("extractEgress: %v", err)
		}

		if got != offertest.WantEgress {
			t.Errorf("egress = %v, want %v. The fixture also carries a $0.0 SKU on the same four "+
				"attributes, a Global- aggregate, and an inbound SKU; returning 0 means one of "+
				"those won.", got, offertest.WantEgress)
		}
	})

	t.Run("wrong location is an error, not zero", func(t *testing.T) {
		t.Parallel()

		_, err := extractEgress("EU (Ireland)", offertest.CompleteDataTransfer(location).JSON(t))
		if err == nil {
			t.Error("extractEgress for a location with no outbound SKU succeeded; a zero egress rate " +
				"prices every byte leaving the region as free, which reads as a cheaper filesystem " +
				"rather than as a missing number")
		}
	})

	t.Run("empty location is an error", func(t *testing.T) {
		t.Parallel()

		// The S3 offer file is where location comes from. If it publishes none, the AWSDataTransfer
		// file cannot be matched at all — its products carry no regionCode.
		if _, err := extractEgress("", offertest.CompleteDataTransfer(location).JSON(t)); err == nil {
			t.Error("extractEgress with an empty location succeeded; it can only match on " +
				"fromLocation, so an empty one would match nothing or everything")
		}
	})

	t.Run("only the Global- aggregate present", func(t *testing.T) {
		t.Parallel()

		f := &offertest.Fixture{}
		f.AddProduct("DT-GLOBAL", "Data Transfer", map[string]string{
			"usagetype":        "Global-DataTransfer-Out-Bytes",
			"fromLocation":     location,
			"fromLocationType": "AWS Region",
			"toLocation":       "External",
			"transferType":     "AWS Outbound",
		}, "0.0900000000")

		if _, err := extractEgress(location, f.JSON(t)); err == nil {
			t.Error("extractEgress matched a Global- usagetype; those are local-zone aggregates " +
				"published at 0.0 in the real files, and excluding them is why the rule exists")
		}
	})

	t.Run("nil data transfer leaves egress zero", func(t *testing.T) {
		t.Parallel()

		// Documented behavior of Extract: nil means the caller takes responsibility. The fetcher
		// never passes nil for exactly this reason — see Fetcher.Region.
		region, err := Extract("us-east-1",
			offertest.CompleteS3Offer("", "US East (N. Virginia)").JSON(t), nil)
		if err != nil {
			t.Fatalf("Extract with nil dataTransfer: %v", err)
		}

		if got := region.Rates[awsname.StorageClassStandard].EgressPerGB; got != 0 {
			t.Errorf("EgressPerGB = %v with nil dataTransfer, want 0", got)
		}
	})
}

// TestDerivePrefixRejectsContradictoryAnchors covers the case where a file publishes two Standard
// storage products implying different prefixes.
//
// Not observed in any live file, and that is the point: if it ever happens, every usagetype lookup in
// that region shifts, so the failure has to be loud at generation time rather than a table of numbers
// from the wrong region.
func TestDerivePrefixRejectsContradictoryAnchors(t *testing.T) {
	t.Parallel()

	f := offertest.CompleteS3Offer("USW2-", "US West (Oregon)")
	f.AddProduct("SKU-STD-STORAGE-2", "Storage", map[string]string{
		"usagetype":    "USE1-TimedStorage-ByteHrs",
		"volumeType":   "Standard",
		"storageClass": "General Purpose",
		"location":     "US West (Oregon)",
	}, "0.0230000000")

	_, err := Extract("us-west-2", f.JSON(t), offertest.CompleteDataTransfer("US West (Oregon)").JSON(t))
	if err == nil {
		t.Fatal("Extract with two contradictory prefix anchors succeeded; one of them was chosen by " +
			"map order, and the losing choice shifts every usagetype in the file")
	}

	if errors.Is(err, ErrNoS3Rates) {
		t.Errorf("contradictory anchors reported as ErrNoS3Rates, which the generator skips: %v", err)
	}
}

// TestAmbiguousPriceInsideOneSKU covers two beginRange-0 dimensions in a single SKU.
//
// Real files do not do this. The assertion exists because the alternative is silently picking one,
// which is the same class of order-dependence the band rule addresses one level up.
func TestAmbiguousPriceInsideOneSKU(t *testing.T) {
	t.Parallel()

	f := &offertest.Fixture{}
	f.AddBanded("SKU-DOUBLE", "API Request", map[string]string{
		"usagetype": "Requests-Tier1",
	}, []offertest.Band{{BeginRange: "0", USD: "0.0000050000"}, {BeginRange: "0", USD: "0.0000090000"}})

	if _, err := lookup(parse(t, f.JSON(t)), "", query{usagetype: "Requests-Tier1"}); err == nil {
		t.Error("a SKU with two different beginRange-0 prices resolved to one of them")
	}
}

// TestQueriesCoversEveryClassAndField pins the exported description used by the live-AWS drift test.
//
// That test drives its lookups from Queries() rather than spelling its own, because the previous
// version spelled its own and pinned the wrong SKU while claiming to pin a PUT. So a class or field
// missing from Queries() is a rate nothing checks against live AWS.
func TestQueriesCoversEveryClassAndField(t *testing.T) {
	t.Parallel()

	q := Queries()

	for _, class := range awsname.StorageClasses() {
		fields, ok := q[class]
		if !ok {
			t.Errorf("Queries() omits storage class %q, so its rates are never checked against the "+
				"live offer files", class)

			continue
		}

		for _, field := range []string{"StoragePerGBMonth", "PutRequest", "GetRequest", "ListRequest"} {
			if fields[field] == "" {
				t.Errorf("Queries()[%q] has no %s", class, field)
			}
		}

		// Retrieval is present exactly for the classes that charge for it. Both directions matter: a
		// missing one means a retrieval fee nothing checks, and a spurious one means the drift test
		// looks for a usagetype AWS does not publish and fails for the wrong reason.
		wantRetrieval := offertest.WantRates[class].Retrieval > 0

		if _, has := fields["RetrievalPerGB"]; has != wantRetrieval {
			t.Errorf("Queries()[%q] RetrievalPerGB present = %v, want %v", class, has, wantRetrieval)
		}
	}
}

// TestByClassCoversAwsname is the same closure one level down, on the specification itself.
func TestByClassCoversAwsname(t *testing.T) {
	t.Parallel()

	want := slices.Sorted(slices.Values(awsname.StorageClasses()))

	if got := ClassesWithQueries(); !slices.Equal(got, want) {
		t.Errorf("byClass covers %v; awsname admits %v. A class awsname admits with no query here "+
			"fails extraction for every region, and a query for a class awsname rejects is "+
			"unreachable.", got, want)
	}

	if got := slices.Sorted(slices.Values(Classes())); !slices.Equal(got, want) {
		t.Errorf("Classes() = %v, want %v", got, want)
	}
}

// TestEmitRoundTripsThroughGofmt asserts the generated source compiles as Go and holds the values it
// was given, in decimal rather than exponent form.
//
// The decimal form is not cosmetic: %v renders the Standard GET rate as 4e-07, and the whole reason
// the generated file is committed and reviewable is that a human can compare each value to AWS's
// pricing page.
func TestEmitRoundTripsThroughGofmt(t *testing.T) {
	t.Parallel()

	region, err := Extract("us-east-1",
		offertest.CompleteS3Offer("", "US East (N. Virginia)").JSON(t),
		offertest.CompleteDataTransfer("US East (N. Virginia)").JSON(t))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	src, err := Emit([]Region{region})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	text := string(src)

	for _, want := range []string{
		"// Code generated by internal/awsrates/offerfile. DO NOT EDIT.",
		"package awsrates",
		`"us-east-1": {`,
		"awsname.StorageClassStandard: {",
		"StoragePerGBMonth: 0.023,",
		// 0.0000004, not 4e-07.
		"GetRequest:        0.0000004,",
		"EgressPerGB:       0.09,",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated source does not contain %q", want)
		}
	}

	if strings.Contains(text, "e-0") {
		t.Error("generated source contains exponent-notation floats; every rate should be a decimal " +
			"literal that can be grepped against https://aws.amazon.com/s3/pricing/")
	}

	// A region missing a class must fail rather than emit a partial table, since the consumer reads a
	// missing class as a zero rate.
	partial := Region{Code: "us-east-1", Rates: maps.Clone(region.Rates)}
	delete(partial.Rates, awsname.StorageClassGlacier)

	if _, err := Emit([]Region{partial}); err == nil {
		t.Error("Emit succeeded for a region with no GLACIER rate; the emitted table would price " +
			"Glacier storage at zero")
	}
}

// TestEmitSortsRegions keeps the generated file's diff readable: without a sort, regenerating it
// reorders 36 blocks and every regeneration is an unreviewable diff.
func TestEmitSortsRegions(t *testing.T) {
	t.Parallel()

	build := func(code, prefix, location string) Region {
		t.Helper()

		r, err := Extract(code,
			offertest.CompleteS3Offer(prefix, location).JSON(t),
			offertest.CompleteDataTransfer(location).JSON(t))
		if err != nil {
			t.Fatalf("Extract(%s): %v", code, err)
		}

		return r
	}

	src, err := Emit([]Region{
		build("us-west-2", "USW2-", "US West (Oregon)"),
		build("ap-south-1", "APS3-", "Asia Pacific (Mumbai)"),
		build("eu-west-1", "EU-", "EU (Ireland)"),
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	text := string(src)

	first := strings.Index(text, `"ap-south-1"`)
	second := strings.Index(text, `"eu-west-1"`)
	third := strings.Index(text, `"us-west-2"`)

	if first >= second || second >= third {
		t.Errorf("regions are not emitted in sorted order: ap-south-1 at %d, eu-west-1 at %d, "+
			"us-west-2 at %d", first, second, third)
	}
}
