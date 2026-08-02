package testaws

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Request is one HTTP request the harness observed, reduced to the fields that matter for asserting
// transfer behavior.
type Request struct {
	// Method is the HTTP method, e.g. "GET".
	Method string

	// Path is the request path, which under path-style addressing is "/bucket/key".
	Path string

	// Query is the raw query string, which is where S3 puts the sub-resource that
	// distinguishes, say, a multipart upload from a plain PUT.
	Query string

	// Range is the Range header, empty for an unranged request. This is the field that makes
	// read amplification visible: an unranged GET of a 64 MiB object and a ranged GET of 4 KiB
	// of it return the same bytes to the caller and differ only here.
	Range string

	// Status is the response status. A served range answers 206.
	//
	// Zero means no response was sent at all, which happens when a [Fault] aborted the request.
	// Such a request is still recorded: a test asserting that a failed request was retried has to
	// be able to count the attempt that failed.
	Status int

	// ResponseBytes is the number of body bytes the server actually sent.
	ResponseBytes int64

	// RequestBytes is the number of body bytes the client sent.
	RequestBytes int64

	// Header is the request's headers as the emulator received them.
	//
	// It is here because for some properties the header *is* the behavior, with no observable
	// consequence anywhere else. Server-side encryption is the case that forced it: the emulator does
	// not model SSE at all, so an object written with x-amz-server-side-encryption reads back
	// byte-identical to one written without it. A test that asserted on the SDK input struct it had
	// just filled in would be checking its own arithmetic, and a test that asserted on the object's
	// bytes would pass with no header sent — which is precisely how audit finding P-7 survived three
	// releases with `at_rest: true` in the shipped defaults.
	//
	// Kept whole rather than as a few named fields, because the next header worth pinning is not
	// predictable and a struct field per header would mean editing the harness for each one.
	Header http.Header
}

// IsRanged reports whether the request carried a Range header.
func (r Request) IsRanged() bool { return r.Range != "" }

// recorder is a byte-counting reverse proxy in front of the emulator.
//
// It exists because read amplification is a *byte-count* property and neither the AWS SDK nor the
// emulator's event store exposes one: the store records service, operation, and duration, and
// captures bodies only when explicitly configured to. Asserting on latency instead would make the
// decisive read-path test a flaky proxy for the thing it means to measure — the v0.10.0 audit
// measured a 4 KiB read of a 256 MiB object taking 49 seconds, but the defect is that it transferred
// 256 MiB, and that is what a regression test has to pin.
type recorder struct {
	mu       sync.Mutex
	requests []Request
	faults   []*fault
}

// Fault makes matching requests fail before they reach the emulator, a bounded number of times.
//
// # Why this is here and not in substrate's fault controller
//
// substrate injects faults by probability (`FaultRule.Probability`), which is the right primitive
// for soak testing and the wrong one for a regression test: "fail chunk 3 once, then let it
// succeed" cannot be expressed as a probability, and a test that retries until a coin lands is a
// flake. The behaviors this harness has to pin are exactly of that shape — a transient failure on
// one chunk of a parallel read must be retried and the read must still return correct bytes — so
// the count has to be exact. A substrate issue tracks adding a max-fires bound upstream; until
// then, this interposes at the proxy the harness already owns.
//
// It sits in front of the emulator rather than inside it, which has a consequence worth knowing:
// the failed request never reaches S3, so it appears in [TestServer.Requests] but not in the
// emulator's event store, and [TestServer.Operations] will not count it.
type Fault struct {
	// Method is the HTTP method to match, e.g. "GET". Empty matches any method.
	Method string

	// KeySuffix matches against the request path, which under path-style addressing is
	// "/bucket/key". Empty matches any path.
	KeySuffix string

	// RangePrefix matches the start of the Range header, which is how a specific chunk of a
	// parallel read is singled out: "bytes=1048576-" picks the chunk starting at 1 MiB. Empty
	// matches any request, ranged or not.
	RangePrefix string

	// QueryKey matches when the request's query string carries this parameter, whatever its value.
	//
	// It is what distinguishes the sub-operations of a multipart upload, and without it they
	// cannot be told apart: CreateMultipartUpload and CompleteMultipartUpload are both a POST to
	// "/bucket/key", differing only in "?uploads" versus "?uploadId=...". A Fault aimed at
	// Complete by method and path alone fires on the create instead — the upload then never
	// exists, so a test asserting "no orphaned upload was left behind" passes because nothing was
	// ever started. That is not a hypothetical; it is how this field came to be added.
	//
	// The useful values: "uploads" for the create, "uploadId" for the complete (a POST) or the
	// abort (a DELETE), "partNumber" for an UploadPart.
	QueryKey string

	// Status is the HTTP status to answer with. Defaults to 500, which the AWS SDK treats as a
	// retryable server error.
	Status int

	// Code is the S3 error code in the XML body, e.g. "InternalError". Defaults to
	// "InternalError". This is what the SDK's retry classifier reads, so a code S3 does not
	// consider retryable produces a request that fails once and stays failed.
	Code string

	// Times is how many matching requests to fail. Zero means one — a Fault that fails nothing is
	// never what a caller meant, and reading `Times: 0` as "unlimited" would turn a typo into a
	// test that hangs on the retry budget.
	Times int

	// OnFire is called when the fault fires, before the error response is written.
	//
	// It exists to make a race deterministic. Some behaviors are only reachable when something
	// happens *between* two requests of one operation — an object replaced between the first chunk
	// of a parallel read and its retry, say — and a test that races a goroutine against the read to
	// arrange that is a flake. Firing a fault is a known point in the sequence, so a hook there
	// turns the interleaving into a fixture.
	//
	// It runs on the proxy's goroutine while the request that triggered it is held, so the caller
	// sees the effect on every subsequent request. It must not itself make a request that the same
	// fault would match — the fault's budget is already claimed by then, so it will not recurse,
	// but a fault armed for more fires would.
	OnFire func()
}

// fault is a Fault plus its remaining budget.
type fault struct {
	spec      Fault
	remaining int
	fired     int
}

func (f *fault) matches(r *http.Request) bool {
	if f.spec.Method != "" && r.Method != f.spec.Method {
		return false
	}

	if f.spec.KeySuffix != "" && !strings.HasSuffix(r.URL.Path, "/"+strings.TrimPrefix(f.spec.KeySuffix, "/")) {
		return false
	}

	if f.spec.RangePrefix != "" && !strings.HasPrefix(r.Header.Get("Range"), f.spec.RangePrefix) {
		return false
	}

	if f.spec.QueryKey != "" && !r.URL.Query().Has(f.spec.QueryKey) {
		return false
	}

	return true
}

// take claims one fire from a matching fault, returning the spec to serve and whether one was
// claimed. Matching and decrementing happen under the same lock, so N concurrent chunk requests
// against a fault with Times 1 produce exactly one failure — the property the whole mechanism
// exists for.
func (r *recorder) take(req *http.Request) (Fault, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, f := range r.faults {
		if f.remaining <= 0 || !f.matches(req) {
			continue
		}

		f.remaining--
		f.fired++

		return f.spec, true
	}

	return Fault{}, false
}

// InjectFault arms a fault. Faults are matched in the order they were added, and the first match
// with budget remaining fires.
func (ts *TestServer) InjectFault(f Fault) {
	if f.Times <= 0 {
		f.Times = 1
	}
	if f.Status == 0 {
		f.Status = http.StatusInternalServerError
	}
	if f.Code == "" {
		f.Code = "InternalError"
	}

	ts.rec.mu.Lock()
	defer ts.rec.mu.Unlock()

	ts.rec.faults = append(ts.rec.faults, &fault{spec: f, remaining: f.Times})
}

// FaultsFired returns how many injected faults have actually fired.
//
// A test that arms a fault and then asserts the operation succeeded has proven nothing unless the
// fault fired: a matcher that matches nothing produces exactly the same passing test as a working
// retry. This is what makes that difference visible.
func (ts *TestServer) FaultsFired() int {
	ts.rec.mu.Lock()
	defer ts.rec.mu.Unlock()

	var total int
	for _, f := range ts.rec.faults {
		total += f.fired
	}

	return total
}

// ClearFaults disarms every fault, including budget not yet spent.
func (ts *TestServer) ClearFaults() {
	ts.rec.mu.Lock()
	defer ts.rec.mu.Unlock()

	ts.rec.faults = nil
}

// serveFault writes an S3-shaped error response. The body matters: the AWS SDK parses the Code out
// of it to decide whether the error is retryable, and a bare status with an empty body classifies
// differently from the same status with an InternalError body.
func serveFault(w http.ResponseWriter, spec Fault) {
	body := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<Error><Code>` + spec.Code + `</Code>` +
		`<Message>injected by testaws</Message>` +
		`<RequestId>testaws-injected</RequestId></Error>`

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(spec.Status)
	_, _ = w.Write([]byte(body))
}

// countingBody wraps a response body to count the bytes that pass through it.
type countingBody struct {
	io.ReadCloser
	n *int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	*c.n += int64(n)

	return n, err
}

// startRecorder fronts target with a recording proxy and returns the proxy's URL. The proxy is shut
// down when the test ends.
func startRecorder(t *testing.T, target string) (string, *recorder) {
	t.Helper()

	upstream, err := url.Parse(target)
	if err != nil {
		t.Fatalf("testaws: parse emulator URL %q: %v", target, err)
	}

	rec := &recorder{}

	// Rewrite/SetURL rather than NewSingleHostReverseProxy: the AWS SDK signs the request including
	// the Host header, and NewSingleHostReverseProxy rewrites only the URL host, leaving Host as the
	// proxy's own address — so the signature would cover a host the emulator never sees. SetURL
	// rewrites Host to the target's, which is exactly what keeps the signature verifiable upstream.
	//
	// The proxy gets its own Transport rather than the nil that means http.DefaultTransport, and that
	// is a correctness requirement, not tidiness. httptest.Server.Close calls
	// http.DefaultTransport.CloseIdleConnections() unconditionally — the standard library says
	// outright it is doing this to "help out" users of the standard transport — so with a shared
	// transport, one fixture ending tore down connections belonging to every other fixture still
	// running. Since tests here are parallel and each holds its own endpoint, that surfaced as
	// "http: CloseIdleConnections called" against an unrelated test, roughly one run in six, blamed
	// on whichever test happened to have a request in flight. A per-proxy transport cannot be closed
	// by anyone else's teardown.
	upstreamTransport := &http.Transport{
		// The emulator is in-process over loopback, so there is nothing to gain from a
		// connection budget and something to lose: a cap below the harness's own concurrency
		// would serialize requests and make a byte-count assertion measure queuing.
		MaxIdleConnsPerHost: 100,
	}

	// Release those idle connections before the emulator's own shutdown waits on them.
	//
	// Cleanups run last-registered-first, and emulator.StartTestServer registers its Server.Stop before
	// this function is ever called — so this necessarily runs ahead of it, which is the ordering that
	// matters. Stop calls http.Server.Shutdown, which polls until every connection is idle *or closed*,
	// and net/http gives a connection in StateNew — dialed, no request sent — a hardcoded 5-second grace
	// before it stops waiting (go.dev/issue/22682). This transport parks exactly that: a connection
	// opened by a dial race whose winner answered the request first.
	//
	// Which turned a 7 ms test into a 5.4-second one, entirely in teardown, and only when requests were
	// concurrent enough to lose such a race — so it appeared and vanished with the pacing of the test
	// rather than with anything it asserted. Closing the client end here means Shutdown observes zero
	// connections on its first poll.
	t.Cleanup(upstreamTransport.CloseIdleConnections)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
		},
		Transport: upstreamTransport,
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// A client that went away is not a proxy failure. It is the expected outcome for any test
		// about cancellation — an abandoned parallel read, a FUSE interrupt, a context deadline —
		// and reporting it would make the mechanism under test indistinguishable from a broken
		// fixture. The cancellation is already visible where it matters: the request is recorded
		// with a zero status and a short byte count.
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			return
		}

		// Anything else must be loud: silently returning a 502 would look to the SDK like an S3
		// error and send a test down the wrong path entirely.
		t.Errorf("testaws: proxy to the emulator failed: %v", err)
		w.WriteHeader(http.StatusBadGateway)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := Request{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Range:  r.Header.Get("Range"),

			// Cloned because the proxy hands this same map to the transport, which mutates it — adding
			// hop-by-hop headers and stripping others. Recording the live map would leave the assertion
			// reading whatever the transport left behind rather than what the client sent.
			Header: r.Header.Clone(),
		}

		if r.Body != nil {
			r.Body = &countingBody{ReadCloser: r.Body, n: &observed.RequestBytes}
		}

		capture := &capturingWriter{ResponseWriter: w}

		// A fault is claimed before proxying, so the request never reaches the emulator and its
		// state is untouched — which is what lets a test arm a failure against an object and still
		// read the object's real bytes on the retry.
		if spec, faulted := rec.take(r); faulted {
			if spec.OnFire != nil {
				// Before the response, so a test that arms a side effect here can rely on it having
				// happened by the time the client observes the failure.
				spec.OnFire()
			}
			serveFault(capture, spec)
		} else {
			proxy.ServeHTTP(capture, r)
		}

		observed.Status = capture.status
		observed.ResponseBytes = capture.written

		rec.mu.Lock()
		rec.requests = append(rec.requests, observed)
		rec.mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	return srv.URL, rec
}

// capturingWriter records the status and the number of body bytes written.
type capturingWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (c *capturingWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *capturingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(p)
	c.written += int64(n)

	return n, err
}

// snapshot returns a copy of the requests observed so far.
func (r *recorder) snapshot() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Request, len(r.requests))
	copy(out, r.requests)

	return out
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests = nil
}

// Requests returns every HTTP request the harness has observed, in order.
func (ts *TestServer) Requests() []Request {
	return ts.rec.snapshot()
}

// ResetRequests clears the observed requests, so a test can assert about one phase of its own work
// without counting the setup it did first.
func (ts *TestServer) ResetRequests() {
	ts.rec.reset()
}

// RequestsFor returns the observed requests whose path targets a key. Under path-style addressing
// the path is "/bucket/key", so this filters on the suffix rather than requiring the caller to
// reconstruct it.
func (ts *TestServer) RequestsFor(key string) []Request {
	var out []Request

	suffix := "/" + strings.TrimPrefix(key, "/")
	for _, r := range ts.Requests() {
		if strings.HasSuffix(r.Path, suffix) {
			out = append(out, r)
		}
	}

	return out
}

// GETs returns the observed GET requests for a key, which is what read-path assertions are about.
// A HEAD is excluded: it transfers no body, so counting it as a read would obscure the byte
// accounting.
func (ts *TestServer) GETs(key string) []Request {
	var out []Request

	for _, r := range ts.RequestsFor(key) {
		if r.Method == http.MethodGet {
			out = append(out, r)
		}
	}

	return out
}

// Writes returns the observed requests that stored bytes or attributes for a key: PUT, POST, and the
// CopyObject that a metadata update or a tier transition performs.
//
// It exists so an assertion about what every write path sent does not have to enumerate the methods
// itself. That enumeration is where the assertion goes wrong: a test that checked only PUT would pass
// with a multipart create sending no encryption header, and multipart is the path every large object
// takes. A CopyObject is a PUT with x-amz-copy-source, so it is included here by method already.
//
// Faulted requests are included, because a write that was rejected still carried headers and a test
// about what was sent should not depend on whether it was accepted.
func (ts *TestServer) Writes(key string) []Request {
	var out []Request

	for _, r := range ts.RequestsFor(key) {
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			out = append(out, r)
		}
	}

	return out
}

// BytesRead sums the response body bytes served for a key across every GET. This is the number that
// makes read amplification a test rather than an anecdote: a 4 KiB read of a 64 MiB object must
// transfer roughly 4 KiB, and the v0.10.0 read path transferred all 64 MiB.
func (ts *TestServer) BytesRead(key string) int64 {
	var total int64
	for _, r := range ts.GETs(key) {
		total += r.ResponseBytes
	}

	return total
}
