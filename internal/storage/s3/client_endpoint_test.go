package s3

import (
	"context"
	"log/slog"
	"testing"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// clientOpts is the subset of s3.Options these tests assert on.
type clientOpts struct {
	endpoint   string
	pathStyle  bool
	accelerate bool
}

func optsOf(c *awss3.Client) clientOpts {
	o := c.Options()
	got := clientOpts{pathStyle: o.UsePathStyle, accelerate: o.UseAccelerate}
	if o.BaseEndpoint != nil {
		got.endpoint = *o.BaseEndpoint
	}
	return got
}

// Every client the manager hands out must address the configured endpoint. The pool's factory once
// called s3.NewFromConfig with no options at all, so HeadObject, DeleteObject, ListObjects, and the
// health check went to real AWS S3 while PutObject and GetObject went to the configured endpoint —
// which made MinIO, Ceph, and emulator deployments fail in a way that reads as a credentials
// problem.
//
// This asserts the property rather than the mechanism: it enumerates the accessors, so a client
// added later without the options is caught here rather than in production.
func TestAllClientsHonourEndpointConfig(t *testing.T) {
	t.Parallel()

	const endpoint = "http://127.0.0.1:14566"

	cfg := NewDefaultConfig()
	cfg.Endpoint = endpoint
	cfg.ForcePathStyle = true
	cfg.Region = "us-west-2"
	cfg.PoolSize = 4
	cfg.UseAccelerate = false

	cm, err := NewClientManager(context.Background(), "bucket", cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewClientManager: %v", err)
	}
	t.Cleanup(func() { _ = cm.Close() })

	// The one that was broken. It backs HeadObject, DeleteObject, ListObjects, and HealthCheck.
	pooled, err := cm.GetPooledClient()
	if err != nil {
		t.Fatalf("GetPooledClient: %v", err)
	}
	// t.Cleanup, not defer: the subtests below are parallel, so this function returns while they are
	// still inspecting the client. Returning it to the pool at that point hands a client another
	// caller may draw to a test that is still asserting on it.
	t.Cleanup(func() { cm.ReturnPooledClient(pooled) })

	clients := []struct {
		name   string
		client *awss3.Client
	}{
		{name: "primary", client: cm.GetClient()},
		{name: "standard", client: cm.GetStandardClient()},
		{name: "pooled", client: pooled},
	}

	for _, tc := range clients {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.client == nil {
				t.Fatalf("%s client is nil", tc.name)
			}
			got := optsOf(tc.client)
			if got.endpoint != endpoint {
				t.Errorf("%s client BaseEndpoint = %q, want %q — this client addresses real AWS S3",
					tc.name, got.endpoint, endpoint)
			}
			if !got.pathStyle {
				t.Errorf("%s client UsePathStyle = false; path-style addressing is required for "+
					"MinIO, Ceph, and most emulators", tc.name)
			}
		})
	}
}

// Repeated pool draws must all be configured. A factory-built client and a pre-warmed one come from
// different code paths, and only one of them was ever wrong.
func TestPooledClientsAreConfiguredAcrossDraws(t *testing.T) {
	t.Parallel()

	const endpoint = "http://127.0.0.1:14568"

	cfg := NewDefaultConfig()
	cfg.Endpoint = endpoint
	cfg.ForcePathStyle = true
	cfg.Region = "us-west-2"
	cfg.PoolSize = 3

	cm, err := NewClientManager(context.Background(), "bucket", cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewClientManager: %v", err)
	}
	t.Cleanup(func() { _ = cm.Close() })

	check := func(label string, i int, c *awss3.Client) {
		if got := optsOf(c); got.endpoint != endpoint || !got.pathStyle {
			t.Fatalf("%s draw %d: BaseEndpoint=%q pathStyle=%v, want %q and true",
				label, i, got.endpoint, got.pathStyle, endpoint)
		}
	}

	// Hold the whole pool at once, so every one of these comes from the factory rather than
	// from the idle set.
	drawn := make([]*awss3.Client, 0, cfg.PoolSize)
	for i := range cfg.PoolSize {
		c, err := cm.GetPooledClient()
		if err != nil {
			t.Fatalf("factory draw %d: %v", i, err)
		}
		drawn = append(drawn, c)
		check("factory", i, c)
	}
	for _, c := range drawn {
		cm.ReturnPooledClient(c)
	}

	// Now every draw is served from the idle set — the other code path.
	for i := range cfg.PoolSize {
		c, err := cm.GetPooledClient()
		if err != nil {
			t.Fatalf("idle draw %d: %v", i, err)
		}
		check("idle", i, c)
		cm.ReturnPooledClient(c)
	}
}

// Transfer Acceleration and a custom endpoint are rarely combined, but the accelerated client must
// still carry the endpoint it was configured with rather than silently falling back to AWS.
func TestAcceleratedClientHonoursEndpointConfig(t *testing.T) {
	t.Parallel()

	const endpoint = "http://127.0.0.1:14567"

	cfg := NewDefaultConfig()
	cfg.Endpoint = endpoint
	cfg.ForcePathStyle = true
	cfg.Region = "us-west-2"
	cfg.PoolSize = 2
	cfg.UseAccelerate = true

	cm, err := NewClientManager(context.Background(), "bucket", cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewClientManager: %v", err)
	}
	t.Cleanup(func() { _ = cm.Close() })

	acc := cm.GetAcceleratedClient()
	if acc == nil {
		t.Fatal("acceleration is configured but GetAcceleratedClient returned nil")
	}
	if got := optsOf(acc); got.endpoint != endpoint {
		t.Errorf("accelerated client BaseEndpoint = %q, want %q", got.endpoint, endpoint)
	} else if !got.accelerate {
		t.Error("accelerated client does not have UseAccelerate set")
	}

	// The standard client is the acceleration fallback. If it lost the endpoint, one acceleration
	// error would silently redirect the whole backend to AWS.
	std := optsOf(cm.GetStandardClient())
	if std.endpoint != endpoint {
		t.Errorf("fallback client BaseEndpoint = %q, want %q", std.endpoint, endpoint)
	}
	if std.accelerate {
		t.Error("fallback client has UseAccelerate set; it is meant to be the non-accelerated path")
	}
}

// An empty endpoint must leave BaseEndpoint unset so the SDK resolves the real regional endpoint.
// Defaulting it to anything non-empty would break every real deployment.
func TestClientsLeaveEndpointUnsetByDefault(t *testing.T) {
	t.Parallel()

	cfg := NewDefaultConfig()
	cfg.Region = "us-west-2"
	cfg.PoolSize = 2
	cfg.Endpoint = ""
	cfg.ForcePathStyle = false

	cm, err := NewClientManager(context.Background(), "bucket", cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewClientManager: %v", err)
	}
	t.Cleanup(func() { _ = cm.Close() })

	pooled, err := cm.GetPooledClient()
	if err != nil {
		t.Fatalf("GetPooledClient: %v", err)
	}
	defer cm.ReturnPooledClient(pooled)

	for name, client := range map[string]*awss3.Client{
		"primary": cm.GetClient(),
		"pooled":  pooled,
	} {
		opts := optsOf(client)
		if opts.endpoint != "" {
			t.Errorf("%s client BaseEndpoint = %q with no endpoint configured; the SDK must resolve "+
				"the regional endpoint itself", name, opts.endpoint)
		}
		if opts.pathStyle {
			t.Errorf("%s client forces path-style addressing without being asked", name)
		}
	}
}
