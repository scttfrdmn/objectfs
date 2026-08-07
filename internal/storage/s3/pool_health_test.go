package s3

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// clientAgainst builds an SDK client pointed at endpoint, with retries off so a test asserting on one
// response is not waiting out a retry budget.
//
// A raw client and not testaws: testaws imports this package, so an internal test here cannot import
// it back. Nothing below needs an S3 that works — the point is what each *failure* means.
func clientAgainst(t *testing.T, endpoint string) *s3.Client {
	t.Helper()

	return s3.New(s3.Options{
		Region:           "us-east-1",
		Credentials:      credentials.NewStaticCredentialsProvider("AKIATEST12345678901", "secret", ""),
		BaseEndpoint:     aws.String(endpoint),
		UsePathStyle:     true,
		RetryMaxAttempts: 1,
	})
}

// errorEndpoint serves the given S3 error code and status to every request.
func errorEndpoint(t *testing.T, status int, code string) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

// TestTestConnection_AnAPIAnswerIsAHealthyConnection is the defect behind this file's contextcheck
// finding, which was not about the context.
//
// The probe was `ListBuckets`, and the verdict was `err == nil`. ListBuckets is an account-level call
// gated by s3:ListAllMyBuckets — a permission distinct from anything granted on the bucket the pool
// serves, and one a least-privilege institutional policy does not grant. Verified against an endpoint
// answering 403 AccessDenied: the old check returned false, so every probe declared its connection
// dead. checkHealth then destroyed up to three working clients each round, forever, and wrote "Found 3
// unhealthy connections" into the pool's LastError for an operator to chase.
//
// What this asks now is whether the client can still reach S3 at all. A 403 or a 404 traveled there and
// came back, so the connection is fine and the answer is about the bucket, not the socket.
func TestTestConnection_AnAPIAnswerIsAHealthyConnection(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		// The case that made this a live defect rather than a wasted request.
		{"access denied on a bucket-scoped policy", http.StatusForbidden, "AccessDenied"},
		{"bucket missing", http.StatusNotFound, "NoSuchBucket"},
		{"wrong region", http.StatusMovedPermanently, "PermanentRedirect"},
		// A 500 is S3 refusing this request, not the connection failing; the retry and circuit-breaker
		// layers in backend.go are what decide about those, and evicting the client hides it from them.
		{"server error", http.StatusInternalServerError, "InternalError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hc := &HealthChecker{bucket: "some-bucket", timeout: 5 * time.Second}

			if !hc.testConnection(t.Context(), clientAgainst(t, errorEndpoint(t, tc.status, tc.code))) {
				t.Errorf("testConnection reported unhealthy for %s (%d); the request reached S3 and "+
					"was answered, so the connection is reusable and destroying it fixes nothing",
					tc.code, tc.status)
			}
		})
	}
}

// TestTestConnection_TransportFailureIsUnhealthy is the other half: the check must still be capable of
// saying no, or "healthy" means nothing.
//
// Port 1 refuses the connection, which is the actual condition a pooled client cannot recover from.
func TestTestConnection_TransportFailureIsUnhealthy(t *testing.T) {
	t.Parallel()

	hc := &HealthChecker{bucket: "some-bucket", timeout: 5 * time.Second}

	if hc.testConnection(t.Context(), clientAgainst(t, "http://127.0.0.1:1")) {
		t.Error("testConnection reported healthy for a client whose endpoint refuses connections")
	}
}

// TestTestConnection_ProbesTheConfiguredBucket asserts the probe is a HeadBucket on the pool's own
// bucket, not a request about the account.
//
// Without this, replacing ListBuckets with HeadBucket and then passing the bucket through as "" — which
// every existing NewConnectionPool caller could have done, since the parameter was new — would leave a
// check that probes nothing and passes this file's other two tests by accident.
func TestTestConnection_ProbesTheConfiguredBucket(t *testing.T) {
	t.Parallel()

	seen := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	hc := &HealthChecker{bucket: "the-pools-bucket", timeout: 5 * time.Second}

	if !hc.testConnection(t.Context(), clientAgainst(t, srv.URL)) {
		t.Fatal("testConnection reported unhealthy against an endpoint answering 200")
	}

	select {
	case got := <-seen:
		if want := "HEAD /the-pools-bucket"; got != want {
			t.Errorf("probe request = %q, want %q: ListBuckets asks about the account, which is a "+
				"different permission and says nothing about the bucket in use", got, want)
		}
	default:
		t.Error("testConnection made no request at all")
	}
}

// TestTestConnection_NoBucketMakesNoRequest covers the pool built without a bucket. No caller in the
// tree does that, but the constructor is exported, and a HeadBucket with an empty name is not a request
// the SDK can even form — so there is nothing to learn from trying.
func TestTestConnection_NoBucketMakesNoRequest(t *testing.T) {
	t.Parallel()

	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	hc := &HealthChecker{timeout: 5 * time.Second}

	if !hc.testConnection(t.Context(), clientAgainst(t, srv.URL)) {
		t.Error("testConnection with no bucket configured reported unhealthy; there is nothing to probe")
	}
	if requests != 0 {
		t.Errorf("testConnection made %d requests with no bucket configured, want 0", requests)
	}
}

// TestCheckHealth_KeepsConnectionsAnApiAnswerCameBackFrom is the pool-level consequence, which is where
// this cost something: checkHealth does not put a connection it judged unhealthy back, and decrements
// currentSize instead. Under the old ListBuckets check on a bucket-scoped policy, that ran every 30
// seconds against three connections at a time.
func TestCheckHealth_KeepsConnectionsAnApiAnswerCameBackFrom(t *testing.T) {
	t.Parallel()

	endpoint := errorEndpoint(t, http.StatusForbidden, "AccessDenied")

	p, err := NewConnectionPool(t.Context(), 4, "the-pools-bucket", func() (*s3.Client, error) {
		return clientAgainst(t, endpoint), nil
	})
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// Fill the pool, then hand every connection back so they are idle and eligible for probing.
	conns := make([]*s3.Client, 0, 3)
	for i := range 3 {
		c, err := p.Get()
		if err != nil {
			t.Fatalf("draw %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		p.Put(c)
	}

	before := p.Stats()
	p.healthCheck.checkHealth(t.Context())
	after := p.Stats()

	if after.Total != before.Total {
		t.Errorf("pool total went %d → %d across one health round against an endpoint that answers "+
			"403 AccessDenied; a probe the bucket refused is not a broken connection",
			before.Total, after.Total)
	}
	if after.Destroyed != before.Destroyed {
		t.Errorf("Destroyed went %d → %d, so the round evicted connections that were still usable",
			before.Destroyed, after.Destroyed)
	}
	if after.LastError != before.LastError {
		t.Errorf("LastError became %q, which sends an operator looking for a connection problem "+
			"that is really a bucket permission", after.LastError)
	}
}
