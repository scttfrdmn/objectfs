package config

// The gate #199 asks for: the set of packages scripts/coverage-gate.sh reports as unfloored has to
// be the same set .coverage-floors' header explains, in both directions.
//
// #199 filed this as a backlog item — three packages with no floor and no explanation — and the
// mechanism is what makes it worth a test rather than one round of editing. The gate already *names*
// unfloored packages in every run, so nothing about the situation was hidden; what was missing is the
// only thing a run cannot supply, which is whether an absence was decided or overlooked. That
// distinction lives in a hand-written comment block, and a hand-written description of state held
// somewhere else is exactly the shape labels_test.go's comment describes: it drifts, and nothing
// compares the two.
//
// It had already drifted three ways by the time #199 was read against the tree:
//
//   - the header said "four packages", listed five, and the gate reported six. internal/cache/cachetest
//     was added by #178 and never explained;
//   - two of the three the issue names as unexplained — pkg/types and tests — are not reported by the
//     gate at all and cannot be, because they contain no statements (see the header). A floor for
//     either *fails* the gate through its stale-floor arm rather than weakening it, so the issue's
//     "floor it, or document it" choice has only one available branch for them;
//   - the third, pkg/optimization, no longer exists.
//
// # Why this reads the gate's own output rather than reimplementing it
//
// The two sets have to be compared as the gate computes them, not as a second implementation guesses
// them. coverage-gate.sh derives its measured set from a coverage profile — statement lines, module
// prefix stripped — and that derivation has its own recorded failure: the module path was once a
// literal inside its awk program, a rename stopped it matching anything, and every package reported
// "no floor set" while the run failed naming nothing. A test that re-derived the set from `go list`
// would have been green through that, because `go list` does not care what go.mod said last week.
//
// So this runs the script. It needs a coverage profile to do that, and generating a real one takes
// twenty minutes, so the profile here is synthesized from `go list` + `go/parser`: one statement line
// per package that has at least one statement. The counts are fiction and it does not matter — the
// unlisted set is the difference of two key sets, and a package's *presence* in the profile is all
// that decides its membership.
//
// The parse deciding that presence is the part that has to be right, and it was checked against a real
// profile rather than reasoned about. Against CI's own coverage.out from the last green run on main,
// the gate names the same six packages under either profile. Two failures found that way rather than
// by argument: CgoFiles are a separate `go list` field from GoFiles, so sdks/c parsed as empty until it
// was added, and `go list`'s trailing empty fields collapse under a plain separator, which made `tests`
// — a package of only _test.go files, and one of the three the header has to classify — unparseable.
//
// The gate's report and this file's header can also disagree for a reason that is neither's fault: two
// copies of the `flatted` npm package vendor a Go file, so on a machine that has run `npm ci` the gate
// names eight packages rather than six. Those two are filtered here rather than floored or explained,
// because they are not this repository's code. CI's coverage job installs no npm dependencies, so it
// sees six either way.

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// moduleFromGoMod reads the module path out of go.mod.
//
// docs_symbols_test.go has the same path as a const, and this reads the file instead for the reason
// coverage-gate.sh does: the gate's measured set is keyed on stripping this prefix off profile paths,
// and a rename that left the prefix behind is the recorded way this machinery breaks silently. A test
// comparing against a hardcoded copy of the old path would have agreed with itself throughout.
func moduleFromGoMod(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1]
		}
	}

	t.Fatal("no module line in go.mod")

	return ""
}

// packagesWithStatements returns the repo-relative import paths of every package in this module, and
// of the subset that would produce at least one line in a coverage profile.
//
// "Has a statement" is the criterion for the second set because that is what `go test -coverprofile`
// emits a line for. A package of pure declarations produces none and `go test` reports it as
// `coverage: [no statements]` rather than 0.0%, so it is invisible to the gate — which is correct, and
// is why the header documents those separately from the ones the gate reports.
//
// Both sets are needed rather than just the second. A stale `# no-floor:` entry has two causes — the
// package is floored now, or it is gone — and telling them apart needs "does this package exist",
// which is not the same question as "does it have statements". Deciding it from the statement set
// alone reported pkg/types as a package that does not exist.
//
// CgoFiles are parsed alongside GoFiles. sdks/c is the whole reason: its only non-test file is
// main.go with an `import "C"`, so it lands in CgoFiles and a GoFiles-only walk would have called the
// package empty and expected the gate not to report it. The gate does report it, at 11.0%.
func packagesWithStatements(t *testing.T) (all, withStatements map[string]bool) {
	t.Helper()

	module := moduleFromGoMod(t)

	// Each field is terminated rather than separated, so a package whose GoFiles and CgoFiles are both
	// empty still produces four fields. `tests` is exactly that — five _test.go files and no non-test
	// Go files — and with a plain separator the trailing empties vanished and the parse rejected the
	// line, which is the one package this test most needs to classify.
	cmd := exec.CommandContext(t.Context(), "go", "list", "-f",
		`{{.ImportPath}}|{{.Dir}}|{{join .GoFiles ","}}|{{join .CgoFiles ","}}|`, "./...")
	cmd.Dir = repoRoot(t)

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	all, withStatements = map[string]bool{}, map[string]bool{}

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 5 {
			t.Fatalf("go list produced %d fields, want 5: %q", len(fields), line)
		}

		importPath, dir := fields[0], fields[1]

		// node_modules is skipped for the same reason .gitignore lists it: two copies of the
		// `flatted` npm package vendor a Go file, so `go list ./...` reports them as packages of
		// this module and the gate names them as unfloored on any machine that has run `npm ci`.
		// They are not this repository's code and no floor should be set for them. CI's coverage job
		// installs no npm dependencies, so its profile does not contain them — which is why the
		// gate's output there is six packages and locally it is eight.
		if strings.Contains(importPath, "/node_modules/") {
			continue
		}

		var files []string

		for _, group := range []string{fields[2], fields[3]} {
			for name := range strings.SplitSeq(group, ",") {
				if name != "" {
					files = append(files, name)
				}
			}
		}

		count := 0

		for _, name := range files {
			fset := token.NewFileSet()

			parsed, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
			}

			ast.Inspect(parsed, func(node ast.Node) bool {
				if _, ok := node.(ast.Stmt); ok {
					count++
				}

				return true
			})
		}

		rel := strings.TrimPrefix(strings.TrimPrefix(importPath, module), "/")
		if rel == "" {
			rel = "."
		}

		all[rel] = true

		if count > 0 {
			withStatements[rel] = true
		}
	}

	if len(withStatements) == 0 {
		t.Fatal("found no packages with statements — go list or the parse is broken, and an empty " +
			"set agrees with any header")
	}

	return all, withStatements
}

// syntheticProfile writes a coverage profile naming every package with statements, one line each.
//
// The line format is the one coverage-gate.sh parses: <import path>/<file>:<from>,<to> <n> <count>.
// The count is 1 throughout, so every package measures 100% — deliberately, because a synthetic
// number that could fail a real floor would make this test a second, worse copy of the coverage job.
// What is under test here is set membership.
func syntheticProfile(t *testing.T, pkgs map[string]bool) string {
	t.Helper()

	module := moduleFromGoMod(t)

	var b strings.Builder

	b.WriteString("mode: atomic\n")

	for _, pkg := range sortedKeys(pkgs) {
		path := module
		if pkg != "." {
			path = module + "/" + pkg
		}

		fmt.Fprintf(&b, "%s/synthetic.go:1.1,2.2 1 1\n", path)
	}

	path := filepath.Join(t.TempDir(), "coverage.out")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write synthetic profile: %v", err)
	}

	return path
}

// gateUnlistedSet runs coverage-gate.sh against a profile and returns the packages it names under
// "no floor set".
func gateUnlistedSet(t *testing.T, profile string) map[string]bool {
	t.Helper()

	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "coverage-gate.sh")

	if _, err := os.Stat(script); err != nil {
		t.Fatalf("stat %s: %v", script, err)
	}

	//nolint:gosec // both arguments are paths this test built from the module root it located itself
	cmd := exec.CommandContext(t.Context(), "bash", script, profile,
		filepath.Join(root, ".coverage-floors"))
	cmd.Dir = root

	out, err := cmd.CombinedOutput()
	if err != nil {
		// Not fatal on its own: the gate exits 1 when a package is below its floor, and under a
		// synthetic profile every package is at 100%, so the only way to get here is a stale floor —
		// a floor for a package the profile does not mention. That is worth reporting in full,
		// because it is the same signal as "this package has no statements and cannot be floored".
		t.Fatalf("coverage-gate.sh failed against the synthetic profile:\n%s", out)
	}

	unlisted := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))

	inBlock := false

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "no floor set") {
			inBlock = true

			continue
		}

		if !inBlock {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(line, "  ") {
			break
		}

		unlisted[fields[0]] = true
	}

	return unlisted
}

// headerMarker matches the two machine-readable markers .coverage-floors' header uses to name a
// package it explains the absence of a floor for.
//
// Markers rather than an inferred shape, and the first attempt here is why. It matched any indented
// path-looking token in the leading comment block, which pulled in the six packages the header's
// opening paragraphs name for unrelated reasons — internal/fuse and pkg/errors in the "furthest from
// 80%" table, internal/cache and internal/adapter in the -coverpkg measurements — and reported all of
// them as stale entries. A prose comment does not have a parseable structure, and a test that
// pretends otherwise fails on prose changes that are not drift. Two explicit prefixes cost one token
// per entry and make the set the file declares unambiguous.
//
//	# no-floor: <pkg>       the gate reports this package as unfloored, and that is intended
//	# no-statements: <pkg>  this package has no statements, so the gate cannot report it at all
var headerMarker = regexp.MustCompile(`(?m)^# (no-floor|no-statements): (\S+)\s*$`)

// headerExplainedSet returns the two marked sets from .coverage-floors' comment header.
func headerExplainedSet(t *testing.T) (noFloor, noStatements map[string]bool) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".coverage-floors"))
	if err != nil {
		t.Fatalf("read .coverage-floors: %v", err)
	}

	// Only the leading comment block counts. Per-floor notes further down the file also name their
	// package, and those are explanations of a floor rather than of an absence — counting them would
	// make every floored package look explained-as-absent and the test would assert nothing.
	var header strings.Builder

	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}

		header.WriteString(line + "\n")
	}

	noFloor, noStatements = map[string]bool{}, map[string]bool{}

	for _, m := range headerMarker.FindAllStringSubmatch(header.String(), -1) {
		if m[1] == "no-floor" {
			noFloor[m[2]] = true
		} else {
			noStatements[m[2]] = true
		}
	}

	if len(noFloor) == 0 || len(noStatements) == 0 {
		t.Fatalf("the .coverage-floors header declares %d no-floor and %d no-statements packages; "+
			"both sets should be non-empty. Either the markers were dropped in an edit, or the "+
			"block moved below the first floor line — an empty set agrees with any gate output",
			len(noFloor), len(noStatements))
	}

	return noFloor, noStatements
}

// TestUnfloorablePackagesAreExplained is #199's invariant: the set the gate reports as unlisted and
// the set the header explains are the same set.
//
// Both directions fail. A package the gate reports and the header does not explain is the drift #199
// filed — an absence that reads as an oversight, so the next person either adds a floor without
// knowing whether the package is meant to be tested or assumes someone decided already. A package
// the header explains and the gate no longer reports is the other half and the cheaper one to miss:
// pkg/optimization was explained here after it had been deleted, and a stale explanation is worse
// than none because it answers a question nobody should still be asking.
func TestUnfloorablePackagesAreExplained(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH; skipping the coverage-floors header gate")
	}

	all, pkgs := packagesWithStatements(t)
	reported := gateUnlistedSet(t, syntheticProfile(t, pkgs))
	noFloor, noStatements := headerExplainedSet(t)

	for _, pkg := range sortedKeys(reported) {
		if !noFloor[pkg] {
			t.Errorf("the coverage gate reports %s as having no floor and the .coverage-floors "+
				"header does not say why. Either give it a floor, or add a `# no-floor: %s` entry "+
				"to the header block with the reason an absence is intended — an unexplained "+
				"absence reads as an oversight", pkg, pkg)
		}
	}

	for _, pkg := range sortedKeys(noFloor) {
		if reported[pkg] {
			continue
		}

		// A package marked no-floor that the gate does not report is stale, and there are three
		// reasons it can be. Each wants a different fix, so each gets its own message.
		switch {
		case !all[pkg]:
			t.Errorf("the .coverage-floors header has a `# no-floor: %s` entry and no such package "+
				"exists in this module. Delete it — pkg/optimization sat in this block for two "+
				"releases after being deleted", pkg)
		case !pkgs[pkg]:
			t.Errorf("the .coverage-floors header has a `# no-floor: %s` entry, but that package "+
				"has no statements, so the gate cannot report it as unfloored and a floor for it "+
				"would fail the gate outright. It wants a `# no-statements:` entry instead", pkg)
		default:
			t.Errorf("the .coverage-floors header has a `# no-floor: %s` entry, but the gate does "+
				"not report that package as unfloored — it has a floor now, so the entry is stale "+
				"and should move to a per-floor note", pkg)
		}
	}

	// A package cannot be in both sets: no-statements means the gate never reports it, no-floor means
	// the gate does report it. Marking one both ways is how an entry would be made unfalsifiable.
	for _, pkg := range sortedKeys(noStatements) {
		if noFloor[pkg] {
			t.Errorf("%s is marked both `# no-floor:` and `# no-statements:` in .coverage-floors. "+
				"Those are mutually exclusive — the first says the gate reports it, the second says "+
				"the gate cannot", pkg)
		}
	}
}

// TestNoStatementPackagesStillHaveNone holds the header's `# no-statements:` entries to what they
// claim.
//
// Those entries are the reason TestUnfloorablePackagesAreExplained does not fail for pkg/types and
// tests: the gate does not report them, and the header says that is because they have no statements
// rather than because they were forgotten. Without this test that explanation is unfalsifiable — if
// pkg/types grew a function body it would start producing profile lines and the gate would start
// reporting it, and the header entry would be simply false, with nothing to say so.
//
// test/benchmarks is checked by stat rather than through go list, because `go list ./...` does not
// report it at all: its one file is behind //go:build benchmark. That is also the only reason it is in
// the header — a reader counting Go directories against the gate's report would otherwise find it
// missing from both and have no way to tell which.
func TestNoStatementPackagesStillHaveNone(t *testing.T) {
	t.Parallel()

	_, pkgs := packagesWithStatements(t)
	_, noStatements := headerExplainedSet(t)

	for _, pkg := range sortedKeys(noStatements) {
		if pkgs[pkg] {
			t.Errorf("%s is marked `# no-statements:` in .coverage-floors, so as correctly invisible "+
				"to the coverage gate rather than missing from it. It has statements now: it will "+
				"start appearing in coverage profiles, and it needs a floor and a per-floor note "+
				"rather than that marker", pkg)
		}
	}

	if !noStatements["test/benchmarks"] {
		t.Error("the .coverage-floors header no longer marks test/benchmarks as a no-statements " +
			"package; the stat check below is about that entry and has nothing to hold")
	}

	// The tagged package has to be reached without go list, since go list ./... does not see it.
	tagged := filepath.Join(repoRoot(t), "test", "benchmarks")
	if _, err := os.Stat(tagged); err != nil {
		t.Errorf("the .coverage-floors header names test/benchmarks as a build-tagged package with "+
			"no untagged statements, and the directory is gone: %v", err)
	}
}
