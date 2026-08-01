package testaws

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/scttfrdmn/substrate/emulator"

	"github.com/objectfs/objectfs/internal/storage/s3"
)

// Shared returns an S3 endpoint that outlives the calling test, starting it on first use.
//
// Use this from a fuzz target and [Start] from everything else. The difference is lifetime, and for a
// fuzz target the distinction is not a nicety:
//
// [emulator.StartTestServer] takes a *testing.T and releases the server through t.Cleanup, which is the
// right design for a test. Inside f.Fuzz the *testing.T is per-iteration, so a server started there is
// torn down and rebuilt every exec. Measured, with 16 workers: 49 executions in 24 seconds, then
// "bind: can't assign requested address" as the ephemeral port range filled with sockets in TIME_WAIT.
// A fuzzer at two executions per second explores nothing, and one that dies on port exhaustion reports
// a harness failure as a finding.
//
// This starts one emulator for the process and hands every caller the same endpoint. Isolation then comes
// from the bucket, not the server: [SharedBucket] gives each caller its own. That is the same boundary
// real S3 has, so nothing is being faked away — two tests sharing an endpoint and using separate buckets
// cannot observe each other any more than two AWS accounts in the same region can.
//
// It is deliberately not the default. A leaked server is invisible where a leaked t.Cleanup is not, and
// the ordinary case wants the emulator's state reset with the test that created it.
//
// It takes a [testing.TB] rather than a *testing.T because its primary caller is a fuzz target, which
// holds an *testing.F at the point where the endpoint has to be established — outside f.Fuzz, once, for
// the whole run.
func Shared(t testing.TB) *SharedServer {
	t.Helper()

	sharedOnce.Do(func() {
		shared, sharedErr = startShared()
	})

	if sharedErr != nil {
		t.Fatalf("testaws: start shared emulator: %v", sharedErr)
	}

	return shared
}

var (
	sharedOnce sync.Once
	shared     *SharedServer
	sharedErr  error
)

// SharedServer is a process-lifetime S3 endpoint.
//
// It intentionally offers less than [TestServer]: no request recorder, no capability probe, no time
// control. Those are per-test facilities and would be meaningless shared — a byte count aggregated
// across 16 concurrent fuzz workers answers no question anyone asked.
type SharedServer struct {
	// URL is the endpoint to configure clients with.
	URL string

	// Server is the underlying substrate server.
	Server *emulator.Server

	mu      sync.Mutex
	buckets int
}

// startShared brings up an emulator bound to a random port, with no test-scoped teardown.
//
// The server runs until the process exits. That is the point — see [Shared] — and it is why nothing here
// takes a *testing.T: accepting one would invite exactly the t.Cleanup this exists to avoid.
func startShared() (*SharedServer, error) {
	cfg := emulator.DefaultConfig()
	cfg.Server.Address = "127.0.0.1:0"
	cfg.Log.Level = "error"

	// The event store is off, unlike StartTestServer's. It retains an event per request with no bound
	// worth relying on, and a fuzz run issues millions — an in-memory store would grow until the
	// process was killed, and the fuzzer would be blamed for the memory.
	cfg.EventStore.Enabled = false

	state := emulator.NewMemoryStateManager()
	tc := emulator.NewTimeController(time.Now())
	registry := emulator.NewPluginRegistry()
	logger := emulator.NewDefaultLogger(slog.LevelError, false)
	store := emulator.NewEventStore(cfg.EventStore.ToEventStoreConfig(), emulator.WithTimeController(tc))

	if err := emulator.RegisterDefaultPlugins(
		context.Background(), registry, state, tc, logger, store, nil,
	); err != nil {
		return nil, fmt.Errorf("register plugins: %w", err)
	}

	// Bind before serving and hand the listener over, so the port is known without a
	// reserve-then-bind window another process could win.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	srv := emulator.NewServer(*cfg, registry, store, state, tc, logger)

	go func() { _ = srv.Serve(context.Background(), ln) }()

	url := fmt.Sprintf("http://%s", ln.Addr().String())
	if err := waitForHealth(url); err != nil {
		return nil, err
	}

	return &SharedServer{URL: url, Server: srv}, nil
}

// waitForHealth blocks until the emulator answers, so the first caller cannot race the server's startup.
func waitForHealth(url string) error {
	const (
		timeout = 10 * time.Second
		poll    = 5 * time.Millisecond
	)

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url+"/health", nil)
		if err != nil {
			return fmt.Errorf("build health request: %w", err)
		}

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}

		time.Sleep(poll)
	}

	return fmt.Errorf("emulator at %s did not become healthy within %s", url, timeout)
}

// Config returns an S3 backend config pointed at this server.
//
// Unlike [TestServer.Config] it carries no bucket, because a shared server has no single one. The caller
// gets a bucket from [SharedServer.Bucket] and passes it to the backend constructor itself.
func (s *SharedServer) Config() *s3.Config {
	cfg := baseConfig()
	cfg.Endpoint = s.URL

	return cfg
}

// Backend constructs an ObjectFS S3 backend against this server, on a bucket of its own.
//
// The backend is not closed for the caller. There is no test scope to close it in — that is what
// "shared" means — and its resources are a connection pool to a local listener, which the process exit
// reclaims. A caller that wants it closed sooner may close the returned backend.
func (s *SharedServer) Backend(ctx context.Context, mutate ...func(*s3.Config)) (*s3.Backend, error) {
	bucket, err := s.Bucket(ctx)
	if err != nil {
		return nil, err
	}

	return s.BackendOn(ctx, bucket, mutate...)
}

// BackendOn constructs a backend on a bucket the caller already has.
//
// This is what [Backend] cannot express: two backends, configured differently, reading and writing the
// same objects. That is the shape of a reconfiguration — a user edits a config file and remounts, and
// the objects they wrote yesterday are still there — and it is the only way to test audit finding C2,
// where an object written under one codec was read back under another and the raw compressed frame was
// returned as file content with a successful exit status.
//
// A test that used two Backend calls would get two buckets and prove nothing, because each backend
// would only ever read what it wrote.
func (s *SharedServer) BackendOn(
	ctx context.Context, bucket string, mutate ...func(*s3.Config),
) (*s3.Backend, error) {
	cfg := s.Config()
	for _, m := range mutate {
		m(cfg)
	}

	backend, err := s3.NewBackend(ctx, bucket, cfg)
	if err != nil {
		return nil, fmt.Errorf("testaws: build backend on %q: %w", bucket, err)
	}

	return backend, nil
}

// Bucket creates a uniquely named bucket on the shared server and returns its name.
//
// Uniqueness comes from a counter rather than the test name, because fuzz iterations all share one test
// name: deriving the bucket from it would hand every iteration the same bucket and let one iteration's
// objects be visible to the next, which is the one thing a differential oracle must not allow.
func (s *SharedServer) Bucket(ctx context.Context) (string, error) {
	s.mu.Lock()
	s.buckets++
	n := s.buckets
	s.mu.Unlock()

	name := fmt.Sprintf("objectfs-shared-%d", n)

	client, err := newClient(ctx, s.URL)
	if err != nil {
		return "", err
	}

	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{
		Bucket: aws.String(name),
	}); err != nil {
		return "", fmt.Errorf("testaws: create bucket %q: %w", name, err)
	}

	return name, nil
}
