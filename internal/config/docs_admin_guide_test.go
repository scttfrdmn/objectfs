package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Gates on docs/admin-guide/operations.md and troubleshooting.md, the two pages #148 asked for.
//
// Same reasoning as docs_distributed_test.go, applied to a different kind of claim. These pages are
// mostly *commands* and *port numbers*, and the existing gates reach neither: docs_symbols_test.go
// checks CLI invocations it can recognize, but a curl against a metrics endpoint is prose to it, and a
// port written in a firewall example is prose to everything.
//
// The specific risk here is a decision tree that sends an operator to a command that does not exist, or
// to the wrong port. Both are worse than no documentation: a troubleshooting page is read by someone who
// has already exhausted what they know, so a wrong branch costs them the time it takes to rule it out
// and some of their trust in the rest of the tree.

// docsAdminGuide reads one page from docs/admin-guide.
func docsAdminGuide(t *testing.T, name string) string {
	t.Helper()

	//nolint:gosec // a path built from the module root this test located itself
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "admin-guide", name))
	if err != nil {
		t.Fatalf("read docs/admin-guide/%s: %v", name, err)
	}

	return string(body)
}

// TestAdminGuidePortsMatchTheDefaults pins the ports the troubleshooting branches send people to.
//
// A wrong port here is the most expensive kind of documentation error this repository can ship, because
// the gossip branch exists for a failure with *no error message at all*. An operator who opens the wrong
// UDP port confirms "the firewall is fine" and moves on, and the cluster of one they came to diagnose is
// still there.
//
// #148 and #152 both specified 7946 — memberlist's default, and a number that appears in this
// repository's own test fixtures. It is the default nowhere in the code.
func TestAdminGuidePortsMatchTheDefaults(t *testing.T) {
	t.Parallel()

	def := NewDefault()

	_, gossipPort, ok := strings.Cut(def.Cluster.ListenAddr, ":")
	if !ok {
		t.Fatalf("NewDefault's cluster.listen_addr %q is not host:port", def.Cluster.ListenAddr)
	}

	metricsAddr := def.Monitoring.Metrics.Addr
	healthAddr := def.Monitoring.HealthChecks.Addr

	for _, page := range []string{"operations.md", "troubleshooting.md"} {
		doc := docsAdminGuide(t, page)

		// Every URL is checked whole — scheme, address and path together. Checking the address anywhere on
		// the page was the first version and it missed a mutation that pointed the metrics curl at
		// Prometheus's own 9090: the page still said "127.0.0.1:8080" further down, in the paragraph about
		// TCP and UDP sharing the number, so a substring test was satisfied by a sentence that had nothing
		// to do with the command. Only the pages that use an endpoint are checked for it.
		for _, endpoint := range []struct{ path, addr string }{
			{"/metrics", metricsAddr},
			{"/health", healthAddr},
		} {
			if !strings.Contains(doc, endpoint.path) {
				continue
			}

			want := "http://" + endpoint.addr + endpoint.path
			if !strings.Contains(doc, want) {
				t.Errorf("%s shows a %s command and none of them is %s. Someone following it curls the "+
					"wrong port and concludes the endpoint is not published", page, endpoint.path, want)
			}

			// And every one of them, so a wrong URL cannot hide behind a correct sibling elsewhere on the
			// page. An operator reads the command next to their symptom, not the whole file.
			for field := range strings.FieldsSeq(doc) {
				url := strings.Trim(field, "`\"'()[],")
				if strings.HasPrefix(url, "http://") && strings.HasSuffix(url, endpoint.path) &&
					url != want {
					t.Errorf("%s contains the URL %s, but the default %s endpoint is %s", page, url,
						endpoint.path, want)
				}
			}
		}
	}

	// The gossip port, in the branch that sends someone to a firewall.
	tsh := docsAdminGuide(t, "troubleshooting.md")
	if !strings.Contains(tsh, "**"+gossipPort+"** by default") {
		t.Errorf("troubleshooting.md does not name %s as the gossip port. The default listen_addr is "+
			"%q, and this is the branch for a failure that has no error message — a wrong port here is "+
			"an operator ruling out the actual cause", gossipPort, def.Cluster.ListenAddr)
	}

	if strings.Contains(tsh, "UDP 7946") || strings.Contains(tsh, "port 7946 is open") {
		t.Error("troubleshooting.md tells an operator to check 7946, which is memberlist's default and " +
			"not this project's. #148's issue body says this; the code does not")
	}
}

// TestAdminGuideCommandsExist checks every objectfs invocation against the binary's own surface.
//
// A decision tree is a list of commands, and a command that does not exist ends the branch in a usage
// error rather than an answer.
func TestAdminGuideCommandsExist(t *testing.T) {
	t.Parallel()

	// The command set from cmd/objectfs's dispatch. Deliberately written out rather than derived: the
	// point is to notice when a page names something outside this list, including a plausible command
	// this project does not have, like `objectfs doctor` or `objectfs status`.
	known := map[string]bool{
		"mount":   true,
		"unmount": true,
		"cluster": true,
		"version": true,
		"help":    true,
	}

	for _, page := range []string{"operations.md", "troubleshooting.md", "distributed.md"} {
		doc := docsAdminGuide(t, page)

		for line := range strings.SplitSeq(doc, "\n") {
			rest, found := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line),
				"$ ")), "objectfs ")
			if !found {
				continue
			}

			sub, _, _ := strings.Cut(rest, " ")
			sub = strings.TrimSuffix(sub, "`")

			if sub == "" {
				continue
			}

			if !known[sub] {
				t.Errorf("%s names `objectfs %s`, which is not a command this binary has. The commands "+
					"are mount, unmount, cluster, version and help — a branch ending at a usage error is "+
					"worse than one that says the question cannot be answered from outside", page, sub)
			}
		}
	}
}

// TestAdminGuideMetricNamesArePublished checks the metric names against the SDK fixture.
//
// Every metric the troubleshooting tree tells someone to grep for has to be one a mount actually
// publishes. sdks/testdata/metrics-scrape.txt is a real scrape of a running instance, regenerated from
// the live registry by TestSDKFixtureMatchesTheLiveScrape — so it is the closest thing to the endpoint
// itself that a unit test can read, and a name absent from it is a grep that silently returns nothing.
//
// Silence is the failure mode worth guarding: `grep objectfs_cache_hit_rate` on an endpoint that
// publishes `objectfs_cache_requests_total` prints nothing at all, and nothing looks exactly like a
// working command on a healthy system.
//
// A name the page names in order to say it does *not* exist is checked in the other direction, which is
// the first thing this test found. troubleshooting.md says "There is deliberately no
// `objectfs_cache_hit_rate` gauge", and the naive version of this test read that as a grep target.
// Exempting it would have been the wrong repair: the claim is falsifiable too, and it becomes false the
// moment somebody adds the gauge — at which point the page is telling an operator to compute by hand
// something the endpoint now publishes. So a negated name must be absent from the fixture.
func TestAdminGuideMetricNamesArePublished(t *testing.T) {
	t.Parallel()

	//nolint:gosec // a path built from the module root this test located itself
	scrape, err := os.ReadFile(filepath.Join(repoRoot(t), "sdks", "testdata", "metrics-scrape.txt"))
	if err != nil {
		t.Fatalf("read the SDK metrics fixture: %v", err)
	}

	published := string(scrape)

	// Names the pages assert are *absent*, each with the sentence that asserts it. Checked in the
	// opposite direction: present in the fixture means the page is now wrong.
	negated := map[string]string{
		"objectfs_cache_hit_rate": "There is deliberately no `objectfs_cache_hit_rate` gauge",
	}

	for _, page := range []string{"operations.md", "troubleshooting.md"} {
		doc := docsAdminGuide(t, page)

		for _, field := range strings.FieldsFunc(doc, func(r rune) bool {
			return r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9')
		}) {
			if !strings.HasPrefix(field, "objectfs_") {
				continue
			}

			if claim, isNegated := negated[field]; isNegated {
				// The page has to still be making the negative claim — otherwise this exemption is
				// silently covering a real grep target that happens to share the name.
				if !strings.Contains(doc, claim) {
					t.Errorf("%s names %q but no longer carries the claim %q that makes it a deliberate "+
						"absence. Either restore the sentence or stop naming the metric", page, field, claim)
					continue
				}

				if strings.Contains(published, field) {
					t.Errorf("%s says there is deliberately no %s, and the metrics fixture now publishes "+
						"one. The page is telling an operator to compute by hand a number the endpoint "+
						"gives them", page, field)
				}

				continue
			}

			// A grep pattern is checked as a prefix, because the page greps for families
			// (objectfs_cache_requests_total matches two series with different labels).
			if !strings.Contains(published, field) {
				t.Errorf("%s tells an operator to look for the metric %q, which does not appear in "+
					"sdks/testdata/metrics-scrape.txt — a real scrape of a running instance. A grep for a "+
					"metric that is not published returns nothing, which is indistinguishable from a "+
					"healthy system", page, field)
			}
		}
	}
}

// TestAdminGuideExitCodesMatchTheCommand pins the exit codes the operations page tells scripts to use.
//
// These are the page's only machine-readable claim: an alert or a health check will branch on them, and
// the one that surprises people is that clustering being *disabled* is a 0. A page that documented it as
// nonzero would have every non-clustered mount alerting.
func TestAdminGuideExitCodesMatchTheCommand(t *testing.T) {
	t.Parallel()

	ops := docsAdminGuide(t, "operations.md")

	// The values from cmd/objectfs: exitOK 0, exitFailure 1, exitUsage 2.
	for _, want := range []string{
		"**0** when no node is\ndead or suspect",
		"**1** when\nunreachable or a node is dead or suspect",
		"**2** for a bad command line",
	} {
		if !strings.Contains(ops, want) {
			t.Errorf("operations.md does not document the exit code as %q. clusterStatusExitCode returns "+
				"0 for a healthy cluster and for clustering being disabled, 1 for dead or suspect or "+
				"unreachable, and 2 for a usage error, and a script branching on these is the stated use",
				want)
		}
	}

	if !strings.Contains(ops, "disabled") {
		t.Error("operations.md documents the exit codes without saying that clustering being disabled " +
			"also exits 0. That is the surprising case: it is the default configuration rather than a " +
			"fault, and a page that omitted it would have every single-node mount alerting")
	}
}

// TestAdminGuideDoesNotPromiseALeader is the counterpart to the absent-keys check in
// docs_distributed_test.go.
//
// #148's body specifies "upgrade nodes one at a time; leader last", "leader failure recovery (Raft
// election)", "verify quorum", and a node that "degrades to ConsistencyEventual". None of the four is a
// thing this code does: nothing elects a leader on a mount path, there is no quorum, and that type was
// removed in 0.12.0. An upgrade procedure that says "leader last" sends an operator looking for a
// leader that `objectfs cluster status` reports as `n/a`, which reads like a broken cluster.
func TestAdminGuideDoesNotPromiseALeader(t *testing.T) {
	t.Parallel()

	for _, page := range []string{"operations.md", "troubleshooting.md"} {
		doc := docsAdminGuide(t, page)

		for _, phrase := range []string{
			"leader last",
			"ConsistencyEventual",
			"verify quorum",
		} {
			if strings.Contains(doc, phrase) {
				t.Errorf("%s contains %q. Nothing elects a leader on a mount path, there is no quorum "+
					"condition anywhere, and the consistency levels were removed in 0.12.0 — each of "+
					"these sends an operator to look for something that is not there", page, phrase)
			}
		}
	}
}
