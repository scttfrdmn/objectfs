package s3

// The dangerous direction of the capability probe.
//
// An endpoint that *rejects* conditional headers is harmless: the request fails, loudly, and nobody
// builds a lease on it. The endpoint to worry about is the one that accepts an If-Match, ignores it,
// and answers 200 — because from every angle except the outcome of a race it is indistinguishable from
// one that honors preconditions. A configuration flag or an endpoint-URL heuristic would call it
// capable. `docs/design/conditional-writes-vs-raft.md` §4 names Ceph RGW as documenting conditional
// headers for GET/HEAD only, with Wasabi unverified and treated the same until probed.
//
// These are in-package tests because they need endpoints that misbehave in specific ways, which
// internal/testaws cannot provide — and testaws imports this package, so an external test could not
// reach the unexported probe anyway. The same reasoning as pool_health_test.go, which established this
// shape.

import (
	stderr "errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/circuit"
	"github.com/scttfrdmn/objectfs/internal/compression"
	"github.com/scttfrdmn/objectfs/pkg/health"
	"github.com/scttfrdmn/objectfs/pkg/types"
)

// backendAgainst builds a backend pointed at endpoint, with the machinery the probe needs and nothing
// else. NewBackend is avoided deliberately: it validates far more than a probe test cares about, and
// several of those checks would fail against a hand-rolled endpoint for reasons unrelated to the
// capability under test.
func backendAgainst(t *testing.T, endpoint string) *Backend {
	t.Helper()

	cfg := NewDefaultConfig()
	cfg.Endpoint = endpoint
	cfg.ForcePathStyle = true
	cfg.Region = "us-east-1"
	cfg.AccessKeyID = "AKIATEST12345678901"
	cfg.SecretAccessKey = "secret"
	cfg.MaxRetries = 1

	logger := slog.New(slog.DiscardHandler)

	cm, err := NewClientManager(t.Context(), "probe-bucket", cfg, logger)
	if err != nil {
		t.Fatalf("build client manager: %v", err)
	}
	t.Cleanup(func() { _ = cm.Close() })

	return &Backend{
		bucket:        "probe-bucket",
		clientManager: cm,
		config:        cfg,
		logger:        logger,
	}
}

// TestProbeDetectsAnEndpointThatIgnoresPreconditions is the assertion that makes the probe worth having
// rather than being an extra round trip at startup.
//
// The endpoint answers 200 to everything, which is exactly what a store that accepts conditional
// headers and drops them does. A probe reading that as "capable" would hand a coordination feature a
// mutual exclusion primitive that excludes nobody — turning "exactly one node performs this transition"
// into "every node does", silently.
func TestProbeDetectsAnEndpointThatIgnoresPreconditions(t *testing.T) {
	t.Parallel()

	// atomic, not plain bools: the handler runs on the server's goroutines and these are read from the
	// test's. The final read happens after the probe returns, so in practice the write is long done —
	// but "in practice" is not a happens-before edge, and this file is one of the places -race would
	// not reliably catch it.
	var sawIfMatch, sawDelete atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") != "" {
			sawIfMatch.Store(true)
		}
		if r.Method == http.MethodDelete {
			sawDelete.Store(true)
		}
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	caps := backendAgainst(t, srv.URL).probeConditionalWrite(t.Context())

	if caps.ConditionalWrite {
		t.Fatal("the probe reported conditional writes supported against an endpoint that ignored the " +
			"If-Match header and performed the write anyway")
	}
	if caps.ConditionalWriteDetail == "" {
		t.Error("ConditionalWriteDetail is empty; an operator has nothing to act on")
	}

	// The header was actually sent. Without this the test would also pass against a probe that
	// established nothing because it forgot to set the precondition — the same false negative for the
	// opposite reason.
	if !sawIfMatch.Load() {
		t.Error("the probe sent no If-Match header, so it established nothing")
	}

	// The probe wrote an object it did not intend to, because the endpoint ignored the assertion. It has
	// to clean up: this key would otherwise appear in the user's bucket on every mount against such an
	// endpoint, and the probe would be creating exactly the object whose absence it was asserting.
	if !sawDelete.Load() {
		t.Error("the probe left behind the object an ignoring endpoint created")
	}
}

// TestPutObjectIfRefusesWhenTheEndpointIgnoresPreconditions is what the probe is *for*.
//
// Detecting an incapable endpoint accomplishes nothing on its own; the value is in PutObjectIf refusing
// rather than writing. Falling back to an unconditional PUT here is the failure this whole mechanism
// exists to prevent, and it would be silent: every node would report having acquired the lease.
//
// The endpoint answers 200 to everything, so a fallback would *succeed* — which is why this asserts an
// error rather than checking a stored object. The count is the other half: a refusal has to happen
// before the write, not after.
func TestPutObjectIfRefusesWhenTheEndpointIgnoresPreconditions(t *testing.T) {
	t.Parallel()

	var writes atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writes.Add(1)
		}
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	backend := backendAgainst(t, srv.URL)

	// The pieces PutObjectIf needs beyond the probe. backendAgainst deliberately builds a minimal
	// backend, so anything the write path touches has to be supplied here rather than by NewBackend.
	backend.metricsCollector = NewMetricsCollector()
	backend.circuitManager = circuit.NewManager(circuit.Config{})
	backend.healthTracker = health.NewTracker(health.DefaultConfig())
	backend.healthTracker.RegisterComponent("s3-writes")
	backend.tierValidator = NewTierValidator("us-east-1", TierStandard, TierConstraints{}, backend.logger)
	backend.currentTier = TierStandard

	compressor, err := compression.NewCompressor(compression.Settings{})
	if err != nil {
		t.Fatalf("build compressor: %v", err)
	}
	backend.compressor = compressor

	// The probe's own PUT is expected and is not the write under test.
	if caps := backend.Capabilities(); caps.ConditionalWrite {
		t.Fatalf("precondition for this test failed: the probe called an ignoring endpoint capable")
	}
	afterProbe := writes.Load()

	etag, err := backend.PutObjectIf(t.Context(), "lease", []byte("node-a"), nil,
		types.Precondition{Absent: true})
	if err == nil {
		t.Fatal("PutObjectIf succeeded against an endpoint that does not evaluate preconditions; " +
			"every contender for this lease would believe it had won")
	}
	if !stderr.Is(err, types.ErrNotSupported) {
		t.Errorf("err = %v, want ErrNotSupported so a caller can tell an incapable endpoint from a lost "+
			"race", err)
	}
	if etag != "" {
		t.Errorf("refused write reported ETag %q, want empty", etag)
	}

	if got := writes.Load() - afterProbe; got != 0 {
		t.Errorf("a refused conditional write issued %d PUT requests, want 0; the refusal has to come "+
			"before the write, not after", got)
	}
}

// TestProbeFailsClosedOnAnUnexpectedAnswer covers everything that is neither the expected 404 nor a
// successful write.
//
// A probe that could not establish the answer has not established it. Reporting the capability present
// on an error it did not recognize is the one failure mode with no recovery — a coordination feature
// would proceed unguarded — so the default is false whatever went wrong. The cases below are the
// plausible ones for a real deployment: a bucket-scoped policy that does not grant PutObject, an
// endpoint that is not S3 at all, and a store that rejects conditional headers outright.
func TestProbeFailsClosedOnAnUnexpectedAnswer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"policy does not grant PutObject", http.StatusForbidden, "AccessDenied"},
		{"bucket missing", http.StatusNotFound, "NoSuchBucket"},
		{"endpoint rejects conditional headers", http.StatusBadRequest, "InvalidRequest"},
		{"store is broken", http.StatusInternalServerError, "InternalError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><Error><Code>%s</Code><Message>%s</Message></Error>`,
					tc.code, tc.code)
			}))
			t.Cleanup(srv.Close)

			caps := backendAgainst(t, srv.URL).probeConditionalWrite(t.Context())

			if caps.ConditionalWrite {
				t.Fatalf("the probe reported conditional writes supported after a %d %s, which establishes "+
					"nothing about whether preconditions are evaluated", tc.status, tc.code)
			}
			if caps.ConditionalWriteDetail == "" {
				t.Error("ConditionalWriteDetail is empty; an operator has nothing to act on")
			}
		})
	}
}

// TestProbeAcceptsA404AsEvidenceThePreconditionWasEvaluated pins the answer the probe is looking for.
//
// A 404 for an If-Match against an absent key means the header was read and the assertion failed
// because there is no object — which is S3's actual behavior, verified rather than inferred. This test
// exists because that arm is what the probe *succeeds* on, and a mutation narrowing it (dropping the
// bare-API-error check in favour of the SDK's typed NoSuchKey, say) would make every real endpoint
// report the capability absent. That is the fail-closed direction, so nothing would break loudly —
// conditional writes would simply stop working everywhere.
func TestProbeAcceptsA404AsEvidenceThePreconditionWasEvaluated(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"NoSuchKey", "NotFound"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><Error><Code>%s</Code><Message>no such key</Message></Error>`,
					code)
			}))
			t.Cleanup(srv.Close)

			caps := backendAgainst(t, srv.URL).probeConditionalWrite(t.Context())

			if !caps.ConditionalWrite {
				t.Fatalf("the probe rejected a 404 %s, which is S3's own answer to an If-Match against an "+
					"absent key: %s", code, caps.ConditionalWriteDetail)
			}
			if caps.ConditionalWriteDetail != "" {
				t.Errorf("ConditionalWriteDetail = %q, want empty when the capability is present",
					caps.ConditionalWriteDetail)
			}
		})
	}
}
