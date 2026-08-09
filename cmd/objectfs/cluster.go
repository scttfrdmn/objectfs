package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/distributed"
	"github.com/scttfrdmn/objectfs/internal/health"
	"github.com/scttfrdmn/objectfs/pkg/utils"
)

// clusterStatusTimeout bounds the request to a local health endpoint.
//
// Five seconds against a loopback HTTP handler that takes two mutexes and copies a membership map. It is
// long enough that a loaded machine does not time out and short enough that an operator running this in a
// shell loop is not left waiting on a process that has stopped answering — which is a real state, since
// the endpoint is served by the same process that serves the mount.
const clusterStatusTimeout = 5 * time.Second

// runCluster dispatches the `cluster` subcommand's own subcommands.
//
// Nested dispatch rather than a flag, because `cluster status` is the first of a family — `cluster
// members` and `cluster peers` are the obvious next ones — and the shape follows `etcdctl endpoint
// status` and `consul members`, which the issue named as the models.
func runCluster(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		clusterUsage(stderr)

		return exitUsage
	}

	switch args[0] {
	case "status":
		return runClusterStatus(args[1:], stdout, stderr)

	case "help", "--help", "-h", "-help":
		clusterUsage(stdout)

		return exitOK
	}

	emit(stderr, "objectfs cluster: unknown subcommand %q\n\n", args[0])
	clusterUsage(stderr)

	return exitUsage
}

func clusterUsage(w io.Writer) {
	emit(w, `Usage: objectfs cluster <subcommand> [options]

Subcommands:
  status    Report the cluster state of a running ObjectFS instance

Run "objectfs cluster status --help" for its options.
`)
}

// clusterStatusFlags is one `cluster status` invocation's command line, parsed.
type clusterStatusFlags struct {
	endpoint string
	asJSON   bool

	// configFile is read for one setting only: monitoring.health_checks.addr, so that an operator who
	// moved the health endpoint does not have to restate it here. --endpoint still wins.
	configFile string
}

func newClusterStatusFlagSet(fs *flag.FlagSet, f *clusterStatusFlags) {
	fs.StringVar(&f.endpoint, "endpoint", "",
		"Base URL of the instance's health endpoint (default from the config file, else "+
			"http://"+config.DefaultHealthAddr+")")
	fs.BoolVar(&f.asJSON, "json", false, "Print the raw status as JSON instead of a report")
	fs.StringVar(&f.configFile, "config", "",
		"Configuration file, read for monitoring.health_checks.addr")
}

// runClusterStatus implements `objectfs cluster status`.
//
// # Exit codes
//
// The issue specified "0: healthy, quorum present" and "1: instance unreachable OR quorum lost", and
// quorum is not the right condition here — it is a Raft concept, and this project does not run Raft on a
// mount. See [distributed.ClusterConfig.EnableConsensus]: coordination is compare-and-swap against S3,
// evaluated by the store on one request, and a two-node cluster with one node down keeps working
// correctly. Exiting non-zero because a majority is absent would make a monitoring script page for a
// cluster that is fine.
//
// What it exits non-zero for instead is what actually indicates a problem an operator must act on:
//
//   - the instance is unreachable, which is the issue's first condition and is kept verbatim;
//   - a node is dead or suspect, which is the honest failure signal a gossip cluster has.
//
// Clustering being disabled is exit 0. It is the default configuration and by far the commonest one, so
// a non-zero status for it would fire on every single-node mount in existence.
func runClusterStatus(args []string, stdout, stderr io.Writer) int {
	var f clusterStatusFlags

	fs := flag.NewFlagSet("objectfs cluster status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		emit(stderr, "Usage: objectfs cluster status [options]\n\n"+
			"Reports the cluster state of a running ObjectFS instance, read from its health\n"+
			"endpoint at "+health.ClusterStatusPath+". The instance must have\n"+
			"monitoring.health_checks.enabled set, which is the default.\n\n"+
			"Exit codes:\n"+
			"  0  the instance answered, and no node is dead or suspect. Clustering being\n"+
			"     disabled is also 0: it is the default configuration, not a fault.\n"+
			"  1  the instance could not be reached, or a node is dead or suspect.\n"+
			"  2  the command line was wrong.\n\nOptions:\n")
		fs.PrintDefaults()
	}
	newClusterStatusFlagSet(fs, &f)

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if rest := fs.Args(); len(rest) > 0 {
		emit(stderr, "objectfs cluster status: takes no arguments, got %d: %s\n",
			len(rest), strings.Join(rest, " "))
		fs.Usage()

		return exitUsage
	}

	endpoint, err := resolveStatusEndpoint(&f)
	if err != nil {
		emit(stderr, "objectfs cluster status: %v\n", err)

		return exitUsage
	}

	status, err := fetchClusterStatus(context.Background(), endpoint)
	if err != nil {
		emit(stderr, "objectfs cluster status: %v\n", err)

		return exitFailure
	}

	if f.asJSON {
		// Re-encoded from the decoded struct rather than passed through, so that --json is documented by
		// distributed.ClusterStatus and cannot silently carry a field the human output has no counterpart
		// for. Indented because the consumer is as often a person reading it as it is jq.
		out, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			emit(stderr, "objectfs cluster status: cannot re-encode the status as JSON: %v\n", err)

			return exitFailure
		}

		emit(stdout, "%s\n", out)
	} else {
		writeClusterStatus(stdout, status)
	}

	return clusterStatusExitCode(status)
}

// resolveStatusEndpoint decides which base URL to query.
//
// Precedence is --endpoint, then the config file's monitoring.health_checks.addr, then the packaged
// default. The middle step is what keeps this usable on a host where the endpoint was moved: the address
// is already a setting, and requiring an operator to repeat it on every invocation is how a command comes
// to be wrapped in a shell alias that goes stale.
func resolveStatusEndpoint(f *clusterStatusFlags) (string, error) {
	if f.endpoint != "" {
		return normalizeEndpoint(f.endpoint)
	}

	addr := config.DefaultHealthAddr

	if f.configFile != "" {
		cfg := config.NewDefault()
		if err := cfg.LoadFromFile(f.configFile); err != nil {
			return "", fmt.Errorf("cannot load %s: %w", f.configFile, err)
		}

		if !cfg.Monitoring.HealthChecks.Enabled {
			return "", fmt.Errorf("%s sets monitoring.health_checks.enabled to false, so that instance "+
				"serves no health endpoint and there is nothing to query", f.configFile)
		}

		if cfg.Monitoring.HealthChecks.Addr != "" {
			addr = cfg.Monitoring.HealthChecks.Addr
		}
	}

	return normalizeEndpoint("http://" + addr)
}

// normalizeEndpoint turns what an operator typed into a base URL, or explains why it is not one.
//
// A bare host:port is accepted and given the http scheme, because that is the form
// monitoring.health_checks.addr takes and an operator copying it across is the expected case. A trailing
// slash is trimmed so that joining the path below cannot produce a double slash — which some mux
// implementations redirect and some 404, and either way is a confusing failure for a typo this can simply
// absorb.
func normalizeEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", errors.New("--endpoint is empty")
	}

	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("--endpoint %q is not a URL: %w", endpoint, err)
	}

	// Checked explicitly, because url.Parse accepts a great deal that is not addressable: "http://" parses
	// with an empty Host and would otherwise become a request to nowhere reported as a connection error,
	// which sends an operator looking at their mount instead of at their command line.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("--endpoint %q has scheme %q; only http and https can be queried",
			endpoint, parsed.Scheme)
	}

	if parsed.Host == "" {
		return "", fmt.Errorf("--endpoint %q names no host", endpoint)
	}

	return strings.TrimSuffix(parsed.String(), "/"), nil
}

// fetchClusterStatus reads and decodes the cluster status from the endpoint.
//
// Every error it returns says what to do about it, because the three failure modes look alike from a
// shell and have completely different causes:
//
//   - connection refused is nothing listening, which is a mount that is not running or a health endpoint
//     that is disabled or on another address. It must not read as a broken cluster, and this is the case
//     the message spends the most words on;
//   - 404 is an instance older than this endpoint;
//   - anything else is the endpoint answering something unexpected.
func fetchClusterStatus(ctx context.Context, endpoint string) (*distributed.ClusterStatus, error) {
	target := endpoint + health.ClusterStatusPath

	ctx, cancel := context.WithTimeout(ctx, clusterStatusTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot build a request for %s: %w", target, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Deliberately not wrapped with %w into something shorter. The transport error names the address
		// and the syscall — "connection refused" — and what an operator needs beside it is the list of
		// reasons that happens, since none of them is a cluster problem.
		return nil, fmt.Errorf("cannot reach an ObjectFS instance at %s: %w.\n"+
			"Nothing is listening there. Either no instance is running, or its health endpoint is\n"+
			"disabled (monitoring.health_checks.enabled), or it is bound to a different address\n"+
			"(monitoring.health_checks.addr) — pass --endpoint or --config to name it.\n"+
			"This does not mean the cluster is unhealthy: an unreachable endpoint says nothing\n"+
			"about the cluster at all", target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("the instance at %s has no %s endpoint. It is running a build from "+
			"before this endpoint existed; `objectfs cluster status` needs %s or newer on the instance",
			endpoint, health.ClusterStatusPath, version)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered HTTP %s", target, resp.Status)
	}

	// Bounded, because this is an HTTP body from a source the caller named on the command line and an
	// endpoint that answers an endless stream would otherwise be read until the process died. 8 MiB is
	// several thousand nodes' worth of report against a payload that is a few hundred bytes per node.
	const maxBody = 8 << 20

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("reading the response from %s: %w", target, err)
	}

	var status distributed.ClusterStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("the response from %s is not a cluster status: %w", target, err)
	}

	return &status, nil
}

// clusterStatusExitCode decides the exit code from the payload.
//
// Dead or suspect nodes, and nothing else. See runClusterStatus for why quorum — which the issue named —
// is not the condition: no leader is elected on a mount path, so a majority is not something this
// cluster has an opinion about, and a node being unreachable is the failure a gossip cluster actually
// reports.
func clusterStatusExitCode(status *distributed.ClusterStatus) int {
	if !status.Enabled {
		return exitOK
	}

	if status.Membership.Dead > 0 || status.Membership.Suspect > 0 {
		return exitFailure
	}

	return exitOK
}

// writeClusterStatus renders the status for a person.
//
// What it does not print is the point, and every omission is a field that exists in the source struct and
// is not populated at runtime — see [distributed.ClusterStatus], which records each one. Two are worth
// repeating here because the issue's mockup asked for them by name:
//
//   - "Role: Leader" / "Role: Follower" is printed only when consensus is running, which on a mount it
//     never is. Printing "Follower" otherwise would report a lost election that was never held, on every
//     node of a healthy cluster.
//   - "Top 5 keys by access" is absent because nothing measures per-key access counts anywhere reachable
//     from here. The announced-key count is printed instead, under its own name, rather than a
//     differently-defined list under the label the mockup used.
//
// A hit rate that has not been measured prints as "not measured" rather than as 0%: an operator reading
// "0%" cannot tell a cache that has served nothing from one that misses every read, and those are a
// just-started mount and an emergency respectively.
func writeClusterStatus(w io.Writer, status *distributed.ClusterStatus) {
	emit(w, "ObjectFS Cluster Status\n")
	emit(w, "=======================\n")

	if !status.Enabled {
		emit(w, "Clustering: disabled\n")

		if status.Reason != "" {
			emit(w, "\n%s\n", status.Reason)
		}

		return
	}

	emit(w, "Node:    %s (this node)\n", status.NodeID)

	if status.GossipAddr != "" {
		emit(w, "Gossip:  %s\n", status.GossipAddr)
	} else {
		// A started manager whose bind failed. Worth naming, because every peer's view of this node
		// depends on it and the mount is otherwise serving normally.
		emit(w, "Gossip:  not bound — this node cannot be reached by peers\n")
	}

	// Only when an election has actually happened. See the function comment.
	if status.Leadership != nil {
		role := "follower"
		if status.Leadership.IsSelf {
			role = "leader"
		}

		emit(w, "Role:    %s (leader: %s, elections: %d)\n",
			role, orNone(status.Leadership.Leader), status.Leadership.Election)
	} else {
		emit(w, "Role:    n/a — leader election is not running; coordination is compare-and-swap\n"+
			"         against S3, which needs no leader and no quorum\n")
	}

	m := status.Membership
	emit(w, "\nMembership: %d nodes (%d alive, %d suspect, %d dead)\n",
		m.Total, m.Alive, m.Suspect, m.Dead)

	writePeerLines(w, status)
	writeCacheSection(w, status)

	emit(w, "\nGossip:\n")
	emit(w, "  Messages:   %d sent, %d received\n",
		status.Gossip.MessagesSent, status.Gossip.MessagesReceived)
	emit(w, "  Bytes:      %s sent, %s received\n",
		utils.FormatBytes(status.Gossip.BytesSent), utils.FormatBytes(status.Gossip.BytesReceived))

	// Printed only when non-zero. Each of these is an anomaly, and a block of zeros every run trains an
	// operator to skip the section that matters when one of them is not zero.
	writeGossipAnomalies(w, status.Gossip)

	emit(w, "\nPeer cache claims held: %d keys\n", status.AnnouncedKeys)
}

// writePeerLines prints one line per node, self first.
func writePeerLines(w io.Writer, status *distributed.ClusterStatus) {
	if status.Self != nil {
		emit(w, "  %s\n", nodeLine(*status.Self, true))
	}

	for _, peer := range status.Peers {
		emit(w, "  %s\n", nodeLine(peer, false))
	}

	if len(status.Peers) == 0 {
		emit(w, "\nNo peers. This is a cluster of one, which is a working configuration:\n"+
			"nothing about ObjectFS coordination requires a second node.\n")
	}
}

// nodeLine formats one node's report.
func nodeLine(n distributed.NodeReport, self bool) string {
	var b strings.Builder

	name := n.ID
	if self {
		name += " (self)"
	}

	fmt.Fprintf(&b, "%-28s %-22s %-8s", name, orNone(n.Address), n.Status)

	if n.Cache != nil {
		fmt.Fprintf(&b, " cache=%s", utils.FormatBytes(n.Cache.Size))

		if n.Cache.Capacity != nil {
			fmt.Fprintf(&b, "/%s", utils.FormatBytes(*n.Cache.Capacity))
		}

		// Not "0%" when nothing has been asked of the cache. See writeClusterStatus.
		if n.Cache.HitRate != nil {
			fmt.Fprintf(&b, " hit=%s", formatRate(*n.Cache.HitRate))
		} else {
			fmt.Fprintf(&b, " hit=not measured")
		}
	} else {
		// Distinguished from an empty cache, which is what "cache=0 B" would say.
		fmt.Fprintf(&b, " cache=not reported")
	}

	if !n.LastSeen.IsZero() {
		fmt.Fprintf(&b, " last_seen=%s ago", time.Since(n.LastSeen).Round(time.Millisecond))
	}

	return b.String()
}

// writeCacheSection prints the cluster-wide cache aggregate.
func writeCacheSection(w io.Writer, status *distributed.ClusterStatus) {
	c := status.Cache

	emit(w, "\nCache (across %d alive nodes):\n", c.AliveNodes)
	emit(w, "  Size:       %s\n", utils.FormatBytes(c.TotalSize))

	// The count is printed with the sum, not instead of it: one node of three reporting capacity would
	// otherwise make the cluster look three times fuller than it is.
	switch {
	case c.NodesReportingCapacity == 0:
		emit(w, "  Capacity:   not reported by any node\n")
	case c.NodesReportingCapacity < c.AliveNodes:
		emit(w, "  Capacity:   %s (from %d of %d nodes; the rest do not report one)\n",
			utils.FormatBytes(c.TotalCapacity), c.NodesReportingCapacity, c.AliveNodes)
	default:
		emit(w, "  Capacity:   %s\n", utils.FormatBytes(c.TotalCapacity))
	}

	if c.HitRate == nil {
		emit(w, "  Hit rate:   not measured — no node has served a cached read yet\n")

		return
	}

	emit(w, "  Hit rate:   %s (mean over the %d node(s) that have served a read)\n",
		formatRate(*c.HitRate), c.NodesMeasured)
}

// writeGossipAnomalies prints the counters that are only interesting when non-zero.
func writeGossipAnomalies(w io.Writer, g distributed.GossipCounters) {
	type counter struct {
		label string
		value int64
		note  string
	}

	counters := []counter{
		{"unauthenticated", g.MessagesUnauthenticated,
			"a peer has the wrong cluster secret, or something that is not a member is sending to the port"},
		{"replayed", g.MessagesReplayed,
			"captured datagrams are being resent, or a node's clock is outside the freshness window"},
		{"wrong version", g.MessagesWrongVersion, "version skew, as during a rolling upgrade"},
		{"truncated", g.MessagesTruncated,
			"a datagram arrived clipped; this reports itself as an authentication failure (#277)"},
		{"oversize", g.MessagesOversize, "this node refused to send a datagram over max_gossip_packet"},
		{"suspicion refutations", g.SuspicionRefutations,
			"this node was falsely accused; suspects packet loss or too short a heartbeat interval"},
		{"suspicion events", g.SuspicionEvents, ""},
		{"death events", g.DeathEvents, ""},
		{"network errors", g.NetworkErrors, ""},
	}

	// Sorted so the same set of anomalies always prints in the same order regardless of the slice above
	// being reordered later, which keeps output diffable across versions.
	sort.SliceStable(counters, func(i, j int) bool { return counters[i].label < counters[j].label })

	headerPrinted := false

	for _, c := range counters {
		if c.value == 0 {
			continue
		}

		if !headerPrinted {
			emit(w, "  Anomalies:\n")

			headerPrinted = true
		}

		if c.note == "" {
			emit(w, "    %s: %d\n", c.label, c.value)
		} else {
			emit(w, "    %s: %d — %s\n", c.label, c.value, c.note)
		}
	}
}

// formatRate renders a 0..1 fraction as a percentage.
func formatRate(rate float64) string {
	return fmt.Sprintf("%.1f%%", rate*100)
}

// orNone renders an empty string as a marker rather than as nothing, so a missing field is visible in a
// column-aligned report instead of looking like whitespace.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}

	return s
}
