//go:build integration

package awsrates_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates"
	"github.com/scttfrdmn/objectfs/internal/awsrates/offerfile"
)

// This file is why the rates in rates_generated.go can be trusted today rather than on the day they
// were generated.
//
// It re-fetches the same public offer files the generator reads, re-runs the same extraction, and
// fails on any difference from the committed table. Run it with:
//
//	go test -race -tags=integration ./internal/awsrates/
//
// A failure here is not necessarily a defect — AWS changes prices, and this is how we find out. The
// fix is `go generate ./internal/awsrates/...` and a commit of the result, which is the only supported
// way to change a value in that file.
//
// # Why it no longer shells out to the AWS CLI or names its own queries
//
// The previous version of this test issued its own `aws pricing get-products` calls against a list of
// usagetypes written out by hand. Two things were wrong with that, and both were defects rather than
// awkwardness:
//
//   - It needed credentials, so it skipped for anyone without a configured profile — including CI.
//     A drift check that skips is not a drift check.
//   - It transcribed the queries a second time. Its Glacier PutRequest entry named Requests-Tier3,
//     which is the *restore* price, and it therefore agreed with a table that was 67% too high on
//     Glacier PUT. Two transcriptions of the same intent check each other, not the intent, and this is
//     the exact failure the offerfile package was written to end.
//
// So the queries now come from the one place that defines them, and the comparison is against the
// whole extracted table rather than a hand-picked subset. There is nothing left for a query list here
// to get wrong.

// fetchTimeout bounds the whole run. Each region is two files of roughly 500 KB.
const fetchTimeout = 5 * time.Minute

// TestGeneratedTableMatchesLiveOfferFiles is the drift check.
//
// It compares every field of every class in the committed table against a fresh extraction, for a
// sample of regions rather than all 36: the extraction rules are identical per region and are covered
// by the offerfile package's own tests, so what this adds is the live *values*. The sample spans the
// four price levels observed across the fleet, so a fleet-wide repricing cannot pass unnoticed.
func TestGeneratedTableMatchesLiveOfferFiles(t *testing.T) {
	t.Parallel()

	// One region per distinct Standard price band, plus the two ObjectFS defaults. sa-east-1 is 76%
	// above us-east-1 and ap-east-2 is below it, so a bug that silently substitutes us-east-1's table
	// for another region's fails here in both directions.
	regions := []string{"us-east-1", "us-west-2", "eu-central-1", "sa-east-1", "ap-east-2"}

	f := offerfile.NewFetcher()

	for _, code := range regions {
		t.Run(code, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), fetchTimeout)
			defer cancel()

			live, err := f.Region(ctx, code)
			if err != nil {
				if errors.Is(err, offerfile.ErrNoS3Rates) {
					t.Fatalf("%s publishes no S3 rates but is in the committed table; either AWS "+
						"withdrew the region or the table holds a region it should not", code)
				}

				t.Fatalf("fetching %s: %v", code, err)
			}

			committed, ok := awsrates.AllForRegion(code)
			if !ok {
				t.Fatalf("rates_generated.go has no table for %s, which AWS publishes rates for; "+
					"run `go generate ./internal/awsrates/...`", code)
			}

			for _, class := range awsname.StorageClasses() {
				want, ok := live.Rates[class]
				if !ok {
					t.Errorf("the live %s offer file yields no rate for %s", code, class)

					continue
				}

				got, ok := committed[class]
				if !ok {
					t.Errorf("rates_generated.go has no %s entry for %s", code, class)

					continue
				}

				compareRates(t, code, class, got, want)
			}
		})
	}
}

// compareRates reports every field that differs, rather than the first, so one run shows the whole
// shape of a repricing.
func compareRates(t *testing.T, region, class string, got, want awsrates.Rate) {
	t.Helper()

	for _, f := range []struct {
		name      string
		got, want float64
		query     string
	}{
		{"StoragePerGBMonth", got.StoragePerGBMonth, want.StoragePerGBMonth, queryFor(class, "StoragePerGBMonth")},
		{"PutRequest", got.PutRequest, want.PutRequest, queryFor(class, "PutRequest")},
		{"GetRequest", got.GetRequest, want.GetRequest, queryFor(class, "GetRequest")},
		{"ListRequest", got.ListRequest, want.ListRequest, queryFor(class, "ListRequest")},
		{"RetrievalPerGB", got.RetrievalPerGB, want.RetrievalPerGB, queryFor(class, "RetrievalPerGB")},
		{"EgressPerGB", got.EgressPerGB, want.EgressPerGB, "AWSDataTransfer DataTransfer-Out-Bytes"},
	} {
		// Exact, not within a tolerance. Both sides are parsed from the same decimal string by the
		// same code, so any difference at all means the published price moved.
		if f.got == f.want {
			continue
		}

		ratio := "n/a"
		if f.want != 0 {
			ratio = fmt.Sprintf("%.4gx", f.got/f.want)
		}

		t.Errorf("%s %s.%s: table has %v, AWS now publishes %v (%s)\n"+
			"  query: %s\n"+
			"  fix:   go generate ./internal/awsrates/... && commit the result\n"+
			"  a round factor of ten here is a per-1,000 request conversion, not a repricing",
			region, class, f.name, f.got, f.want, ratio, f.query)
	}
}

// queryFor names the query behind a field, so a failure says which SKU to go look at.
func queryFor(class, field string) string {
	fields, ok := offerfile.Queries()[class]
	if !ok {
		return "no query registered"
	}

	if q, ok := fields[field]; ok {
		return q
	}

	return "no query for this field"
}

// TestEveryPricedFieldHasALiveQuery fails when a field carries a non-zero rate that no query in
// offerfile produces.
//
// Without it the drift check narrows silently: a rate gets added, nothing extracts it, and
// TestGeneratedTableMatchesLiveOfferFiles still passes because it only compares what the extractor
// returns. RetrievalPerGB is exempt where it is genuinely zero — Standard and Intelligent-Tiering
// charge no retrieval fee, and a zero there is a fact rather than a gap.
func TestEveryPricedFieldHasALiveQuery(t *testing.T) {
	t.Parallel()

	queries := offerfile.Queries()

	for class, rate := range awsrates.All() {
		fields, ok := queries[class]
		if !ok {
			t.Errorf("%s has a rate table entry but internal/awsrates/offerfile has no queries for "+
				"it, so nothing checks any of its numbers against AWS", class)

			continue
		}

		for name, v := range map[string]float64{
			"StoragePerGBMonth": rate.StoragePerGBMonth,
			"PutRequest":        rate.PutRequest,
			"GetRequest":        rate.GetRequest,
			"ListRequest":       rate.ListRequest,
			"RetrievalPerGB":    rate.RetrievalPerGB,
		} {
			if v == 0 {
				continue // a genuine zero has no SKU to check
			}

			if _, ok := fields[name]; !ok {
				t.Errorf("%s.%s is %v in the table but offerfile has no query for it; the number is "+
					"therefore unverified in a table whose whole claim is that it was generated",
					class, name, v)
			}
		}
	}
}

// TestOfferFilesNeedNoCredentials pins the property that makes this test runnable in CI.
//
// The offer files are unauthenticated public objects. If a future change routes this through the
// Pricing API — which does need credentials, and which #227 owns deciding about — this test would
// start skipping or failing for reasons unrelated to prices, and that should be a deliberate choice
// rather than something discovered later.
// Not parallel: t.Setenv is incompatible with it, and clearing the process's credential environment
// is the whole mechanism here.
func TestOfferFilesNeedNoCredentials(t *testing.T) {
	for _, v := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE",
		"AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE", "AWS_ROLE_ARN",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		t.Setenv(v, "")
	}

	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	ctx, cancel := context.WithTimeout(t.Context(), fetchTimeout)
	defer cancel()

	region, err := offerfile.NewFetcher().Region(ctx, awsrates.DefaultRegion)
	if err != nil {
		t.Fatalf("fetching %s with every credential source cleared: %v; the offer files are public "+
			"objects and this test is the guard on that staying true", awsrates.DefaultRegion, err)
	}

	if got := region.Rates[awsname.StorageClassStandard].StoragePerGBMonth; got <= 0 {
		t.Errorf("Standard storage extracted as %v with no credentials; a zero here means the fetch "+
			"returned something that parsed but held no rates", got)
	}
}
