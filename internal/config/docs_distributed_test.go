package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Gates on docs/admin-guide/distributed.md, the operator guide #152 asked for.
//
// The four existing documentation gates do not reach what this page gets wrong. docs_test.go parses its
// YAML blocks, docs_symbols_test.go checks the Go symbols and CLI invocations it names, docs_links_test.go
// resolves its links, and changelog_test.go checks the entry. None of them can see a *default value*
// written as prose in a table cell, and that is most of this page: a config reference is a list of
// claims about numbers, each of which stops being true the moment someone changes a constant.
//
// The page has a second problem the other gates cannot see, and it is the one that motivated #152's
// rewrite. Three of the six sections the issue specified describe mechanisms that do not exist —
// cluster.persistent_log, cluster.data_dir, peer_fetch_timeout — so the page says they are absent
// instead. A statement that a key does not exist is a claim about the tree, and it inverts the usual
// failure: it goes stale when someone does the work, at which point the page is telling an operator not
// to use a feature that shipped.

// docsDistributed reads the operator guide.
func docsDistributed(t *testing.T) string {
	t.Helper()

	path := filepath.Join(repoRoot(t), "docs", "admin-guide", "distributed.md")

	//nolint:gosec // a path built from the module root this test located itself
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(body)
}

// TestDistributedGuideDefaultsMatchNewDefault pins the config reference's Default column.
//
// Each of these is a value an operator will not set, having read here that they do not need to. A
// default that has moved on is worse than an undocumented one: it is a reason to leave a key out of a
// config file, and the value they get is not the value they read.
func TestDistributedGuideDefaultsMatchNewDefault(t *testing.T) {
	t.Parallel()

	doc := docsDistributed(t)
	def := NewDefault().Cluster

	// Written as `| ... | `value` | ...` in the table, so the backticked form is what must appear.
	for _, tc := range []struct {
		what string
		want string
	}{
		{"cluster.listen_addr", "`" + def.ListenAddr + "`"},
		{"cluster.advertise_addr", "`" + def.AdvertiseAddr + "`"},
		{"cluster.replication_factor", "`3`"},
	} {
		if !strings.Contains(doc, tc.want) {
			t.Errorf("the config reference does not give %s as %s. NewDefault is the authority; the "+
				"table is a transcription of it, and a stale default is a reason not to set a key",
				tc.what, tc.want)
		}
	}

	if def.ReplicationFactor != 3 {
		t.Errorf("NewDefault sets replication_factor to %d and the guide says 3 in two places — the "+
			"table and the sizing section, which argues about what the factor is not",
			def.ReplicationFactor)
	}

	if def.Enabled {
		t.Error("NewDefault enables clustering. The guide's premise is that almost no mount is " +
			"clustered, which is why --dry-run prints nothing about a cluster that is off")
	}

	// A default that must stay empty for a reason the page states: the generated ID is new on every
	// restart, which is the whole of that table cell.
	if def.NodeID != "" {
		t.Errorf("NewDefault sets node_id to %q. The guide says an empty value generates one at startup, "+
			"new on every restart, and lists that under limitations", def.NodeID)
	}
}

// TestDistributedGuideDocumentsEveryClusterKey is the completeness half.
//
// A config reference that omits a key is how an operator finds a setting by reading source, and the
// reference claims to be "every key under cluster:" — which is a checkable claim, since the YAML tags
// on ClusterConfig are the enumeration.
func TestDistributedGuideDocumentsEveryClusterKey(t *testing.T) {
	t.Parallel()

	doc := docsDistributed(t)

	// A table *row*, not a mention. Checking for a backticked name anywhere on the page does not work:
	// verified by deleting the secret_file row, which this test then passed, because the key is also
	// named in the prose of "The cluster secret" section three paragraphs later. Prose about a setting is
	// not a reference entry — it has no default and no type, which is what a reader came to the table for.
	//
	// The yaml tags of ClusterConfig, each of which is a scalar with its own row. Redis is the one nested
	// struct and gets a family row rather than one row per key: its settings are a cache backend that
	// happens to be gated on cluster.enabled, so enumerating them here would duplicate
	// examples/config.yaml and put the authority in two places.
	for _, key := range []string{
		"enabled", "node_id", "listen_addr", "advertise_addr", "seed_nodes", "secret_file",
		"replication_factor", "redis.*",
	} {
		if !strings.Contains(doc, "| `"+key+"` |") {
			t.Errorf("the config reference has no table row for `%s`. It says it lists every key under "+
				"cluster:, so an omission here is an operator reading internal/config to find a setting — "+
				"and a mention in the prose is not a row, because it carries neither the type nor the "+
				"default", key)
		}
	}
}

// TestDistributedGuideEnvVarsExist checks the Environment column against the override table.
//
// This column is not decoration: one ConfigMap is shared by every replica of a StatefulSet, so node_id
// and advertise_addr *have* to come from the environment, and a wrong variable name there is a pod that
// comes up with the ConfigMap's value — which is the same value on every replica, and therefore a
// wildcard or a loopback advertise address on all of them. The failure this page exists to prevent,
// reached by following the page.
func TestDistributedGuideEnvVarsExist(t *testing.T) {
	t.Parallel()

	doc := docsDistributed(t)

	// getEnvMappings rather than grep, for #146's reason: the four variables that were documented and
	// unread were all findable by grep, in the documentation that named them.
	known := make(map[string]bool)
	for _, m := range getEnvMappings() {
		known[m.envVar] = true
	}

	for _, name := range []string{
		"OBJECTFS_CLUSTER_ENABLED",
		"OBJECTFS_CLUSTER_NODE_ID",
		"OBJECTFS_CLUSTER_LISTEN_ADDR",
		"OBJECTFS_CLUSTER_ADVERTISE_ADDR",
		"OBJECTFS_CLUSTER_SEEDS",
	} {
		if !known[name] {
			t.Errorf("the guide documents %s and no override by that name is applied. #146 was this "+
				"exact defect for OBJECTFS_CLUSTER_ENABLED: documented, built on by a compose file, and "+
				"never read", name)
		}

		if !strings.Contains(doc, name) {
			t.Errorf("%s is applied by the loader and the guide's Environment column omits it", name)
		}
	}

	// The secret is deliberately not in that column, and the page says why. If a config key for it ever
	// appears, the page's argument — that /etc/objectfs/config.yaml is world-readable, so the secret
	// cannot live there — has to be revisited rather than left standing.
	if strings.Contains(doc, "`secret:`") {
		t.Error("the guide appears to document a config key for the cluster secret. There is none, by " +
			"design: the file is installed world-readable, so only a path or an environment variable can " +
			"carry it")
	}
}

// TestDistributedGuideAbsentKeysAreStillAbsent inverts the usual staleness direction.
//
// The page states that three keys from #152's specification do not exist. That is the honest thing to
// write today and becomes a lie the moment one is implemented — at which point the page is telling an
// operator a shipped feature is unavailable, and the limitations section is the part of the page a
// cautious reader trusts most.
//
// So: if this fails, the fix is to write the section #152 originally asked for, not to delete the test.
func TestDistributedGuideAbsentKeysAreStillAbsent(t *testing.T) {
	t.Parallel()

	schema := make(map[string]bool, len(TopLevelKeys()))
	for _, k := range TopLevelKeys() {
		schema[k] = true
	}

	// Cheapest possible check that the claim is about this schema: parse a config that sets each key and
	// require strict decoding to reject it. A key that has been implemented parses.
	for _, key := range []string{"persistent_log", "data_dir", "peer_fetch_timeout"} {
		doc := "cluster:\n  enabled: true\n  " + key + ": /tmp/x\n"

		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		if err := NewDefault().LoadFromFile(path); err == nil {
			t.Errorf("cluster.%s now parses. docs/admin-guide/distributed.md states it does not exist "+
				"and lists it under limitations, and #152 asked for a section documenting it — write that "+
				"section rather than removing this check, because the page currently tells operators a "+
				"shipped feature is unavailable", key)
		}
	}
}

// TestDistributedGuideNamesTheRealGossipPort is the one number the issue itself got wrong.
//
// #152 and #148 both say 7946 — memberlist's default, and the port in some of this package's own test
// fixtures. It is not the default anywhere in the code. An operator who opens 7946 between nodes and
// nothing else gets a cluster of one, which is the failure mode with no error message, reached by
// following the documentation.
func TestDistributedGuideNamesTheRealGossipPort(t *testing.T) {
	t.Parallel()

	doc := docsDistributed(t)
	def := NewDefault().Cluster

	_, port, ok := strings.Cut(def.ListenAddr, ":")
	if !ok {
		t.Fatalf("NewDefault's listen_addr %q is not host:port", def.ListenAddr)
	}

	// The diagnosis section says "The default is `8080`, not 7946".
	if !strings.Contains(doc, "The default is `"+port+"`") {
		t.Errorf("the diagnosis section does not name %s as the gossip port. The default listen_addr is "+
			"%q, and the firewall step is the one that sends an operator to open a port", port,
			def.ListenAddr)
	}
}
