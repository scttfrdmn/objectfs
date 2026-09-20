// Command mount.objectfs is the kernel mount helper that makes `mount -t objectfs` and an /etc/fstab
// entry work.
//
// mount(8) resolves a filesystem type it does not know to /sbin/mount.TYPE and hands it the whole
// invocation:
//
//	mount.objectfs <device> <mountpoint> [-sfnv] [-o options] [-t type]
//
// So this program exists to translate that calling convention into an `objectfs mount` command line,
// and nothing else. The fstab entry it enables is:
//
//	s3://my-bucket  /mnt/data  objectfs  _netdev,config=/etc/objectfs/data.yaml  0  0
//
// # It returns once the mount is live, rather than exec'ing into it
//
// `objectfs mount` blocks for the lifetime of the mount — --foreground is accepted and ignored because
// foreground is the only mode there is. A helper that exec'd into it, which is the obvious translation
// and the one #136 specifies, would never return, and `mount -a` at boot would hang there forever
// waiting for a process that exits when the filesystem is unmounted.
//
// So the mount is started as a detached child and this program waits for the mount point to appear in
// the kernel's own mount table before reporting success. That is the contract mount(8) has with a
// helper: exit 0 means mounted, and exit non-zero means it is not. Polling the kernel rather than
// trusting the child to still be running is the difference between the two — a child that is alive is
// not yet a mount, and reporting success at the moment of fork would make every failure look like a
// success that broke immediately afterwards.
//
// The child's startup output goes to /var/log/objectfs/mount-<mountpoint>.log, and the tail of what this
// attempt wrote is relayed onto stderr if the mount fails. Without it a failed fstab mount reports nothing
// at all, which sends an operator to check the bucket, the credentials and the network — none of which is
// necessarily the problem. A named file rather than a pipe or an unlinked temporary, because the child
// keeps writing to it for the lifetime of the mount: a pipe would fill and block the filesystem on its own
// log, and an unlinked file would grow where nobody could find it or rotate it.
//
// # Options it refuses, and why refusing is the point
//
// #136 specified translations for `ro`, `uid=` and `gid=`. `ro` is one as of #532: it becomes
// `objectfs mount --read-only`, and the refusal that used to be here is gone.
//
// `uid=` and `gid=` are still refused, and not for want of a flag to map them to — ownership is not
// remappable at all. internal/fuse reports the uid and gid of the process that made the request, falling
// back to the ones that ran the mount, with no flag or config key that overrides either. Verified
// against the code, not assumed.
//
// They are refused rather than dropped, and `ro`'s history is why that rule is worth keeping for the two
// that remain. Through v0.16.0 this program refused `ro` because the mount was always writable; had it
// dropped the option instead, every operator who wrote `ro` in fstab would have been told something
// untrue by this program about the filesystem's most consequential property, and would have found out
// when something was already overwritten. The same goes for ownership. A mount that does not come up is
// a problem an operator can see; a mount that came up meaning something other than what the entry says
// is one they cannot.
//
// Everything not in optionTable is refused too, by the same reasoning generalized: an option this
// program has not been taught about is one whose effect it cannot promise. `-s` (sloppy) downgrades that
// to a warning, which is what -s is for, but it does not extend to the three above — "ignore options you
// do not recognize" is not "ignore options you recognize and cannot honor".
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Exit codes are mount(8)'s, because mount(8) and `mount -a` are what read them.
//
//	1  incorrect invocation or permissions — nothing was attempted
//	32 mount failure — it was attempted and did not come up
//
// A bad or unsupported option is exit 1: the entry is wrong, and no mount was tried.
const (
	exitOK          = 0
	exitUsage       = 1
	exitMountFailed = 32
)

// defaultTimeout bounds the wait for the mount point to appear.
//
// A mount has to contact S3, which means DNS, TLS, credentials and a bucket check, so seconds rather
// than milliseconds — and a cold instance-metadata credential lookup is the slow case. Sixty is the same
// order as a systemd unit's default TimeoutStartSec, so an entry that works under `mount -a` works under
// the generated .mount unit too. `mount-timeout=` overrides it for a link where that is not enough.
const defaultTimeout = 60 * time.Second

// pollInterval is how often the kernel's mount table is re-read while waiting.
const pollInterval = 50 * time.Millisecond

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// emit writes a message to a stream and discards the write error, as cmd/objectfs does and for the same
// reason: everything written here is usage text or a diagnostic, and there is no second channel on which
// to report that reporting failed.
func emit(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// run is the whole program, with its exit code returned and its output injected, so that every decision
// below is reachable from a test. cmd/objectfs is shaped the same way and .coverage-floors records what
// the alternative cost: a package whose entire job is reading an operator's command line, with no test
// able to reach a single argument-handling decision because main called log.Fatalf.
func run(args []string, stdout, stderr io.Writer) int {
	inv, err := parseArgs(args)
	if err != nil {
		emit(stderr, "mount.objectfs: %v\n", err)
		usage(stderr)

		return exitUsage
	}

	if inv.help {
		usage(stdout)

		return exitOK
	}

	plan, warnings, err := translate(inv)
	for _, w := range warnings {
		emit(stderr, "mount.objectfs: warning: %s\n", w)
	}
	if err != nil {
		emit(stderr, "mount.objectfs: %v\n", err)

		return exitUsage
	}

	binary, err := objectfsPath()
	if err != nil {
		emit(stderr, "mount.objectfs: %v\n", err)

		return exitMountFailed
	}

	if inv.verbose {
		emit(stderr, "mount.objectfs: %s %s\n", binary, strings.Join(plan.argv, " "))
	}

	// -f is mount(8)'s fake mount: check everything and mount nothing. `objectfs mount --dry-run`
	// validates the configuration and resolves both arguments without touching the mount point, which is
	// the same promise, so this one runs in the foreground and relays its result directly.
	if inv.fake {
		// Under the same deadline as a real mount. `objectfs mount --dry-run` resolves the bucket, which
		// means DNS and credentials, so it can hang for the same reasons — and a `mount -a -f` that hangs
		// at boot is the failure this whole program is shaped to avoid, fake mount or not.
		ctx, cancel := context.WithTimeout(context.Background(), plan.timeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, binary, plan.argv...) // #nosec G204 -- binary is resolved by
		// objectfsPath from a fixed set of paths and plan.argv is built by translate from the option table,
		// never from an option's raw text.
		cmd.Stdout = stdout
		cmd.Stderr = stderr

		// The deadline above is not self-enforcing, which a test found rather than a reading of the docs:
		// with these three lines absent, a `mount -f` against a dry run that slept for 30 seconds took the
		// whole 30 seconds despite a 300ms timeout.
		//
		// Two mechanisms were missing, and they are separate. CommandContext's default cancel signals only
		// the direct child, so a grandchild survived; and stdout here is an io.Writer rather than an
		// *os.File, so exec copies through a pipe and Wait blocks until every holder of the write end closes
		// it — which the surviving grandchild did not do. Measured: 5.3 seconds for a 300ms deadline with
		// the group kill removed, and 30 seconds with all three removed.
		//
		// So the kill goes to the process group, and WaitDelay bounds the wait for the pipe rather than
		// trusting it to close. WaitDelay is the weaker of the two and it is not redundant: a descendant
		// that called setsid is in no group this can signal, and then the delay is the only bound there is.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = terminateGrace

		if err := cmd.Run(); err != nil {
			// The deadline named, rather than the `signal: killed` a canceled CommandContext reports. That
			// error says the child was killed and nothing about who killed it or why, which for the one
			// failure an operator can act on — raise the timeout — is the least useful thing to print.
			if ctx.Err() != nil {
				emit(stderr, "mount.objectfs: %s did not finish checking %s on %s within %s\n",
					binary, inv.device, inv.mountPoint, plan.timeout)
				emit(stderr, "mount.objectfs: raise the wait with -o mount-timeout=<duration>\n")

				return exitMountFailed
			}

			emit(stderr, "mount.objectfs: %s would not mount %s on %s: %v\n",
				binary, inv.device, inv.mountPoint, err)

			return exitMountFailed
		}

		return exitOK
	}

	return startAndWait(binary, plan, inv, stderr)
}

func usage(w io.Writer) {
	emit(w, `Usage: mount.objectfs <storage-uri> <mountpoint> [-sfnv] [-o options] [-t objectfs]

The mount helper mount(8) calls for `+"`-t objectfs`"+`. It is not normally run by hand; write an
/etc/fstab entry instead:

    s3://my-bucket  /mnt/data  objectfs  _netdev,config=/etc/objectfs/data.yaml  0  0

Options passed with -o:

    config=PATH           configuration file, as objectfs mount --config
    cache-size=SIZE       as objectfs mount --cache-size (e.g. 4GB)
    log-level=LEVEL       as objectfs mount --log-level (DEBUG, INFO, WARN, ERROR)
    max-concurrency=N     as objectfs mount --max-concurrency
    debug                 as objectfs mount --debug
    mount-timeout=DUR     how long to wait for the mount to come up (default %s); read by
                          this helper and not passed on

Flags:

    -f  fake mount: validate and exit, as objectfs mount --dry-run
    -s  tolerate an option this helper does not recognize, with a warning
    -v  print the objectfs command line before running it
    -n  accepted and ignored; /etc/mtab is /proc/mounts on every system this runs on
    -t  accepted and ignored; the type is already known

Options mount(8) and systemd read for themselves — _netdev, noauto, nofail, x-systemd.*, and the
rest — are accepted and not passed on. Anything else is refused rather than dropped, including ro,
uid= and gid=, which ObjectFS has no way to honor: a mount that came up meaning something other
than what the entry says is worse than one that did not come up.

Exit codes are mount(8)'s: 1 for a bad invocation, 32 for a mount that failed.
`, defaultTimeout)
}

// invocation is one mount(8) call, parsed.
type invocation struct {
	device     string
	mountPoint string

	// options are the -o values in the order given, accumulated across repeated -o flags.
	options []string

	fake    bool // -f
	sloppy  bool // -s
	verbose bool // -v
	help    bool
}

// parseArgs reads mount(8)'s calling convention.
//
// Hand-rolled rather than a flag.FlagSet, because the two positional arguments come *first* and Go's
// flag package stops at the first non-flag argument — so every flag mount(8) appends would land in
// fs.Args() unparsed. The short flags also arrive combined (`-sfnv`), which flag does not accept in any
// form.
func parseArgs(args []string) (invocation, error) {
	var (
		inv        invocation
		positional []string
	)

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--help" || arg == "-h" || arg == "-help":
			inv.help = true

		case arg == "-o" || arg == "--options":
			if i+1 >= len(args) {
				return inv, errors.New("-o needs an option list after it")
			}
			i++
			inv.options = append(inv.options, splitOptions(args[i])...)

		// The attached form. mount(8) uses the separated one, but `mount -t objectfs -oro ...` reaches a
		// helper as `-oro` through some callers, and a helper that read that as five short flags would
		// refuse an invocation it was given correctly.
		case strings.HasPrefix(arg, "-o") && len(arg) > 2:
			inv.options = append(inv.options, splitOptions(arg[2:])...)

		// The type, which is already known: mount(8) passes it so that a single helper can serve several
		// types, and there is one here.
		case arg == "-t":
			if i+1 >= len(args) {
				return inv, errors.New("-t needs a type after it")
			}
			i++

		case strings.HasPrefix(arg, "-t") && len(arg) > 2:

		// A run of single-letter flags, combined or not.
		case len(arg) > 1 && arg[0] == '-' && !strings.HasPrefix(arg, "--"):
			for _, c := range arg[1:] {
				switch c {
				case 'f':
					inv.fake = true
				case 's':
					inv.sloppy = true
				case 'v':
					inv.verbose = true
				case 'n':
					// Accepted and ignored. -n means "do not write /etc/mtab", and on every system that
					// has a mount helper directory /etc/mtab is a symlink to /proc/mounts, which the
					// kernel maintains and nothing here writes.
				default:
					return inv, fmt.Errorf("unknown flag -%c", c)
				}
			}

		case strings.HasPrefix(arg, "--"):
			return inv, fmt.Errorf("unknown flag %s", arg)

		default:
			positional = append(positional, arg)
		}
	}

	if inv.help {
		return inv, nil
	}

	switch len(positional) {
	case 0:
		return inv, errors.New("needs a storage URI and a mount point")
	case 1:
		return inv, fmt.Errorf("needs a mount point after %q", positional[0])
	case 2:
		inv.device, inv.mountPoint = positional[0], positional[1]
	default:
		return inv, fmt.Errorf("takes a storage URI and a mount point, got %d arguments: %s",
			len(positional), strings.Join(positional, " "))
	}

	return inv, nil
}

// splitOptions splits one -o value on commas, discarding empties.
//
// Empties are discarded rather than refused because `-o ` and a trailing comma both reach a helper from
// fstab lines that are otherwise fine — `defaults,` is a typo that changes nothing, and refusing it
// would fail a boot over a comma.
func splitOptions(s string) []string {
	var out []string

	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

// optionKind is what this helper does with one fstab option.
type optionKind int

const (
	// kindValue translates `key=value` into an `objectfs mount` flag and its argument.
	kindValue optionKind = iota

	// kindBool translates a bare `key` into a boolean flag.
	kindBool

	// kindIgnore is an option mount(8), systemd or `mount -a` reads for itself. It arrives here because
	// mount(8) passes the whole option list to a helper, and it is dropped because the filesystem was
	// never its audience.
	kindIgnore

	// kindLocal is consumed by this helper and not passed on.
	kindLocal

	// kindRefuse is an option ObjectFS cannot honor, where dropping it would change what the entry says.
	kindRefuse
)

// option is one row of optionTable.
type option struct {
	kind optionKind

	// flag is the `objectfs mount` flag, for kindValue and kindBool.
	flag string

	// why is the operator-facing reason, for kindRefuse. It has to say what to do instead: a refusal that
	// only says no leaves an fstab entry that cannot be fixed without reading this source.
	why string
}

// optionTable is every option this helper knows. Anything absent is refused — see the package comment.
var optionTable = map[string]option{
	// The six that reach `objectfs mount`. Each is a flag that exists; newMountFlagSet in
	// cmd/objectfs/main.go is the list, and an entry here for a flag that does not exist would fail at
	// the point of mounting with objectfs's own "flag provided but not defined".
	"config":          {kind: kindValue, flag: "--config"},
	"cache-size":      {kind: kindValue, flag: "--cache-size"},
	"log-level":       {kind: kindValue, flag: "--log-level"},
	"max-concurrency": {kind: kindValue, flag: "--max-concurrency"},
	"debug":           {kind: kindBool, flag: "--debug"},

	// Read by this helper.
	"mount-timeout": {kind: kindLocal},

	// Read by mount(8), `mount -a` or systemd, and never by a filesystem.
	"_netdev":  {kind: kindIgnore}, // ordering: mount after the network is up
	"auto":     {kind: kindIgnore}, // `mount -a` selection
	"noauto":   {kind: kindIgnore},
	"nofail":   {kind: kindIgnore}, // `mount -a` error handling
	"defaults": {kind: kindIgnore}, // a token that expands to kernel flags no helper mount uses
	"user":     {kind: kindIgnore}, // mount(8)'s own permission checks
	"nouser":   {kind: kindIgnore},
	"users":    {kind: kindIgnore},
	"owner":    {kind: kindIgnore},
	"group":    {kind: kindIgnore},
	"comment":  {kind: kindIgnore}, // an fstab comment, with a value
	"bind":     {kind: kindRefuse, why: "a bind mount does not go through a filesystem's mount helper"},

	// rw is the default, so accepting it is a true statement rather than a dropped one. ro is a real
	// translation as of #532 — it was a kindRefuse through v0.16.0, when the mount was always writable —
	// and the two are no longer asymmetric.
	//
	// ro is deliberately *not* `{kind: kindIgnore}` even though `rw` is. The difference is which way the
	// mistake falls: dropping `rw` leaves a writable mount, which is what the entry asked for, while
	// dropping `ro` would leave a writable mount where the entry asked for read-only. So `rw` can be a
	// no-op and `ro` has to reach a flag.
	//
	// That asymmetry also decides `-o ro,rw`, which is an entry an operator did not mean to write either
	// way: `ro` wins, whichever order the two appear in, because `rw` contributes nothing and `ro`
	// appends a flag. mount(8) would let the last one win. This does not, and the reason is that the two
	// outcomes are not equally bad — read-only where they meant read-write is a write that gets refused
	// and noticed, read-write where they meant read-only is bytes overwritten in a bucket they asked to
	// protect. Pinned by TestTranslate rather than left to fall out of the table.
	"rw": {kind: kindIgnore},
	"ro": {kind: kindBool, flag: "--read-only"},

	"uid": {kind: kindRefuse, why: "ObjectFS does not remap ownership. A file reports the uid of the " +
		"process that created it, and anything the kernel did not attribute reports the uid that ran " +
		"the mount; no flag or configuration key overrides either. Remove `uid=`, which a mount would " +
		"otherwise report ownership contradicting"},

	"gid": {kind: kindRefuse, why: "ObjectFS does not remap ownership. A file reports the gid of the " +
		"process that created it, and anything the kernel did not attribute reports the gid that ran " +
		"the mount; no flag or configuration key overrides either. Remove `gid=`, which a mount would " +
		"otherwise report ownership contradicting"},
}

// ignoredPrefixes are namespaces reserved for userspace by convention, so that nothing in them is ever
// passed to a filesystem. systemd's x-systemd.* lives here, and so does anything a future fstab
// generator adds, which is why this is a prefix rule and not ten more table rows.
var ignoredPrefixes = []string{"x-", "X-"}

// plan is a translated invocation: the `objectfs mount` argv, plus what this helper kept for itself.
type plan struct {
	argv    []string
	timeout time.Duration
}

// translate maps the fstab option list onto an `objectfs mount` command line.
//
// It returns warnings separately from the error because -s makes an unrecognized option a warning and
// the mount then proceeds: a function that folded the two together could not express "this went ahead
// and here is what it ignored", which is exactly what -s asks for.
func translate(inv invocation) (plan, []string, error) {
	p := plan{timeout: defaultTimeout}

	var (
		flags    []string
		warnings []string
	)

	for _, opt := range inv.options {
		key, value, hasValue := strings.Cut(opt, "=")

		if ignoredPrefix(key) {
			continue
		}

		known, ok := optionTable[key]
		if !ok {
			if inv.sloppy {
				warnings = append(warnings, fmt.Sprintf("ignoring unrecognized option %q, because -s "+
					"was given", opt))

				continue
			}

			return p, warnings, fmt.Errorf("unrecognized option %q. mount.objectfs --help lists the "+
				"options it translates; pass -s to ignore this one and mount anyway", opt)
		}

		switch known.kind {
		case kindRefuse:
			// -s deliberately does not reach here. See the package comment: "ignore options you do not
			// recognize" is a different instruction from "ignore options you recognize and cannot honor".
			return p, warnings, fmt.Errorf("cannot honor option %q: %s", opt, known.why)

		case kindIgnore:
			continue

		case kindLocal:
			if !hasValue {
				return p, warnings, fmt.Errorf("option %q needs a value, as %s=<duration>", opt, key)
			}

			d, err := time.ParseDuration(value)
			if err != nil {
				return p, warnings, fmt.Errorf("option %q: %q is not a duration; write it as 90s or "+
					"2m", opt, value)
			}
			if d <= 0 {
				return p, warnings, fmt.Errorf("option %q: a timeout of %s would fail the mount before "+
					"it was attempted", opt, value)
			}
			p.timeout = d

		case kindValue:
			if !hasValue || value == "" {
				return p, warnings, fmt.Errorf("option %q needs a value, as %s=<value>", opt, key)
			}
			flags = append(flags, known.flag, value)

		case kindBool:
			if hasValue {
				return p, warnings, fmt.Errorf("option %q takes no value; write it as %q", opt, key)
			}
			flags = append(flags, known.flag)
		}
	}

	// Flags first, then the two positional arguments, because that is the only order `objectfs mount`
	// accepts — it says so itself when a flag follows the storage URI, and its own flag.FlagSet stops at
	// the first non-flag argument.
	p.argv = append([]string{"mount"}, flags...)
	p.argv = append(p.argv, inv.device, inv.mountPoint)

	if inv.fake {
		// --dry-run validates and resolves both arguments without creating anything, which is what
		// mount(8) means by a fake mount. Appended last, where the flag parser still sees it: `objectfs
		// mount` puts every flag before the positional arguments, so this goes in ahead of them.
		p.argv = append([]string{"mount", "--dry-run"}, p.argv[1:]...)
	}

	return p, warnings, nil
}

func ignoredPrefix(key string) bool {
	for _, prefix := range ignoredPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}

	return false
}

// candidatePaths are where the objectfs binary is looked for, in order, when OBJECTFS_BINARY is unset.
//
// mount(8) runs a helper with a PATH of its own choosing, so LookPath is tried but not trusted, and the
// two package locations are named explicitly. /usr/bin is where the .deb and the .rpm put it;
// /usr/local/bin is where scripts/install.sh defaults.
var candidatePaths = []string{"/usr/bin/objectfs", "/usr/local/bin/objectfs"}

// objectfsPath finds the objectfs binary this helper drives.
//
// The error names every path tried, because "objectfs: not found" from inside a boot-time `mount -a` is
// the least actionable message this program could produce.
func objectfsPath() (string, error) {
	if p := os.Getenv("OBJECTFS_BINARY"); p != "" {
		if err := executable(p); err != nil {
			return "", fmt.Errorf("OBJECTFS_BINARY is %q, which %w", p, err)
		}

		return p, nil
	}

	tried := make([]string, 0, len(candidatePaths)+1)

	if p, err := exec.LookPath("objectfs"); err == nil {
		return p, nil
	}
	tried = append(tried, "objectfs on PATH ("+os.Getenv("PATH")+")")

	for _, p := range candidatePaths {
		if executable(p) == nil {
			return p, nil
		}
		tried = append(tried, p)
	}

	// Beside this helper, last. A helper installed to /usr/sbin has objectfs in /usr/bin rather than
	// beside it, so this is for an uninstalled pair — a build directory, or a tarball unpacked whole.
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), "objectfs")
		if executable(beside) == nil {
			return beside, nil
		}
		tried = append(tried, beside)
	}

	return "", fmt.Errorf("cannot find the objectfs binary. Tried: %s. Set OBJECTFS_BINARY to its "+
		"path, or install the objectfs package", strings.Join(tried, ", "))
}

// executable reports why a path is not a runnable file, or nil.
//
// It is called on OBJECTFS_BINARY, which is an environment variable and so tainted from gosec's point of
// view. Deciding whether an operator-supplied path is runnable is the whole job: refusing the taint would
// mean refusing to check, and the alternative to checking is exec'ing it and reporting "cannot run" with no
// reason. The caller of this program is already root and already choosing what mount(8) exec's, so a path
// it can reach here is one it could have put at /sbin/mount.objectfs directly.
//
// #nosec G703 -- an operator-supplied path is the input; see above.
func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot be read: %w", err)
	}
	if info.IsDir() {
		return errors.New("is a directory")
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("is not executable (mode %s)", info.Mode())
	}

	return nil
}
