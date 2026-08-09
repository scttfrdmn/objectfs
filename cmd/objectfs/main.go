package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/scttfrdmn/objectfs/internal/adapter"
	"github.com/scttfrdmn/objectfs/internal/awsname"
	"github.com/scttfrdmn/objectfs/internal/config"
	"github.com/scttfrdmn/objectfs/internal/fuse"
	"github.com/scttfrdmn/objectfs/pkg/utils"
)

const (
	version = "0.13.0"
	banner  = `
 ___  _     _           _   ___ ___
/ _ \| |__ (_) ___  ___| |_| __/ __|
| | | | '_ \| |/ _ \/ __| __|  _\__ \
| |_| | |_) | |  __| (__| |_| | |___/
 \___/|_.__/| |\___|\___|\__|_| |___/
           |_|

A POSIX interface over object storage, for research computing
Version: %s
`
)

// main is a wrapper around run so that every exit goes through one place.
//
// The package had no tests at all before v0.11.0, and .coverage-floors recorded why: "argument parsing
// and signal handling, currently untestable because main() calls log.Fatalf directly". log.Fatalf calls
// os.Exit, which takes the test binary with it, so no test could reach a single argument-handling
// decision — in the one package whose entire job is to read an operator's command line. Everything below
// returns an exit code and writes to an injected io.Writer instead.
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// emit writes a message to a stream and discards the write error.
//
// One helper rather than twenty-six `_, _ = fmt.Fprintf` assignments, so the decision is stated once
// instead of being repeated as punctuation nobody reads. And it is a decision: everything written
// here is usage text, progress, or a diagnostic going to stdout or stderr, and if that write fails
// there is no second channel to report it on — reporting a failed write to stderr by writing to
// stderr is not a plan. The case that would matter, a caller piping `objectfs version` into a closed
// pipe, delivers SIGPIPE and never reaches the return value.
//
// Not used for anything durable. The one write in this program whose failure is a data-loss event is
// the object PUT, and that goes through the adapter's Stop, whose error is in the exit code.
func emit(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// exit codes. Distinguished because a systemd unit and a shell script both branch on them.
const (
	exitOK = 0

	// exitUsage is the command line being wrong: an unknown subcommand, a missing argument, a bad flag.
	// Nothing was attempted.
	exitUsage = 2

	// exitFailure is the command being right and the operation failing.
	exitFailure = 1
)

// run dispatches a subcommand and returns the process exit code.
//
// Dispatch is on args[0] and is stdlib flag only — no third-party command framework, per #134. Each
// subcommand gets its own flag.FlagSet, which is what makes `objectfs mount --config x` work at all:
// the package-level flag.CommandLine cannot parse flags that appear after a positional argument, so the
// single flag set this used to have could not have accepted a subcommand name in front of its flags.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)

		return exitUsage
	}

	switch args[0] {
	case "mount":
		return runMount(args[1:], stdout, stderr)

	case "unmount", "umount":
		// Both spellings. `umount` is the program name on Linux and macOS and `unmount` is the English
		// word; an operator reaching for one and getting "unknown command" has learned nothing.
		return runUnmount(args[1:], stdout, stderr)

	case "cluster":
		return runCluster(args[1:], stdout, stderr)

	case "version", "--version", "-version":
		emit(stdout, "objectfs version %s\n", version)

		return exitOK

	case "help", "--help", "-h", "-help":
		usage(stdout)

		return exitOK
	}

	// The legacy form, which is what every pre-v0.11.0 invocation looks like — including the ones in
	// this repository's own README, in cmd/objectfs/doc.go, and in whatever scripts users have written.
	// Two shapes, because the old binary had global flags and two positional arguments:
	//
	//	objectfs s3://my-bucket /mnt/s3
	//	objectfs --config /etc/objectfs/config.yaml s3://my-bucket /mnt/s3
	//
	// Recognized by a URI scheme or a leading dash rather than by "does not match a subcommand", so
	// that a misspelled subcommand is still a usage error: `objectfs moutn s3://b /mnt` says "unknown
	// command" instead of trying to mount a bucket named "moutn". The flag spellings that name a
	// subcommand — --version, --help — are matched above, so they never reach this.
	if strings.Contains(args[0], "://") || strings.HasPrefix(args[0], "-") {
		return runMount(args, stdout, stderr)
	}

	emit(stderr, "objectfs: unknown command %q\n\n", args[0])
	usage(stderr)

	return exitUsage
}

func usage(w io.Writer) {
	emit(w, banner, version)
	emit(w, `
Usage: objectfs <command> [options]

Commands:
  mount     Mount a bucket on a directory
  unmount   Unmount a filesystem by path
  cluster   Inspect the cluster state of a running instance
  version   Print the version and exit
  help      Print this message

Run "objectfs <command> --help" for a command's options.

Examples:
  objectfs mount s3://my-bucket /mnt/s3
  objectfs mount --config /etc/objectfs/config.yaml s3://my-bucket /mnt/s3
  objectfs mount --config /etc/objectfs/research-data.yaml --foreground
  objectfs unmount /mnt/s3
  objectfs cluster status
  objectfs cluster status --json

The form without a subcommand still works:
  objectfs s3://my-bucket /mnt/s3

Documentation: https://github.com/scttfrdmn/objectfs
`)
}

// mountFlags is one mount invocation's command line, parsed.
type mountFlags struct {
	configFile     string
	foreground     bool
	mountPoint     string
	logLevel       string
	cacheSize      string
	maxConcurrency int
	dryRun         bool
	debug          bool

	// storageURI and mountArg are the positional arguments, either of which may be absent when the
	// config file supplies it.
	storageURI string
	mountArg   string
}

func newMountFlagSet(fs *flag.FlagSet, f *mountFlags) {
	fs.StringVar(&f.configFile, "config", "", "Configuration file path")
	fs.BoolVar(&f.foreground, "foreground", false,
		"Stay in the foreground. This is the only mode ObjectFS has; see --help")
	fs.StringVar(&f.mountPoint, "mount-point", "",
		"Directory to mount on, when it is not given as an argument")
	fs.StringVar(&f.logLevel, "log-level", "", "Log level (DEBUG, INFO, WARN, ERROR)")
	fs.StringVar(&f.cacheSize, "cache-size", "", "Cache size (e.g. 2GB)")
	fs.IntVar(&f.maxConcurrency, "max-concurrency", 0, "Maximum concurrent operations")
	fs.BoolVar(&f.dryRun, "dry-run", false, "Validate the configuration and exit without mounting")
	fs.BoolVar(&f.debug, "debug", false, "Enable debug logging (equivalent to --log-level DEBUG)")
}

func runMount(args []string, stdout, stderr io.Writer) int {
	var f mountFlags

	fs := flag.NewFlagSet("objectfs mount", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		emit(stderr, "Usage: objectfs mount [options] [<storage-uri>] [<mount-point>]\n\n"+
			"Both arguments may instead come from the config file's mount block, as mount.uri and\n"+
			"mount.mount_point — which is how a systemd template unit names a per-instance bucket,\n"+
			"since the unit only knows its instance name.\n\n"+
			"ObjectFS always runs in the foreground: it does not fork, and it serves the mount until\n"+
			"it is signaled. --foreground is accepted because init systems and scripts pass it, and\n"+
			"it names what already happens; there is no background mode to ask for. To run it as a\n"+
			"service, use a systemd unit (Type=simple) rather than backgrounding it with &, so that\n"+
			"the unmount on stop is the one ObjectFS does rather than a SIGKILL.\n\nOptions:\n")
		fs.PrintDefaults()
	}
	newMountFlagSet(fs, &f)

	if err := fs.Parse(args); err != nil {
		// flag has already printed the error and the usage.
		return exitUsage
	}

	switch rest := fs.Args(); len(rest) {
	case 0:
	case 1:
		// One positional argument is the URI. The mount point then has to come from --mount-point or
		// from the config file, and resolveMountTarget says so if it does not.
		f.storageURI = rest[0]
	case 2:
		f.storageURI, f.mountArg = rest[0], rest[1]
	default:
		emit(stderr, "objectfs mount: expected at most a storage URI and a mount point, got %d "+
			"arguments: %s\n", len(rest), strings.Join(rest, " "))

		// The overwhelmingly likely cause, named explicitly. Go's flag package stops parsing at the
		// first non-flag argument, so `objectfs mount s3://b /mnt --foreground` leaves --foreground as a
		// third positional rather than setting it — the flag is silently not applied, and the only
		// visible symptom is this count. The old binary behaved the same way and said only "Expected
		// exactly 2 arguments", which sent people counting their arguments instead of moving a flag.
		for _, arg := range rest {
			if strings.HasPrefix(arg, "-") {
				emit(stderr, "\n%s looks like a flag. Flags have to come before the storage URI, "+
					"because argument parsing stops at the first one that is not a flag:\n"+
					"  objectfs mount %s %s\n", arg, arg, strings.Join(withoutArg(rest, arg), " "))

				break
			}
		}

		return exitUsage
	}

	return mountWithFlags(&f, stdout, stderr)
}

// withoutArg returns args with the first occurrence of drop removed, for building the corrected command
// line the error message suggests.
func withoutArg(args []string, drop string) []string {
	out := make([]string, 0, len(args))
	dropped := false

	for _, a := range args {
		if a == drop && !dropped {
			dropped = true

			continue
		}

		out = append(out, a)
	}

	return out
}

func mountWithFlags(f *mountFlags, stdout, stderr io.Writer) int {
	cfg, err := loadConfiguration(f)
	if err != nil {
		emit(stderr, "objectfs mount: %v\n", err)

		return exitFailure
	}

	storageURI, mountPoint, err := resolveMountTarget(f, cfg)
	if err != nil {
		emit(stderr, "objectfs mount: %v\n", err)

		return exitUsage
	}

	// --dry-run stops here, before the mount point is checked and before anything is created. It is
	// what a config-management run uses to tell an operator their file is good, so it must not require
	// the mount point to exist yet.
	if f.dryRun {
		emit(stdout, "Configuration is valid.\n")
		emit(stdout, "  storage URI:     %s\n", storageURI)
		emit(stdout, "  mount point:     %s\n", mountPoint)
		emit(stdout, "  cache size:      %s\n", cfg.Performance.CacheSize)
		emit(stdout, "  max concurrency: %d\n", cfg.Performance.MaxConcurrency)

		return exitOK
	}

	if err := validateMountPoint(mountPoint); err != nil {
		emit(stderr, "objectfs mount: %v\n", err)

		return exitFailure
	}

	if err := utils.SetupLogging(cfg.Global.LogLevel, cfg.Global.LogFile); err != nil {
		emit(stderr, "objectfs mount: cannot set up logging: %v\n", err)

		return exitFailure
	}

	return mountAndWait(storageURI, mountPoint, cfg, stdout, stderr)
}

func mountAndWait(storageURI, mountPoint string, cfg *config.Configuration, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	adapterInstance, err := adapter.New(ctx, storageURI, mountPoint, cfg)
	if err != nil {
		emit(stderr, "objectfs mount: %v\n", err)

		return exitFailure
	}

	// Registered before Start, not after. A SIGTERM arriving during Start — which a systemd unit with a
	// TimeoutStartSec will send — would otherwise be the default disposition and kill the process with
	// the mount half-established and nothing run to tear it down.
	//
	// SIGHUP is deliberately absent from this set. It used to be here, alongside SIGINT and SIGTERM,
	// with any of the three treated as shutdown — so `kill -HUP` unmounted the filesystem while
	// README.md advertised "zero-downtime configuration reloading". Reload is not implemented; the
	// honest handling of SIGHUP is Go's default, which for a process with no handler is to die, but
	// which at least is not documented as a reload. When reload lands it goes here.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	if err := adapterInstance.Start(ctx); err != nil {
		emit(stderr, "objectfs mount: %v\n", err)

		return exitFailure
	}

	emit(stdout, "objectfs %s mounted %s at %s\n", version, storageURI, mountPoint)

	sig := <-sigChan
	emit(stdout, "objectfs: received %v, unmounting %s\n", sig, mountPoint)

	// The exit code reports whether the unmount succeeded, because this is the last point at which
	// buffered writes are flushed. A shutdown that could not flush is a data-loss event, and reporting
	// it as a clean exit is what makes it silent — `systemctl stop` would show success.
	if err := adapterInstance.Stop(ctx); err != nil {
		emit(stderr, "objectfs: shutdown failed, so data may not have reached S3: %v\n", err)

		return exitFailure
	}

	emit(stdout, "objectfs: %s unmounted\n", mountPoint)

	return exitOK
}

// resolveMountTarget decides what to mount and where, from the command line and the config file.
//
// The precedence is the command line, then the file. That way `objectfs mount s3://other /mnt/tmp` can
// point an existing config file somewhere else for a one-off without editing it, while a systemd
// template unit — which can pass neither, knowing only its instance name — gets both from the file that
// `%i` selected (#134, #135).
//
// Neither is defaulted. A missing URI is an error naming both places it could have come from, and a
// missing mount point likewise: a mount point guessed wrong is not a mount that fails, it is a mount
// that succeeds somewhere the operator is not looking, over whatever was already in that directory.
func resolveMountTarget(f *mountFlags, cfg *config.Configuration) (uri, mountPoint string, err error) {
	uri = f.storageURI
	if uri == "" {
		uri = cfg.Mount.URI
	}

	if uri == "" {
		return "", "", errors.New("no storage URI. Give one as an argument — `objectfs mount " +
			"s3://my-bucket /mnt/point` — or set mount.uri in the config file")
	}

	// Checked here rather than left to adapter.New, because --dry-run returns before the adapter is
	// built. Without this, `objectfs mount --dry-run gs://bucket /mnt` printed "Configuration is valid"
	// for a URI nothing in this build can mount — and --dry-run exists to be the check that tells an
	// operator their invocation is good before they run it for real. A validating dry run that does not
	// validate is worse than no dry run.
	//
	// Not a second opinion: it is the same awsname function internal/config applies to mount.uri and
	// internal/adapter applies to its argument. A URI from the file has already been through it, and
	// running it twice on the same string cannot disagree.
	if err := awsname.ValidateStorageURI(uri); err != nil {
		return "", "", err
	}

	// --mount-point beats the second positional argument, and giving both conflicting values is an
	// error rather than a silent preference. They are the same setting written two ways, so an
	// invocation that sets them differently means something the command cannot do.
	switch {
	case f.mountPoint != "" && f.mountArg != "" && f.mountPoint != f.mountArg:
		return "", "", fmt.Errorf("mount point given twice and differently: --mount-point %q and "+
			"argument %q", f.mountPoint, f.mountArg)

	case f.mountPoint != "":
		mountPoint = f.mountPoint

	case f.mountArg != "":
		mountPoint = f.mountArg

	default:
		mountPoint = cfg.Mount.MountPoint
	}

	if mountPoint == "" {
		return "", "", errors.New("no mount point. Give one as an argument — `objectfs mount " +
			"s3://my-bucket /mnt/point` — or with --mount-point, or set mount.mount_point in the " +
			"config file")
	}

	return uri, mountPoint, nil
}

func runUnmount(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("objectfs unmount", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		emit(stderr, "Usage: objectfs unmount <mount-point>\n\n"+
			"Unmounts an ObjectFS filesystem. Run by an operator, or by a systemd unit's ExecStop.\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	rest := fs.Args()
	if len(rest) != 1 {
		emit(stderr, "objectfs unmount: expected exactly one mount point, got %d\n", len(rest))
		fs.Usage()

		return exitUsage
	}

	if err := fuse.UnmountPath(context.Background(), rest[0]); err != nil {
		emit(stderr, "objectfs unmount: %v\n", err)

		return exitFailure
	}

	emit(stdout, "objectfs: %s unmounted\n", rest[0])

	return exitOK
}

// validateMountPoint checks the directory is one that can be mounted on.
//
// All three checks are about the same failure: mounting over a directory that already has something in
// it hides that content for the life of the mount, and an operator who mounts over /home does not get
// an error, they get a system whose users have no files. So a non-empty directory is refused rather
// than accepted with a warning.
func validateMountPoint(mountPoint string) error {
	clean := filepath.Clean(mountPoint)
	if strings.Contains(clean, "..") {
		return fmt.Errorf("mount point %q contains \"..\" even after cleaning, so it is not a path "+
			"this can resolve", mountPoint)
	}

	info, err := os.Stat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("mount point %s does not exist; create it first (mkdir -p %s)",
				clean, clean)
		}

		return fmt.Errorf("cannot access mount point %s: %w", clean, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("mount point %s is not a directory", clean)
	}

	f, err := os.Open(clean)
	if err != nil {
		return fmt.Errorf("cannot open mount point %s: %w", clean, err)
	}
	defer func() { _ = f.Close() }()

	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("cannot read mount point %s: %w", clean, err)
	}

	if len(names) > 0 {
		return fmt.Errorf("mount point %s is not empty (it contains %q); mounting over it would hide "+
			"what is already there for as long as the mount lasts", clean, names[0])
	}

	return nil
}

func loadConfiguration(f *mountFlags) (*config.Configuration, error) {
	cfg := config.NewDefault()

	if f.configFile != "" {
		if err := cfg.LoadFromFile(f.configFile); err != nil {
			return nil, fmt.Errorf("cannot load %s: %w", f.configFile, err)
		}
	}

	if err := cfg.LoadFromEnv(); err != nil {
		return nil, fmt.Errorf("cannot read configuration from the environment: %w", err)
	}

	applyCommandLineOverrides(cfg, f)

	// Validated after the overrides, so that a flag cannot introduce a value nothing checks. --cache-size
	// is a string that has to parse as a size, and before v0.11.0 an unparseable one became a silent
	// 1 GiB (#159).
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

func applyCommandLineOverrides(cfg *config.Configuration, f *mountFlags) {
	if f.logLevel != "" {
		cfg.Global.LogLevel = f.logLevel
	}

	if f.cacheSize != "" {
		cfg.Performance.CacheSize = f.cacheSize
	}

	if f.maxConcurrency > 0 {
		cfg.Performance.MaxConcurrency = f.maxConcurrency
	}

	// Last, so that it wins over --log-level. The two together are contradictory and --debug is the
	// more specific request.
	if f.debug {
		cfg.Global.LogLevel = "DEBUG"
	}
}
