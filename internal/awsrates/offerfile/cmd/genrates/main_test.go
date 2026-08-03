package main

// The generator, driven end to end against a local server.
//
// This command writes the file every price ObjectFS quotes comes from, and its decisions are the kind
// that produce a plausible file rather than an error: a region skipped by the wrong rule disappears
// from the table silently, and an empty table prices every tier at zero. So what is tested here is not
// that it can fetch — that is the fetcher's own test — but the four judgments it makes about what to do
// with what it fetched.
//
// It reaches AWS in exactly one place, main(), which passes offerfile.NewFetcher(). Everything below
// passes a fetcher pointed at httptest, so this runs with no network and no credentials.

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/awsrates/offerfile"
	"github.com/scttfrdmn/objectfs/internal/awsrates/offerfile/offertest"
)

const timeout = 30 * time.Second

// prefixes are the usagetype prefixes for the regions the fixture index serves, and they are the point
// of using more than one region: a generator that derived one prefix and applied it everywhere would
// pass a single-region test.
var prefixes = map[string]struct{ prefix, location string }{
	"us-east-1": {"", "US East (N. Virginia)"},
	"us-west-2": {"USW2-", "US West (Oregon)"},
	"eu-west-1": {"EU-", "EU (Ireland)"},
}

// server serves a region index plus both offer files for each region named, and 404s everything else.
//
// localZones are listed in the index but served an offer file with no S3 storage products, which is the
// shape of the three local-zone entries in AWS's real index.
func server(t *testing.T, regions []string, localZones ...string) *httptest.Server {
	t.Helper()

	bodies := map[string][]byte{}

	all := append(append([]string{}, regions...), localZones...)
	bodies["/offers/v1.0/aws/AmazonS3/current/region_index.json"] = regionIndexJSON(t, all...)

	for _, code := range regions {
		p, ok := prefixes[code]
		if !ok {
			t.Fatalf("this test has no prefix for %s", code)
		}

		bodies["/offers/v1.0/aws/AmazonS3/current/"+code+"/index.json"] =
			offertest.CompleteS3Offer(p.prefix, p.location).JSON(t)
		bodies["/offers/v1.0/aws/AWSDataTransfer/current/"+code+"/index.json"] =
			offertest.CompleteDataTransfer(p.location).JSON(t)
	}

	for _, code := range localZones {
		// An offer file that parses and holds no S3 storage product. Extract returns ErrNoS3Rates.
		bodies["/offers/v1.0/aws/AmazonS3/current/"+code+"/index.json"] = (&offertest.Fixture{}).JSON(t)
		bodies["/offers/v1.0/aws/AWSDataTransfer/current/"+code+"/index.json"] =
			offertest.CompleteDataTransfer("nowhere").JSON(t)
	}

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

func regionIndexJSON(t *testing.T, codes ...string) []byte {
	t.Helper()

	type entry struct {
		RegionCode        string `json:"regionCode"`
		CurrentVersionURL string `json:"currentVersionUrl"`
	}

	idx := struct {
		Regions map[string]entry `json:"regions"`
	}{Regions: map[string]entry{}}

	for _, c := range codes {
		idx.Regions[c] = entry{
			RegionCode:        c,
			CurrentVersionURL: "/offers/v1.0/aws/AmazonS3/current/" + c + "/index.json",
		}
	}

	body, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshaling region index: %v", err)
	}

	return body
}

func fetcherFor(srv *httptest.Server) *offerfile.Fetcher {
	return offerfile.NewFetcherAt(srv.Client(), srv.URL)
}

// TestRunWritesCompilableGoForEveryRegion is the whole command's happy path.
//
// The output is parsed with go/parser rather than only grepped, because the failure this guards is a
// generator that emits something that reads correctly and does not compile — at which point the package
// every price comes from is unbuildable, and `go generate` has already overwritten the previous file.
func TestRunWritesCompilableGoForEveryRegion(t *testing.T) {
	t.Parallel()

	srv := server(t, []string{"us-east-1", "us-west-2", "eu-west-1"})
	out := filepath.Join(t.TempDir(), "rates_generated.go")

	if err := run(fetcherFor(srv), out, false, timeout); err != nil {
		t.Fatalf("run: %v", err)
	}

	src, err := os.ReadFile(out) //nolint:gosec // a path this test built under t.TempDir()
	if err != nil {
		t.Fatalf("reading the generated file: %v", err)
	}

	if _, err := parser.ParseFile(token.NewFileSet(), out, src, parser.SkipObjectResolution); err != nil {
		t.Fatalf("the generated file is not valid Go: %v", err)
	}

	for _, code := range []string{"us-east-1", "us-west-2", "eu-west-1"} {
		if !strings.Contains(string(src), `"`+code+`"`) {
			t.Errorf("the generated table has no entry for %s, which the index listed and which "+
				"published rates; a region dropped here is priced from us-east-1's table forever "+
				"with nothing reporting it", code)
		}
	}

	if !strings.Contains(string(src), "DO NOT EDIT") {
		t.Error("the generated file carries no DO NOT EDIT marker, so the next person to find a " +
			"wrong number in it edits it by hand and loses the provenance of every other number")
	}
}

// TestRunSkipsLocalZonesAndKeepsRealRegions is the skip rule, in both directions.
//
// Three entries in AWS's real S3 region index are local zones whose offer files contain no S3 storage
// product. Failing on them means this command can never succeed; skipping them silently means a real
// region whose file failed to download disappears the same way. So the rule is narrow — only
// ErrNoS3Rates is skipped — and this asserts both halves: the zone is absent from the output and the
// real regions are still present.
func TestRunSkipsLocalZonesAndKeepsRealRegions(t *testing.T) {
	t.Parallel()

	srv := server(t, []string{"us-east-1", "us-west-2"}, "eu-central-1-ath-1", "us-east-1-chi-1a")
	out := filepath.Join(t.TempDir(), "rates_generated.go")

	if err := run(fetcherFor(srv), out, false, timeout); err != nil {
		t.Fatalf("run: %v; a local zone in the index must not fail the whole run", err)
	}

	src, err := os.ReadFile(out) //nolint:gosec // a path this test built under t.TempDir()
	if err != nil {
		t.Fatalf("reading the generated file: %v", err)
	}

	for _, zone := range []string{"eu-central-1-ath-1", "us-east-1-chi-1a"} {
		if strings.Contains(string(src), `"`+zone+`"`) {
			t.Errorf("%s is in the generated table; it publishes no S3 rates, so every rate under "+
				"it would be zero — which reads as free storage", zone)
		}
	}

	for _, code := range []string{"us-east-1", "us-west-2"} {
		if !strings.Contains(string(src), `"`+code+`"`) {
			t.Errorf("%s is missing from the table; the skip rule caught a region that does publish "+
				"rates", code)
		}
	}
}

// TestRunRefusesToWriteAnEmptyTable is the case where failing is the whole value.
//
// If every region yields no rates — a total outage, a moved endpoint, an index shape change — the
// alternative to erroring is writing a table with no regions in it. That file compiles. Every lookup
// against it falls back to us-east-1, finds nothing there either, and reports zero, and $0/GB-month
// reads as free storage rather than as a broken generator. And it has already overwritten the good
// file by then.
func TestRunRefusesToWriteAnEmptyTable(t *testing.T) {
	t.Parallel()

	srv := server(t, nil, "eu-central-1-ath-1")
	out := filepath.Join(t.TempDir(), "rates_generated.go")

	err := run(fetcherFor(srv), out, false, timeout)
	if err == nil {
		t.Fatal("run succeeded with no region yielding rates; it wrote a table that compiles and " +
			"prices every tier at zero, over the top of the one that was correct")
	}

	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q does not say the table would have been empty", err)
	}

	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the output file exists after the refusal (stat err: %v); refusing to write and "+
			"then writing is worse than either", err)
	}
}

// TestRunFailsOnATransportErrorRatherThanSkipping is the distinction the skip rule turns on.
//
// A 404 on a real region's offer file is not ErrNoS3Rates, and treating it as one is how a region
// silently leaves the table. The fixture serves no eu-west-1 files while listing it in the index, which
// is exactly that shape.
func TestRunFailsOnATransportErrorRatherThanSkipping(t *testing.T) {
	t.Parallel()

	bodies := regionIndexJSON(t, "us-east-1", "eu-west-1")

	mixed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "region_index.json") {
			_, _ = w.Write(bodies)

			return
		}

		http.NotFound(w, r)
	}))
	t.Cleanup(mixed.Close)

	out := filepath.Join(t.TempDir(), "rates_generated.go")

	err := run(offerfile.NewFetcherAt(mixed.Client(), mixed.URL), out, false, timeout)
	if err == nil {
		t.Fatal("run succeeded with every offer file 404ing; a failed download must not look like a " +
			"local zone, or a region leaves the table with nothing reporting it")
	}

	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not carry the HTTP status, so the operator cannot tell a missing "+
			"region from a network problem", err)
	}
}

// TestDryRunWritesNothing pins the flag's contract.
//
// -dry-run exists so someone can see what a regeneration would produce before replacing the file, and a
// dry run that writes is worse than no flag at all.
func TestDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	srv := server(t, []string{"us-east-1", "us-west-2"})
	out := filepath.Join(t.TempDir(), "rates_generated.go")

	if err := run(fetcherFor(srv), out, true, timeout); err != nil {
		t.Fatalf("run -dry-run: %v", err)
	}

	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("-dry-run wrote %s (stat err: %v)", out, err)
	}
}

// TestOutputPathIsRequiredUnlessDryRun covers the argument check.
//
// Without it, `go run ./cmd/genrates` with a forgotten -o makes ~80 HTTP requests and discards the
// result, which looks like a successful regeneration in a terminal.
func TestOutputPathIsRequiredUnlessDryRun(t *testing.T) {
	t.Parallel()

	// No server: the check must happen before anything is fetched.
	err := run(offerfile.NewFetcherAt(http.DefaultClient, "http://127.0.0.1:1"), "", false, timeout)
	if err == nil {
		t.Fatal("run with no -o and no -dry-run succeeded")
	}

	if !strings.Contains(err.Error(), "-o") {
		t.Errorf("error %q does not name the missing flag", err)
	}
}
