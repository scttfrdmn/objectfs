package offerfile

// The fetcher, driven against a local server rather than against AWS.
//
// NewFetcherAt exists for this: the extraction rules are testable without a network for the same
// reason internal/testaws exists for the S3 backend. What is under test here is the URL construction,
// the region-index filtering, and the two error paths that would otherwise turn a failed download into
// a table of zeros.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/awsrates/offerfile/offertest"
)

// parse unmarshals an offer file body for the tests that call lookup directly.
func parse(t *testing.T, body []byte) *offer {
	t.Helper()

	var o offer
	if err := json.Unmarshal(body, &o); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}

	return &o
}

// offerServer serves the three paths the fetcher asks for, out of a map keyed by path.
//
// Anything not in the map is a 404, which is how the "AWSDataTransfer file missing" case is built:
// the real failure mode is a region present in the S3 index and absent from the transfer one.
func offerServer(t *testing.T, bodies map[string][]byte) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))

	t.Cleanup(srv.Close)

	return srv
}

func s3Path(region string) string {
	return "/offers/v1.0/aws/AmazonS3/current/" + region + "/index.json"
}

func dtPath(region string) string {
	return "/offers/v1.0/aws/AWSDataTransfer/current/" + region + "/index.json"
}

const regionIndexPath = "/offers/v1.0/aws/AmazonS3/current/region_index.json"

// regionIndexJSON renders a region_index.json listing the given codes.
func regionIndexJSON(t *testing.T, codes ...string) []byte {
	t.Helper()

	idx := regionIndex{Regions: map[string]struct {
		RegionCode        string `json:"regionCode"`
		CurrentVersionURL string `json:"currentVersionUrl"`
	}{}}

	for _, c := range codes {
		idx.Regions[c] = struct {
			RegionCode        string `json:"regionCode"`
			CurrentVersionURL string `json:"currentVersionUrl"`
		}{RegionCode: c, CurrentVersionURL: "/offers/v1.0/aws/AmazonS3/current/" + c + "/index.json"}
	}

	body, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshaling region index: %v", err)
	}

	return body
}

// TestFetcherRegionsFiltersNonRegions pins the region-index filtering.
//
// "aws-other" is an entry in the real index and is not a region; asking for its offer file returns
// products belonging to no region, from which no prefix can be derived. The local zones are
// deliberately *not* filtered here — they are fetched and then surface as ErrNoS3Rates, so that
// skipping them cannot also skip a region whose file failed to download.
func TestFetcherRegionsFiltersNonRegions(t *testing.T) {
	t.Parallel()

	srv := offerServer(t, map[string][]byte{
		regionIndexPath: regionIndexJSON(t,
			"us-east-1", "us-west-2", "aws-other", "eu-central-1-ath-1"),
	})

	got, err := NewFetcherAt(srv.Client(), srv.URL).Regions(t.Context())
	if err != nil {
		t.Fatalf("Regions: %v", err)
	}

	want := []string{"eu-central-1-ath-1", "us-east-1", "us-west-2"}

	if len(got) != len(want) {
		t.Fatalf("Regions() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Regions() = %v, want %v (sorted, aws-other removed, local zones kept so they "+
				"fail loudly as ErrNoS3Rates rather than being silently absent)", got, want)
		}
	}
}

// TestFetcherRegionFetchesBothFiles asserts the two URLs and the assembled result.
func TestFetcherRegionFetchesBothFiles(t *testing.T) {
	t.Parallel()

	const (
		code     = "us-west-2"
		prefix   = "USW2-"
		location = "US West (Oregon)"
	)

	srv := offerServer(t, map[string][]byte{
		s3Path(code): offertest.CompleteS3Offer(prefix, location).JSON(t),
		dtPath(code): offertest.CompleteDataTransfer(location).JSON(t),
	})

	region, err := NewFetcherAt(srv.Client(), srv.URL).Region(t.Context(), code)
	if err != nil {
		t.Fatalf("Region: %v", err)
	}

	if region.Code != code || region.Prefix != prefix || region.Location != location {
		t.Errorf("Region = {%q, %q, %q}, want {%q, %q, %q}",
			region.Code, region.Prefix, region.Location, code, prefix, location)
	}

	if got := region.Rates[awsname.StorageClassStandard].EgressPerGB; got != offertest.WantEgress {
		t.Errorf("EgressPerGB = %v, want %v; the AWSDataTransfer file was not read", got, offertest.WantEgress)
	}
}

// TestFetcherRegionFailsWithoutTheDataTransferFile is the reason that file is fetched
// unconditionally.
//
// Fetching it lazily, or tolerating a 404, leaves EgressPerGB at zero for that region — and a zero
// egress rate is not an obviously wrong number. It reads as a region where transfer out is free.
func TestFetcherRegionFailsWithoutTheDataTransferFile(t *testing.T) {
	t.Parallel()

	const code = "us-west-2"

	srv := offerServer(t, map[string][]byte{
		s3Path(code): offertest.CompleteS3Offer("USW2-", "US West (Oregon)").JSON(t),
		// no dtPath entry
	})

	_, err := NewFetcherAt(srv.Client(), srv.URL).Region(t.Context(), code)
	if err == nil {
		t.Fatal("Region succeeded with no AWSDataTransfer file; EgressPerGB would be zero, which " +
			"prices every byte leaving the region as free")
	}

	if !strings.Contains(err.Error(), "AWSDataTransfer") {
		t.Errorf("error %q does not name the file that was missing", err)
	}
}

// TestFetcherReportsHTTPStatus pins that a non-200 is an error rather than a parse of an error page.
//
// AWS serves an XML error document for a missing offer file. Unmarshaling that into the offer struct
// succeeds — the fields are simply absent — so without the status check the failure would surface as
// ErrNoS3Rates and the generator would skip the region by design.
func TestFetcherReportsHTTPStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>AccessDenied</Code></Error>`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewFetcherAt(srv.Client(), srv.URL).Region(t.Context(), "us-west-2")
	if err == nil {
		t.Fatal("Region succeeded against a 403; an XML error page unmarshals into the offer struct " +
			"as an empty file, which would surface as ErrNoS3Rates and be skipped as a local zone")
	}

	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q does not carry the HTTP status", err)
	}
}

// TestNewFetcherTargetsThePublicEndpoint pins the default base URL, which is the one thing about the
// production fetcher a local server cannot check.
func TestNewFetcherTargetsThePublicEndpoint(t *testing.T) {
	t.Parallel()

	f := NewFetcher()

	if f.baseURL != BaseURL {
		t.Errorf("baseURL = %q, want %q", f.baseURL, BaseURL)
	}

	if !strings.HasPrefix(BaseURL, "https://") {
		t.Errorf("BaseURL %q is not https; these files are fetched unauthenticated, so the "+
			"transport is the only thing establishing they came from AWS", BaseURL)
	}

	if f.client == nil || f.client.Timeout == 0 {
		t.Error("NewFetcher's client has no timeout; a generator that hangs on a stalled connection " +
			"hangs `go generate` with no output")
	}
}
