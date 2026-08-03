// Package offertest builds AWS price list offer files for tests.
//
// It is a package rather than a _test.go file because two suites need the same fixtures: the offerfile
// package's own rule tests, and the genrates command's tests, which drive the whole generator against a
// local server. A Fixture that lives in one package's test file cannot be reached from another, and the
// alternative — a second copy — is how two transcriptions of the same intent end up checking each other
// instead of the intent.
//
// Nothing in the production path imports it.
package offertest

// Fixtures are built from structs rather than pasted JSON.
//
// The real us-east-1 file is 470 KB of 381 products, and a test that pastes a slice of it can only
// assert what that slice happens to contain. Building the file programmatically lets each test state
// the one thing it is about — two SKUs on the same usagetype, a suffix that collides, a storage
// product with three volume types — with everything else at a known-good default.
//
// The fixtures are not a mock of AWS. They are the shapes real files were observed to have, each one
// traceable to a probe of the live offer file; the comments on the individual cases name which. What
// keeps them honest is internal/awsrates/rates_aws_test.go, which runs the same queries against the
// live files under -tags=integration. These tests pin the rules; that one pins the input.

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Fixture builds an offer file's JSON.
type Fixture struct {
	products []product
}

// product is one product plus the price dimensions of its single on-demand term.
type product struct {
	sku    string
	family string
	attrs  map[string]string
	bands  []Band
}

// Band is one price dimension. An empty BeginRange renders the field absent, which is a shape real
// single-band products have.
type Band struct {
	BeginRange string
	USD        string
}

// StandardBand is the one-dimension shape most request-priced products have.
func StandardBand(usd string) []Band {
	return []Band{{BeginRange: "0", USD: usd}}
}

// AddProduct appends a product with the given attributes and a single BeginRange-0 price.
func (f *Fixture) AddProduct(sku, family string, attrs map[string]string, usd string) *Fixture {
	return f.AddBanded(sku, family, attrs, StandardBand(usd))
}

// AddBanded appends a product with explicit price dimensions.
func (f *Fixture) AddBanded(sku, family string, attrs map[string]string, bands []Band) *Fixture {
	f.products = append(f.products, product{sku: sku, family: family, attrs: attrs, bands: bands})

	return f
}

// JSON renders the Fixture as an offer file body.
func (f *Fixture) JSON(t *testing.T) []byte {
	t.Helper()

	type dimension struct {
		BeginRange   string            `json:"beginRange,omitempty"`
		EndRange     string            `json:"endRange,omitempty"`
		Unit         string            `json:"unit,omitempty"`
		PricePerUnit map[string]string `json:"pricePerUnit"`
	}

	type term struct {
		PriceDimensions map[string]dimension `json:"priceDimensions"`
	}

	type jsonProduct struct {
		SKU           string            `json:"sku"`
		ProductFamily string            `json:"productFamily,omitempty"`
		Attributes    map[string]string `json:"attributes"`
	}

	doc := struct {
		Products map[string]jsonProduct `json:"products"`
		Terms    struct {
			OnDemand map[string]map[string]term `json:"OnDemand"`
		} `json:"terms"`
	}{
		Products: map[string]jsonProduct{},
	}

	doc.Terms.OnDemand = map[string]map[string]term{}

	for _, p := range f.products {
		doc.Products[p.sku] = jsonProduct{SKU: p.sku, ProductFamily: p.family, Attributes: p.attrs}

		dims := map[string]dimension{}

		for i, b := range p.bands {
			dims[fmt.Sprintf("%s.JRTCKXETXF.%d", p.sku, i)] = dimension{
				BeginRange:   b.BeginRange,
				Unit:         "GB-Mo",
				PricePerUnit: map[string]string{"USD": b.USD},
			}
		}

		doc.Terms.OnDemand[p.sku] = map[string]term{
			p.sku + ".JRTCKXETXF": {PriceDimensions: dims},
		}
	}

	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshaling Fixture: %v", err)
	}

	return body
}

// CompleteS3Offer is a well-formed S3 offer file: every query in byClass resolves, unambiguously, to
// the value recorded in WantRates below.
//
// prefix is the region's usagetype prefix, so a test can build the same file for us-east-1 (empty
// prefix) and for a prefixed region and require the same rates out of both.
func CompleteS3Offer(prefix, location string) *Fixture {
	f := &Fixture{}

	// Storage. The Standard product is also what derivePrefix anchors on, so it carries the three
	// attributes that identify it and the three volume bands the real one has.
	f.AddBanded("SKU-STD-STORAGE", "Storage", map[string]string{
		"usagetype":    prefix + "TimedStorage-ByteHrs",
		"volumeType":   "Standard",
		"storageClass": "General Purpose",
		"location":     location,
	}, []Band{
		{BeginRange: "0", USD: "0.0230000000"},
		{BeginRange: "51200", USD: "0.0220000000"},
		{BeginRange: "512000", USD: "0.0210000000"},
	})

	storage := map[string]string{
		"TimedStorage-INT-FA-ByteHrs":  "0.0230000000",
		"TimedStorage-SIA-ByteHrs":     "0.0125000000",
		"TimedStorage-ZIA-ByteHrs":     "0.0100000000",
		"TimedStorage-GIR-ByteHrs":     "0.0040000000",
		"TimedStorage-GlacierByteHrs":  "0.0036000000",
		"TimedStorage-INT-DAA-ByteHrs": "0.0009900000",
		"TimedStorage-RRS-ByteHrs":     "0.0240000000",
	}

	for ut, usd := range storage {
		f.AddProduct("SKU-"+ut, "Storage", map[string]string{
			"usagetype": prefix + ut,
			"location":  location,
		}, usd)
	}

	// Requests. Values are the real us-east-1 per-request prices.
	requests := map[string]string{
		"Requests-Tier1":         "0.0000050000",
		"Requests-Tier2":         "0.0000004000",
		"Requests-INT-Tier1":     "0.0000050000",
		"Requests-INT-Tier2":     "0.0000004000",
		"Requests-SIA-Tier1":     "0.0000100000",
		"Requests-SIA-Tier2":     "0.0000010000",
		"Requests-ZIA-Tier1":     "0.0000100000",
		"Requests-ZIA-Tier2":     "0.0000010000",
		"Requests-GIR-Tier1":     "0.0000200000",
		"Requests-GIR-Tier2":     "0.0000100000",
		"Requests-GLACIER-Tier2": "0.0000100000",
		"Requests-GDA-Tier2":     "0.0001000000",
	}

	for ut, usd := range requests {
		f.AddProduct("SKU-"+ut, "API Request", map[string]string{
			"usagetype": prefix + ut,
			"location":  location,
		}, usd)
	}

	// Retrieval, for the four classes that have one.
	for ut, usd := range map[string]string{
		"Retrieval-SIA": "0.0100000000",
		"Retrieval-ZIA": "0.0100000000",
		"Retrieval-GIR": "0.0300000000",
	} {
		f.AddProduct("SKU-"+ut, "Fee", map[string]string{
			"usagetype": prefix + ut,
			"location":  location,
		}, usd)
	}

	// The three ambiguous usagetypes, each with the several SKUs the real file has. A query that
	// omitted the operation attribute would resolve to whichever of these map iteration reached
	// first, so their presence is what makes this Fixture a test of the rule rather than of nothing.
	f.AddProduct("SKU-GLACIER-T1-PUT", "API Request", map[string]string{
		"usagetype": prefix + "Requests-GLACIER-Tier1",
		"operation": "PutObject",
		"location":  location,
	}, "0.0000300000")

	f.AddProduct("SKU-GLACIER-T1-TRANSITION", "API Request", map[string]string{
		"usagetype": prefix + "Requests-GLACIER-Tier1",
		"operation": "S3-GlacierTransition",
		"location":  location,
	}, "0.0000300000")

	f.AddProduct("SKU-TIER3-RESTORE", "API Request", map[string]string{
		"usagetype": prefix + "Requests-Tier3",
		"operation": "RestoreObject",
		"location":  location,
	}, "0.0000500000")

	f.AddProduct("SKU-TIER3-GDA", "API Request", map[string]string{
		"usagetype": prefix + "Requests-Tier3",
		"operation": "S3-GDATransition",
		"location":  location,
	}, "0.0000500000")

	f.AddProduct("SKU-TIER3-GLACIER", "API Request", map[string]string{
		"usagetype": prefix + "Requests-Tier3",
		"operation": "S3-GlacierTransition",
		"location":  location,
	}, "0.0000300000")

	f.AddProduct("SKU-STDRETRIEVAL-GLACIER", "Fee", map[string]string{
		"usagetype": prefix + "Standard-Retrieval-Bytes",
		"operation": "RestoreObject",
		"location":  location,
	}, "0.0100000000")

	f.AddProduct("SKU-STDRETRIEVAL-GDA", "Fee", map[string]string{
		"usagetype": prefix + "Standard-Retrieval-Bytes",
		"operation": "DeepArchiveRestoreObject",
		"location":  location,
	}, "0.0200000000")

	return f
}

// CompleteDataTransfer is a well-formed AWSDataTransfer file for one location.
//
// It carries the shapes that made extractEgress's rules necessary: a $0.0 SKU on the same
// fromLocation, a Global- usagetype, and an inbound SKU. Each of those matches three of the four
// attribute filters.
func CompleteDataTransfer(location string) *Fixture {
	f := &Fixture{}

	f.AddProduct("DT-OUT", "Data Transfer", map[string]string{
		"usagetype":        "DataTransfer-Out-Bytes",
		"fromLocation":     location,
		"fromLocationType": "AWS Region",
		"toLocation":       "External",
		"transferType":     "AWS Outbound",
	}, "0.0900000000")

	// Free tier on its own SKU, same four attributes. Taking the lowest rate would return 0 and
	// price every byte leaving the region as free.
	f.AddProduct("DT-OUT-FREE", "Data Transfer", map[string]string{
		"usagetype":        "DataTransfer-Out-Bytes",
		"fromLocation":     location,
		"fromLocationType": "AWS Region",
		"toLocation":       "External",
		"transferType":     "AWS Outbound",
	}, "0.0000000000")

	// Local-zone aggregate, published at 0.0 and matching all four attributes.
	f.AddProduct("DT-OUT-GLOBAL", "Data Transfer", map[string]string{
		"usagetype":        "Global-DataTransfer-Out-Bytes",
		"fromLocation":     location,
		"fromLocationType": "AWS Region",
		"toLocation":       "External",
		"transferType":     "AWS Outbound",
	}, "0.0000000000")

	// Inbound is free and must not be confused for egress.
	f.AddProduct("DT-IN", "Data Transfer", map[string]string{
		"usagetype":        "DataTransfer-In-Bytes",
		"fromLocation":     "External",
		"fromLocationType": "External",
		"toLocation":       location,
		"transferType":     "AWS Inbound",
	}, "0.0000000000")

	return f
}

// WantRates is what CompleteS3Offer plus CompleteDataTransfer must extract to.
//
// Spelled out per class rather than computed from the Fixture, so a bug that reads the wrong SKU is
// caught. Deriving these from the same maps the Fixture is built from would assert only that the code
// is self-consistent — the failure mode the Glacier PUT defect had, where the test and the table were
// wrong together.
var WantRates = map[string]struct {
	Storage, Put, Get, List, Retrieval float64
}{
	"STANDARD":            {0.023, 0.000005, 0.0000004, 0.000005, 0},
	"INTELLIGENT_TIERING": {0.023, 0.000005, 0.0000004, 0.000005, 0},
	"STANDARD_IA":         {0.0125, 0.00001, 0.000001, 0.00001, 0.01},
	"ONEZONE_IA":          {0.01, 0.00001, 0.000001, 0.00001, 0.01},
	"GLACIER_IR":          {0.004, 0.00002, 0.00001, 0.00002, 0.03},
	// PUT is 0.00003 from Requests-GLACIER-Tier1 operation=PutObject, not 0.00005 from
	// Requests-Tier3; that substitution is the defect this whole package exists to have prevented.
	"GLACIER":      {0.0036, 0.00003, 0.00001, 0.000005, 0.01},
	"DEEP_ARCHIVE": {0.00099, 0.00005, 0.0001, 0.000005, 0.02},
	// RRS is priced above Standard, which is why nothing should select it.
	"REDUCED_REDUNDANCY": {0.024, 0.000005, 0.0000004, 0.000005, 0},
}

const WantEgress = 0.09
