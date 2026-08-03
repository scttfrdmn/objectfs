package offerfile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

// BaseURL is the root of the AWS price list bulk API.
//
// Unauthenticated and public: these URLs need no credentials, no SDK, and no account. That is why
// the generator uses them rather than the Pricing API — the Pricing API needs credentials, is only
// reachable in three regions, and returns the same numbers one SKU at a time.
const BaseURL = "https://pricing.us-east-1.amazonaws.com"

// regionIndex is the shape of an offer's region_index.json.
type regionIndex struct {
	Regions map[string]struct {
		RegionCode        string `json:"regionCode"`
		CurrentVersionURL string `json:"currentVersionUrl"`
	} `json:"regions"`
}

// nonRegions are entries in the S3 region index that are not regions ObjectFS can price.
//
// "aws-other" is a bucket for products that belong to no region. The three local zones parse fine
// and publish no S3 storage product at all, so they surface as [ErrNoS3Rates] rather than being
// listed here — the list is only for what should not be fetched in the first place.
var nonRegions = []string{"aws-other"}

// Fetcher retrieves offer files over HTTP.
//
// The zero value is not usable; use [NewFetcher]. It exists as a type so the tests can point it at
// a local server instead of at AWS, which is what makes the extraction rules testable without a
// network — the same reason internal/testaws exists for the S3 backend.
type Fetcher struct {
	client  *http.Client
	baseURL string
}

// NewFetcher returns a Fetcher against AWS's public price list endpoint.
func NewFetcher() *Fetcher {
	return &Fetcher{
		// A generous timeout: the per-region files are around 500 KB and the AWSDataTransfer
		// files are several MB, and this runs under `go generate` rather than on any hot path.
		client:  &http.Client{Timeout: 2 * time.Minute},
		baseURL: BaseURL,
	}
}

// NewFetcherAt returns a Fetcher against an arbitrary base URL, for tests.
func NewFetcherAt(client *http.Client, baseURL string) *Fetcher {
	return &Fetcher{client: client, baseURL: baseURL}
}

// Regions lists the region codes the S3 offer publishes, excluding the non-regions.
func (f *Fetcher) Regions(ctx context.Context) ([]string, error) {
	body, err := f.get(ctx, "/offers/v1.0/aws/AmazonS3/current/region_index.json")
	if err != nil {
		return nil, err
	}

	var idx regionIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("parse S3 region index: %w", err)
	}

	codes := make([]string, 0, len(idx.Regions))

	for code := range idx.Regions {
		if slices.Contains(nonRegions, code) {
			continue
		}

		codes = append(codes, code)
	}

	slices.Sort(codes)

	return codes, nil
}

// Region fetches and extracts one region's rates.
//
// Returns an error wrapping [ErrNoS3Rates] for a local zone, which a caller generating a table should
// skip rather than treat as a failure.
func (f *Fetcher) Region(ctx context.Context, code string) (Region, error) {
	s3Offer, err := f.get(ctx, fmt.Sprintf("/offers/v1.0/aws/AmazonS3/current/%s/index.json", code))
	if err != nil {
		return Region{}, err
	}

	// Fetched unconditionally rather than lazily, because a region with no AWSDataTransfer file is
	// a condition worth failing on: EgressPerGB silently zero would price every byte leaving the
	// region as free.
	dt, err := f.get(ctx, fmt.Sprintf("/offers/v1.0/aws/AWSDataTransfer/current/%s/index.json", code))
	if err != nil {
		return Region{}, err
	}

	return Extract(code, s3Offer, dt)
}

func (f *Fetcher) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", path, err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return body, nil
}
