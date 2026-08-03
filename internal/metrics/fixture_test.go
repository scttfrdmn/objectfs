package metrics

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scttfrdmn/objectfs/internal/testhttp"
)

// fixturePath is the scrape both SDKs parse in their own test suites.
const fixturePath = "../../sdks/testdata/metrics-scrape.txt"

// updateFixture regenerates fixturePath from the live collector rather than comparing against it.
var updateFixture = flag.Bool("update-fixture", false,
	"rewrite sdks/testdata/metrics-scrape.txt from a live scrape")

// TestSDKFixtureMatchesTheLiveScrape keeps the SDK test fixture honest.
//
// sdks/testdata/metrics-scrape.txt is a real /metrics response, captured from this collector, and both
// the Python and TypeScript SDKs parse it in their own tests. That makes it the contract between the
// Go exporter and its two consumers — and a fixture is worth exactly as much as its resemblance to
// what the server sends, which is why this test regenerates the scrape and compares.
//
// The alternative was a fixture somebody wrote by hand, and that alternative is the reason this test
// exists at all. The SDKs' parsers were written against imagined names — cache_hits,
// objectfs_cache_hits_total, objectfs_io_read_operations_total, objectfs_network_requests_total — none
// of which any version of ObjectFS has ever exported. Nothing failed, because nothing compared what
// the SDK expected against what the server produced.
//
// Update with: go test ./internal/metrics/ -run TestSDKFixtureMatchesTheLiveScrape -update-fixture
// then run the SDK suites, which is the point: a rename here fails them, loudly, in the same commit.
//
// Deliberately not parallel: under -update-fixture it rewrites a file in the source tree, and a test
// that writes a shared path must not run concurrently with one that reads it.
//
//nolint:paralleltest // writes sdks/testdata/metrics-scrape.txt under -update-fixture
func TestSDKFixtureMatchesTheLiveScrape(t *testing.T) {
	got := liveScrape(t)

	if *updateFixture {
		if err := os.WriteFile(fixturePath, []byte(got), 0o600); err != nil {
			t.Fatalf("writing the fixture: %v", err)
		}
		t.Logf("wrote %s — now run the Python and TypeScript SDK suites against it", fixturePath)

		return
	}

	want, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("reading the fixture: %v — regenerate with -update-fixture", err)
	}

	if got != string(want) {
		t.Errorf("the live scrape no longer matches %s.\n\n"+
			"Both SDKs parse this file in their own tests, so whatever changed here changes what they "+
			"see. If the change is intended, regenerate with:\n\n"+
			"    go test ./internal/metrics/ -run %s -update-fixture\n\n"+
			"and then run the SDK suites — a renamed metric or a dropped label breaks their extractors, "+
			"which is exactly what this fixture exists to surface.\n\nlive scrape:\n%s",
			filepath.Base(fixturePath), t.Name(), got)
	}
}

// liveScrape starts a collector, records one observation in every family, and returns the body of a
// real HTTP scrape.
//
// The observations are fixed values, not arbitrary ones, so that the SDK suites can assert on
// specific numbers: three hits and one miss make a 0.75 hit rate, which is a value a broken parser
// cannot produce by accident the way 0 or 1 can.
func liveScrape(t *testing.T) string {
	t.Helper()

	c, err := NewCollector(exactAdapterConfig(anyLoopbackAddr))
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	c.RecordOperation("read", 12*time.Millisecond, 4096, true)
	c.RecordOperation("read", 9*time.Millisecond, 4096, false)
	c.RecordOperation("write", 30*time.Millisecond, 8192, true)
	c.RecordCacheHit("a", 4096)
	c.RecordCacheHit("b", 4096)
	c.RecordCacheHit("c", 4096)
	c.RecordCacheMiss("d", 4096)
	c.UpdateCacheSize("L1", 1<<20)
	c.UpdateCacheSize("L2", 5<<20)
	c.UpdateActiveConnections(4)
	c.RecordError("read", errors.New("timeout while reading"))

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	return testhttp.Get(t, c.Addr(), c.config.Path, "Start bound no listener")
}
