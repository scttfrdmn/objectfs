package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/distributed"
)

// Gates on the cluster section of `objectfs mount --dry-run`, which #152 added.
//
// The section carries the two checks that could not go in Validate. A cluster with no seeds and a
// cluster advertising a loopback address are both *legal and often correct* — the first node of a new
// cluster has nothing to seed from, and the shipped compose file advertises loopback on purpose — so
// refusing either would break real deployments. But on any other node each one means the cluster
// silently never formed: the node mounts, serves reads, reports healthy, and receives no invalidation
// from any peer, so it can serve an object another node has already overwritten.
//
// A warning is the only honest answer to a value that is right for one deployment and wrong for
// another, and --dry-run is where an operator is already reading output. That makes these assertions
// load-bearing rather than cosmetic: the warning is the entire mechanism.

// dryRunConfig writes a config file with the given cluster block and returns its path.
func dryRunConfig(t *testing.T, clusterBlock string) string {
	t.Helper()

	doc := `storage:
  s3:
    region: us-west-2
mount:
  uri: s3://objectfs-example
  mount_point: /mnt/objectfs
` + clusterBlock

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	return path
}

// TestDryRunPrintsNothingAboutAClusterThatIsOff is the noise check, and it is why the section is
// conditional.
//
// cluster.enabled is false for almost every ObjectFS mount. A block of "enabled: false" followed by six
// empty values, printed by the command an operator runs to check whether their config file is good, is
// output that makes the answer harder to find.
func TestDryRunPrintsNothingAboutAClusterThatIsOff(t *testing.T) {
	t.Parallel()

	path := dryRunConfig(t, "")

	code, stdout, stderr := runArgs(t, "mount", "--config", path, "--dry-run")
	if code != exitOK {
		t.Fatalf("--dry-run exited %d, want 0. stdout: %q stderr: %q", code, stdout, stderr)
	}

	if strings.Contains(stdout, "Cluster config") {
		t.Errorf("--dry-run printed a cluster section for a mount with clustering off:\n%s", stdout)
	}

	if strings.Contains(stderr, "warning") {
		t.Errorf("--dry-run warned about a cluster that is not enabled: %q", stderr)
	}
}

// TestDryRunPrintsTheResolvedClusterBlock asserts the values, and that the secret is not one of them.
func TestDryRunPrintsTheResolvedClusterBlock(t *testing.T) {
	t.Parallel()

	secret := filepath.Join(t.TempDir(), "cluster.secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("a", 64)), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	path := dryRunConfig(t, `cluster:
  enabled: true
  node_id: node-1
  listen_addr: 0.0.0.0:8080
  advertise_addr: 10.0.1.1:8080
  seed_nodes:
    - 10.0.1.2:8080
    - 10.0.1.3:8080
  replication_factor: 3
  secret_file: `+secret+"\n")

	code, stdout, stderr := runArgs(t, "mount", "--config", path, "--dry-run")
	if code != exitOK {
		t.Fatalf("--dry-run exited %d, want 0. stdout: %q stderr: %q", code, stdout, stderr)
	}

	for _, want := range []string{
		"Cluster config",
		"node_id:",
		"node-1",
		"listen_addr:",
		"0.0.0.0:8080",
		"advertise_addr:",
		"10.0.1.1:8080",
		"seed_nodes:",
		"10.0.1.2:8080, 10.0.1.3:8080",
		"replication_factor: 3",
		secret,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--dry-run output does not contain %q:\n%s", want, stdout)
		}
	}

	// The secret's *path* is printed and its contents are not. A dry run is the kind of thing that gets
	// pasted into an issue.
	if strings.Contains(stdout, strings.Repeat("a", 64)) {
		t.Error("--dry-run printed the cluster secret itself. The path is what an operator needs; the " +
			"material is what ends up in a pasted terminal log")
	}

	// This configuration is correct, so it must produce no warnings at all — otherwise the warnings
	// below mean nothing, because every cluster would emit them.
	if strings.Contains(stderr, "warning") {
		t.Errorf("a well-formed cluster config produced warnings, which makes every warning noise: %q",
			stderr)
	}
}

// TestDryRunWarnsAboutAClusterOfOne covers the two legal-but-usually-wrong values.
//
// Both are warnings rather than errors and both exit 0, which is the part worth pinning: a
// config-management tool that treated either as a failure could not bring up the first node of any
// cluster, and one that never saw them would deploy a silent partition.
func TestDryRunWarnsAboutAClusterOfOne(t *testing.T) {
	t.Parallel()

	t.Run("no seed nodes", func(t *testing.T) {
		t.Parallel()

		path := dryRunConfig(t, `cluster:
  enabled: true
  node_id: node-1
  advertise_addr: 10.0.1.1:8080
`)

		code, stdout, stderr := runArgs(t, "mount", "--config", path, "--dry-run")
		if code != exitOK {
			t.Fatalf("--dry-run exited %d for a cluster with no seeds, want 0: an empty seed list is how "+
				"the first node of a new cluster is configured. stderr: %q", code, stderr)
		}

		if !strings.Contains(stdout, "seed_nodes:         (none)") {
			t.Errorf("the resolved output does not show that the seed list is empty:\n%s", stdout)
		}

		if !strings.Contains(stderr, "cluster.seed_nodes is empty") {
			t.Errorf("no warning about an empty seed list: %q\n\nWithout it the node forms a cluster of "+
				"one, mounts, reports healthy, and receives no peer invalidations — with nothing "+
				"anywhere saying so", stderr)
		}
	})

	t.Run("loopback advertise address", func(t *testing.T) {
		t.Parallel()

		path := dryRunConfig(t, `cluster:
  enabled: true
  node_id: node-1
  advertise_addr: 127.0.0.1:8080
  seed_nodes:
    - 10.0.1.2:8080
`)

		code, _, stderr := runArgs(t, "mount", "--config", path, "--dry-run")
		if code != exitOK {
			t.Fatalf("--dry-run exited %d for a loopback advertise address, want 0: it is what "+
				"deploy/docker/docker-compose.yaml uses and what NewDefault sets. stderr: %q", code, stderr)
		}

		if !strings.Contains(stderr, "cluster.advertise_addr is 127.0.0.1:8080") {
			t.Errorf("no warning about a loopback advertise address: %q\n\nThis is also the default "+
				"value, so a config file that enables clustering and sets nothing else lands here", stderr)
		}
	})
}

// TestDryRunSaysWhenNoClusterSecretIsConfigured covers the value whose absence stops the mount.
//
// A cluster refuses to start without a secret (#206), from OBJECTFS_CLUSTER_SECRET or from
// cluster.secret_file. Neither is visible in a config file when the environment is the source, which
// makes "which one is in use" the single hardest thing to establish from outside — and "not configured"
// the most useful thing --dry-run can say, because it is the difference between a mount that works and
// one that fails at startup.
func TestDryRunSaysWhenNoClusterSecretIsConfigured(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it, and this has to control the environment variable because a
	// developer with it exported would otherwise see the other branch.
	t.Setenv(distributed.ClusterSecretEnv, "")

	path := dryRunConfig(t, `cluster:
  enabled: true
  node_id: node-1
  advertise_addr: 10.0.1.1:8080
  seed_nodes:
    - 10.0.1.2:8080
`)

	code, stdout, _ := runArgs(t, "mount", "--config", path, "--dry-run")
	if code != exitOK {
		t.Fatalf("--dry-run exited %d, want 0", code)
	}

	if !strings.Contains(stdout, "not configured") {
		t.Errorf("--dry-run does not report a missing cluster secret:\n%s\n\nThe mount will refuse to "+
			"start, and this is the check that exists to say so beforehand", stdout)
	}

	t.Run("and names the environment when that is the source", func(t *testing.T) {
		t.Setenv(distributed.ClusterSecretEnv, strings.Repeat("b", 64))

		_, stdout, _ := runArgs(t, "mount", "--config", path, "--dry-run")

		if !strings.Contains(stdout, distributed.ClusterSecretEnv) {
			t.Errorf("--dry-run does not name the environment variable the secret came from:\n%s", stdout)
		}

		if strings.Contains(stdout, strings.Repeat("b", 64)) {
			t.Error("--dry-run printed the secret from the environment")
		}
	})
}

// TestClusterSecretEnvMatchesTheDistributedPackage pins the duplicated constant.
//
// cmd/objectfs declares its own clusterSecretEnv rather than importing distributed for one string. That
// is fine right up until one of the two changes, at which point --dry-run reports "not configured" for
// a secret that is configured — a warning about a problem that does not exist, which is worse than
// silence because it sends an operator to look at a correct setting.
func TestClusterSecretEnvMatchesTheDistributedPackage(t *testing.T) {
	t.Parallel()

	if clusterSecretEnv != distributed.ClusterSecretEnv {
		t.Errorf("cmd/objectfs's clusterSecretEnv is %q and distributed.ClusterSecretEnv is %q. The "+
			"duplicate exists to avoid an import for one string; this test is the thing that keeps it "+
			"honest", clusterSecretEnv, distributed.ClusterSecretEnv)
	}
}
