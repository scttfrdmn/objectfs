package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Gates on the cluster block's validation, which #152 added.
//
// Nothing validated this block at all before, and the reason it needed validating is not the reason
// #152 gave. Its proposed table had three rows: two written against `cluster.persistent_log` and
// `cluster.data_dir`, which do not exist anywhere in the tree, and one — `enabled && advertise_addr
// == ""` — that can never fire, because NewDefault sets 127.0.0.1:8080 and applyConfigDefaults sets
// it again. Verified by calling Validate on that exact configuration, not by reading the file.
//
// What probing found instead is worse than what was specified, and TestValidateRejectsAWildcardAdvertise
// is the test for it. The rules here are the two that are unreachable-by-any-other-means wrong plus the
// address shapes; everything a reasonable operator might legitimately want — loopback, no seeds — is a
// warning on --dry-run rather than a refusal, because refusing those would make it impossible to bring
// up the first node of a cluster or to run the shipped compose file.

// TestClusterBlockIsOnlyCheckedWhenEnabled is the compatibility floor, and it comes first because every
// other test here is about a refusal.
//
// Almost every ObjectFS mount has cluster.enabled false and a cluster block full of defaults it has
// never looked at. If validation applied to a disabled block, this change would refuse mounts over
// settings that start nothing — the same reasoning that keeps validateListenAddr scoped to an enabled
// monitoring listener, and the same reasoning that makes the default advertise address (loopback) a
// warning rather than an error.
func TestClusterBlockIsOnlyCheckedWhenEnabled(t *testing.T) {
	t.Parallel()

	cfg := NewDefault()
	if cfg.Cluster.Enabled {
		t.Fatal("NewDefault() enables clustering, which would make every mount start a gossip listener; " +
			"the rest of this test assumes the default is off")
	}

	// Every value below is one the enabled path refuses. With clustering off they must all pass, or a
	// single-node user is failed over a block that binds nothing.
	cfg.Cluster.AdvertiseAddr = "0.0.0.0:8080"
	cfg.Cluster.ListenAddr = ""
	cfg.Cluster.ReplicationFactor = -1

	if err := validateClusterConfig(cfg.Cluster); err != nil {
		t.Errorf("a disabled cluster block was refused: %v\n\nEvery value here is one the enabled path "+
			"rejects, and that is the point: with cluster.enabled false none of them starts anything, "+
			"so refusing them fails a mount over a setting with no effect", err)
	}
}

// TestValidateRejectsAWildcardAdvertise is the defect this validation exists for.
//
// It is not the rule #152 asked for and it is the one that matters. `cluster.listen_addr` defaults to
// 0.0.0.0:8080, so copying that value into `advertise_addr` is the obvious thing for an operator to do
// — and advertise_addr is what peers are told to *dial*. Measured with net.DialUDP: dialing
// "0.0.0.0:8080" yields a connection whose remote is 127.0.0.1:8080, the dialer's own loopback; ":8080"
// behaves identically and "[::]:8080" reaches [::1]:8080.
//
// So every peer that learns this address sends this node's traffic to itself. Gossip is one-way UDP, so
// no send fails and no receive is expected: the advertising node stays alive in its own memberlist, each
// peer silently substitutes itself, and a three-node cluster becomes three clusters of one that each
// report healthy. A node in that state can serve an object another node has already overwritten, which
// is why it is refused rather than warned about.
func TestValidateRejectsAWildcardAdvertise(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"0.0.0.0:8080", ":8080", "[::]:8080"} {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()

			// The premise, asserted rather than assumed. If a future Go or platform made dialing a
			// wildcard fail loudly instead of silently reaching loopback, the refusal below would be
			// over-strict and this is where that shows up.
			ua, err := net.ResolveUDPAddr("udp", addr)
			if err != nil {
				t.Fatalf("resolving %q, which is what a peer does with an advertised address: %v", addr, err)
			}

			conn, err := net.DialUDP("udp", nil, ua)
			if err != nil {
				t.Fatalf("dialing the advertised %q failed outright: %v\n\nThe refusal under test is "+
					"justified by this dial *succeeding* against the dialer's own loopback. If it now "+
					"fails, the failure is no longer silent and this rule should be reconsidered",
					addr, err)
			}

			remote := conn.RemoteAddr().String()
			_ = conn.Close()

			host, _, splitErr := net.SplitHostPort(remote)
			if splitErr != nil {
				t.Fatalf("parsing the dialed remote %q: %v", remote, splitErr)
			}

			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				t.Fatalf("dialing the advertised %q reached %s, which is not loopback. This test's whole "+
					"premise is that a peer dialing a wildcard talks to itself", addr, remote)
			}

			cfg := clusterEnabled(func(c *ClusterConfig) { c.AdvertiseAddr = addr })

			err = validateClusterConfig(cfg)
			if err == nil {
				t.Fatalf("cluster.advertise_addr = %q was accepted. A peer dialing it reaches %s — its "+
					"own loopback — so the cluster partitions into single nodes with no error at either "+
					"end", addr, remote)
			}

			// The message has to name the key and the value, because the symptom an operator sees is a
			// cluster of one with nothing in any log.
			for _, want := range []string{"cluster.advertise_addr", addr, "cluster.listen_addr"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v\n\nIt must name the key, the value, and "+
						"where 0.0.0.0 does belong — an operator who set this copied it from listen_addr",
						want, err)
				}
			}
		})
	}
}

// TestValidateRejectsANegativeReplicationFactor pins the second value nothing downstream clamps.
//
// applyConfigDefaults replaces a replication factor of 0 and only 0, so -1 reaches
// Coordinator.selectTargetNodes, where min(replicationFactor, len(aliveNodes)) stays negative.
// LoadBalancer.SelectNodes returns an empty slice for any count <= 0 — verified by calling it with -1,
// 0, 1, 3 and 99 — and executeOnce turns an empty target list into "no target nodes selected". Every
// write in the cluster fails with a message that names neither this key nor its value.
func TestValidateRejectsANegativeReplicationFactor(t *testing.T) {
	t.Parallel()

	cfg := clusterEnabled(func(c *ClusterConfig) { c.ReplicationFactor = -1 })

	err := validateClusterConfig(cfg)
	if err == nil {
		t.Fatal("cluster.replication_factor = -1 was accepted. It is not clamped anywhere downstream: " +
			"target selection returns an empty node list and every write fails with \"no target nodes " +
			"selected\", naming neither the key nor the value")
	}

	if !strings.Contains(err.Error(), "cluster.replication_factor") {
		t.Errorf("the refusal does not name the key: %v", err)
	}

	// Zero is not an error — applyConfigDefaults turns it into 3 — and refusing it would break every
	// config file that omits the key, which is all of them.
	if err := validateClusterConfig(clusterEnabled(func(c *ClusterConfig) { c.ReplicationFactor = 0 })); err != nil {
		t.Errorf("cluster.replication_factor = 0 was refused: %v\n\nZero means \"unset\" and "+
			"applyConfigDefaults replaces it with 3, so every file that omits the key would fail", err)
	}
}

// TestClusterAddrsAllowPortZeroOnlyWhereItBinds is the asymmetry, and it is measured.
//
// A single rule for both addresses is wrong in one direction or the other, which is why this does not
// reuse validateListenAddr. net.ListenUDP on 127.0.0.1:0 binds and the kernel assigns a port — this
// repository's own distributed tests configure exactly that, and ClusterManager.GossipAddr exists to
// report the assigned port back. net.DialUDP to a port 0 fails with "can't assign requested address",
// so an advertised port 0 is an address no peer can reach.
//
// validateListenAddr's port-0 message is also wrong here on its own terms: it tells the operator to use
// the `enabled` flag beside the field, which the cluster block does not have per address.
func TestClusterAddrsAllowPortZeroOnlyWhereItBinds(t *testing.T) {
	t.Parallel()

	// The listening side, asserted against the kernel rather than assumed.
	ua, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolving 127.0.0.1:0: %v", err)
	}

	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		t.Fatalf("binding 127.0.0.1:0, which is what internal/distributed's tests configure: %v", err)
	}

	bound := conn.LocalAddr().String()
	_ = conn.Close()

	if _, port, _ := net.SplitHostPort(bound); port == "0" {
		t.Fatalf("binding 127.0.0.1:0 reported back port 0 (%s); the premise here is that the kernel "+
			"assigns a real one", bound)
	}

	if err := validateClusterConfig(clusterEnabled(func(c *ClusterConfig) {
		c.ListenAddr = "127.0.0.1:0"
	})); err != nil {
		t.Errorf("cluster.listen_addr = 127.0.0.1:0 was refused: %v\n\nThe kernel bound it to %s. This "+
			"is what internal/distributed's testConfig sets, so refusing it would fail that suite and "+
			"every single-host test that wants a free port", err, bound)
	}

	// The dialing side. Refused, because the dial itself fails.
	dialErr := func() error {
		da, resolveErr := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		if resolveErr != nil {
			return resolveErr
		}

		c, connErr := net.DialUDP("udp", nil, da)
		if connErr != nil {
			return connErr
		}

		_ = c.Close()

		return nil
	}()

	if dialErr == nil {
		t.Error("dialing 127.0.0.1:0 succeeded. The refusal of an advertised port 0 rests on this dial " +
			"failing; if it now works, that rule should be reconsidered rather than kept on faith")
	}

	err = validateClusterConfig(clusterEnabled(func(c *ClusterConfig) {
		c.AdvertiseAddr = "127.0.0.1:0"
	}))
	if err == nil {
		t.Fatalf("cluster.advertise_addr = 127.0.0.1:0 was accepted, but dialing it fails: %v. A node "+
			"advertising it is unreachable while appearing configured", dialErr)
	}

	if !strings.Contains(err.Error(), "cluster.listen_addr") {
		t.Errorf("the port-0 refusal does not mention cluster.listen_addr: %v\n\nAn operator who set "+
			"port 0 on the advertise address probably wanted it on the listen address, and the message "+
			"is the only place that can be said", err)
	}
}

// TestValidateRejectsAMalformedClusterAddr covers the shapes, and the point is *where* they are caught.
//
// An unparseable advertise address is refused nowhere downstream on the node that configured it.
// net.ResolveUDPAddr runs on the *receiving* peer, when it dials back, so the error surfaces on another
// host as "failed to resolve address" against a string that host never set — in a Warn-level log line,
// which most deployments do not collect.
func TestValidateRejectsAMalformedClusterAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*ClusterConfig)
		mustName string
	}{
		{
			name:     "advertise_addr with no port",
			mutate:   func(c *ClusterConfig) { c.AdvertiseAddr = "10.0.1.1" },
			mustName: "cluster.advertise_addr",
		},
		{
			name:     "advertise_addr with a service name",
			mutate:   func(c *ClusterConfig) { c.AdvertiseAddr = "10.0.1.1:gossip" },
			mustName: "cluster.advertise_addr",
		},
		{
			name:     "advertise_addr port out of range",
			mutate:   func(c *ClusterConfig) { c.AdvertiseAddr = "10.0.1.1:99999" },
			mustName: "cluster.advertise_addr",
		},
		{
			name:     "listen_addr empty",
			mutate:   func(c *ClusterConfig) { c.ListenAddr = "" },
			mustName: "cluster.listen_addr",
		},
		{
			name:     "advertise_addr empty",
			mutate:   func(c *ClusterConfig) { c.AdvertiseAddr = "" },
			mustName: "cluster.advertise_addr",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateClusterConfig(clusterEnabled(tc.mutate))
			if err == nil {
				t.Fatal("accepted; a peer discovers this as a resolve failure against a string it never " +
					"configured, logged at Warn on a different host")
			}

			if !strings.Contains(err.Error(), tc.mustName) {
				t.Errorf("the refusal does not name %s: %v", tc.mustName, err)
			}
		})
	}
}

// TestClusterValidationIsReachedThroughValidate is the wiring check.
//
// validateClusterConfig can be correct and unreachable — that is the shape of defect this repository
// keeps finding, most recently four environment variables that nothing read. Every other test here
// calls the function directly, so this is the only one that would fail if the call in Validate were
// deleted. It goes through LoadFromFile as well, since that is how an operator's configuration arrives.
func TestClusterValidationIsReachedThroughValidate(t *testing.T) {
	t.Parallel()

	const doc = `storage:
  s3:
    region: us-west-2
cluster:
  enabled: true
  node_id: node-1
  listen_addr: 0.0.0.0:8080
  advertise_addr: 0.0.0.0:8080
`

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := NewDefault()
	if err := cfg.LoadFromFile(path); err != nil {
		t.Fatalf("the document does not load, so this test is not exercising validation: %v", err)
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a config whose advertise_addr is a wildcard. validateClusterConfig " +
			"refuses it when called directly, so this is the call in Validate being absent — the " +
			"function would be correct and unreachable, which is the exact shape of #146's defect")
	}

	if !strings.Contains(err.Error(), "cluster.advertise_addr") {
		t.Errorf("Validate failed for some other reason: %v", err)
	}
}

// clusterEnabled builds a cluster block that is valid, then applies one mutation.
//
// Starting from valid and breaking one thing is what makes each test above about its own rule: a
// helper that started from the zero value would have every test failing on the first check reached,
// whichever that happened to be.
func clusterEnabled(mutate func(*ClusterConfig)) ClusterConfig {
	cfg := ClusterConfig{
		Enabled:           true,
		NodeID:            "node-1",
		ListenAddr:        "0.0.0.0:8080",
		AdvertiseAddr:     "10.0.1.1:8080",
		SeedNodes:         []string{"10.0.1.2:8080"},
		ReplicationFactor: 3,
	}

	if mutate != nil {
		mutate(&cfg)
	}

	return cfg
}

// TestTheHelperStartsFromSomethingValid keeps clusterEnabled honest.
//
// Every refusal test above asserts that one mutation is rejected, and each of those passes vacuously if
// the unmutated base is rejected too — for a reason that has nothing to do with the rule under test.
func TestTheHelperStartsFromSomethingValid(t *testing.T) {
	t.Parallel()

	if err := validateClusterConfig(clusterEnabled(nil)); err != nil {
		t.Fatalf("the base cluster block this file's tests mutate is itself invalid: %v\n\nEvery "+
			"refusal test would then pass for the wrong reason", err)
	}
}
