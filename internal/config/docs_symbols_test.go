package config

// The gate #182 asks for: every Go symbol and every objectfs CLI invocation in a fenced code block
// has to exist.
//
// #182 cataloged nineteen files claiming things the code does not do, and its most mechanical
// pattern was documentation calling APIs and commands that are not there —
// metrics.NewDetailedPerformanceMetricsWithOptions, objectfs list-mounts, objectfs status. Each was
// correct-looking prose that no build, vet, lint, or test could contradict, because a fenced block
// is a string as far as the toolchain is concerned.
//
// Correcting the nineteen files resets the clock. This makes the next one fail at authoring time,
// which is the only point at which the author still remembers what they meant.
//
// # What is checked, and why not more
//
// Two things, both decidable:
//
//   - a `pkg.Symbol` reference in a ```go block whose *same block* imports that objectfs package
//     under that name — the symbol must be exported by that package;
//   - an `objectfs` command line in a shell block — its flags must be flags the binary parses, and
//     it must not use a subcommand, because there are none.
//
// What is deliberately not checked is compilation. A doc snippet is an excerpt: it references
// variables declared in prose above it, elides error handling, and shows method signatures without
// receivers. Demanding it compile would make every honest excerpt fail and the gate would be
// switched off within a week. Symbol existence is the largest subset of "does this work" that an
// excerpt can be held to.
//
// # The admission rule is the load-bearing part
//
// docs_test.go's comment on nestedSectionNames records what happens when the rule deciding *what to
// check* is wrong: five blocks of invented config keys sailed through the test written to catch
// them, because the admission test skipped them. So the choice here was measured rather than
// guessed. Three scopings were run against the tree:
//
//   - Every `lowercase.Uppercase` in every go block: 93 hits, of which 3 real. s3.Client is the AWS
//     SDK, cache.Get is a method on a local variable, errors.Is is the standard library. Unusable.
//   - File-scoped imports — any objectfs import anywhere in the file names the package for every
//     block in it: 5 hits, 3 real. The two false ones are docs/s3-acceleration.md's method-signature
//     listing, where `*s3.Client` is the AWS SDK's type in a file that elsewhere imports
//     internal/storage/s3 under the same name. A collision the test cannot resolve.
//   - Block-scoped: 3 hits, 3 real, 0 false. Chosen.
//
// The cost of block scoping is stated rather than hidden: a continuation block that uses a package
// imported by the block above it is not checked. docs/features/read-ahead.md has one — a second
// snippet calling cache.NewPredictiveCache with no import line. That call happens to be correct, and
// if it were not, this test would not say so. Under-reaching in a way that produces no false
// positives is the right trade for a gate whose whole value is that people leave it turned on, but
// it is a real gap and not a solved problem.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// modulePath is this module, as go.mod declares it. Only imports under it are checked: a snippet
// importing the AWS SDK is describing someone else's API, which this repository cannot hold to
// anything.
const modulePath = "github.com/scttfrdmn/objectfs"

// cliFlags is every flag cmd/objectfs/main.go passes to the flag package.
//
// Hand-maintained against main.go rather than extracted, because cmd/objectfs is package main and
// its flag variables are unexported — there is nothing to import, which is the same reason
// version_test.go reads the version with a regexp. TestCLIFlagsMatchTheBinary below keeps this list
// honest, so the duplication cannot drift silently.
//
// One flat set across every subcommand, which over-accepts: `objectfs unmount --cache-size 8GB` is
// not a runnable command and this would not say so. Per-subcommand scoping is what the binary
// actually has, since each subcommand builds its own FlagSet — but the failure it would catch is a
// documented flag on the wrong subcommand, and the failure this catches is a documented flag that
// exists nowhere. The second is the one that shipped four times.
var cliFlags = map[string]bool{
	"version":         true,
	"help":            true,
	"h":               true,
	"debug":           true,
	"config":          true,
	"log-level":       true,
	"dry-run":         true,
	"cache-size":      true,
	"max-concurrency": true,
	"foreground":      true,
	"mount-point":     true,
}

// cliFlagsTakingAValue are the non-boolean flags, whose following argument is a value and not a
// positional. Without this, `--cache-size 8GB` reads 8GB as a subcommand.
var cliFlagsTakingAValue = map[string]bool{
	"config":          true,
	"log-level":       true,
	"cache-size":      true,
	"max-concurrency": true,
	"mount-point":     true,
}

// subcommands is every first word cmd/objectfs/main.go dispatches on.
//
// Hand-maintained for the same reason as cliFlags — package main exports nothing — and checked
// against the binary by TestSubcommandsMatchTheBinary.
var subcommands = map[string]bool{
	"mount":   true,
	"unmount": true,
	"umount":  true,
	"version": true,
	"help":    true,
}

// TestSubcommandsMatchTheBinary keeps the subcommands list from drifting from run()'s switch.
//
// Before #134 the binary had none, and this file asserted that flatly: any bare first word in
// documentation was an error. The four commands then added make that assertion wrong for `objectfs
// mount` and still right for `objectfs status`, which is exactly the case a hardcoded list gets
// wrong six months later. So the list is compared against the case arms in main.go.
func TestSubcommandsMatchTheBinary(t *testing.T) {
	t.Parallel()

	//nolint:gosec // a path built from the module root this test located itself
	mainGo, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "objectfs", "main.go"))
	if err != nil {
		t.Fatalf("reading cmd/objectfs/main.go, which dispatches the subcommands: %v", err)
	}

	dispatched := dispatchedSubcommands(t, string(mainGo))

	for name := range dispatched {
		if !subcommands[name] {
			t.Errorf("the binary dispatches %q and subcommands does not list it, so documentation "+
				"using that command would be reported as broken when it is correct", name)
		}
	}

	for name := range subcommands {
		if !dispatched[name] {
			t.Errorf("subcommands lists %q and the binary no longer dispatches it, so this gate would "+
				"approve a command that exits 2 — and documentation may still be using it", name)
		}
	}
}

// dispatchedSubcommands returns the bare words run's dispatch switch matches on args[0].
//
// Parsed with go/parser rather than grepped for `case "x":`, because a regexp over the file would
// also collect the case arms of every other switch in it — the flag-parsing switch on len(fs.Args())
// has integer cases and would be skipped, but the next string switch anyone adds would not be. This
// finds the switch whose tag is `args[0]` and reads only its arms.
func dispatchedSubcommands(t *testing.T, src string) map[string]bool {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "main.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing cmd/objectfs/main.go: %v", err)
	}

	found := make(map[string]bool)

	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || !isArgsZero(sw.Tag) {
			return true
		}

		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}

			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}

				// A flag spelling of a subcommand — "--version", "-h" — is matched here too, and is a
				// flag for documentation's purposes rather than a command. cliFlags covers those.
				if name := strings.Trim(lit.Value, `"`); !strings.HasPrefix(name, "-") {
					found[name] = true
				}
			}
		}

		return true
	})

	if len(found) == 0 {
		t.Fatal("found no switch on args[0] in cmd/objectfs/main.go. If dispatch moved or changed " +
			"shape, point this test at it — without the comparison, subcommands above is an " +
			"unverified copy, which is the defect this test exists to prevent")
	}

	return found
}

// isArgsZero reports whether an expression is `args[0]`, the tag of run's dispatch switch.
func isArgsZero(expr ast.Expr) bool {
	index, ok := expr.(*ast.IndexExpr)
	if !ok {
		return false
	}

	ident, ok := index.X.(*ast.Ident)
	if !ok || ident.Name != "args" {
		return false
	}

	lit, ok := index.Index.(*ast.BasicLit)

	return ok && lit.Kind == token.INT && lit.Value == "0"
}

// TestDocumentedGoSymbolsExist checks every objectfs symbol named in a ```go block.
//
// A failure means documentation is instructing a reader to call something that is not there. The
// three this found when written:
//
//   - docs/features/read-ahead.md's cache.GetPredictiveCache(), which does not exist under any name.
//     The PredictiveCache a mount builds is wrapped inside MultiLevelCache.initializeLevels and
//     there is no exported accessor for it, so the documented way to reach those statistics is not
//     merely misspelled — it is unreachable.
//   - docs-platform/playground/index.md's config.Config, which is config.Configuration.
//   - the same file's import of pkg/client, a package that does not exist. The import is checked
//     before its symbols, because "pkg/client does not exist" is the useful message and four
//     missing-symbol errors under it are noise.
func TestDocumentedGoSymbolsExist(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	for _, path := range markdownFiles(t) {
		rel := shortName(t, path)

		body, err := os.ReadFile(path) //nolint:gosec // a path from a walk of this repository
		if err != nil {
			t.Errorf("reading %s: %v", rel, err)

			continue
		}

		for _, block := range fencedBlocks(string(body)) {
			if block.language != "go" {
				continue
			}

			checkGoBlock(t, root, rel, block)
		}
	}
}

// checkGoBlock verifies the objectfs symbols one go block references against the packages that same
// block imports.
func checkGoBlock(t *testing.T, root, rel string, block fencedBlock) {
	t.Helper()

	for name, importPath := range objectFSImports(block.body) {
		exported, ok := exportedSymbols(t, root, importPath)
		if !ok {
			t.Errorf("%s:%d imports %s/%s, which is not a package in this repository.\n"+
				"A reader following this snippet cannot get past the import line. Name the package "+
				"that does the job, or if it is planned rather than present, say so in the prose "+
				"instead of in an import.",
				rel, block.line, modulePath, importPath)

			continue
		}

		for _, ref := range block.references(name) {
			if exported[ref.symbol] {
				continue
			}

			t.Errorf("%s:%d calls %s.%s, which %s does not export.\n"+
				"Either the symbol was renamed, or it never existed and the snippet was written "+
				"from intent rather than from the code. Documentation is what a reader acts on, so "+
				"an API that is not there is a defect and not a typo.",
				rel, ref.line, name, ref.symbol, importPath)
		}
	}
}

// TestDocumentedCLIInvocationsAreRunnable checks every `objectfs` command line in a shell block.
//
// Documentation shipped `objectfs mount`, `objectfs list-mounts`, `objectfs status`, and `objectfs
// config validate` against a binary that had no subcommands at all and took exactly two positional
// arguments. That is worse than an undocumented feature: a reader types it, it fails, and their
// conclusion is that the filesystem is broken.
//
// #134 added mount/unmount/version/help, so `objectfs mount` is now correct and the other three are
// still not. Both the flag set and the subcommand set are therefore extracted from the binary rather
// than asserted from a list here — see TestCLIFlagsMatchTheBinary and TestSubcommandsMatchTheBinary.
func TestDocumentedCLIInvocationsAreRunnable(t *testing.T) {
	t.Parallel()

	for _, path := range markdownFiles(t) {
		rel := shortName(t, path)

		body, err := os.ReadFile(path) //nolint:gosec // a path from a walk of this repository
		if err != nil {
			t.Errorf("reading %s: %v", rel, err)

			continue
		}

		for _, block := range fencedBlocks(string(body)) {
			if !block.isShell() {
				continue
			}

			checkShellBlock(t, rel, block)
		}
	}
}

// checkShellBlock reports unknown flags and subcommands in the objectfs invocations of one block.
func checkShellBlock(t *testing.T, rel string, block fencedBlock) {
	t.Helper()

	for _, invocation := range block.invocations() {
		for _, flag := range invocation.flags {
			if cliFlags[flag] {
				continue
			}

			t.Errorf("%s:%d passes --%s, which objectfs does not parse:\n    %s\n"+
				"Go's flag package exits 1 on an unrecognized flag, so this whole command fails. "+
				"The parsed set is: %s. A setting that exists only in a configuration file belongs "+
				"in a YAML example, not on a command line.",
				rel, invocation.line, flag, invocation.text, strings.Join(sortedKeys(cliFlags), ", "))
		}

		if invocation.subcommand != "" && !subcommands[invocation.subcommand] {
			t.Errorf("%s:%d uses the subcommand %q, which objectfs does not have:\n    %s\n"+
				"The set is: %s. An unknown first word exits 2 with a usage error, which a reader will "+
				"read as a broken filesystem rather than as a wrong example. Documentation shipped "+
				"`list-mounts`, `status`, and `config validate` this way, none of which was ever a "+
				"command.",
				rel, invocation.line, invocation.subcommand, invocation.text,
				strings.Join(sortedKeys(subcommands), ", "))
		}
	}
}

// TestCLIFlagsMatchTheBinary keeps the hand-maintained cliFlags list from drifting.
//
// cliFlags has to be written out because main's flag variables are unexported, and a duplicated
// list with no check is precisely the mechanism that gave this repository five different version
// numbers. So the list is compared against the flag declarations in main.go: a flag added there and
// not here would go unchecked in documentation, and one removed there but left here would let this
// gate bless a command that no longer runs.
//
// Two flags are in cliFlags and cannot be declared: --version and --help are dispatch cases in run's
// switch, not flags, since they have to work before any FlagSet is chosen. They are exempted by name
// rather than by loosening the comparison, so the exemption is visible and stays small.
func TestCLIFlagsMatchTheBinary(t *testing.T) {
	t.Parallel()

	//nolint:gosec // a path built from the module root this test located itself
	mainGo, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "objectfs", "main.go"))
	if err != nil {
		t.Fatalf("reading cmd/objectfs/main.go, which declares the flags: %v", err)
	}

	declared := make(map[string]bool)
	for _, m := range flagDeclaration.FindAllStringSubmatch(string(mainGo), -1) {
		declared[m[1]] = true
	}

	if len(declared) == 0 {
		t.Fatal("found no flag declarations in cmd/objectfs/main.go. If flag parsing moved to " +
			"another file or another package, point this test at it — without this comparison " +
			"cliFlags below is an unverified copy, which is the defect this test exists to prevent")
	}

	// Spellings that name a subcommand rather than a flag. run() matches these on args[0] before any
	// FlagSet exists, so they are runnable but never declared.
	for _, dispatched := range []string{"version", "help", "h"} {
		declared[dispatched] = true
	}

	for name := range declared {
		if !cliFlags[name] {
			t.Errorf("the binary parses --%s and cliFlags does not list it, so documentation using "+
				"that flag would be reported as broken when it is correct. Add it to cliFlags, and "+
				"to cliFlagsTakingAValue if it is not a boolean.", name)
		}
	}

	for name := range cliFlags {
		if !declared[name] {
			t.Errorf("cliFlags lists --%s and the binary no longer parses it, so this gate would "+
				"approve a command that exits 1. Remove it from cliFlags — and check whether "+
				"documentation still uses it.", name)
		}
	}

	for name := range cliFlagsTakingAValue {
		if !cliFlags[name] {
			t.Errorf("cliFlagsTakingAValue lists --%s, which is not in cliFlags at all", name)
		}
	}
}

// flagDeclaration matches a flag declaration in main.go, capturing the flag name.
//
// Both spellings, because #134 changed which one main.go uses: the package-level `flag.String("x",
// ...)` form, and the `fs.StringVar(&f.x, "x", ...)` methods on a per-subcommand FlagSet. The Var
// forms are what a subcommand needs — flag.CommandLine cannot parse flags appearing after a
// positional argument, so a single package-level flag set could not have accepted `objectfs mount
// --config x` at all. A regexp matching only the old form kept compiling and matched nothing, which
// is why TestCLIFlagsMatchTheBinary asserts a non-empty result before comparing.
var flagDeclaration = regexp.MustCompile(
	`(?:flag\.|\.)(?:Bool|String|Int|Int64|Uint|Uint64|Float64|Duration)(?:Var)?\(` +
		`(?:\s*&[\w.]+\s*,)?\s*"([^"]+)"`)

// fencedBlock is one fenced code block: its info-string language, the 1-based line its body starts
// on, and that body.
type fencedBlock struct {
	language string
	line     int
	body     string
}

// shellLanguages are the info strings whose contents are commands a reader can type.
//
// The empty string is included because an unlabeled block is overwhelmingly shell in this
// repository — 40 of them — and excluding it would skip real invocations.
var shellLanguages = map[string]bool{
	"":             true,
	"bash":         true,
	"sh":           true,
	"shell":        true,
	"shellsession": true,
	"console":      true,
	"zsh":          true,
}

// isShell reports whether this block holds commands rather than source.
func (b fencedBlock) isShell() bool { return shellLanguages[b.language] }

// symbolReference is one `pkg.Symbol` mention and the line it is on.
type symbolReference struct {
	symbol string
	line   int
}

// qualifiedIdentifier matches a `name.Symbol` reference with an exported symbol. It is applied only
// to blocks that import the package under `name`, which is what makes it specific enough to use.
var qualifiedIdentifier = regexp.MustCompile(`\b([a-z][a-zA-Z0-9_]*)\.([A-Z]\w*)`)

// references returns every exported symbol this block reads off the given package name.
func (b fencedBlock) references(name string) []symbolReference {
	var refs []symbolReference

	for _, m := range qualifiedIdentifier.FindAllStringSubmatchIndex(b.body, -1) {
		if b.body[m[2]:m[3]] != name {
			continue
		}

		refs = append(refs, symbolReference{
			symbol: b.body[m[4]:m[5]],
			line:   b.line + strings.Count(b.body[:m[0]], "\n"),
		})
	}

	return refs
}

// fencedBlocks splits markdown into its fenced code blocks.
//
// Fences are paired by alternation — the first ``` opens, the next closes — rather than matched by a
// regexp. A regexp of the form "```lang\n(.*?)```" gets out of phase on a file whose closing fence
// is followed by an unlabeled opening one, and then reports blocks that do not exist while missing
// ones that do. That was not hypothetical: the first draft of this test missed
// OBJECTFS.md's three `objectfs config` invocations for exactly that reason, which is a gate
// silently checking nothing.
func fencedBlocks(markdown string) []fencedBlock {
	var (
		blocks []fencedBlock
		open   *fencedBlock
		buf    []string
	)

	for i, line := range strings.Split(markdown, "\n") {
		fence := fenceLine.FindStringSubmatch(line)
		if fence == nil {
			if open != nil {
				buf = append(buf, line)
			}

			continue
		}

		if open == nil {
			open = &fencedBlock{language: strings.ToLower(fence[1]), line: i + 2}
			buf = nil

			continue
		}

		open.body = strings.Join(buf, "\n")
		blocks = append(blocks, *open)
		open = nil
	}

	// An unclosed fence at end of file is malformed markdown; take what was collected rather than
	// dropping it, since its contents are what a reader still sees rendered.
	if open != nil {
		open.body = strings.Join(buf, "\n")
		blocks = append(blocks, *open)
	}

	return blocks
}

// fenceLine matches a code fence, capturing the language word of its info string. Indented fences
// count: they are how a fenced block appears inside a list item, and getting-started.md uses them.
var fenceLine = regexp.MustCompile("^\\s*```+\\s*([A-Za-z0-9_+-]*)")

// objectFSImportLine matches an import of a package in this module, with an optional alias, in
// either the single-line `import "path"` form or a line inside an import group.
var objectFSImportLine = regexp.MustCompile(
	`(?m)^\s*(?:import\s+)?(?:([A-Za-z_]\w*)\s+)?"` + regexp.QuoteMeta(modulePath) + `/([\w/.-]+)"`)

// objectFSImports maps the name a block refers to each objectfs package by, to that package's path
// relative to the module root. The name is the alias if there is one and the last path element
// otherwise — which is right for every package in this repository, since none declares a package
// name differing from its directory.
func objectFSImports(body string) map[string]string {
	imports := make(map[string]string)

	for _, m := range objectFSImportLine.FindAllStringSubmatch(body, -1) {
		alias, path := m[1], m[2]
		if alias == "" {
			alias = path[strings.LastIndex(path, "/")+1:]
		}

		imports[alias] = path
	}

	return imports
}

// exportedSymbols returns the exported top-level names of the package at a module-relative path,
// and whether that path is a package at all.
//
// Parsed with go/parser rather than grepped, so a symbol in a grouped `type (...)` declaration or
// one whose declaration wraps across lines is found the same as any other. Test files are excluded:
// a symbol only a test declares is not part of the API a reader can call.
//
// Every .go file in the directory is parsed, deliberately ignoring build constraints. That is the
// behavior wanted here and the reason this reads files itself rather than using
// golang.org/x/tools/go/packages: internal/fuse is //go:build linux || darwin, and a symbol
// documented for Linux should not be reported as nonexistent because the test happens to be running
// on a Mac. Documentation is not built per-platform.
func exportedSymbols(t *testing.T, root, importPath string) (map[string]bool, bool) {
	t.Helper()

	dir := filepath.Join(root, filepath.FromSlash(importPath))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}

	fset := token.NewFileSet()
	symbols := make(map[string]bool)
	parsed := false

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Errorf("parsing %s/%s to collect its exported symbols: %v", importPath, name, err)

			continue
		}

		parsed = true

		collectExported(file, symbols)
	}

	return symbols, parsed
}

// collectExported adds one file's exported top-level declarations to symbols.
func collectExported(file *ast.File, symbols map[string]bool) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			// Methods are reached through a value, not through the package name, so only plain
			// functions are package-qualified symbols.
			if d.Recv == nil && d.Name.IsExported() {
				symbols[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						symbols[s.Name.Name] = true
					}
				case *ast.ValueSpec:
					for _, name := range s.Names {
						if name.IsExported() {
							symbols[name.Name] = true
						}
					}
				}
			}
		}
	}
}

// cliInvocation is one objectfs command line: the flag names it passes, the subcommand it uses if
// any, and the source line for the error message.
type cliInvocation struct {
	line       int
	text       string
	flags      []string
	subcommand string
}

// objectFSCommand matches `objectfs` in a command position — at the start of a line, after a pipe,
// `&&`, `;`, or inside `$(...)` — optionally behind sudo or leading VAR=value assignments, and
// optionally as ./objectfs.
//
// The negative lookahead-equivalent is done by hand below rather than in the regexp, since Go's
// regexp has no lookahead: a match must not be followed by a character that would make it a
// different word, so `objectfs-linux-amd64`, `objectfs.yaml`, and `objectfs/` are not invocations.
var objectFSCommand = regexp.MustCompile(
	`(?:^|[|&;(]\s*|\$\(\s*)\s*(?:sudo\s+)?(?:[A-Z_]+=\S+\s+)*(?:\./)?objectfs(.*)$`)

// invocations extracts the objectfs command lines from a shell block.
//
// Line continuations are joined first, so `objectfs \` followed by indented flags is one command
// rather than a command with no arguments plus several lines of orphaned flags.
func (b fencedBlock) invocations() []cliInvocation {
	var found []cliInvocation

	for offset, line := range joinContinuations(strings.Split(b.body, "\n")) {
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "#") {
			continue
		}

		m := objectFSCommand.FindStringSubmatch(text)
		if m == nil {
			continue
		}

		rest := m[1]
		// A match immediately followed by one of these is a different word — a filename, a path, or
		// a package. `objectfs --help` and `objectfs s3://b /mnt` are invocations; `objectfs.yaml`,
		// `objectfs-test-$(whoami)`, and `objectfs/sdks/python` are not.
		if rest != "" && strings.ContainsAny(rest[:1], "-./_@:0123456789abcdefghijklmnopqrstuvwxyz") &&
			!strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
			continue
		}

		found = append(found, parseInvocation(b.line+offset, text, rest))
	}

	return found
}

// parseInvocation splits an objectfs command's arguments into flags and positionals.
func parseInvocation(line int, text, rest string) cliInvocation {
	invocation := cliInvocation{line: line, text: text}

	var positionals []string

	fields := strings.Fields(rest)
	for i := 0; i < len(fields); i++ {
		field := fields[i]

		switch {
		case strings.HasPrefix(field, "-"):
			name, _, hasValue := strings.Cut(strings.TrimLeft(field, "-"), "=")
			invocation.flags = append(invocation.flags, name)

			if !hasValue && cliFlagsTakingAValue[name] {
				i++ // the next field is this flag's value, not a positional
			}
		case shellNoise[field]:
		default:
			positionals = append(positionals, field)
		}
	}

	// A subcommand is a first positional that is a bare word: a storage URI has a scheme and a
	// mount point has a slash, so neither can be mistaken for one.
	if len(positionals) > 0 && bareWord.MatchString(positionals[0]) {
		invocation.subcommand = positionals[0]
	}

	return invocation
}

// shellNoise are tokens that are shell syntax rather than arguments.
var shellNoise = map[string]bool{
	"\\": true, "&": true, "|": true, ">": true, ">>": true,
	"2>&1": true, "&>": true, ">/dev/null": true,
}

// bareWord matches a lowercase hyphenated word — what a subcommand looks like, and what neither a
// URI nor a path can be.
var bareWord = regexp.MustCompile(`^[a-z][a-z-]*$`)

// joinContinuations folds backslash-continued lines into one, returning each result with the offset
// of the line it started on so error messages point at the command's first line.
func joinContinuations(lines []string) map[int]string {
	joined := make(map[int]string)

	for i := 0; i < len(lines); i++ {
		start := i
		text := strings.TrimSpace(lines[i])

		for strings.HasSuffix(text, "\\") && i+1 < len(lines) {
			i++
			text = strings.TrimSuffix(text, "\\") + " " + strings.TrimSpace(lines[i])
		}

		joined[start] = text
	}

	return joined
}

// sortedKeys returns a set's keys in a stable order, so a failure message reads the same twice.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
