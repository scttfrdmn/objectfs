package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/distributed"
)

// A secret long enough for LoadClusterSecret's minimum, which is what `openssl rand -hex 32` produces.
const testClusterSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestStartClusterIsANoOpUnlessEnabled is the whole safety argument for #139 in one assertion.
//
// Every path added for cluster coordination is guarded by a nil coordinator, and nil is what a
// single-node mount must get — not an empty one, not a disabled one. If this leaves a cluster manager
// behind, then every mount on the planet opens a UDP socket and runs a gossip ticker to talk to
// nobody, and does it without a cluster secret configured, which would fail the mount outright.
//
// Note what it asserts about the coordinator specifically. Dropping clusterCoordinator's nil guard
// makes this test panic on the spot, because GetCoordinator dereferences the manager — so the guard is
// load-bearing at every single-node start, not only in the subtler case. The subtler case is why the
// assertion is on the interface value rather than on a.clusterMgr: GetCoordinator returns a
// *coordinatorWrapper, which is a non-nil types.DistributedCoordinator whatever it wraps, so a
// nil-check moved one layer down would hand internal/fuse something every `if fs.coordinator != nil`
// treats as present.
func TestStartClusterIsANoOpUnlessEnabled(t *testing.T) {
	t.Parallel()

	cfg := config.NewDefault()
	if cfg.Cluster.Enabled {
		t.Fatal("cluster.enabled defaults to true, which would put gossip on every single-node mount")
	}

	a := &Adapter{config: cfg}

	if err := a.startCluster(t.Context()); err != nil {
		t.Fatalf("startCluster with clustering disabled: %v", err)
	}

	if a.clusterMgr != nil {
		t.Error("a cluster manager was constructed with cluster.enabled false")
	}

	if coord := a.clusterCoordinator(); coord != nil {
		t.Errorf("clusterCoordinator returned %T rather than nil, so the filesystem's nil guards would "+
			"all take the coordinated branch on a single-node mount", coord)
	}
}

// TestStartClusterStartsGossipAndReachesTheMount covers the other half: with clustering on, a manager
// exists, it is running, and the coordinator handed to the mount is the live one.
//
// Started on loopback with port 0 so it does not need the network or a fixed port. The gossip socket's
// own reported address is the evidence it bound — a cluster manager that failed to start is non-nil
// too, which is the same trap TestStartMetricsBindsTheEndpoint was written for.
func TestStartClusterStartsGossipAndReachesTheMount(t *testing.T) {
	t.Parallel()

	a := &Adapter{config: clusterEnabledConfig(t)}

	if err := a.startCluster(t.Context()); err != nil {
		t.Fatalf("startCluster: %v", err)
	}
	t.Cleanup(func() { _ = a.clusterMgr.Stop() })

	if a.clusterMgr == nil {
		t.Fatal("cluster.enabled is set and no cluster manager was constructed, which is #139 exactly")
	}

	if addr := a.clusterMgr.GossipAddr(); addr == "" {
		t.Error("the gossip socket reports no local address, so nothing bound and no peer could reach " +
			"this node — the manager is non-nil either way")
	}

	if coord := a.clusterCoordinator(); coord == nil {
		t.Error("clusterCoordinator returned nil with a running cluster, so the mount would coordinate " +
			"nothing")
	}
}

// TestStartClusterDoesNotAskForConsensus asserts the decision recorded on
// [distributed.ClusterConfig.EnableConsensus] survives the wiring: a mount asks for gossip and not for
// Raft. What can regress here is a future buildClusterConfig setting the field, and the consequence
// lands on a mount — an election on the path of a filesystem read, deciding nothing that path asks
// about, and a cluster below quorum degrading a mount that never needed one.
//
// It asserts on the request and not on the outcome, deliberately, and the reason is worth stating so
// nobody strengthens this into something that cannot fail. The behavior — no leader, no term, state
// stays follower — is asserted by TestClusterManager_Start_DoesNotStartConsensusUnlessAskedTo in
// internal/distributed, which can set a 20ms election timeout and watch ~25 of them pass. From here
// those same assertions are unmeasurable: ElectionTimeout is not an operator-facing setting, so this
// cluster runs on the 5-second default, and a freshly started node has not held an election yet
// whether the engine is running or not. Leadership checks added at this level would pass with the gate
// removed, which is exactly the shape of a test that measures nothing.
func TestStartClusterDoesNotAskForConsensus(t *testing.T) {
	t.Parallel()

	a := &Adapter{config: clusterEnabledConfig(t)}

	if a.buildClusterConfig().EnableConsensus {
		t.Error("buildClusterConfig sets EnableConsensus, so every clustered mount would run leader " +
			"elections to decide nothing a filesystem read asks about")
	}

	// And that a cluster actually starts under that config, so the assertion above is not passing
	// because the whole thing is inert.
	if err := a.startCluster(t.Context()); err != nil {
		t.Fatalf("startCluster: %v", err)
	}
	t.Cleanup(func() { _ = a.clusterMgr.Stop() })

	if a.clusterMgr.GossipAddr() == "" {
		t.Error("gossip did not bind, so consensus being off is not evidence of anything")
	}
}

// TestBuildClusterConfigCarriesTheOperatorSettings guards the conversion between the two disjoint
// ClusterConfig types, which is the only one in the repository.
//
// Every value here differs from both the config default and the distributed package's default, because
// a fixture equal to either passes whether the field was copied or not. That is how a mapping gets
// written, tested, and quietly stops carrying a field.
func TestBuildClusterConfigCarriesTheOperatorSettings(t *testing.T) {
	t.Parallel()

	cfg := config.NewDefault()
	cfg.Cluster.Enabled = true
	cfg.Cluster.NodeID = "node-from-config"
	cfg.Cluster.ListenAddr = "127.0.0.1:19001"
	cfg.Cluster.AdvertiseAddr = "10.1.2.3:19001"
	cfg.Cluster.SeedNodes = []string{"10.1.2.4:19001", "10.1.2.5:19001"}
	cfg.Cluster.SecretFile = "/etc/objectfs/from-config.secret"
	cfg.Cluster.ReplicationFactor = 5

	a := &Adapter{config: cfg}
	got := a.buildClusterConfig()

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"NodeID", got.NodeID, "node-from-config"},
		{"ListenAddr", got.ListenAddr, "127.0.0.1:19001"},
		{"AdvertiseAddr", got.AdvertiseAddr, "10.1.2.3:19001"},
		{"SecretFile", got.SecretFile, "/etc/objectfs/from-config.secret"},
		{"ReplicationFactor", got.ReplicationFactor, 5},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v: the operator's setting did not reach the cluster",
				c.field, c.got, c.want)
		}
	}

	if len(got.SeedNodes) != 2 || got.SeedNodes[0] != "10.1.2.4:19001" {
		t.Errorf("SeedNodes = %v, want the two configured seeds; without them a node cannot join "+
			"anything and forms a cluster of one", got.SeedNodes)
	}
}

// TestStartClusterFailsTheMountWithoutASecret asserts the degradation rule for a correctness
// capability: refuse, with a reason, rather than continue unclustered.
//
// A mount that silently fell back to single-node here would serve cached bytes a peer had already
// overwritten, with nothing in its logs to say why — and its configuration would still say it was
// clustered. The project thesis's rule is that a correctness capability fails closed, and coherence is
// one.
func TestStartClusterFailsTheMountWithoutASecret(t *testing.T) {
	// Not Parallel: it unsets a process-wide environment variable. t.Setenv would fail the test
	// outright in a parallel test, which is the runtime telling us the same thing.
	t.Setenv(distributed.ClusterSecretEnv, "")
	if err := os.Unsetenv(distributed.ClusterSecretEnv); err != nil {
		t.Fatalf("unsetting %s: %v", distributed.ClusterSecretEnv, err)
	}

	cfg := config.NewDefault()
	cfg.Cluster.Enabled = true
	cfg.Cluster.ListenAddr = "127.0.0.1:0"
	// No SecretFile and no environment variable, which is the state an operator who enabled clustering
	// and read no further is in.

	a := &Adapter{config: cfg}

	err := a.startCluster(context.Background())
	if err == nil {
		t.Fatal("clustering started with no secret configured; an unauthenticated gossip port lets any " +
			"host on the network announce ownership of cached objects")
	}
	if a.clusterMgr != nil {
		t.Error("a failed startCluster left a cluster manager on the adapter, which Stop would then " +
			"try to stop")
	}

	// The operator has to be able to act on this. "failed to start cluster manager" alone sends them to
	// the network; naming the environment variable names the missing step.
	if !strings.Contains(err.Error(), distributed.ClusterSecretEnv) {
		t.Errorf("the error does not name %s, so it does not say what to do: %v",
			distributed.ClusterSecretEnv, err)
	}
}

// clusterEnabledConfig returns a configuration that starts a real one-node cluster on loopback.
//
// The secret goes in a file rather than in OBJECTFS_CLUSTER_SECRET, for two reasons. t.Setenv forbids
// t.Parallel, so an environment-based helper would serialize every test that used it. And the file is
// the path an operator takes and the one `cluster.secret_file` was added for, so this exercises the new
// config field end to end instead of bypassing it.
//
// Port 0 rather than a preallocated one: a port read back from a closed listener is already stale, and
// this package's tests run in parallel with a miniredis that asks for ephemeral ports too.
func clusterEnabledConfig(t *testing.T) *config.Configuration {
	t.Helper()

	cfg := config.NewDefault()
	cfg.Cluster.Enabled = true
	cfg.Cluster.NodeID = "adapter-cluster-test"
	cfg.Cluster.ListenAddr = "127.0.0.1:0"
	cfg.Cluster.AdvertiseAddr = "127.0.0.1:0"
	cfg.Cluster.SecretFile = writeClusterSecret(t)

	return cfg
}

// writeClusterSecret writes a valid cluster secret to a temporary file and returns its path.
//
// Mode 0600, because LoadClusterSecret refuses anything group- or world-readable rather than chmod-ing
// a file the operator placed. A fixture written 0644 fails with a permissions error and looks like a
// wiring defect.
func writeClusterSecret(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "cluster.secret")
	if err := os.WriteFile(path, []byte(testClusterSecret), 0o600); err != nil {
		t.Fatalf("writing the cluster secret fixture: %v", err)
	}

	return path
}
