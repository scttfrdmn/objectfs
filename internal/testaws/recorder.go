package testaws

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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
	Status int

	// ResponseBytes is the number of body bytes the server actually sent.
	ResponseBytes int64

	// RequestBytes is the number of body bytes the client sent.
	RequestBytes int64
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
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
		},
		Transport: &http.Transport{
			// The emulator is in-process over loopback, so there is nothing to gain from a
			// connection budget and something to lose: a cap below the harness's own concurrency
			// would serialize requests and make a byte-count assertion measure queuing.
			MaxIdleConnsPerHost: 100,
		},
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		// A proxy failure must be loud: silently returning a 502 would look to the SDK like an
		// S3 error and send a test down the wrong path entirely.
		t.Errorf("testaws: proxy to the emulator failed: %v", err)
		w.WriteHeader(http.StatusBadGateway)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := Request{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Range:  r.Header.Get("Range"),
		}

		if r.Body != nil {
			r.Body = &countingBody{ReadCloser: r.Body, n: &observed.RequestBytes}
		}

		capture := &capturingWriter{ResponseWriter: w}
		proxy.ServeHTTP(capture, r)

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
