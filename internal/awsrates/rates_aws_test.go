//go:build integration

package awsrates_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// This file is why the rates in awsrates.go can be trusted.
//
// Every value there was read from the AWS Pricing API rather than from a pricing web page, and this
// re-runs those reads against the live API and fails on any difference. It is the difference between
// a table someone believed and a table something checked.
//
// Run it with:
//
//	AWS_PROFILE=aws go test -race -tags=integration ./internal/awsrates/
//
// A failure here is not necessarily a defect — AWS changes prices, and this is how we find out. The
// fix is to update the table and the query comment together, in one commit, so the provenance of
// each number stays attached to it.
//
// # Why it shells out to the AWS CLI
//
// The Pricing API needs github.com/aws/aws-sdk-go-v2/service/pricing, which is not a dependency of
// this module, and adding it is not free: its transitive requirement moves smithy-go to >= 1.26.0,
// and smithy-go also sits under the S3 client that serves every read and write. Taking that bump for
// a drift check would put dependency risk on the data path to test a table. #183 owns adding the
// pricing client deliberately, with the SDK work that belongs to it; until then this test gets the
// same JSON over a subprocess and costs the module nothing.

// pricingQuery is one rate we claim, and the API query that produced it.
//
// usagetype is the key because it is the only attribute that maps 1:1 to a rate and stays stable —
// productFamily is absent from most S3 products, so filtering on it silently drops SKUs.
type pricingQuery struct {
	class     string  // the storage class in our table, "" for rates that are not per-class
	field     string  // which Rate field this checks
	usagetype string  // the AWS usagetype to query
	published float64 // dollars as AWS prints them
	per       float64 // per how many units (1 for per-GB, 1000/10000 for requests)
}

// want returns the per-unit rate this query expects our table to hold.
func (q pricingQuery) want() float64 { return q.published / q.per }

// queries covers every field of every tier the table prices.
//
// Deep Archive storage and the Glacier/GDA retrieval rates are absent: their usagetypes are not
// queryable by the location+usagetype filter this test uses (verified — the region-prefixed forms
// exist but the bare ones return nothing), so pinning them would mean pinning a query that does not
// work. They are noted in awsrates.go with the source they came from instead, and TestQueriesCover
// below fails if the set of unqueried fields grows beyond that.
var queries = []pricingQuery{
	// Storage, per GB-month.
	{awsname.StorageClassStandard, "StoragePerGBMonth", "TimedStorage-ByteHrs", 0.023, 1},
	{awsname.StorageClassStandardIA, "StoragePerGBMonth", "TimedStorage-SIA-ByteHrs", 0.0125, 1},
	{awsname.StorageClassOneZoneIA, "StoragePerGBMonth", "TimedStorage-ZIA-ByteHrs", 0.01, 1},
	{awsname.StorageClassGlacierIR, "StoragePerGBMonth", "TimedStorage-GIR-ByteHrs", 0.004, 1},
	{awsname.StorageClassGlacier, "StoragePerGBMonth", "TimedStorage-GlacierByteHrs", 0.0036, 1},
	{awsname.StorageClassIntelligent, "StoragePerGBMonth", "TimedStorage-INT-FA-ByteHrs", 0.023, 1},
	{awsname.StorageClassReducedRedundancy, "StoragePerGBMonth", "TimedStorage-RRS-ByteHrs", 0.024, 1},

	// Requests. AWS groups these as Tier1 (PUT/COPY/POST/LIST) and Tier2 (GET and everything else).
	{awsname.StorageClassStandard, "PutRequest", "Requests-Tier1", 0.005, 1_000},
	{awsname.StorageClassStandard, "GetRequest", "Requests-Tier2", 0.004, 10_000},
	{awsname.StorageClassStandardIA, "PutRequest", "Requests-SIA-Tier1", 0.01, 1_000},
	{awsname.StorageClassStandardIA, "GetRequest", "Requests-SIA-Tier2", 0.01, 10_000},
	{awsname.StorageClassOneZoneIA, "PutRequest", "Requests-ZIA-Tier1", 0.01, 1_000},
	{awsname.StorageClassOneZoneIA, "GetRequest", "Requests-ZIA-Tier2", 0.01, 10_000},
	{awsname.StorageClassGlacierIR, "PutRequest", "Requests-GIR-Tier1", 0.02, 1_000},
	{awsname.StorageClassGlacierIR, "GetRequest", "Requests-GIR-Tier2", 0.1, 10_000},
	{awsname.StorageClassIntelligent, "PutRequest", "Requests-INT-Tier1", 0.005, 1_000},
	{awsname.StorageClassIntelligent, "GetRequest", "Requests-INT-Tier2", 0.004, 10_000},
	{awsname.StorageClassGlacier, "PutRequest", "Requests-Tier3", 0.05, 1_000},
	{awsname.StorageClassDeepArchive, "PutRequest", "Requests-Tier3", 0.05, 1_000},

	// Retrieval, per GB.
	{awsname.StorageClassStandardIA, "RetrievalPerGB", "Retrieval-SIA", 0.01, 1},
	{awsname.StorageClassOneZoneIA, "RetrievalPerGB", "Retrieval-ZIA", 0.01, 1},
	{awsname.StorageClassGlacierIR, "RetrievalPerGB", "Retrieval-GIR", 0.03, 1},
	{awsname.StorageClassGlacier, "RetrievalPerGB", "Standard-Retrieval-Bytes", 0.01, 1},
}

// TestRatesAgainstLiveAWSPricing is the drift check.
func TestRatesAgainstLiveAWSPricing(t *testing.T) {
	t.Parallel()

	requireAWSCLI(t)

	for _, q := range queries {
		t.Run(q.usagetype+"/"+q.field, func(t *testing.T) {
			t.Parallel()

			live, desc, err := fetchRate(t, q.usagetype)
			if err != nil {
				t.Fatalf("querying %s: %v", q.usagetype, err)
			}

			// First: does AWS still charge what the table's comment says it charges?
			if diff := live - q.want(); diff > 1e-12 || diff < -1e-12 {
				t.Errorf("AWS now charges %v per unit for %s, not the %v this table was built from\n"+
					"  AWS says: %s\n"+
					"  Update awsrates.go and the query in this file together, so the provenance stays "+
					"attached to the number.",
					live, q.usagetype, q.want(), desc)

				return
			}

			// Second: does our table hold what we just confirmed AWS charges?
			got, err := rateField(q.class, q.field)
			if err != nil {
				t.Fatalf("%v", err)
			}

			if diff := got - live; diff > 1e-12 || diff < -1e-12 {
				t.Errorf("%s.%s is %v, but AWS charges %v (%s)\n"+
					"  ratio: %.4gx — a round factor of ten is the per-1,000 conversion going wrong",
					q.class, q.field, got, live, q.usagetype, got/live)
			}
		})
	}
}

// TestQueriesCoverEveryPricedField fails when a field is added to the table, or a tier gains a
// non-zero rate, without a live query to check it.
//
// Without this, the drift test silently narrows: someone adds a rate, no query covers it, and the
// suite still passes green while the new number is unverified. The exemptions are listed explicitly
// so that adding one is a visible decision.
func TestQueriesCoverEveryPricedField(t *testing.T) {
	t.Parallel()

	// Fields with no working location+usagetype query, and why. Verified absent from the API under
	// that filter rather than assumed.
	exempt := map[string]string{
		awsname.StorageClassDeepArchive + ".StoragePerGBMonth": "TimedStorage-GDA-ByteHrs is not returned by a bare usagetype filter",
		awsname.StorageClassDeepArchive + ".RetrievalPerGB":    "GDA retrieval usagetypes are region-prefixed only",
		awsname.StorageClassGlacier + ".ListRequest":           "LIST bills at the Standard Tier1 rate, checked there",
		awsname.StorageClassDeepArchive + ".ListRequest":       "as above",
		awsname.StorageClassDeepArchive + ".GetRequest":        "Tier2, checked under Standard",
		awsname.StorageClassGlacier + ".GetRequest":            "Tier2, checked under Standard",
		awsname.StorageClassReducedRedundancy + ".PutRequest":  "RRS has no distinct request usagetype; bills as Tier1",
		awsname.StorageClassReducedRedundancy + ".GetRequest":  "as above",
		awsname.StorageClassReducedRedundancy + ".ListRequest": "as above",
	}

	covered := make(map[string]bool, len(queries))
	for _, q := range queries {
		covered[q.class+"."+q.field] = true
	}

	// ListRequest and EgressPerGB are excluded wholesale: LIST bills in the Tier1 group already
	// checked via PutRequest, and egress is not a per-class rate at all.
	checked := []string{"StoragePerGBMonth", "PutRequest", "GetRequest", "RetrievalPerGB"}

	for class, r := range awsrates.All() {
		for _, field := range checked {
			v, err := rateField(class, field)
			if err != nil {
				t.Fatalf("%v", err)
			}

			if v == 0 {
				continue // a genuine zero (no retrieval fee) has nothing to verify
			}

			key := class + "." + field
			if covered[key] || exempt[key] != "" {
				continue
			}

			t.Errorf("%s is %v in the table but no live query checks it; add one to queries, or an "+
				"entry to exempt saying why it cannot be queried. An unverified rate in a table whose "+
				"whole claim is that it was verified is the failure this test exists to prevent.",
				key, r)
		}
	}
}

// requireAWSCLI skips when there is no usable CLI, rather than failing.
//
// Skipping is right here: this test needs credentials by design, and a developer without them has
// not broken anything. It reports what is missing so the skip is actionable.
func requireAWSCLI(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("aws"); err != nil {
		t.Skip("the aws CLI is not on PATH; this test reads the live Pricing API")
	}

	// The Pricing API lives only in a few regions and us-east-1 is the canonical one; a caller's
	// AWS_REGION must not redirect it. That is exactly the trap #183 records: an unlisted region
	// templates into an opaque DNS failure rather than a clear error.
	out, err := run("aws", "sts", "get-caller-identity", "--region", "us-east-1", "--output", "json")
	if err != nil {
		t.Skipf("no usable AWS credentials for the Pricing API (%v); "+
			"run with AWS_PROFILE=aws as CLAUDE.md describes", err)
	}

	if !strings.Contains(out, "Account") {
		t.Skipf("sts get-caller-identity returned no account: %s", out)
	}
}

// fetchRate returns the per-unit USD price AWS currently publishes for a usagetype in us-east-1,
// along with the description it came with.
func fetchRate(t *testing.T, usagetype string) (float64, string, error) {
	t.Helper()

	out, err := run("aws", "pricing", "get-products",
		"--region", "us-east-1",
		"--service-code", "AmazonS3",
		"--filters",
		"Type=TERM_MATCH,Field=location,Value=US East (N. Virginia)",
		"--filters",
		"Type=TERM_MATCH,Field=usagetype,Value="+usagetype,
		"--max-results", "1",
		"--output", "json",
	)
	if err != nil {
		return 0, "", err
	}

	var resp struct {
		PriceList []string `json:"PriceList"`
	}

	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return 0, "", fmt.Errorf("decoding the get-products envelope: %w", err)
	}

	if len(resp.PriceList) == 0 {
		return 0, "", errors.New("no product returned; the usagetype may have been renamed")
	}

	// PriceList is a list of JSON *strings*, each holding a whole product document. That shape is
	// deliberate on AWS's side and is the detail most reimplementations get wrong.
	var product struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit map[string]string `json:"pricePerUnit"`
					Description  string            `json:"description"`
					BeginRange   string            `json:"beginRange"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}

	if err := json.Unmarshal([]byte(resp.PriceList[0]), &product); err != nil {
		return 0, "", fmt.Errorf("decoding the product document: %w", err)
	}

	// Take the first band. Several storage rates are banded by volume and our table documents that it
	// holds the first (most expensive) band, so beginRange must be 0 or the comparison is against a
	// different band than the table describes.
	for _, term := range product.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			if dim.BeginRange != "" && dim.BeginRange != "0" {
				continue
			}

			// pricePerUnit values are strings, not numbers — another shape worth preserving.
			raw, ok := dim.PricePerUnit["USD"]
			if !ok {
				continue
			}

			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return 0, "", fmt.Errorf("parsing pricePerUnit %q: %w", raw, err)
			}

			return v, dim.Description, nil
		}
	}

	return 0, "", errors.New("no OnDemand price dimension with beginRange 0")
}

// rateField reads one named field off a class's Rate, so the query table can name fields as strings.
func rateField(class, field string) (float64, error) {
	r, ok := awsrates.For(class)
	if !ok {
		return 0, fmt.Errorf("%s has no rate entry", class)
	}

	switch field {
	case "StoragePerGBMonth":
		return r.StoragePerGBMonth, nil
	case "PutRequest":
		return r.PutRequest, nil
	case "GetRequest":
		return r.GetRequest, nil
	case "ListRequest":
		return r.ListRequest, nil
	case "RetrievalPerGB":
		return r.RetrievalPerGB, nil
	case "EgressPerGB":
		return r.EgressPerGB, nil
	default:
		return 0, fmt.Errorf("unknown Rate field %q", field)
	}
}

// run executes a command with a bound timeout and returns its stdout.
func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)

	if err := cmd.Start(); err != nil {
		return "", err
	}

	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
		}

		return stdout.String(), nil

	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()

		return "", fmt.Errorf("%s timed out after 60s", name)
	}
}
