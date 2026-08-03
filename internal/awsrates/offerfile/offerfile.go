// Package offerfile extracts ObjectFS's rate table from an AWS price list offer file.
//
// It exists so that the rules for reading a rate out of AWS's published JSON are ordinary Go code
// with ordinary tests, rather than a script someone ran once. Every rule below was arrived at by
// finding a case where the obvious version returns the wrong number, and each of those cases is a
// test in this package. Nothing in the production path imports this — [awsrates] holds the generated
// result — so a mount carries none of this code and needs no network to price a tier.
//
// # Where the input comes from
//
// The per-region offer file, not the AWS Pricing API and not the global index:
//
//	https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonS3/current/<region>/index.json
//
// It needs no credentials and is about 500 KB. The global AmazonS3 index is 59 MB and contains every
// region, which is what the removed v0.10.1 pricing code downloaded in order to use one region of
// it. Egress comes from a second file, AWSDataTransfer, for the reason recorded on [Extract].
//
// # The rules, and the case that forces each one
//
// **The region prefix is derived, never assumed.** A usagetype is prefixed with an abbreviation of
// the region — USE1, USW2, APS8 — that is not the region code and has no published mapping. So it is
// recovered structurally: find the product with productFamily "Storage", volumeType "Standard" and
// storageClass "General Purpose", and strip the known suffix TimedStorage-ByteHrs from its
// usagetype. Whatever remains is the prefix for that file. In us-east-1 it is the empty string,
// which is itself a case worth having: a prefix-derivation bug that returns "" looks correct there.
//
// **Suffixes match exactly, never with strings.HasSuffix.** USW2-Tables-TimedStorage-ByteHrs,
// -Annotation-TimedStorage-ByteHrs, -Files-TimedStorage-ByteHrs and
// -Vectors-TimedStorage-ByteHrs all end in TimedStorage-ByteHrs. A suffix match picks up whichever
// of those the map iteration reaches first, and prices S3 Tables storage as Standard storage.
//
// **usagetype is not a unique key for a rate.** Six S3 usagetypes carry several distinct prices at
// the same beginRange, separated only by the operation attribute. Two of them decide numbers in this
// table: Requests-Tier3 is $0.00005 for operation RestoreObject and $0.00003 for
// S3-GlacierTransition, and Standard-Retrieval-Bytes is $0.01 for RestoreObject and $0.02 for
// DeepArchiveRestoreObject. Requests-GLACIER-Tier1 has fifteen SKUs at two prices. Where a rate is
// ambiguous the operation is part of the query.
//
// **volumeType is checked as well, as a belt rather than a brace.** ap-southeast-6 publishes three
// SKUs on APS8-TimedStorage-ByteHrs: Standard at $0.02625, and two Intelligent-Tiering
// object-overhead SKUs at $0.02415. An earlier version of this comment said volumeType was the only
// thing separating them. It is not: the two overhead SKUs carry operation "INTAAS3ObjectOverhead" and
// "INTDAAS3ObjectOverhead", so the empty-operation rule above already excludes them — checked in all
// 36 rate-bearing regions, where dropping the volumeType clause changes no extracted rate. The field
// is kept anyway, on the one query where AWS has repeatedly added SKUs to an existing usagetype, and
// TestVolumeTypeSeparatesAnOperationlessOverheadSKU pins what it would catch. Believing a redundant
// check is the load-bearing one is its own hazard, so the redundancy is recorded rather than implied.
//
// **Storage is volume-banded, so the band is selected explicitly.** Standard storage is three price
// dimensions — $0.023 for the first 50 TB, then $0.022, then $0.021. Taking the first entry of the
// priceDimensions map yields a different band on different runs, because Go randomizes map order.
// beginRange "0" is the band a deployment under 50 TB pays, and erring toward the more expensive
// band keeps a tier-transition recommendation conservative.
package offerfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates"
)

// offer is the subset of an AWS price list offer file this package reads.
//
// Deliberately not the whole schema. The files carry per-SKU attribute sets that differ between
// products — 315 of us-east-1's 381 S3 products omit productFamily entirely — so attributes is a
// map rather than a struct, and a missing key is a normal condition rather than a parse error.
type offer struct {
	Products map[string]struct {
		SKU           string            `json:"sku"`
		ProductFamily string            `json:"productFamily"`
		Attributes    map[string]string `json:"attributes"`
	} `json:"products"`

	Terms struct {
		OnDemand map[string]map[string]struct {
			PriceDimensions map[string]struct {
				BeginRange   string            `json:"beginRange"`
				EndRange     string            `json:"endRange"`
				Unit         string            `json:"unit"`
				PricePerUnit map[string]string `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// query identifies one published rate.
//
// Every field that is set has to match. operation and volumeType are empty for the queries where the
// usagetype alone is unambiguous, and an empty operation means "the product has no operation
// attribute, or it is the empty string" rather than "any operation" — see [product.matches]. That
// distinction is load-bearing: Requests-Tier1 has no operation, and treating empty as a wildcard
// would let it match Requests-Tier1 SKUs from other services in files that have them.
type query struct {
	// usagetype is the suffix after the region prefix, matched exactly.
	usagetype string

	// operation disambiguates a usagetype that carries several prices. Empty means the product's
	// operation must also be empty.
	operation string

	// volumeType disambiguates further where operation cannot. Empty means unchecked, because
	// most products that need no volumeType check do not publish one.
	volumeType string
}

func (q query) String() string {
	s := q.usagetype
	if q.operation != "" {
		s += " operation=" + q.operation
	}

	if q.volumeType != "" {
		s += " volumeType=" + q.volumeType
	}

	return s
}

// classQueries names the query for each field of an [awsrates.Rate], per storage class.
//
// Read this as the documentation of what ObjectFS means by each rate. Several entries are a choice
// among things AWS prices separately, and the choice is recorded next to it rather than in a
// changelog.
type classQueries struct {
	storage   query
	put       query
	get       query
	list      query
	retrieval query // zero value means "this class has no retrieval fee", not "not found"
}

// byClass is the whole specification.
//
// The five archival entries are where this stops being transcription. AWS does not publish a "PUT to
// Deep Archive" price, does not publish Deep Archive storage under the name the tier has, and prices
// four different retrieval speeds; each substitution below is a decision with a cost attached if it
// is wrong.
var byClass = map[string]classQueries{
	awsname.StorageClassStandard: {
		storage: query{usagetype: "TimedStorage-ByteHrs", volumeType: "Standard"},
		put:     query{usagetype: "Requests-Tier1"},
		get:     query{usagetype: "Requests-Tier2"},
		list:    query{usagetype: "Requests-Tier1"},
	},
	awsname.StorageClassIntelligent: {
		// Frequent Access, the tier an object lands in and the one it is in while being written.
		// Intelligent-Tiering's cheaper tiers are what it moves to, so quoting one of them would
		// describe a saving the object has not made yet.
		storage: query{usagetype: "TimedStorage-INT-FA-ByteHrs"},
		put:     query{usagetype: "Requests-INT-Tier1"},
		get:     query{usagetype: "Requests-INT-Tier2"},
		list:    query{usagetype: "Requests-INT-Tier1"},
	},
	awsname.StorageClassStandardIA: {
		storage:   query{usagetype: "TimedStorage-SIA-ByteHrs"},
		put:       query{usagetype: "Requests-SIA-Tier1"},
		get:       query{usagetype: "Requests-SIA-Tier2"},
		list:      query{usagetype: "Requests-SIA-Tier1"},
		retrieval: query{usagetype: "Retrieval-SIA"},
	},
	awsname.StorageClassOneZoneIA: {
		storage:   query{usagetype: "TimedStorage-ZIA-ByteHrs"},
		put:       query{usagetype: "Requests-ZIA-Tier1"},
		get:       query{usagetype: "Requests-ZIA-Tier2"},
		list:      query{usagetype: "Requests-ZIA-Tier1"},
		retrieval: query{usagetype: "Retrieval-ZIA"},
	},
	awsname.StorageClassGlacierIR: {
		storage:   query{usagetype: "TimedStorage-GIR-ByteHrs"},
		put:       query{usagetype: "Requests-GIR-Tier1"},
		get:       query{usagetype: "Requests-GIR-Tier2"},
		list:      query{usagetype: "Requests-GIR-Tier1"},
		retrieval: query{usagetype: "Retrieval-GIR"},
	},
	awsname.StorageClassGlacier: {
		storage: query{usagetype: "TimedStorage-GlacierByteHrs"},

		// Requests-GLACIER-Tier1 operation=PutObject: a direct PUT with
		// x-amz-storage-class: GLACIER, which is what ObjectFS issues.
		//
		// This is the field that made the operation attribute non-optional. The rate here was
		// $0.00005 until this generator replaced it, taken from Requests-Tier3 — which is
		// RestoreObject, the price of *thawing* an object, 67% above the PUT. The same usagetype
		// also carries S3-GlacierTransition at $0.00003, the lifecycle price, which happens to
		// equal the PUT in every region checked; a query that resolved to it would have been
		// right for the wrong reason and would drift the first time AWS priced them apart.
		put: query{usagetype: "Requests-GLACIER-Tier1", operation: "PutObject"},

		// GET against a Glacier object is a Tier2 request; the request succeeds and returns
		// InvalidObjectState until the object is restored. The bytes are priced by retrieval.
		get: query{usagetype: "Requests-GLACIER-Tier2"},

		// LIST is not per-class: keys live in the bucket regardless of what tier the objects are
		// in, so a listing is billed at the Standard Tier1 rate.
		list: query{usagetype: "Requests-Tier1"},

		// Standard retrieval speed. Expedited is roughly 3× and Bulk is free; modeling that needs
		// a retrieval-speed concept the cost code does not have, and Standard is what a restore
		// gets when nothing asks for a speed.
		retrieval: query{usagetype: "Standard-Retrieval-Bytes", operation: "RestoreObject"},
	},
	awsname.StorageClassDeepArchive: {
		// TimedStorage-GDA-ByteHrs does not exist. Not in us-east-1, and not in any of the 36
		// regions that publish S3 rates — checked in every one rather than inferred from one.
		//
		// The only Deep Archive storage usagetype AWS publishes is TimedStorage-GDA-Staging at
		// $0.021, which is the *staging* charge and is 21× the real rate; a generator that reached
		// for the plausible name and fell back to the one that exists would report Deep Archive as
		// more expensive than Standard, which is worse than the static table it replaced.
		//
		// TimedStorage-INT-DAA-ByteHrs is the Intelligent-Tiering Deep Archive Access tier, whose
		// price is the Deep Archive price — $0.00099 in us-east-1, matching AWS's published Deep
		// Archive rate. The substitution is not assumed to be safe in general: it holds because
		// GLACIER storage equals INT-AA storage in 36 of 36 regions, so the two families are
		// priced together, and TestArchiveSubstitutionHoldsAcrossRegions pins that. It is
		// deliberately not applied to GLACIER_IR, where INT-AIA runs about 16% higher in 15 of the
		// 36 regions.
		storage: query{usagetype: "TimedStorage-INT-DAA-ByteHrs"},

		// There is no PUT-to-Deep-Archive usagetype at all — unlike Glacier, which has
		// Requests-GLACIER-Tier1. Requests-Tier3 operation=S3-GDATransition is the lifecycle
		// transition price, $0.00005, and it is what this field has always held; naming the
		// operation here changes nothing about the number and everything about whether the query
		// is deterministic, since Requests-Tier3 resolves to three SKUs at two prices.
		//
		// It is the closest published rate to what a direct PUT costs, and it is the rate an
		// operator using a lifecycle rule to reach this tier actually pays.
		put: query{usagetype: "Requests-Tier3", operation: "S3-GDATransition"},

		get:  query{usagetype: "Requests-GDA-Tier2"},
		list: query{usagetype: "Requests-Tier1"},

		// Standard retrieval, 12 hours. Bulk is 48 hours and about an eighth of the price.
		retrieval: query{usagetype: "Standard-Retrieval-Bytes", operation: "DeepArchiveRestoreObject"},
	},
	awsname.StorageClassReducedRedundancy: {
		// Deprecated by AWS and priced above Standard, which is the whole reason nothing should
		// choose it. The entry exists because awsname admits the class, and a class with no rate
		// would price at zero — see TestEveryStorageClassHasARate in the parent package.
		storage: query{usagetype: "TimedStorage-RRS-ByteHrs"},

		// RRS has no request usagetypes of its own; requests against it bill as Tier1/Tier2.
		put:  query{usagetype: "Requests-Tier1"},
		get:  query{usagetype: "Requests-Tier2"},
		list: query{usagetype: "Requests-Tier1"},
	},
}

// standardStorageSuffix is the usagetype suffix the region prefix is derived from.
const standardStorageSuffix = "TimedStorage-ByteHrs"

// Region is one region's extracted rates.
type Region struct {
	// Code is the AWS region code, e.g. "us-west-2".
	Code string

	// Location is the human-readable location AWS labels the region with, e.g. "US West (Oregon)".
	// Carried because the AWSDataTransfer offer file keys its products on this rather than on the
	// region code — see [Extract].
	Location string

	// Prefix is the usagetype prefix derived from this file, e.g. "USW2-". Empty for us-east-1.
	Prefix string

	// Rates is keyed by storage class, covering every class in awsname.StorageClasses.
	Rates map[string]awsrates.Rate
}

// ErrNoS3Rates reports that a file parsed but published no S3 storage rates.
//
// Three of the 40 entries in the S3 region index are local zones — ap-southeast-1-han-1,
// eu-central-1-ath-1, eu-central-1-ist-1 — whose files are well-formed and contain no S3 storage
// product at all, and "aws-other" is not a region. A caller generating a table skips these; the
// distinct error is so that skipping them cannot also swallow a real parse failure.
var ErrNoS3Rates = errors.New("offer file publishes no S3 storage rates")

// Extract reads one region's rates from its S3 offer file and the egress rate from the
// AWSDataTransfer file for the same region.
//
// Two files, because S3's own DataTransfer-Out-Bytes is not internet egress. In us-east-1 it carries
// exactly two prices: $0.0033, the Multi-Region Access Point data routing charge, and $0.0. The
// $0.09 a byte leaving the region to the internet costs is published by AWSDataTransfer, under
// toLocation "External", transferType "AWS Outbound".
//
// dataTransfer may be nil, in which case EgressPerGB is left at zero and the caller is responsible
// for deciding whether that is acceptable. It is separate from the S3 file for a structural reason
// as well as a pricing one: AWSDataTransfer products carry no regionCode attribute, so they can only
// be matched on fromLocation — which is why [Region.Location] is read out of the S3 file first.
func Extract(regionCode string, s3Offer, dataTransfer []byte) (Region, error) {
	var o offer
	if err := json.Unmarshal(s3Offer, &o); err != nil {
		return Region{}, fmt.Errorf("parse S3 offer file for %s: %w", regionCode, err)
	}

	prefix, location, err := derivePrefix(&o)
	if err != nil {
		return Region{}, err
	}

	egress := 0.0

	if dataTransfer != nil {
		egress, err = extractEgress(location, dataTransfer)
		if err != nil {
			return Region{}, fmt.Errorf("egress for %s: %w", regionCode, err)
		}
	}

	rates := make(map[string]awsrates.Rate, len(byClass))

	for _, class := range awsname.StorageClasses() {
		q, ok := byClass[class]
		if !ok {
			return Region{}, fmt.Errorf("%s: no queries defined for storage class %q; "+
				"awsname admits it, so leaving it out prices it at zero", regionCode, class)
		}

		rate, err := q.resolve(&o, prefix)
		if err != nil {
			return Region{}, fmt.Errorf("%s %s: %w", regionCode, class, err)
		}

		rate.EgressPerGB = egress
		rates[class] = rate
	}

	return Region{Code: regionCode, Location: location, Prefix: prefix, Rates: rates}, nil
}

// resolve looks up every field of one class's Rate.
func (q classQueries) resolve(o *offer, prefix string) (awsrates.Rate, error) {
	storage, err := lookup(o, prefix, q.storage)
	if err != nil {
		return awsrates.Rate{}, fmt.Errorf("storage: %w", err)
	}

	put, err := lookup(o, prefix, q.put)
	if err != nil {
		return awsrates.Rate{}, fmt.Errorf("put: %w", err)
	}

	get, err := lookup(o, prefix, q.get)
	if err != nil {
		return awsrates.Rate{}, fmt.Errorf("get: %w", err)
	}

	list, err := lookup(o, prefix, q.list)
	if err != nil {
		return awsrates.Rate{}, fmt.Errorf("list: %w", err)
	}

	// A zero query means the class has no retrieval fee. Distinguished from "the lookup found
	// nothing", which is an error: Standard genuinely has no Retrieval- usagetype, and treating a
	// missing one as free would also report Glacier retrieval as free if AWS renamed it.
	retrieval := 0.0

	if q.retrieval != (query{}) {
		retrieval, err = lookup(o, prefix, q.retrieval)
		if err != nil {
			return awsrates.Rate{}, fmt.Errorf("retrieval: %w", err)
		}
	}

	return awsrates.Rate{
		StoragePerGBMonth: storage,
		PutRequest:        put,
		GetRequest:        get,
		ListRequest:       list,
		RetrievalPerGB:    retrieval,
	}, nil
}

// derivePrefix recovers the region's usagetype prefix and location from the file itself.
//
// See the package comment for why this is not a lookup table. The Standard storage product is the
// anchor because it is the one product every S3 offer file has and whose identity is expressible
// without knowing the prefix: productFamily Storage, volumeType Standard, storageClass
// General Purpose.
func derivePrefix(o *offer) (prefix, location string, err error) {
	found := false

	for _, p := range o.Products {
		if p.ProductFamily != "Storage" ||
			p.Attributes["volumeType"] != "Standard" ||
			p.Attributes["storageClass"] != "General Purpose" {
			continue
		}

		ut := p.Attributes["usagetype"]
		if !strings.HasSuffix(ut, standardStorageSuffix) {
			return "", "", fmt.Errorf("a Standard storage product has usagetype %q, "+
				"which does not end in %q; the prefix cannot be derived from it",
				ut, standardStorageSuffix)
		}

		candidate := ut[:len(ut)-len(standardStorageSuffix)]

		if found && candidate != prefix {
			return "", "", fmt.Errorf("two Standard storage products imply different region "+
				"prefixes, %q and %q", prefix, candidate)
		}

		prefix, location, found = candidate, p.Attributes["location"], true
	}

	if !found {
		return "", "", ErrNoS3Rates
	}

	return prefix, location, nil
}

// lookup returns the price for one query, at beginRange "0".
//
// Every SKU matching the query must agree on that price. Disagreement is an error rather than a
// choice, because choosing would mean the answer depends on map iteration order — which is exactly
// how the Glacier PUT rate came to be a restore price.
func lookup(o *offer, prefix string, q query) (float64, error) {
	want := prefix + q.usagetype

	var (
		price   float64
		matched []string
		found   bool
	)

	for sku, p := range o.Products {
		a := p.Attributes
		if a["usagetype"] != want ||
			a["operation"] != q.operation ||
			(q.volumeType != "" && a["volumeType"] != q.volumeType) {
			continue
		}

		v, ok, err := firstBandPrice(o, sku)
		if err != nil {
			return 0, err
		}

		if !ok {
			continue
		}

		if found && v != price {
			return 0, fmt.Errorf("%s is ambiguous: SKUs %s and %s publish %.10f and %.10f at "+
				"beginRange 0; the query needs another attribute to separate them",
				q, strings.Join(matched, ","), sku, price, v)
		}

		price, matched, found = v, append(matched, sku), true
	}

	if !found {
		return 0, fmt.Errorf("no product matches %s", q)
	}

	return price, nil
}

// firstBandPrice returns the USD price of a SKU's beginRange-0 on-demand dimension.
//
// ok is false when the SKU has on-demand terms but no dimension starting at zero, which happens for
// products priced only above a threshold. That is a skip rather than an error so that such a SKU
// cannot make an otherwise-unambiguous query fail.
func firstBandPrice(o *offer, sku string) (price float64, ok bool, err error) {
	for _, term := range o.Terms.OnDemand[sku] {
		for _, dim := range term.PriceDimensions {
			// "" as well as "0": a single-band dimension sometimes omits beginRange entirely.
			if dim.BeginRange != "" && dim.BeginRange != "0" {
				continue
			}

			raw, has := dim.PricePerUnit["USD"]
			if !has {
				return 0, false, fmt.Errorf("SKU %s has no USD price", sku)
			}

			v, convErr := strconv.ParseFloat(raw, 64)
			if convErr != nil {
				return 0, false, fmt.Errorf("SKU %s price %q: %w", sku, raw, convErr)
			}

			if ok && v != price {
				return 0, false, fmt.Errorf("SKU %s has two beginRange-0 dimensions, "+
					"%.10f and %.10f", sku, price, v)
			}

			price, ok = v, true
		}
	}

	return price, ok, nil
}

// extractEgress returns the per-GB internet egress rate for a region's location.
//
// Keyed on fromLocation because AWSDataTransfer products carry no regionCode. The three other
// attributes are what separate internet egress from the several dozen other things this file prices:
// inter-region transfer, CloudFront origin fetches, Direct Connect, and local-zone traffic.
//
// The lowest beginRange-0 rate among the matching SKUs, not the only one: several regions publish
// both a $0.0 dimension and the real rate on separate SKUs, and a few publish a free tier. Taking
// the maximum would be wrong in the other direction, so the rule is the highest non-zero — a $0.0
// egress rate would silently price every transfer out of the region as free.
func extractEgress(location string, dataTransfer []byte) (float64, error) {
	if location == "" {
		return 0, errors.New("the S3 offer file gave no location for this region, " +
			"and AWSDataTransfer products can only be matched on fromLocation")
	}

	var o offer
	if err := json.Unmarshal(dataTransfer, &o); err != nil {
		return 0, fmt.Errorf("parse AWSDataTransfer offer file: %w", err)
	}

	best := 0.0

	for sku, p := range o.Products {
		a := p.Attributes
		if a["fromLocation"] != location ||
			a["fromLocationType"] != "AWS Region" ||
			a["toLocation"] != "External" ||
			a["transferType"] != "AWS Outbound" {
			continue
		}

		// Global- usagetypes are the local-zone aggregates, published at 0.0.
		if strings.Contains(a["usagetype"], "Global-") {
			continue
		}

		v, ok, err := firstBandPrice(&o, sku)
		if err != nil {
			return 0, err
		}

		if ok && v > best {
			best = v
		}
	}

	if best == 0 {
		return 0, fmt.Errorf("no non-zero AWS Outbound rate for fromLocation %q", location)
	}

	return best, nil
}

// Queries returns a description of the query used for each field of each class, for documentation
// and for the drift test in the parent package.
//
// The parent package's integration test re-runs these against the live Pricing API. Exporting the
// strings rather than re-deriving them there is the point: a drift test that spells its own queries
// tests two transcriptions against each other, and this repository already had that bug — the
// Requests-Tier3 query in rates_aws_test.go pinned a restore rate while claiming to pin a PUT.
func Queries() map[string]map[string]string {
	out := make(map[string]map[string]string, len(byClass))

	for class, q := range byClass {
		fields := map[string]string{
			"StoragePerGBMonth": q.storage.String(),
			"PutRequest":        q.put.String(),
			"GetRequest":        q.get.String(),
			"ListRequest":       q.list.String(),
		}

		if q.retrieval != (query{}) {
			fields["RetrievalPerGB"] = q.retrieval.String()
		}

		out[class] = fields
	}

	return out
}

// Classes returns the storage classes this package extracts, warmest-first.
func Classes() []string {
	return slices.Clone(awsname.StorageClasses())
}

// ClassesWithQueries reports the classes byClass covers, for the test that pins it to awsname.
func ClassesWithQueries() []string {
	return slices.Sorted(maps.Keys(byClass))
}
