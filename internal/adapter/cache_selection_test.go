package adapter

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/scttfrdmn/objectfs/internal/cache"
	"github.com/scttfrdmn/objectfs/internal/cache/redis"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/testaws"
)

// TestStartReachesTheCacheTheConfigNames is the mount-path half of #178.
//
// internal/cache's own tests can show that NewFromConfig selects a Redis cache; only this one can show
// that a *mount* does. That distinction is the entire defect: NewFromConfig was correct and had no
// caller, so every assertion about it passed while `cluster.redis.enabled: true` on a real config
// produced a private in-process cache. A test that stops at the constructor cannot tell those apart.
//
// Start rather than a hand-built Adapter, and a substrate-backed S3 endpoint rather than a mock, for the
// same reason: the thing under test is the wiring between the config the operator wrote and the object
// the filesystem ends up holding, and every layer skipped is a layer where the value can be dropped.
//
// Start's last step is the mount itself, which most hosts running this suite cannot perform — see
// startAdapterWithCluster for how that is handled without weakening the assertion.
func TestStartReachesTheCacheTheConfigNames(t *testing.T) {
	t.Parallel()

	t.Run("redis when configured", func(t *testing.T) {
		t.Parallel()

		mr := miniredis.RunT(t)
		a := startAdapterWithCluster(t, func(cfg *config.Configuration) {
			cfg.Cluster.Enabled = true

			// A gossip secret and a loopback port, because as of #139 `cluster.enabled` starts the gossip
			// layer as well as selecting this cache — and a cluster that cannot start fails the mount, so
			// without these Start fails before reaching the assertion below. That coupling is deliberate:
			// a shared cache with no invalidation between its users is the incoherence the whole cluster
			// block exists to prevent.
			cfg.Cluster.ListenAddr = "127.0.0.1:0"
			cfg.Cluster.SecretFile = writeClusterSecret(t)

			cfg.Cluster.Redis = config.RedisConfig{
				Enabled:    true,
				Address:    mr.Addr(),
				KeyPrefix:  "mount",
				TTL:        time.Minute,
				MaxRetries: 1,
			}
		})

		if _, ok := a.cache.(*redis.Cache); !ok {
			t.Fatalf("a mount configured with cluster.redis.enabled holds a %T, so the whole cluster: "+
				"block is unreachable from Start and an operator's shared-cache setting does nothing", a.cache)
		}

		// And the mount's cache is really that Redis, not merely of that type: put through the adapter's
		// own reference and read the key out of the server.
		a.cache.Put("obj", 0, []byte("through-the-mount"))

		got, err := mr.Get("mount:obj")
		if err != nil {
			t.Fatalf("the key is absent from Redis after a Put through the mount's cache: %v", err)
		}

		if got != "through-the-mount" {
			t.Errorf("Redis holds %q, want %q", got, "through-the-mount")
		}
	})

	t.Run("the in-process cache by default", func(t *testing.T) {
		t.Parallel()

		a := startAdapterWithCluster(t, nil)

		if _, ok := a.cache.(*cache.MultiLevelCache); !ok {
			t.Fatalf("a default mount holds a %T, want the in-process MultiLevelCache", a.cache)
		}
	})
}

// TestStartRefusesAnUnreachableRedis asserts the mount does not come up with the wrong cache.
//
// Silently falling back is the failure this whole issue is about, one layer further out: two nodes both
// start, both believe they share a cache, and disagree about a file with nothing in either log to
// explain it. Refusing at Start costs a restart and says which setting to fix.
func TestStartRefusesAnUnreachableRedis(t *testing.T) {
	t.Parallel()

	cfg := configForSubstrate(t)
	cfg.Cluster.Enabled = true
	cfg.Cluster.Redis = config.RedisConfig{
		Enabled:    true,
		Address:    "127.0.0.1:1", // nothing listens here
		MaxRetries: 0,
	}

	a, err := New(context.Background(), "s3://"+substrateBucket(t), t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = a.Start(context.Background())
	if err == nil {
		_ = a.Stop(context.Background())
		t.Fatal("Start succeeded with an unreachable Redis, so the mount is serving from a private cache " +
			"while the configuration says the cache is shared")
	}

	// Which failure, not just that one occurred. Start's last step is the mount, which fails on a host
	// with no FUSE helper — so "Start returned an error" alone is satisfied on those hosts by a build
	// that ignored the Redis setting entirely, and this test would pass while asserting nothing.
	if !strings.Contains(err.Error(), "cache") {
		t.Fatalf("Start failed for some other reason than the cache, so nothing here shows the "+
			"unreachable Redis was refused: %v", err)
	}

	if a.cache != nil {
		t.Errorf("Start failed but left a %T on the adapter, so a later call would use a cache the mount "+
			"was refused permission to use", a.cache)
	}

	// The backend is built at step 2, before the cache at step 3, so it exists even though Start failed.
	if a.backend != nil {
		if err := a.backend.Close(); err != nil {
			t.Errorf("closing the backend: %v", err)
		}
	}
}

// startAdapterWithCluster runs Adapter.Start against the substrate endpoint, applying mutate to the
// config first, and returns the adapter however far Start got.
//
// Start's seven steps end with the mount, and the mount is the one step a test host may be unable to
// perform: it needs a FUSE mount helper — fusermount3 on Linux, macFUSE on darwin — plus permission to
// use it, and CI has neither. So a mount failure is tolerated and every earlier failure is fatal.
//
// That tolerance costs nothing here, because the cache is built at step 3 and the mount is step 7: by
// the time Mount is reached, a.cache holds whatever the configuration selected, which is the entire
// assertion. Skipping instead would be worse than tolerating — the wiring under test is testable on
// every host, and a skip would silently retire the test on the machines that run it most.
//
// Stop is only called when Start finished, since Stop refuses an adapter that never started. On the
// partial path the pieces Start did build are closed by hand, so a Redis client and an S3 connection
// pool are not left behind per subtest.
func startAdapterWithCluster(t *testing.T, mutate func(*config.Configuration)) *Adapter {
	t.Helper()

	cfg := configForSubstrate(t)
	if mutate != nil {
		mutate(cfg)
	}

	a, err := New(context.Background(), "s3://"+substrateBucket(t), t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	startErr := a.Start(context.Background())

	switch {
	case startErr == nil:
		t.Cleanup(func() {
			if err := a.Stop(context.Background()); err != nil {
				t.Errorf("Stop: %v", err)
			}
		})

	case strings.Contains(startErr.Error(), "failed to mount filesystem"):
		t.Cleanup(func() { closePartialStart(t, a) })

	default:
		t.Fatalf("Start failed before reaching the mount, so it failed at a step this test is "+
			"about: %v", startErr)
	}

	if a.cache == nil {
		t.Fatal("Start left no cache on the adapter at all, so step 3 did not run")
	}

	return a
}

// closePartialStart releases what Start built before the mount failed.
func closePartialStart(t *testing.T, a *Adapter) {
	t.Helper()

	if a.cancelMount != nil {
		a.cancelMount()
	}

	if closer, ok := a.cache.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			t.Errorf("closing the cache: %v", err)
		}
	}

	if a.writeBuffer != nil {
		if err := a.writeBuffer.Close(); err != nil {
			t.Errorf("closing the write path: %v", err)
		}
	}

	if a.backend != nil {
		if err := a.backend.Close(); err != nil {
			t.Errorf("closing the backend: %v", err)
		}
	}
}

// TestMain supplies credentials for the whole package before any test runs.
//
// It has to be here rather than in a helper, and the reason is the seam this file is about.
// buildS3Config deliberately maps no credential fields — a YAML key for a long-lived secret invites it
// into version control — so an adapter built from a Configuration gets its credentials from the AWS
// default chain, exactly as a real mount does. With nothing in the environment that chain reaches EC2
// IMDS and fails, which is what CI does: no credentials, no instance role, and
// `NewBackend`'s closing HealthCheck fails before the cache is ever built.
//
// A developer machine with an AWS profile hides this completely — these tests passed locally with no
// credentials of their own, because the chain found the ambient ones. So the values are set here, once,
// for the whole test binary. Not with t.Setenv, which cannot be used from a parallel test and mutates
// process-wide state that other parallel tests in this package read.
//
// The substrate emulator does not verify signatures, so any non-empty pair would do; using testaws's
// keeps one source for them.
func TestMain(m *testing.M) {
	// Errors are impossible for a well-formed name and there is no test scope to report one in.
	_ = os.Setenv("AWS_ACCESS_KEY_ID", testaws.AccessKeyID)
	_ = os.Setenv("AWS_SECRET_ACCESS_KEY", testaws.SecretAccessKey)

	os.Exit(m.Run())
}

// configForSubstrate returns a default config pointed at the in-process substrate endpoint.
//
// Defaults except for the endpoint and the two listeners: NewDefault is what a mount with no config
// file runs, so starting from it is what makes this a test of the shipped path. The listeners are off
// because both default to fixed ports, and two Starts on fixed ports cannot coexist under t.Parallel.
//
// Credentials come from the environment, set in TestMain — see there for why they cannot come from the
// Configuration.
func configForSubstrate(t *testing.T) *config.Configuration {
	t.Helper()

	base := testaws.Shared(t).Config()

	cfg := config.NewDefault()
	cfg.Storage.S3.Endpoint = base.Endpoint
	cfg.Storage.S3.ForcePathStyle = base.ForcePathStyle
	cfg.Storage.S3.Region = base.Region
	cfg.Monitoring.Metrics.Enabled = false
	cfg.Monitoring.HealthChecks.Enabled = false

	return cfg
}

// substrateBucket creates a bucket on the shared substrate endpoint.
func substrateBucket(t *testing.T) string {
	t.Helper()

	bucket, err := testaws.Shared(t).Bucket(context.Background())
	if err != nil {
		t.Fatalf("testaws: bucket: %v", err)
	}

	return bucket
}
