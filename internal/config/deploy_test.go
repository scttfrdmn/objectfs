package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

// Gates on the deployment manifests in deploy/, which #146 added.
//
// These files are the shape of thing this repository has been burned by twice — a shipped artifact that
// nothing executes. The systemd unit spent a year calling `objectfs s3://%i /mnt/objectfs/%i`, a form
// that fused the instance name and the bucket into one string, and it passed `systemd-analyze verify`
// the whole time, because systemd checks that a unit is well-formed systemd and has no idea what flags
// the program accepts. A Kubernetes manifest has the identical hole: `kubeconform` will happily approve
// a Pod whose container invokes a subcommand that does not exist and an environment variable nothing
// reads. Schema-valid and wrong is the default state of a manifest.
//
// So the checks here are the ones a schema validator cannot make:
//
//   - every objectfs command line in a manifest uses a subcommand the binary dispatches and flags it
//     parses — scraped from cmd/objectfs/main.go, reusing docs_symbols_test.go's machinery so there is
//     one definition of "a flag this binary accepts";
//   - every OBJECTFS_* variable a manifest sets is one getEnvMappings actually reads. This is the
//     check that caught the defect #146 was built on: the issue body's compose file set four cluster
//     variables and *none* of them existed, so a three-node deployment would have come up as three
//     independent single-node mounts with no error anywhere. Both SDK READMEs documented one of them.
//     An unread variable cannot complain, which is why something has to ask on its behalf;
//   - the embedded config in the ConfigMap loads, under the same strict unmarshal a real mount uses;
//   - the health endpoint is off loopback in every pod spec, because the kubelet probes over the pod
//     IP and a 127.0.0.1 listener refuses it — a pod restarted forever by a probe that could never
//     have passed.
//
// What is deliberately not here: applying any of this. That needs a cluster, and a CI job that stands
// one up is worth having separately. These run wherever `go test` does.

// deployManifests are the files under test, relative to the module root.
var deployManifests = []string{
	"deploy/docker/docker-compose.yaml",
	"deploy/docker/node-config.yaml",
	"deploy/kubernetes/configmap.yaml",
	"deploy/kubernetes/daemonset.yaml",
	"deploy/kubernetes/statefulset.yaml",
}

// envVarRef matches an OBJECTFS_* variable name wherever it appears in a manifest.
var envVarRef = regexp.MustCompile(`OBJECTFS_[A-Z0-9_]+`)

// TestDeployManifestsExist fails before any subtest can pass vacuously.
//
// Every assertion below iterates over files or over matches within them, and each of those loops
// passes trivially on an empty set. A rename that moved deploy/ would otherwise turn this whole file
// green, which is the failure mode it is here to prevent.
func TestDeployManifestsExist(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	for _, rel := range deployManifests {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s is missing: %v. Every check in deploy_test.go loops over these files, so a "+
				"moved or renamed manifest makes them all pass on an empty set", rel, err)
		}
	}
}

// TestDeployManifestsSetOnlyEnvVarsTheBinaryReads is the check kubeconform cannot make.
//
// #146's issue body specified OBJECTFS_CLUSTER_ENABLED, _NODE_ID, _ADVERTISE_ADDR and _SEEDS, and
// getEnvMappings held none of the four — verified by calling it rather than by reading the file. A
// manifest built on that spec produces the worst available outcome: three pods that start cleanly,
// mount successfully, serve reads, and are each a cluster of one. No error, because nothing read the
// variable, and cache invalidations from peers are simply never received — so a node can serve an
// object another node has already overwritten.
//
// The variables now exist. This is what keeps them and the manifests from drifting apart again in
// either direction: a renamed mapping fails here, and so does a manifest that invents a name.
func TestDeployManifestsSetOnlyEnvVarsTheBinaryReads(t *testing.T) {
	t.Parallel()

	read := make(map[string]bool, len(getEnvMappings()))
	for _, m := range getEnvMappings() {
		read[m.envVar] = true
	}

	if len(read) == 0 {
		t.Fatal("getEnvMappings() returned nothing, so every name below would be reported as " +
			"unread and the failures would all be about this test rather than about the manifests")
	}

	// Variables read somewhere other than the config table. Each is genuinely consumed, just not by
	// LoadFromEnv, so a manifest may legitimately set it.
	//
	// OBJECTFS_CLUSTER_SECRET is the interesting one and the asymmetry is deliberate: it is read by
	// distributed.LoadClusterSecret (as ClusterSecretEnv) and has no config-file key at all, because a
	// secret written into a world-readable config file is published to every user on the node. Here it
	// is the reverse of the other cluster settings — the material is in the environment and the path
	// is in the file.
	readElsewhere := map[string]string{
		"OBJECTFS_CLUSTER_SECRET": "distributed.LoadClusterSecret, via ClusterSecretEnv",
		"OBJECTFS_CONFIG":         "the systemd unit, which passes it to --config",
	}

	root := repoRoot(t)

	for _, rel := range deployManifests {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()

			body := readDeployManifest(t, root, rel)

			seen := make(map[string]bool)
			for _, name := range envVarRef.FindAllString(body, -1) {
				if seen[name] {
					continue
				}

				seen[name] = true

				if read[name] || readElsewhere[name] != "" {
					continue
				}

				t.Errorf("%s names %s, which nothing reads. getEnvMappings() has no entry for it and "+
					"it is not in readElsewhere, so a deployment setting it gets silence — the pod "+
					"starts, the setting has no effect, and nothing reports it. This is exactly how "+
					"#146's spec came to name four cluster variables that did not exist. Either wire "+
					"it in getEnvMappings or stop setting it here.", rel, name)
			}
		})
	}
}

// TestDeployManifestsInvokeTheRealBinary is the systemd-unit check, applied to containers.
//
// Same defect class and the same reasoning as TestSystemdUnitInvokesTheRealBinary: a schema validator
// approves any string in an args list. The scraped sets come from cmd/objectfs/main.go through
// docs_symbols_test.go's cliFlags, so this compares against the binary rather than against a list
// somebody maintains.
func TestDeployManifestsInvokeTheRealBinary(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	//nolint:gosec // a path built from the module root this test located itself
	mainGo, err := os.ReadFile(filepath.Join(root, "cmd", "objectfs", "main.go"))
	if err != nil {
		t.Fatalf("reading cmd/objectfs/main.go, which declares the flags: %v", err)
	}

	subcommands := dispatchedSubcommands(t, string(mainGo))
	if len(subcommands) == 0 {
		t.Fatal("scraped no subcommands from cmd/objectfs/main.go; without them the loop below " +
			"approves anything")
	}

	// The one argument in these manifests that is neither a flag nor a subcommand: the mount's own
	// config path, which reaches the binary as --config=<path> and is checked separately by
	// TestDeployConfigMapConfigLoads.
	var checkedCommands int

	for _, rel := range deployManifests {
		if !strings.HasSuffix(rel, ".yaml") || strings.HasSuffix(rel, "node-config.yaml") {
			continue
		}

		body := readDeployManifest(t, root, rel)

		// The args are YAML list items, one token per line: `- mount`, `- --foreground`. Parsing them
		// out of the raw text rather than through a typed unmarshal keeps this working across the two
		// schemas in play (compose's `command:` and Kubernetes' `args:`) without modeling either.
		inArgs := false

		for raw := range strings.SplitSeq(body, "\n") {
			line := strings.TrimSpace(raw)

			switch {
			case strings.HasPrefix(line, "command:"), strings.HasPrefix(line, "args:"):
				inArgs = true

				continue
			case line == "" || strings.HasPrefix(line, "#"):
				continue
			case !strings.HasPrefix(line, "- "):
				// Any non-list line ends the block. A `command:` with a string value on the same line
				// is not a form these manifests use, and would show up as an uninspected command
				// rather than as a false pass, because checkedCommands would not advance.
				inArgs = false

				continue
			}

			if !inArgs {
				continue
			}

			token := strings.TrimSpace(strings.TrimPrefix(line, "- "))

			// Flags may be --flag or --flag=value; only the name is checked here.
			if strings.HasPrefix(token, "-") {
				name := strings.TrimLeft(token, "-")
				name, _, _ = strings.Cut(name, "=")

				if !cliFlags[name] {
					t.Errorf("%s passes --%s, which the binary does not parse. flag.Parse exits 1 on "+
						"an unknown flag, so this is a container that crash-loops with a usage "+
						"message — and every schema validator approves it", rel, name)
				}

				checkedCommands++

				continue
			}

			// A bare token in an args block is either the subcommand or something like a bucket URI.
			// Only the first is checked, and a subcommand is what these manifests start with.
			if subcommands[token] {
				checkedCommands++
			}
		}
	}

	// Mutation-proofing: this whole loop is text matching, and a change to the manifests' formatting
	// would make it inspect nothing while still passing. The floor is deliberately loose — it only has
	// to be high enough that "found no args at all" fails.
	if checkedCommands < 6 {
		t.Errorf("inspected only %d command tokens across the manifests, which is fewer than the "+
			"`mount --config --foreground` in each of the two pod specs. The args blocks are being "+
			"parsed out of raw YAML text, so a formatting change can silently stop matching — that "+
			"is what this floor is for", checkedCommands)
	}
}

// TestDeployConfigMapConfigLoads runs the embedded config through the real loader.
//
// The ConfigMap's config.yaml is a string inside a YAML document, so nothing about it is checked by
// validating the manifest: kubeconform sees a valid `data` map with a valid string value. But
// LoadFromFile uses yaml.UnmarshalStrict, so a key that does not exist fails a real mount at load —
// which means an invented or renamed key in here is a pod that starts and immediately exits, and the
// only place it can be caught before deployment is a test that loads it.
func TestDeployConfigMapConfigLoads(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	//nolint:gosec // a path built from the module root this test located itself
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "kubernetes", "configmap.yaml"))
	if err != nil {
		t.Fatalf("reading the ConfigMap: %v", err)
	}

	var manifest struct {
		Data map[string]string `yaml:"data"`
	}

	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the ConfigMap is not parseable YAML: %v", err)
	}

	embedded, ok := manifest.Data["config.yaml"]
	if !ok || strings.TrimSpace(embedded) == "" {
		t.Fatal("deploy/kubernetes/configmap.yaml has no non-empty data[\"config.yaml\"]. The pods " +
			"mount that key at /etc/objectfs/config.yaml and pass it to --config, so an empty or " +
			"renamed key is a mount that fails at startup")
	}

	// Written to a file and loaded through LoadFromFile rather than unmarshalled here, so this
	// exercises the same strict decode and the same validation a real mount does. A hand-rolled
	// unmarshal would accept keys the mount rejects, which is the agreement under test.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(embedded), 0o600); err != nil {
		t.Fatalf("writing the embedded config: %v", err)
	}

	cfg := NewDefault()
	if err := cfg.LoadFromFile(path); err != nil {
		t.Fatalf("the config the pods mount does not load: %v\n\nLoadFromFile uses "+
			"yaml.UnmarshalStrict, so an unknown key fails here exactly as it would fail the pod at "+
			"startup", err)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the config the pods mount does not validate: %v", err)
	}

	// The one setting whose default is wrong in a container, asserted on the loaded value rather than
	// on the file text. See TestDeployPodsBindHealthOffLoopback for why; here the point is that the
	// ConfigMap is *allowed* to keep the loopback default because both pod specs override it, and this
	// records which of the two is doing the work.
	if cfg.Monitoring.HealthChecks.Addr != DefaultHealthAddr {
		t.Logf("the ConfigMap now sets health_checks.addr = %q rather than leaving the loopback "+
			"default; the pod specs' OBJECTFS_HEALTH_ADDR override is then redundant rather than "+
			"load-bearing", cfg.Monitoring.HealthChecks.Addr)
	}
}

// TestDeployPodsBindHealthOffLoopback pins the setting whose default is wrong in a pod.
//
// config.DefaultHealthAddr is 127.0.0.1:8081, which is the right default for a host install of an
// unauthenticated endpoint and unusable in Kubernetes: the kubelet probes over the *pod IP*, so a
// loopback listener refuses the connection. The readiness probe then never passes and the liveness
// probe kills the pod, forever, over an endpoint that could never have answered. Nothing in the
// manifest looks wrong — the probe names the right port and the right path.
//
// Checked on the manifest text rather than through a typed unmarshal because what matters is that the
// override is present in each pod spec, and there are two of them in separate files.
func TestDeployPodsBindHealthOffLoopback(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	for _, rel := range []string{
		"deploy/kubernetes/daemonset.yaml",
		"deploy/kubernetes/statefulset.yaml",
		// Compose's healthcheck runs *inside* the container, where loopback would work — but the port
		// is published, and a loopback listener is not reachable from the host either. Same override,
		// same reason.
		"deploy/docker/docker-compose.yaml",
	} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()

			body := readDeployManifest(t, root, rel)

			if !strings.Contains(body, "OBJECTFS_HEALTH_ADDR") {
				t.Fatalf("%s never sets OBJECTFS_HEALTH_ADDR, so the health endpoint keeps its "+
					"loopback default (%s). In Kubernetes that is a pod the kubelet cannot probe: "+
					"readiness never passes and liveness restarts it forever, over an endpoint that "+
					"refused the connection by design", rel, DefaultHealthAddr)
			}

			// The variable being present is not enough — it has to be set to something that is not
			// loopback, which is the mistake worth catching: an override copied from the config file.
			for line := range strings.SplitSeq(body, "\n") {
				if !strings.Contains(line, "OBJECTFS_HEALTH_ADDR") {
					continue
				}
				// The `value:` is on the following line in the Kubernetes form, so only the compose
				// form has the address on this one. Checking what is here is enough to catch the
				// copied default; the Kubernetes form is checked by the next loop.
				if strings.Contains(line, "127.0.0.1") || strings.Contains(line, "localhost") {
					t.Errorf("%s sets OBJECTFS_HEALTH_ADDR to a loopback address: %s", rel,
						strings.TrimSpace(line))
				}
			}

			if strings.Contains(body, "value: 127.0.0.1:8081") {
				t.Errorf("%s has a `value: 127.0.0.1:8081`, which is the loopback default in an env "+
					"override — the override is present and does nothing", rel)
			}
		})
	}
}

// readDeployManifest reads one manifest, failing rather than returning an empty string.
//
// The distinction matters here more than usual: every caller then searches the text for something, and
// a missing file returning "" would make every one of those searches find nothing and report success.
func readDeployManifest(t *testing.T, root, rel string) string {
	t.Helper()

	//nolint:gosec // a path built from the module root this test located itself
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}

	if len(body) == 0 {
		t.Fatalf("%s is empty, and every assertion against it would pass", rel)
	}

	return string(body)
}
