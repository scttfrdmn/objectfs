package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scttfrdmn/objectfs/internal/distributed"
	"github.com/scttfrdmn/objectfs/internal/health"
)

// serveStatus starts a test server that answers ClusterStatusPath with payload and returns its base URL.
//
// A real HTTP server rather than an injected client, because the path is half of what these tests assert:
// a client that requested the wrong path would pass against a stub that answers everything. It also
// exercises the JSON round trip, which is where a field the report reads but the endpoint does not send
// would show up.
func serveStatus(t *testing.T, payload any) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc(health.ClusterStatusPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("encoding the test payload: %v", err)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv.URL
}

// TestClusterSubcommandIsDispatched checks the nested dispatch, including the two ways of getting it
// wrong.
//
// `cluster` with no subcommand is a usage error rather than an implicit `cluster status`, because the
// family is meant to grow — `cluster members` is the obvious next one — and a default that quietly means
// one member of a set is what makes the set impossible to extend without changing what an existing
// invocation does.
func TestClusterSubcommandIsDispatched(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantCode int
		mustSay  string
	}{
		{
			name:     "cluster alone is a usage error",
			args:     []string{"cluster"},
			wantCode: exitUsage,
			mustSay:  "Usage: objectfs cluster",
		},
		{
			name:     "an unknown subcommand names itself",
			args:     []string{"cluster", "sttaus"},
			wantCode: exitUsage,
			mustSay:  `unknown subcommand "sttaus"`,
		},
		{
			name:     "cluster help exits 0",
			args:     []string{"cluster", "help"},
			wantCode: exitOK,
			mustSay:  "status",
		},
		{
			name:     "cluster --help exits 0",
			args:     []string{"cluster", "--help"},
			wantCode: exitOK,
			mustSay:  "status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runArgs(t, tt.args...)

			if code != tt.wantCode {
				t.Fatalf("objectfs %s exited %d, want %d. stderr: %s",
					strings.Join(tt.args, " "), code, tt.wantCode, stderr)
			}

			// Usage goes to stderr on an error and stdout when it was asked for, the same split the
			// top-level dispatch uses: a script running `objectfs cluster help` captures it with $().
			out := stdout
			if tt.wantCode != exitOK {
				out = stderr
			}

			if !strings.Contains(out, tt.mustSay) {
				t.Errorf("objectfs %s printed %q, which does not contain %q",
					strings.Join(tt.args, " "), out, tt.mustSay)
			}
		})
	}
}

// TestClusterStatusRejectsPositionalArguments keeps a typo from being silently ignored.
//
// `objectfs cluster status http://host:8081` is the invocation an operator reaches for by analogy with
// curl, and Go's flag package leaves it in Args() rather than complaining. Accepting and ignoring it
// would query the default endpoint and report on the wrong instance — a wrong answer that looks like a
// right one.
func TestClusterStatusRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	code, _, stderr := runArgs(t, "cluster", "status", "http://host:8081")

	if code != exitUsage {
		t.Fatalf("exited %d, want %d", code, exitUsage)
	}

	if !strings.Contains(stderr, "takes no arguments") {
		t.Errorf("the error does not say the command takes no arguments: %q", stderr)
	}

	// And it names --endpoint, which is what the operator meant.
	if !strings.Contains(stderr, "endpoint") {
		t.Errorf("the error does not point at --endpoint, so the operator has to guess the flag: %q", stderr)
	}
}

// TestNormalizeEndpoint covers what an operator can type.
//
// The bare host:port form is the one that matters most: it is the shape monitoring.health_checks.addr
// takes, so copying that value onto the command line is the expected path and rejecting it would be a
// gratuitous failure.
func TestNormalizeEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		want       string
		wantErrSay string
		why        string
	}{
		{
			name: "a bare host and port gets the http scheme",
			in:   "127.0.0.1:8081",
			want: "http://127.0.0.1:8081",
			why:  "the form monitoring.health_checks.addr takes, which is what an operator copies",
		},
		{
			name: "a full URL passes through",
			in:   "http://10.0.0.5:8081",
			want: "http://10.0.0.5:8081",
		},
		{
			name: "https is accepted",
			in:   "https://health.example.edu",
			want: "https://health.example.edu",
			why:  "an institutional deployment behind a TLS-terminating proxy",
		},
		{
			name: "a trailing slash is trimmed",
			in:   "http://127.0.0.1:8081/",
			want: "http://127.0.0.1:8081",
			why: "joining the path onto an untrimmed base yields //health/cluster, which some muxes " +
				"redirect and some 404 — a confusing failure for a typo this can absorb",
		},
		{
			name:       "an empty endpoint is refused",
			in:         "   ",
			wantErrSay: "empty",
		},
		{
			name:       "a scheme that cannot be queried is refused",
			in:         "unix:///var/run/objectfs.sock",
			wantErrSay: "only http and https",
			why: "a unix socket is a plausible thing to want and is not what this speaks; failing here " +
				"names the reason instead of producing a connection error about a path",
		},
		{
			name:       "a URL with no host is refused",
			in:         "http://",
			wantErrSay: "names no host",
			why: "url.Parse accepts this, and it would otherwise become a request to nowhere reported " +
				"as a connection error — sending the operator to look at their mount instead of their " +
				"command line",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeEndpoint(tt.in)

			if tt.wantErrSay != "" {
				if err == nil {
					t.Fatalf("normalizeEndpoint(%q) = %q, want an error saying %q. %s",
						tt.in, got, tt.wantErrSay, tt.why)
				}
				if !strings.Contains(err.Error(), tt.wantErrSay) {
					t.Errorf("normalizeEndpoint(%q) error = %q, want it to contain %q",
						tt.in, err, tt.wantErrSay)
				}

				return
			}

			if err != nil {
				t.Fatalf("normalizeEndpoint(%q) = %v, want %q. %s", tt.in, err, tt.want, tt.why)
			}
			if got != tt.want {
				t.Errorf("normalizeEndpoint(%q) = %q, want %q. %s", tt.in, got, tt.want, tt.why)
			}
		})
	}
}

// TestResolveStatusEndpointPrecedence pins where the address comes from.
//
// The config-file step is what makes this usable on a host where the endpoint was moved: the address is
// already a setting, and requiring an operator to repeat it on every invocation is how a command comes to
// be wrapped in a shell alias that goes stale when the file changes.
func TestResolveStatusEndpointPrecedence(t *testing.T) {
	t.Parallel()

	writeConfig := func(t *testing.T, body string) string {
		t.Helper()

		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		return path
	}

	t.Run("the default when nothing is given", func(t *testing.T) {
		t.Parallel()

		got, err := resolveStatusEndpoint(&clusterStatusFlags{})
		if err != nil {
			t.Fatalf("resolveStatusEndpoint: %v", err)
		}

		if got != "http://127.0.0.1:8081" {
			t.Errorf("endpoint = %q, want the packaged default: an operator who has changed nothing "+
				"must not have to pass a flag", got)
		}
	})

	t.Run("the config file's address", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `monitoring:
  health_checks:
    enabled: true
    addr: 127.0.0.1:9099
`)

		got, err := resolveStatusEndpoint(&clusterStatusFlags{configFile: path})
		if err != nil {
			t.Fatalf("resolveStatusEndpoint: %v", err)
		}

		if got != "http://127.0.0.1:9099" {
			t.Errorf("endpoint = %q, want the address from the file", got)
		}
	})

	t.Run("--endpoint beats the config file", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `monitoring:
  health_checks:
    enabled: true
    addr: 127.0.0.1:9099
`)

		got, err := resolveStatusEndpoint(&clusterStatusFlags{
			configFile: path, endpoint: "127.0.0.1:7777",
		})
		if err != nil {
			t.Fatalf("resolveStatusEndpoint: %v", err)
		}

		if got != "http://127.0.0.1:7777" {
			t.Errorf("endpoint = %q, want --endpoint's value: an explicit flag silently losing to a "+
				"file the operator may not have read is the wrong precedence", got)
		}
	})

	t.Run("health checks disabled in the named file", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `monitoring:
  health_checks:
    enabled: false
`)

		_, err := resolveStatusEndpoint(&clusterStatusFlags{configFile: path})
		if err == nil {
			t.Fatal("a config file that disables health checks was accepted; the command would then " +
				"produce a connection error, which reads as a dead instance rather than as an endpoint " +
				"that was never started")
		}

		if !strings.Contains(err.Error(), "health_checks.enabled") {
			t.Errorf("the error does not name the setting to change: %v", err)
		}
	})

	t.Run("a config file that does not exist", func(t *testing.T) {
		t.Parallel()

		_, err := resolveStatusEndpoint(&clusterStatusFlags{
			configFile: filepath.Join(t.TempDir(), "absent.yaml"),
		})
		if err == nil {
			t.Fatal("a missing --config file was accepted, so a typo in the path silently queries the " +
				"default endpoint and reports on whatever instance is there")
		}
	})
}

// TestClusterStatusExitCode is the semantics an alerting script branches on, and it is where the issue
// was wrong.
//
// The issue specified "1: instance unreachable OR quorum lost". Quorum is a Raft concept and no leader is
// elected on a mount path, so a majority is not something this cluster has an opinion about — a two-node
// cluster with one node down keeps working correctly, because coordination is compare-and-swap against S3
// and the store evaluates it on one request. Exiting non-zero for an absent majority would page for a
// cluster that is fine.
func TestClusterStatusExitCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status distributed.ClusterStatus
		want   int
		why    string
	}{
		{
			name:   "clustering disabled is not a fault",
			status: distributed.ClusterStatus{Enabled: false, Reason: "not configured"},
			want:   exitOK,
			why: "cluster.enabled defaults to false, so a non-zero status here would fire on every " +
				"single-node mount in existence",
		},
		{
			name: "a healthy cluster",
			status: distributed.ClusterStatus{
				Enabled:    true,
				Membership: distributed.MembershipStatus{Total: 3, Alive: 3},
			},
			want: exitOK,
		},
		{
			name: "a cluster of one is healthy",
			status: distributed.ClusterStatus{
				Enabled:    true,
				Membership: distributed.MembershipStatus{Total: 1, Alive: 1},
			},
			want: exitOK,
			why:  "nothing about ObjectFS coordination requires a second node",
		},
		{
			name: "a dead node",
			status: distributed.ClusterStatus{
				Enabled:    true,
				Membership: distributed.MembershipStatus{Total: 3, Alive: 2, Dead: 1},
			},
			want: exitFailure,
			why:  "the honest failure signal a gossip cluster has",
		},
		{
			name: "a suspect node",
			status: distributed.ClusterStatus{
				Enabled:    true,
				Membership: distributed.MembershipStatus{Total: 3, Alive: 2, Suspect: 1},
			},
			want: exitFailure,
			why: "suspect is the state a node enters before being declared dead, and an operator wants " +
				"to know while it is still recoverable",
		},
		{
			name: "one node alive of three is still not a quorum question",
			status: distributed.ClusterStatus{
				Enabled:    true,
				Membership: distributed.MembershipStatus{Total: 3, Alive: 1, Dead: 2},
			},
			want: exitFailure,
			why: "non-zero because two nodes are dead, not because a majority is absent — the same code " +
				"a single dead node produces",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := clusterStatusExitCode(&tt.status); got != tt.want {
				t.Errorf("clusterStatusExitCode = %d, want %d. %s", got, tt.want, tt.why)
			}
		})
	}
}

// TestClusterStatusReportsAnUnreachableEndpointAsSuchIsNotAClusterFault is the case the issue's mockup
// could not express, and the one an operator will hit first.
//
// Nothing listening means no instance is running, or its health endpoint is off, or it is on another
// address. None of those is a cluster problem, and a message that merely says "connection refused" is
// read as one. The exit code is still 1 — the command could not answer — but the text has to say what it
// does and does not mean.
func TestClusterStatusReportsAnUnreachableEndpointAsSuchIsNotAClusterFault(t *testing.T) {
	t.Parallel()

	// A port that is closed: bind one and shut it down, so the address is real and refuses. A hardcoded
	// high port would be a coin flip against whatever else is running on the machine.
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close()

	code, stdout, stderr := runArgs(t, "cluster", "status", "--endpoint", endpoint)

	if code != exitFailure {
		t.Fatalf("exited %d, want %d. stdout: %q", code, exitFailure, stdout)
	}

	for _, want := range []string{
		"Nothing is listening",
		"health_checks",
		"does not mean the cluster is unhealthy",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the unreachable-endpoint error does not contain %q, so a connection refused reads "+
				"as a broken cluster: %q", want, stderr)
		}
	}
}

// TestClusterStatusReportsAnOldInstanceDistinctly separates the two HTTP failures that look alike.
//
// A 404 is a different diagnosis from a refused connection, and an operator who cannot tell them apart
// will restart a mount that is running perfectly well.
func TestClusterStatusReportsAnOldInstanceDistinctly(t *testing.T) {
	t.Parallel()

	// A server that answers, but not at this path: an instance from before the endpoint existed.
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	code, _, stderr := runArgs(t, "cluster", "status", "--endpoint", srv.URL)

	if code != exitFailure {
		t.Fatalf("exited %d, want %d", code, exitFailure)
	}

	if !strings.Contains(stderr, "before this endpoint existed") {
		t.Errorf("a 404 is not distinguished from an unreachable endpoint: %q", stderr)
	}

	if strings.Contains(stderr, "Nothing is listening") {
		t.Errorf("a 404 was reported as nothing listening, which sends the operator to restart an "+
			"instance that is running: %q", stderr)
	}
}

// TestClusterStatusOnADisabledClusterExitsZero is the answer almost every invocation will get.
func TestClusterStatusOnADisabledClusterExitsZero(t *testing.T) {
	t.Parallel()

	endpoint := serveStatus(t, distributed.ClusterStatusDisabled(
		"this mount is not running cluster coordination: set cluster.enabled"))

	code, stdout, stderr := runArgs(t, "cluster", "status", "--endpoint", endpoint)

	if code != exitOK {
		t.Fatalf("exited %d, want 0: clustering being off is the default configuration, not a fault. "+
			"stderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "disabled") {
		t.Errorf("the report does not say clustering is disabled: %q", stdout)
	}

	// The reason, so the operator is told it is a configuration state.
	if !strings.Contains(stdout, "cluster.enabled") {
		t.Errorf("the report does not name the setting that would turn it on: %q", stdout)
	}
}

// TestClusterStatusNeverPrintsAnUnmeasuredZero is the constraint that shaped this whole feature.
//
// This project has shipped the opposite twice: percentile fields declared and never assigned that
// published as zeros (#222), and six resource fields broadcast as zeros for four releases (#132). An
// operator reading "cache_hit=0%" cannot tell a cache that has served nothing from one that misses every
// read — a just-started mount and an emergency respectively.
//
// So a node with no cache reports "not reported", a cache that has served nothing reports "not measured",
// and neither of them prints a number.
func TestClusterStatusNeverPrintsAnUnmeasuredZero(t *testing.T) {
	t.Parallel()

	endpoint := serveStatus(t, &distributed.ClusterStatus{
		Enabled:    true,
		NodeID:     "node-a",
		GossipAddr: "127.0.0.1:7946",
		Membership: distributed.MembershipStatus{Total: 2, Alive: 2},
		Self: &distributed.NodeReport{
			ID: "node-a", Address: "127.0.0.1:7946", Status: "alive",
			// No cache at all: the shape of a mount with no cache injected.
		},
		Peers: []distributed.NodeReport{{
			ID: "node-b", Address: "127.0.0.1:7947", Status: "alive",
			// A cache that exists, holds bytes, and has served nothing: HitRate nil, Requests zero.
			Cache: &distributed.NodeCacheReport{Size: 4096},
		}},
		Cache: distributed.ClusterCacheStatus{TotalSize: 4096, AliveNodes: 2},
	})

	code, stdout, stderr := runArgs(t, "cluster", "status", "--endpoint", endpoint)

	if code != exitOK {
		t.Fatalf("exited %d, want 0. stderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "cache=not reported") {
		t.Errorf("a node with no cache does not report as unreported, so its line cannot be told from "+
			"a node whose cache is empty: %q", stdout)
	}

	if !strings.Contains(stdout, "hit=not measured") {
		t.Errorf("a cache that has served nothing does not report as unmeasured: %q", stdout)
	}

	if strings.Contains(stdout, "hit=0.0%") {
		t.Errorf("the report prints a 0%% hit rate for a cache that has served nothing, which is the "+
			"defect #222 shipped: %q", stdout)
	}

	if !strings.Contains(stdout, "Hit rate:   not measured") {
		t.Errorf("the cluster-wide hit rate prints a figure nothing measured: %q", stdout)
	}

	if !strings.Contains(stdout, "Capacity:   not reported by any node") {
		t.Errorf("the cluster capacity prints a sum no node reported: %q", stdout)
	}
}

// TestClusterStatusDoesNotPrintARoleWithoutAnElection is the mockup field this drops on purpose.
//
// The issue asked for "Role: Leader"/"Role: Follower" and leader=true/false per peer. Nothing elects a
// leader on a mount path, so IsLeader is false on every node of a healthy cluster and "Follower" would
// report a lost election that was never held. The report says so in words instead.
func TestClusterStatusDoesNotPrintARoleWithoutAnElection(t *testing.T) {
	t.Parallel()

	endpoint := serveStatus(t, &distributed.ClusterStatus{
		Enabled:    true,
		NodeID:     "node-a",
		Membership: distributed.MembershipStatus{Total: 1, Alive: 1},
		Self:       &distributed.NodeReport{ID: "node-a", Status: "alive"},
		// Leadership deliberately nil, which is what a mount produces.
	})

	code, stdout, _ := runArgs(t, "cluster", "status", "--endpoint", endpoint)
	if code != exitOK {
		t.Fatalf("exited %d, want 0", code)
	}

	if strings.Contains(stdout, "follower") || strings.Contains(stdout, "Follower") {
		t.Errorf("the report calls this node a follower without an election having happened: %q", stdout)
	}

	if !strings.Contains(stdout, "leader election is not running") {
		t.Errorf("the report does not explain why there is no role, leaving an operator to wonder "+
			"whether the field is missing or the election failed: %q", stdout)
	}

	// And it names the mechanism that is actually in use, so the absence is an answer rather than a gap.
	if !strings.Contains(stdout, "compare-and-swap") {
		t.Errorf("the report does not say what coordination does instead: %q", stdout)
	}
}

// TestClusterStatusPrintsARoleWhenConsensusIsRunning is the other half, so the branch above is a
// distinction rather than dead code.
func TestClusterStatusPrintsARoleWhenConsensusIsRunning(t *testing.T) {
	t.Parallel()

	endpoint := serveStatus(t, &distributed.ClusterStatus{
		Enabled:    true,
		NodeID:     "node-a",
		Leadership: &distributed.LeadershipStatus{Leader: "node-a", IsSelf: true, Election: 3},
		Membership: distributed.MembershipStatus{Total: 1, Alive: 1},
		Self:       &distributed.NodeReport{ID: "node-a", Status: "alive"},
	})

	code, stdout, _ := runArgs(t, "cluster", "status", "--endpoint", endpoint)
	if code != exitOK {
		t.Fatalf("exited %d, want 0", code)
	}

	if !strings.Contains(stdout, "leader") {
		t.Errorf("a node that holds leadership is not reported as leader: %q", stdout)
	}
	if !strings.Contains(stdout, "elections: 3") {
		t.Errorf("the election count is not reported: %q", stdout)
	}
}

// TestClusterStatusPrintsAnomaliesOnlyWhenNonZero keeps the section readable.
//
// A block of zeros printed every run trains an operator to skip the section, which is precisely the
// section that matters on the one run where a counter is not zero.
func TestClusterStatusPrintsAnomaliesOnlyWhenNonZero(t *testing.T) {
	t.Parallel()

	base := func() *distributed.ClusterStatus {
		return &distributed.ClusterStatus{
			Enabled:    true,
			NodeID:     "node-a",
			Membership: distributed.MembershipStatus{Total: 1, Alive: 1},
			Self:       &distributed.NodeReport{ID: "node-a", Status: "alive"},
			Gossip:     distributed.GossipCounters{MessagesSent: 10, MessagesReceived: 9},
		}
	}

	t.Run("a clean cluster prints no anomaly section", func(t *testing.T) {
		t.Parallel()

		code, stdout, _ := runArgs(t, "cluster", "status", "--endpoint", serveStatus(t, base()))
		if code != exitOK {
			t.Fatalf("exited %d", code)
		}

		if strings.Contains(stdout, "Anomalies") {
			t.Errorf("a cluster with no anomalies printed an anomaly section: %q", stdout)
		}
	})

	t.Run("a truncated datagram is named with its cause", func(t *testing.T) {
		t.Parallel()

		status := base()
		status.Gossip.MessagesTruncated = 4

		code, stdout, _ := runArgs(t, "cluster", "status", "--endpoint", serveStatus(t, status))
		if code != exitOK {
			t.Fatalf("exited %d", code)
		}

		if !strings.Contains(stdout, "truncated: 4") {
			t.Errorf("the truncation count is not reported: %q", stdout)
		}

		// The note matters more than the number: a clipped datagram fails the authentication envelope
		// parse, so it reports itself as a wrong cluster secret (#277). Without the note an operator
		// rotates a secret that is correct.
		if !strings.Contains(stdout, "authentication failure") {
			t.Errorf("the truncation line does not explain that it masquerades as an authentication "+
				"failure, which is the whole diagnostic value: %q", stdout)
		}
	})
}

// TestClusterStatusJSONIsTheWireFormat asserts --json emits something a script can consume.
//
// Re-encoded from the decoded struct rather than passed through, so that --json is documented by
// distributed.ClusterStatus and cannot carry a field the human report has no counterpart for. That means
// this test also fails if the two drift.
func TestClusterStatusJSONIsTheWireFormat(t *testing.T) {
	t.Parallel()

	endpoint := serveStatus(t, &distributed.ClusterStatus{
		Enabled:    true,
		NodeID:     "node-a",
		GossipAddr: "127.0.0.1:7946",
		Membership: distributed.MembershipStatus{Total: 2, Alive: 1, Dead: 1},
		Self:       &distributed.NodeReport{ID: "node-a", Status: "alive"},
		Peers:      []distributed.NodeReport{{ID: "node-b", Status: "dead"}},
	})

	code, stdout, stderr := runArgs(t, "cluster", "status", "--json", "--endpoint", endpoint)

	// Exit 1 because a node is dead, which is orthogonal to --json working.
	if code != exitFailure {
		t.Fatalf("exited %d, want %d (a dead node). stderr: %s", code, exitFailure, stderr)
	}

	var decoded distributed.ClusterStatus
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("--json produced something that is not a cluster status (%q): %v", stdout, err)
	}

	if decoded.NodeID != "node-a" || decoded.Membership.Dead != 1 {
		t.Errorf("--json round-tripped to %+v, which is not what the endpoint served", decoded)
	}

	// No human report mixed into the JSON: a script pipes this into jq.
	if strings.Contains(stdout, "ObjectFS Cluster Status") {
		t.Errorf("--json emitted the human report as well, so the output is not parseable: %q", stdout)
	}
}

// TestClusterStatusSaysWhenThereAreNoPeers is about a single-node cluster not looking broken.
//
// A report that lists one node and nothing else reads as a cluster that lost its peers. It is in fact a
// working configuration, and saying so is the difference between an operator investigating and an
// operator moving on.
func TestClusterStatusSaysWhenThereAreNoPeers(t *testing.T) {
	t.Parallel()

	endpoint := serveStatus(t, &distributed.ClusterStatus{
		Enabled:    true,
		NodeID:     "only-node",
		Membership: distributed.MembershipStatus{Total: 1, Alive: 1},
		Self:       &distributed.NodeReport{ID: "only-node", Status: "alive"},
	})

	code, stdout, _ := runArgs(t, "cluster", "status", "--endpoint", endpoint)
	if code != exitOK {
		t.Fatalf("exited %d, want 0: a cluster of one is a working configuration", code)
	}

	if !strings.Contains(stdout, "cluster of one") {
		t.Errorf("a single-node cluster is not described as such, so it reads as a cluster that lost "+
			"its peers: %q", stdout)
	}
}

// TestClusterStatusReportsAnUnboundGossipSocket names a failure that is otherwise invisible.
//
// A node whose gossip bind failed serves its mount normally and is unreachable by every peer. The mount
// working is exactly what makes this hard to notice from anywhere else.
func TestClusterStatusReportsAnUnboundGossipSocket(t *testing.T) {
	t.Parallel()

	endpoint := serveStatus(t, &distributed.ClusterStatus{
		Enabled:    true,
		NodeID:     "unbound",
		GossipAddr: "",
		Membership: distributed.MembershipStatus{Total: 1, Alive: 1},
		Self:       &distributed.NodeReport{ID: "unbound", Status: "alive"},
	})

	code, stdout, _ := runArgs(t, "cluster", "status", "--endpoint", endpoint)
	if code != exitOK {
		t.Fatalf("exited %d", code)
	}

	if !strings.Contains(stdout, "not bound") {
		t.Errorf("an unbound gossip socket is not reported, and the mount serving normally is what "+
			"makes it invisible everywhere else: %q", stdout)
	}
}

// TestClusterStatusRejectsABadEndpointBeforeConnecting keeps a command-line error out of the
// connection-error path.
//
// Exit 2, not 1: nothing was attempted, and a monitoring script that treats 1 as "the cluster is in
// trouble" must not see that for a typo in its own arguments.
func TestClusterStatusRejectsABadEndpointBeforeConnecting(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{"unix:///var/run/objectfs.sock", "http://", "   "} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()

			code, _, stderr := runArgs(t, "cluster", "status", "--endpoint", endpoint)

			if code != exitUsage {
				t.Errorf("--endpoint %q exited %d, want %d: the command line is what is wrong, and a "+
					"script branching on 1 would read this as a cluster fault. stderr: %s",
					endpoint, code, exitUsage, stderr)
			}
		})
	}
}

// TestClusterStatusRejectsANonStatusResponse is the endpoint answering something else entirely, which is
// what a reverse proxy in front of the wrong service looks like.
func TestClusterStatusRejectsANonStatusResponse(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc(health.ClusterStatusPath, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>Grafana</html>"))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	code, _, stderr := runArgs(t, "cluster", "status", "--endpoint", srv.URL)

	if code != exitFailure {
		t.Fatalf("exited %d, want %d", code, exitFailure)
	}

	if !strings.Contains(stderr, "not a cluster status") {
		t.Errorf("a non-JSON response is not reported as such, so an operator pointed at the wrong "+
			"service gets a decode error rather than a diagnosis: %q", stderr)
	}
}

// TestClusterStatusRejectsAnUnexpectedStatusCode covers the third HTTP failure mode.
//
// A 500 from the endpoint is neither an old build nor a dead instance, and reporting it as either would
// be wrong. It is also what a proxy returns when the instance behind it is unreachable.
func TestClusterStatusRejectsAnUnexpectedStatusCode(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc(health.ClusterStatusPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	code, _, stderr := runArgs(t, "cluster", "status", "--endpoint", srv.URL)

	if code != exitFailure {
		t.Fatalf("exited %d, want %d", code, exitFailure)
	}

	if !strings.Contains(stderr, "502") {
		t.Errorf("the error does not name the status code, which is the only thing that identifies "+
			"what answered: %q", stderr)
	}
}

// TestNodeLineDistinguishesEveryAbsentField is the formatting contract, asserted on the function rather
// than through a server so every combination is cheap to cover.
func TestNodeLineDistinguishesEveryAbsentField(t *testing.T) {
	t.Parallel()

	capacity := int64(8192)
	rate := 0.0

	tests := []struct {
		name       string
		report     distributed.NodeReport
		mustSay    []string
		mustNotSay []string
		why        string
	}{
		{
			name:    "no cache reported",
			report:  distributed.NodeReport{ID: "a", Status: "alive"},
			mustSay: []string{"cache=not reported"},
			// "0 B" would describe a cache that exists and holds nothing.
			mustNotSay: []string{"0 B"},
		},
		{
			name: "a cache with no capacity",
			report: distributed.NodeReport{ID: "a", Status: "alive",
				Cache: &distributed.NodeCacheReport{Size: 100, Requests: 4, HitRate: &rate}},
			mustSay: []string{"cache=100 B", "hit=0.0%"},
			why: "a measured 0% is printed as 0%: this is the case the 'not measured' text exists to be " +
				"distinguished from, and suppressing it too would lose the real emergency",
		},
		{
			name: "a cache with capacity",
			report: distributed.NodeReport{ID: "a", Status: "alive",
				Cache: &distributed.NodeCacheReport{Size: 100, Capacity: &capacity}},
			mustSay: []string{"100 B/8.0 KB"},
		},
		{
			name:    "an address that is not known",
			report:  distributed.NodeReport{ID: "a", Status: "alive"},
			mustSay: []string{"(none)"},
			why: "an empty column looks like whitespace in a column-aligned report, so a missing " +
				"address would be invisible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := nodeLine(tt.report, false)

			for _, want := range tt.mustSay {
				if !strings.Contains(got, want) {
					t.Errorf("nodeLine = %q, want it to contain %q. %s", got, want, tt.why)
				}
			}

			for _, unwanted := range tt.mustNotSay {
				if strings.Contains(got, unwanted) {
					t.Errorf("nodeLine = %q, want it not to contain %q. %s", got, unwanted, tt.why)
				}
			}
		})
	}
}

// TestClusterStatusReportsAPartialCapacitySum keeps the count beside the sum.
//
// The Redis-backed cache reports no capacity of its own, so a mixed cluster genuinely has nodes that
// cannot answer. One node of three reporting capacity would otherwise make the cluster look three times
// fuller than it is.
func TestClusterStatusReportsAPartialCapacitySum(t *testing.T) {
	t.Parallel()

	endpoint := serveStatus(t, &distributed.ClusterStatus{
		Enabled:    true,
		NodeID:     "node-a",
		Membership: distributed.MembershipStatus{Total: 3, Alive: 3},
		Self:       &distributed.NodeReport{ID: "node-a", Status: "alive"},
		Cache: distributed.ClusterCacheStatus{
			TotalSize: 300, TotalCapacity: 1000, NodesReportingCapacity: 1, AliveNodes: 3,
		},
	})

	code, stdout, _ := runArgs(t, "cluster", "status", "--endpoint", endpoint)
	if code != exitOK {
		t.Fatalf("exited %d", code)
	}

	if !strings.Contains(stdout, "from 1 of 3 nodes") {
		t.Errorf("a partial capacity sum is printed without saying how partial it is, so the cluster "+
			"looks three times fuller than it is: %q", stdout)
	}
}
