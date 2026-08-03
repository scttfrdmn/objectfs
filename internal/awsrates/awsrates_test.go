package awsrates_test

import (
	"testing"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// TestEveryStorageClassHasARate pins the direction that costs money to get wrong.
//
// awsname decides which storage classes exist and the config loader validates against that set, so a
// class admitted there with no entry here would price at whatever For's fallback returns. The failure
// mode is not an error — it is a confident number for a tier nobody costed.
func TestEveryStorageClassHasARate(t *testing.T) {
	t.Parallel()

	for _, sc := range awsname.StorageClasses() {
		t.Run(sc, func(t *testing.T) {
			t.Parallel()

			r, ok := awsrates.For(sc)
			if !ok {
				t.Fatalf("%s is a storage class the config loader accepts, but it has no rate; "+
					"For fell back to Standard, so any cost reported for it is another tier's price", sc)
			}

			if r.StoragePerGBMonth <= 0 {
				t.Errorf("%s has StoragePerGBMonth %v; a zero storage rate reads as free", sc, r.StoragePerGBMonth)
			}

			if r.PutRequest <= 0 {
				t.Errorf("%s has PutRequest %v; every tier charges for writes", sc, r.PutRequest)
			}

			if r.GetRequest <= 0 {
				t.Errorf("%s has GetRequest %v; every tier charges for reads", sc, r.GetRequest)
			}
		})
	}
}

// TestRatesMatchThePublishedPerThousandPrices is the check that catches an absolute error.
//
// AWS publishes request prices per 1,000 or per 10,000 calls; this package stores them per call.
// That conversion is where the original defect came from — 0.0005 was the per-1,000 PUT price for
// Standard written into a per-request field, so every write was costed at a tenth of its price. The
// ordering test cannot see it (a uniformly-low rate inverts nothing), and dividing the published
// figure here would just repeat whatever mistake the table made.
//
// So the expectation is the *published* price and the *published* divisor, stated separately as
// literals, with the product compared to the stored value. The description column is the string AWS
// returns in priceDimensions, which is what makes each line checkable by eye against the console.
func TestRatesMatchThePublishedPerThousandPrices(t *testing.T) {
	t.Parallel()

	cases := []struct {
		class       string
		op          string
		published   float64 // dollars, as AWS prints them
		per         float64 // per how many requests
		description string  // the priceDimensions description this came from
	}{
		{awsname.StorageClassStandard, "PUT", 0.005, 1_000, "$0.005 per 1,000 PUT, COPY, POST, or LIST requests"},
		{awsname.StorageClassStandard, "GET", 0.004, 10_000, "$0.004 per 10,000 GET and all other requests"},
		{awsname.StorageClassStandardIA, "PUT", 0.01, 1_000, "$0.01 per 1,000 PUT, COPY, POST or LIST requests to Standard-Infrequent Access"},
		{awsname.StorageClassStandardIA, "GET", 0.01, 10_000, "$0.01 per 10,000 GET and all other requests to Standard-Infrequent Access"},
		{awsname.StorageClassOneZoneIA, "PUT", 0.01, 1_000, "$0.01 per 1,000 PUT, COPY, POST or LIST requests to One Zone-Infrequent Access"},
		{awsname.StorageClassOneZoneIA, "GET", 0.01, 10_000, "$0.01 per 10,000 GET and all other requests to One Zone-Infrequent Access"},
		{awsname.StorageClassGlacierIR, "PUT", 0.02, 1_000, "$0.02 per 1,000 PUT, COPY, POST or LIST requests to Glacier Instant Retrieval"},
		{awsname.StorageClassGlacierIR, "GET", 0.1, 10_000, "$0.1 per 10,000 GET and all other requests to Glacier Instant Retrieval"},
		// GLACIER's PUT is $0.03/1,000, and this line asserted $0.05 until the generated table
		// disagreed with it. Both were reading the same wrong SKU: Requests-Tier3, which is
		// RestoreObject — the price of *thawing* an object, 67% above the write. It passed because
		// the table it checked had been built by the same query.
		//
		// Worth keeping as a comment because the description column is what makes these lines
		// checkable by eye, and this one's description was Deep Archive's, on a Glacier row. A
		// mismatched description is the visible symptom of a rate read from the wrong product, and
		// it sat here through review.
		{awsname.StorageClassGlacier, "PUT", 0.03, 1_000, "$0.03 per 1,000 PUT requests to Glacier Flexible Retrieval"},

		// Deep Archive genuinely is the lifecycle rate: AWS publishes no PUT usagetype for it at
		// all, so a direct write is priced at the transition. See the DEEP_ARCHIVE entry in
		// internal/awsrates/offerfile.
		{awsname.StorageClassDeepArchive, "PUT", 0.05, 1_000, "$0.05 per 1,000 Lifecycle Transition requests into Glacier Deep Archive"},
		{awsname.StorageClassIntelligent, "PUT", 0.005, 1_000, "$0.005 per 1,000 PUT, COPY, POST, or LIST requests to Intelligent-Tiering"},
		{awsname.StorageClassIntelligent, "GET", 0.004, 10_000, "$0.004 per 10,000 GET and all other requests to Intelligent-Tiering"},
	}

	for _, tc := range cases {
		t.Run(tc.class+"/"+tc.op, func(t *testing.T) {
			t.Parallel()

			r, ok := awsrates.For(tc.class)
			if !ok {
				t.Fatalf("%s has no rate", tc.class)
			}

			got := r.PutRequest
			if tc.op == "GET" {
				got = r.GetRequest
			}

			want := tc.published / tc.per
			if diff := got - want; diff > 1e-12 || diff < -1e-12 {
				t.Errorf("%s %s is %v per request, want %v (%s)\n"+
					"ratio to expected: %.4gx — a round factor of ten here means the published "+
					"per-1,000 price was stored as if it were per-request",
					tc.class, tc.op, got, want, tc.description, got/want)
			}
		})
	}
}

// TestForUnknownClassFallsBackToStandardAndSaysSo covers the contract that keeps an unknown tier from
// costing zero. Both halves matter: the rate has to be usable, and ok has to be false so a caller
// that checks can tell it was a guess.
func TestForUnknownClassFallsBackToStandardAndSaysSo(t *testing.T) {
	t.Parallel()

	std, _ := awsrates.For(awsname.StorageClassStandard)

	for _, sc := range []string{"", "GLACIER_DEEP", "standard", "MADE_UP"} {
		t.Run("class="+sc, func(t *testing.T) {
			t.Parallel()

			got, ok := awsrates.For(sc)
			if ok {
				t.Fatalf("For(%q) reported the class as known", sc)
			}

			if got != std {
				t.Errorf("For(%q) = %+v, want the Standard rate %+v — a zero Rate would price the "+
					"tier at nothing, which reads as free rather than unknown", sc, got, std)
			}
		})
	}
}

// TestAllReturnsACopy guards the shared table. A caller that mutated it would change what every
// other package believes an object costs, and nothing would report the change.
func TestAllReturnsACopy(t *testing.T) {
	t.Parallel()

	before, _ := awsrates.For(awsname.StorageClassStandard)

	table := awsrates.All()
	table[awsname.StorageClassStandard] = awsrates.Rate{StoragePerGBMonth: 999}
	delete(table, awsname.StorageClassGlacier)

	after, _ := awsrates.For(awsname.StorageClassStandard)
	if after != before {
		t.Errorf("mutating the map from All changed the canonical Standard rate: %+v became %+v", before, after)
	}

	if _, ok := awsrates.For(awsname.StorageClassGlacier); !ok {
		t.Error("deleting from the map from All removed Glacier from the canonical table")
	}
}

// TestGBFromBytesUsesDecimalGB is the whole point of the helper.
//
// AWS bills a GB-month as 10^9 bytes. The code this replaced divided by 2^30 with a comment
// asserting the binary reading was correct, which is how a 7.4% understatement survived review — so
// this test states the expected values as literals rather than recomputing them from a constant,
// because a test that recomputes the formula agrees with any formula.
func TestGBFromBytesUsesDecimalGB(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		bytes int64
		want  float64
		why   string
	}{
		{
			name:  "one decimal GB is exactly one GB",
			bytes: 1_000_000_000,
			want:  1.0,
			why:   "the binary reading gives 0.93132, which is the 7.4% error",
		},
		{
			name:  "one binary GiB is more than one billed GB",
			bytes: 1_073_741_824,
			want:  1.073741824,
			why:   "a GiB of data is billed as 1.0737 GB, not 1.0",
		},
		{
			name:  "a 100 MB object",
			bytes: 100_000_000,
			want:  0.1,
			why:   "",
		},
		{
			name:  "zero",
			bytes: 0,
			want:  0,
			why:   "",
		},
		{
			name:  "negative sizes do not yield a negative cost",
			bytes: -1_000_000_000,
			want:  0,
			why:   "a negative byte count is a bug upstream; billing it as a credit would hide it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := awsrates.GBFromBytes(tc.bytes)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				msg := ""
				if tc.why != "" {
					msg = " — " + tc.why
				}

				t.Errorf("GBFromBytes(%d) = %v, want %v%s", tc.bytes, got, tc.want, msg)
			}
		})
	}
}

// TestStorageClassesMatchesAwsname keeps the two orderings from drifting. Callers iterate this to
// render tier comparisons, so a class present in the rate table but absent from the list would be
// costed and never shown.
func TestStorageClassesMatchesAwsname(t *testing.T) {
	t.Parallel()

	got := awsrates.StorageClasses()
	want := awsname.StorageClasses()

	if len(got) != len(want) {
		t.Fatalf("StorageClasses returned %d classes, awsname has %d", len(got), len(want))
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %s, want %s", i, got[i], want[i])
		}
	}
}

// TestRateOrderingIsEconomicallySane asserts the shape the tiers have to have: colder storage is
// cheaper to keep and dearer to write. A table where that inverts is wrong somewhere, and which side
// is wrong is then a short question.
//
// What it does not catch is worth stating, because the obvious reading of it is too generous: this
// only sees *relative* errors. Verified by injection — scaling Standard's PUT rate down by ten, the
// exact defect this package was created for, leaves every colder tier still above it, so the
// ordering holds and the test passes. It catches a rate moved in one tier; it cannot catch a rate
// that is uniformly wrong. TestRatesAgainstLiveAWSPricing is the one that checks the values
// themselves, and it needs credentials — so this pair is deliberate rather than redundant.
func TestRateOrderingIsEconomicallySane(t *testing.T) {
	t.Parallel()

	std, _ := awsrates.For(awsname.StorageClassStandard)

	colder := []string{
		awsname.StorageClassStandardIA,
		awsname.StorageClassOneZoneIA,
		awsname.StorageClassGlacierIR,
		awsname.StorageClassGlacier,
		awsname.StorageClassDeepArchive,
	}

	for _, sc := range colder {
		t.Run(sc, func(t *testing.T) {
			t.Parallel()

			r, ok := awsrates.For(sc)
			if !ok {
				t.Fatalf("%s has no rate", sc)
			}

			if r.StoragePerGBMonth >= std.StoragePerGBMonth {
				t.Errorf("%s stores at %v, which is not cheaper than Standard's %v — an archive tier "+
					"that costs more to store than Standard has no reason to exist, so one of the two is wrong",
					sc, r.StoragePerGBMonth, std.StoragePerGBMonth)
			}

			if r.PutRequest < std.PutRequest {
				t.Errorf("%s writes at %v, cheaper than Standard's %v — colder tiers charge more per "+
					"request, so this is the shape of a units error",
					sc, r.PutRequest, std.PutRequest)
			}
		})
	}
}

// TestRegionsIsEveryRegionInTheGeneratedTable pins the accessor internal/storage/s3 warns from.
//
// The list is generated, so the value worth asserting is not its length but its shape: sorted, no
// duplicates, us-east-1 present, and every entry also resolvable through the other two accessors. A
// region in Regions() that HasRegion denies would make PricingManager warn about a region it then
// priced correctly, which is the kind of inconsistency operators learn to distrust the whole log over.
func TestRegionsIsEveryRegionInTheGeneratedTable(t *testing.T) {
	t.Parallel()

	regions := awsrates.Regions()

	if len(regions) == 0 {
		t.Fatal("Regions() is empty; the generated table holds no regions, so every lookup falls " +
			"back to us-east-1 and reports false")
	}

	seen := make(map[string]bool, len(regions))

	for i, r := range regions {
		if seen[r] {
			t.Errorf("Regions() lists %q twice", r)
		}

		seen[r] = true

		if i > 0 && regions[i-1] > r {
			t.Errorf("Regions() is not sorted: %q precedes %q. It is rendered into warnings and "+
				"docs, and an unsorted list makes a diff of it unreadable", regions[i-1], r)
		}

		if !awsrates.HasRegion(r) {
			t.Errorf("Regions() lists %q but HasRegion says it is unknown; the two read the same "+
				"map, so this means one of them stopped doing that", r)
		}

		if _, ok := awsrates.AllForRegion(r); !ok {
			t.Errorf("Regions() lists %q but AllForRegion reports it as a fallback", r)
		}
	}

	if !seen[awsrates.DefaultRegion] {
		t.Errorf("Regions() does not include %s, which is the fallback every unknown region resolves "+
			"to; the fallback resolving to a region that is not in the table is a silent zero table",
			awsrates.DefaultRegion)
	}
}

// TestHasRegionRejectsWhatIsNotARegion covers the false arm, which is the one with a consequence.
//
// The true arm is exercised by every region in the test above. This one pins that a plausible-looking
// string is rejected: "us-east" and "US-EAST-1" are the two shapes a hand-edited config produces, and
// accepting either would price a deployment from a table nobody chose.
func TestHasRegionRejectsWhatIsNotARegion(t *testing.T) {
	t.Parallel()

	for _, r := range []string{
		"",
		"mars-1",
		"us-east",            // truncated
		"US-EAST-1",          // wrong case; AWS region codes are lowercase
		"us-east-1 ",         // trailing space, as a YAML quoting accident produces
		"aws-other",          // a real entry in AWS's offer index that is not a region
		"eu-central-1-ath-1", // a local zone; it has an offer file but no S3 rates
	} {
		if awsrates.HasRegion(r) {
			t.Errorf("HasRegion(%q) is true; that region has no table, so a caller that trusts this "+
				"skips the warning and reports us-east-1 prices labeled as %q", r, r)
		}
	}
}

// TestForRegionFallsBackWithoutLying is the property every caller of it depends on.
//
// Three cases, and the bool is the whole point of each: an unknown region must return us-east-1's
// numbers *and* false, an unknown class must return Standard's numbers *and* false, and a known pair
// must return that pair's numbers and true. Returning a zero Rate instead would be worse — a $0 cost
// reads as free storage rather than as a lookup that missed — and returning true alongside a fallback
// would make the fallback undetectable.
func TestForRegionFallsBackWithoutLying(t *testing.T) {
	t.Parallel()

	defaultStandard, ok := awsrates.ForRegion(awsrates.DefaultRegion, awsname.StorageClassStandard)
	if !ok {
		t.Fatalf("ForRegion(%s, STANDARD) reports false; the default region and the default class "+
			"are the one pair that must always be known", awsrates.DefaultRegion)
	}

	t.Run("unknown region yields the default region's rate and false", func(t *testing.T) {
		t.Parallel()

		got, ok := awsrates.ForRegion("mars-1", awsname.StorageClassStandard)
		if ok {
			t.Error("ForRegion reports true for an unknown region; the caller cannot tell it is " +
				"being handed another region's prices")
		}

		if got != defaultStandard {
			t.Errorf("ForRegion(mars-1, STANDARD) = %+v, want %s's %+v; a zero Rate here reads as "+
				"free storage rather than as a missing region",
				got, awsrates.DefaultRegion, defaultStandard)
		}
	})

	t.Run("unknown class yields Standard's rate and false", func(t *testing.T) {
		t.Parallel()

		got, ok := awsrates.ForRegion("sa-east-1", "GLACIER_FLEXIBLE")
		if ok {
			t.Error("ForRegion reports true for a storage class that does not exist")
		}

		saStandard, _ := awsrates.ForRegion("sa-east-1", awsname.StorageClassStandard)
		if got != saStandard {
			t.Errorf("ForRegion(sa-east-1, GLACIER_FLEXIBLE) = %+v, want sa-east-1's Standard %+v. "+
				"Note it must be *that region's* Standard, not us-east-1's: falling back on the "+
				"class must not also silently fall back on the region.", got, saStandard)
		}
	})

	t.Run("both unknown yields the default pair and false", func(t *testing.T) {
		t.Parallel()

		got, ok := awsrates.ForRegion("mars-1", "GLACIER_FLEXIBLE")
		if ok {
			t.Error("ForRegion reports true when neither the region nor the class is known")
		}

		if got != defaultStandard {
			t.Errorf("ForRegion(mars-1, GLACIER_FLEXIBLE) = %+v, want %+v", got, defaultStandard)
		}
	})

	t.Run("a known pair is not a fallback", func(t *testing.T) {
		t.Parallel()

		// sa-east-1 storage is 76% above us-east-1, so this fails loudly if the region is ignored —
		// which is the defect #161 exists to close, one layer up.
		got, ok := awsrates.ForRegion("sa-east-1", awsname.StorageClassStandard)
		if !ok {
			t.Fatal("ForRegion(sa-east-1, STANDARD) reports false for a region in Regions()")
		}

		if got.StoragePerGBMonth == defaultStandard.StoragePerGBMonth {
			t.Errorf("sa-east-1 and %s both price Standard at %v. They do not: sa-east-1 is the "+
				"most expensive region AWS publishes. Equal values here mean the region argument "+
				"selected nothing.", awsrates.DefaultRegion, got.StoragePerGBMonth)
		}
	})
}

// TestAllForRegionReturnsACopyOfTheRightRegion covers both of its contracts at once.
//
// The copy matters because the returned map is the shared generated table otherwise, and a caller that
// mutated it would change what every other package in the process believes an object costs — with no
// error and no way to trace it back. TestAllReturnsACopy covers the us-east-1 wrapper; this covers the
// regional form and the unknown-region arm, which is where the bool comes from.
func TestAllForRegionReturnsACopyOfTheRightRegion(t *testing.T) {
	t.Parallel()

	t.Run("mutating the result does not affect the table", func(t *testing.T) {
		t.Parallel()

		first, ok := awsrates.AllForRegion("sa-east-1")
		if !ok {
			t.Fatal("AllForRegion(sa-east-1) reports it as unknown")
		}

		want := first[awsname.StorageClassStandard]

		delete(first, awsname.StorageClassStandard)
		first[awsname.StorageClassGlacier] = awsrates.Rate{}

		second, _ := awsrates.AllForRegion("sa-east-1")

		if got, ok := second[awsname.StorageClassStandard]; !ok || got != want {
			t.Errorf("after deleting STANDARD from one AllForRegion result, a second call returns "+
				"%+v (present=%v), want %+v; the map is shared, so any caller can silently reprice "+
				"the whole process", got, ok, want)
		}

		if second[awsname.StorageClassGlacier].StoragePerGBMonth == 0 {
			t.Error("a zeroed Rate written into one AllForRegion result reached the shared table; " +
				"Glacier now prices at $0/GB-month process-wide")
		}
	})

	t.Run("unknown region yields the default table and false", func(t *testing.T) {
		t.Parallel()

		got, ok := awsrates.AllForRegion("mars-1")
		if ok {
			t.Error("AllForRegion reports true for an unknown region")
		}

		want, _ := awsrates.AllForRegion(awsrates.DefaultRegion)

		if len(got) != len(want) {
			t.Fatalf("AllForRegion(mars-1) has %d classes, want %s's %d; an empty map here prices "+
				"every tier at zero", len(got), awsrates.DefaultRegion, len(want))
		}

		for class, r := range want {
			if got[class] != r {
				t.Errorf("AllForRegion(mars-1)[%s] = %+v, want %+v", class, got[class], r)
			}
		}
	})
}
