package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// This file is #533: the release tarball carries mount.objectfs and scripts/install.sh installs,
// registers and removes it — so a tarball install is equivalent to a package install rather than
// silently lacking the one thing that makes `mount -t objectfs` and /etc/fstab work.
//
// Two kinds of assertion live here, and the second is the reason the file exists at all.
//
// The manifest half reads .goreleaser.yml. It is a gate on a fact that is invisible everywhere else: a
// tarball with no mount.objectfs in it installs cleanly, runs, reports its version, and then fails every
// fstab entry with mount(8)'s "unknown filesystem type 'objectfs'". Nothing in a build log distinguishes
// the two releases.
//
// The drift half is the answer to what #533 actually asked for. It asked for *one shared shell function*
// rather than a second copy of the registration logic, on the correct reasoning that two copies drift.
// There is nothing for the three consumers to share it through — install.sh is fetched over HTTPS and
// piped into bash, so it has no library beside it on disk, and dpkg runs prerm with the package's own
// files in whatever state a `--force` left them, so a scriptlet whose unlink silently no-ops because a
// sourced file was gone leaves exactly the dangling /sbin/mount.objectfs the function exists to prevent.
// The reasoning is written out at scripts/install.sh's "The /etc/fstab mount helper" banner.
//
// So the guard is behavioral instead: [TestBothInstallPathsRegisterTheHelperTheSameWay] runs *both
// copies* — the packaged scriptlet and install.sh's — through one table of cases and asserts they reach
// the same outcome. That is a stronger guarantee than a shared function, not a weaker one: a shared
// function is exercised once by whichever caller a test happened to drive, and these are exercised twice
// with the same expectations.

// mountHelperBuildID is the .goreleaser.yml build whose binary is the helper.
const mountHelperBuildID = "mount-helper"

// TestTheArchiveCarriesTheMountHelper is the manifest half, and the invariant it pins is the one v0.16.0
// shipped without.
//
// Through v0.16.0 the `mount-helper` build existed and was named only by `nfpms.ids`, so the helper
// reached a machine if and only if the operator installed a .deb or an .rpm. This project's documented
// install path is a tarball into ~/.local, because its users are frequently on a login node where they
// have no root — which made the helper unavailable to exactly the population that most needs `mount -a`
// to work.
//
// The `allow_different_binary_count` assertion is not decoration. goreleaser hard-fails the whole release
// when the per-platform binary count differs — "archive has different count of binaries for each
// platform, which may cause your users confusion" — and builds *nothing*, which is what happens here the
// moment that key is dropped, because the three Linux tarballs hold two binaries and the two darwin ones
// hold one. The asymmetry is deliberate: mount(8)'s helper protocol is util-linux's and there is no
// /sbin/mount.TYPE on macOS.
func TestTheArchiveCarriesTheMountHelper(t *testing.T) {
	t.Parallel()

	cfg := readGoreleaser(t)

	if len(cfg.Archives) != 1 {
		t.Fatalf("%s has %d `archives:` entries, and this package reads one", packagingFile,
			len(cfg.Archives))
	}

	archive := cfg.Archives[0]

	for _, want := range []string{"archives", mountHelperBuildID} {
		if !slices.Contains(archive.IDs, want) {
			t.Errorf("%s's archive does not name the %q build in `ids:` (it names %v).\n"+
				"Without %q the Linux tarballs carry no mount.objectfs, so `mount -t objectfs` and every "+
				"/etc/fstab entry fail with mount(8)'s \"unknown filesystem type 'objectfs'\" for anyone "+
				"who installed from a tarball — which is this project's default path, because its users "+
				"frequently have no root. Nothing in a build log distinguishes that release from a "+
				"correct one.", packagingFile, want, archive.IDs, mountHelperBuildID)
		}
	}

	// And not the `packages` build, whose binary is named `objectfs` rather than `objectfs-linux-amd64`.
	// Archiving it would produce a second set of five tarballs colliding with the first on name_template.
	if slices.Contains(archive.IDs, "packages") {
		t.Errorf("%s's archive names the \"packages\" build, whose binary is `objectfs`. Its tarballs "+
			"would be named by the same name_template as the platform-named build's, so the two sets "+
			"collide asset for asset.", packagingFile)
	}

	if !archive.AllowDifferentBinaryCount {
		t.Errorf("%s's archive names more than one build but does not set "+
			"`allow_different_binary_count: true`. goreleaser refuses the whole release with \"archive "+
			"has different count of binaries for each platform\" and builds nothing — measured, not "+
			"predicted. The counts differ on purpose: three Linux tarballs hold two binaries and two "+
			"darwin tarballs hold one, because there is no /sbin/mount.TYPE on macOS for a darwin "+
			"mount.objectfs to be registered with.", packagingFile)
	}

	var helper *goreleaserBuild

	for i := range cfg.Builds {
		if cfg.Builds[i].ID == mountHelperBuildID {
			helper = &cfg.Builds[i]
		}
	}

	if helper == nil {
		t.Fatalf("%s has no build with id %q, so the `ids:` entry above names nothing and goreleaser "+
			"fails on an unknown build id", packagingFile, mountHelperBuildID)
	}

	// The filename *is* the registration — mount(8) exec's /sbin/mount.$TYPE, a path compiled into
	// util-linux — so this string is not a label. `objectfs-mount` or `mountobjectfs` would produce a
	// binary that installs, links, and is never reached by anything.
	if got := strings.TrimSpace(helper.Binary); got != "mount.objectfs" {
		t.Errorf("%s's %q build names its binary %q, want \"mount.objectfs\". mount(8) does not search "+
			"for a helper: it exec's /sbin/mount.$TYPE, so the filename is the whole registration "+
			"mechanism and any other spelling is a binary nothing will ever run. scripts/install.sh, "+
			"scripts/postinstall.sh and scripts/preremove.sh all hardcode the same string.",
			packagingFile, mountHelperBuildID, got)
	}

	if len(helper.Goos) != 1 || helper.Goos[0] != "linux" {
		t.Errorf("%s's %q build targets %v, want linux only. A darwin mount.objectfs has nothing to "+
			"register it with, and adding one changes what `allow_different_binary_count` is papering "+
			"over — the counts would match and the binary would still be inert.",
			packagingFile, mountHelperBuildID, helper.Goos)
	}
}

// ------------------------------------------------------------------------------------------------
// The drift half: both copies of the registration logic, through one table.
// ------------------------------------------------------------------------------------------------

// helperLinkCase is one state /sbin/mount.objectfs can be in when a registration runs.
type helperLinkCase struct {
	name string

	// place puts the case's starting state under root. target is the path *that runner's* script links
	// to, which differs between the two — /usr/bin/mount.objectfs for the package, $PREFIX/bin/
	// mount.objectfs for the tarball — so a case that stages "the link we would have made" has to be
	// told which one it is staging.
	place func(t *testing.T, root, target string)

	// want is the state of $ROOT/sbin/mount.objectfs afterwards, in describeHelperLink's encoding:
	// "" for nothing at that path, "file:<contents>" for a regular file, and otherwise the symlink
	// target, where the token $TARGET stands for the runner's own target path.
	want string

	// says is a substring both scripts must print. Every wrong reason for leaving a path alone is
	// still leaving it alone, so the message is what says which check fired.
	says string

	// silent demands that neither script mention the helper at all. This is the re-run case, and it is
	// an assertion about operator attention: a package scriptlet warns on every `apt upgrade`, so a
	// warning about a link the installer made itself trains operators to ignore the stream where the
	// checks that matter are printed.
	silent bool

	// remedy demands that the message carry the `ln -s` that finishes the job by hand.
	//
	// Not every refusal has one, which is why this is per case rather than implied by [says]. When the
	// helper binary itself is absent there is nothing to link *to*, and both scripts say so instead —
	// the package's copy calls it a packaging problem to report, because that is what it is. A message
	// offering `ln -s <a path that is not there> /sbin/mount.objectfs` would talk an operator into
	// creating the dangling link both scripts exist to avoid.
	remedy bool
}

// helperLinkCases is the shared table. Six states, which is every branch both implementations have.
var helperLinkCases = []helperLinkCase{
	{
		name:  "nothing there, which is a first install",
		place: func(*testing.T, string, string) {},
		want:  "$TARGET",
	},
	{
		name: "our own link, which is every re-run and every upgrade",
		place: func(t *testing.T, root, target string) {
			t.Helper()

			if err := os.Symlink(target, mountHelperLink(root)); err != nil {
				t.Fatalf("stage our own link: %v", err)
			}
		},
		want:   "$TARGET",
		silent: true,
	},
	{
		name: "a symlink an operator repointed somewhere else",
		place: func(t *testing.T, root, _ string) {
			t.Helper()

			if err := os.Symlink("/opt/objectfs/bin/mount.objectfs", mountHelperLink(root)); err != nil {
				t.Fatalf("stage a foreign link: %v", err)
			}
		},
		want:   "/opt/objectfs/bin/mount.objectfs",
		says:   "is a symlink to /opt/objectfs/bin/mount.objectfs",
		remedy: true,
	},
	{
		name: "a real file, which is how another package would have shipped a helper",
		place: func(t *testing.T, root, _ string) {
			t.Helper()

			body := "#!/bin/sh\necho someone else's helper\n"
			if err := os.WriteFile(mountHelperLink(root), []byte(body), 0o755); err != nil { // #nosec G306 -- a mount helper is world-executable
				t.Fatalf("stage a real file: %v", err)
			}
		},
		want:   "file:#!/bin/sh\necho someone else's helper\n",
		says:   "exists and is not a symlink",
		remedy: true,
	},
	{
		name: "no /sbin at all, which is a chroot or a minimal image",
		place: func(t *testing.T, root, _ string) {
			t.Helper()

			if err := os.Remove(filepath.Join(root, "sbin")); err != nil {
				t.Fatalf("remove the staged /sbin: %v", err)
			}
		},
		want:   "",
		says:   "does not exist, so the mount helper could not be registered",
		remedy: true,
	},
	{
		name: "the helper binary is not there",
		place: func(t *testing.T, root, _ string) {
			t.Helper()

			if err := os.Remove(filepath.Join(root, "usr", "bin", "mount.objectfs")); err != nil {
				t.Fatalf("remove the staged helper: %v", err)
			}
		},
		// Emphatically not a link. A dangling /sbin/mount.objectfs is worse than none: mount(8) reports
		// its own "no such file or directory" against a helper path rather than "unknown filesystem
		// type", which sends the reader looking for a missing mount point.
		want: "",
		says: "so 'mount -t objectfs' and /etc/fstab entries will not work",
	},
}

// helperLinkRunner is one of the two implementations, reduced to "register the helper under this root".
type helperLinkRunner struct {
	name string

	// target is the path this implementation links /sbin/mount.objectfs to, given a scratch root.
	target func(root string) string

	// register runs the implementation's registration step.
	register func(t *testing.T, root, target string) scriptRun
}

// helperLinkRunners is the pair the drift gate compares.
//
// The package path's target is the *unprefixed* /usr/bin/mount.objectfs, because a link made under
// OBJECTFS_ROOT still has to point where the path will be on the real filesystem — a scriptlet that
// interpolated its ROOT into the target would build a package installing a link into nowhere. The
// tarball path's target is genuinely absolute and genuinely inside the scratch root, because --prefix is
// where the file really is.
var helperLinkRunners = []helperLinkRunner{
	{
		name:   "postinstall.sh",
		target: func(string) string { return "/usr/bin/mount.objectfs" },
		register: func(t *testing.T, root, _ string) scriptRun {
			t.Helper()

			return runScript(t, "postinstall.sh", root, []string{"configure"})
		},
	},
	{
		name: "install.sh link_mount_helper",
		target: func(root string) string {
			return filepath.Join(root, "usr", "bin", "mount.objectfs")
		},
		register: func(t *testing.T, root, target string) scriptRun {
			t.Helper()

			return sourceInstallScript(t, root, `link_mount_helper "$TARGET"`, "TARGET="+target)
		},
	},
}

// TestBothInstallPathsRegisterTheHelperTheSameWay is the anti-drift gate #533 asked for, built as a
// shared table rather than a shared function.
//
// The two implementations are scripts/postinstall.sh's link_mount_helper and scripts/install.sh's. They
// differ in exactly two things that are not behavior — which path they link to, and whether they report
// through `warn` or `say` — and in nothing else. Both must be idempotent, both must refuse to touch
// anything they did not create, both must name the `ln -s` that would fix what they refused, and neither
// may ever create a dangling link.
//
// Running both is what makes this worth more than the shared function would have been. A shared function
// is exercised by whichever caller a test drives; this exercises the copy the .deb runs *and* the copy a
// piped-from-curl install runs, against the same expectations, so a fix applied to one and not the other
// is a red test naming which.
func TestBothInstallPathsRegisterTheHelperTheSameWay(t *testing.T) {
	t.Parallel()

	for _, runner := range helperLinkRunners {
		t.Run(runner.name, func(t *testing.T) {
			t.Parallel()

			for _, tc := range helperLinkCases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()

					root := stagedRoot(t)
					target := runner.target(root)

					tc.place(t, root, target)

					run := runner.register(t, root, target)

					if run.exit != 0 {
						t.Fatalf("exited %d. Failing to register a helper is not a reason to fail an "+
							"install that has already put a working objectfs on PATH, and on the package "+
							"path a non-zero prerm blocks every subsequent apt operation.\nstderr:\n%s",
							run.exit, run.stderr)
					}

					want := strings.ReplaceAll(tc.want, "$TARGET", target)

					if got := describeHelperLink(t, root); got != want {
						t.Errorf("/sbin/mount.objectfs is %s, want %s", describe(got), describe(want))
					}

					if tc.says != "" && !strings.Contains(run.stderr, tc.says) {
						t.Errorf("nothing printed containing %q. Every wrong reason for leaving the path "+
							"alone is still leaving it alone, so the message is the only thing that says "+
							"which check fired.\nstderr:\n%s", tc.says, run.stderr)
					}

					if tc.silent && strings.Contains(run.stderr, "mount.objectfs") {
						t.Errorf("mentioned the helper on a run that had nothing to do. This case is "+
							"every re-run and every `apt upgrade`; a warning about a link the installer "+
							"made itself trains operators to ignore the stream the checks that matter "+
							"are printed on.\nstderr:\n%s", run.stderr)
					}

					if tc.remedy && !strings.Contains(run.stderr, "ln -s") {
						t.Errorf("refused without naming the `ln -s` that would fix it. A message saying "+
							"a path was left alone and not saying what to run instead leaves the operator "+
							"to reconstruct a symlink command against a target they have to look "+
							"up.\nstderr:\n%s", run.stderr)
					}

					// And the inverse, which is the dangerous direction. With no helper binary on disk
					// there is nothing to link to, so an `ln -s` here would be advice to create the
					// dangling /sbin/mount.objectfs both scripts refuse to create themselves — and a
					// dangling helper is worse than an absent one, because mount(8) then reports its own
					// "no such file or directory" against a helper path instead of "unknown filesystem
					// type 'objectfs'".
					if !tc.remedy && strings.Contains(run.stderr, "ln -s /") {
						t.Errorf("offered an `ln -s` for a case that has nothing to link to.\nstderr:\n%s",
							run.stderr)
					}
				})
			}
		})
	}
}

// TestBothUninstallPathsUnlinkTheHelperTheSameWay is the same gate on the way out.
//
// The link is in neither dpkg's nor rpm's file database, by design — see scripts/postinstall.sh's
// aliased-directory reasoning — so removing it is the removal path's own job in both worlds, and the rule
// is the same one as on the way in: only a symlink, and only one pointing where this installer would have
// pointed it. A link a .deb created, on a machine that also has a tarball install, belongs to an fstab
// entry that has nothing to do with the prefix being removed.
func TestBothUninstallPathsUnlinkTheHelperTheSameWay(t *testing.T) {
	t.Parallel()

	runners := []struct {
		name       string
		target     func(root string) string
		unregister func(t *testing.T, root, target string) scriptRun
	}{
		{
			name:   "preremove.sh",
			target: func(string) string { return "/usr/bin/mount.objectfs" },
			unregister: func(t *testing.T, root, _ string) scriptRun {
				t.Helper()

				return runScript(t, "preremove.sh", root, []string{"remove"})
			},
		},
		{
			name: "install.sh unlink_mount_helper",
			target: func(root string) string {
				return filepath.Join(root, "usr", "bin", "mount.objectfs")
			},
			unregister: func(t *testing.T, root, target string) scriptRun {
				t.Helper()

				return sourceInstallScript(t, root, `unlink_mount_helper "$TARGET"`, "TARGET="+target)
			},
		},
	}

	cases := []struct {
		name  string
		place func(t *testing.T, root, target string)
		want  string
	}{
		{
			name: "our own link is removed",
			place: func(t *testing.T, root, target string) {
				t.Helper()

				if err := os.Symlink(target, mountHelperLink(root)); err != nil {
					t.Fatalf("stage our own link: %v", err)
				}
			},
			want: "",
		},
		{
			name: "a link somewhere else is left alone",
			place: func(t *testing.T, root, _ string) {
				t.Helper()

				if err := os.Symlink("/opt/objectfs/bin/mount.objectfs", mountHelperLink(root)); err != nil {
					t.Fatalf("stage a foreign link: %v", err)
				}
			},
			want: "/opt/objectfs/bin/mount.objectfs",
		},
		{
			name: "a real file is left alone",
			place: func(t *testing.T, root, _ string) {
				t.Helper()

				if err := os.WriteFile(mountHelperLink(root), []byte("someone else's helper\n"), 0o755); err != nil { // #nosec G306 -- a mount helper is world-executable
					t.Fatalf("stage a real file: %v", err)
				}
			},
			want: "file:someone else's helper\n",
		},
		{
			name:  "nothing there is not an error",
			place: func(*testing.T, string, string) {},
			want:  "",
		},
	}

	for _, runner := range runners {
		t.Run(runner.name, func(t *testing.T) {
			t.Parallel()

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()

					// preremove.sh reads /proc/mounts and unmounts what it finds, so the root needs an
					// empty mount table as well as the staged binary — an unreadable one would make the
					// script skip a step this test is not about, silently.
					root := rootWithMounts(t, "")
					stageMountHelper(t, root)

					target := runner.target(root)

					tc.place(t, root, target)

					run := runner.unregister(t, root, target)

					if run.exit != 0 {
						t.Fatalf("exited %d\nstderr:\n%s", run.exit, run.stderr)
					}

					want := strings.ReplaceAll(tc.want, "$TARGET", target)

					if got := describeHelperLink(t, root); got != want {
						t.Errorf("/sbin/mount.objectfs is %s, want %s", describe(got), describe(want))
					}
				})
			}
		})
	}
}

// TestBothScriptsAgreeOnWhatAMountedObjectfsLooksLike is the defect scripts/preremove.sh shipped with,
// stated so that install.sh's copy cannot reintroduce it.
//
// internal/fuse sets Subtype "s3" alongside FSName "objectfs", so the kernel records an ObjectFS mount as
// fstype `fuse.s3` with device `objectfs`. A grep for `fuse.objectfs` — the obvious single match, and the
// one preremove.sh had for several releases — matched nothing on a real mount, so the unmount loop it was
// written to perform had never once run.
//
// All three forms are checked because the fstype depends on mount options a user can change, and the
// union is still narrow enough that no non-ObjectFS FUSE filesystem satisfies it. This runs both scripts
// against the same /proc/mounts rather than grepping their source, because "matches" is a claim about
// behavior and the three cases are three arms of one `case` statement that a rewrite can reorder.
func TestBothScriptsAgreeOnWhatAMountedObjectfsLooksLike(t *testing.T) {
	t.Parallel()

	// One row per form the kernel can record, plus two that must not match. The non-matching rows are
	// the non-vacuity guard: a function that printed every line of /proc/mounts would satisfy every
	// positive row and nothing else here.
	//
	// /mnt/renamed is the row that makes the first `case` arm load-bearing, and it is here because the
	// obvious fixture did not. With `objectfs` as the device on every ObjectFS row, all three arms agree
	// on all three rows — deleting the whole `fuse.objectfs |` alternative changed no outcome, measured by
	// mutation — because the third arm's `[ "$device" = "objectfs" ]` already covers them. A device that is
	// *not* objectfs with an fstype that is literally fuse.objectfs can only be matched by the first arm.
	//
	// `| fuse.s3` is the one alternative this fixture still cannot isolate, and the reason is worth
	// recording rather than papering over with a row that asserts a rule nobody has decided.
	// internal/adapter.buildMountOptions hardcodes `FSName: "objectfs"` — the operator-facing key was
	// removed by #180 for lack of a reader — so every mount ObjectFS can currently produce has
	// device=objectfs and is caught by the third arm regardless. Making it isolable means asserting that
	// *any* fuse.s3 mount is ObjectFS whatever its device, and that is a claim about other people's
	// software: `-o subtype=s3` is not reserved, and treating a stranger's mount as ours would make
	// `--uninstall` refuse over a filesystem it has nothing to do with. Narrowing the union instead is a
	// change to a shipped scriptlet's matching rule and belongs in its own issue, not in #533.
	const mounts = "objectfs /mnt/plain fuse.objectfs rw,nosuid,nodev 0 0\n" +
		"notobjectfs /mnt/renamed fuse.objectfs rw,nosuid,nodev 0 0\n" +
		"objectfs /mnt/subtype fuse.s3 rw,nosuid,nodev 0 0\n" +
		"objectfs /mnt/device fuse.somethingelse rw,nosuid,nodev 0 0\n" +
		"sshfs /mnt/other fuse.sshfs rw,nosuid,nodev 0 0\n" +
		"/dev/sda1 / ext4 rw,relatime 0 0\n"

	want := []string{"/mnt/plain", "/mnt/renamed", "/mnt/subtype", "/mnt/device"}

	runners := []struct {
		name string
		run  func(t *testing.T, root string) scriptRun
	}{
		{
			name: "install.sh live_mounts",
			run: func(t *testing.T, root string) scriptRun {
				t.Helper()

				return sourceInstallScript(t, root, `live_mounts`)
			},
		},
		{
			// preremove.sh's objectfs_mountpoints is not reachable without running the whole script, and
			// running it would attempt five unmounts. So this drives the same function the same way
			// install.sh's copy is driven — sourced — which is safe here because the file has no
			// top-level side effects beyond reading $1 into ACTION.
			name: "preremove.sh objectfs_mountpoints",
			run: func(t *testing.T, root string) scriptRun {
				t.Helper()

				return sourceScript(t, "preremove.sh", root, `objectfs_mountpoints`)
			},
		},
	}

	for _, runner := range runners {
		t.Run(runner.name, func(t *testing.T) {
			t.Parallel()

			root := rootWithMounts(t, mounts)

			run := runner.run(t, root)

			if run.exit != 0 {
				t.Fatalf("exited %d\nstderr:\n%s", run.exit, run.stderr)
			}

			var got []string

			for line := range strings.SplitSeq(strings.TrimSpace(run.stdout), "\n") {
				if line != "" {
					got = append(got, line)
				}
			}

			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("found mounts %v, want %v.\n"+
					"All three of fstype fuse.objectfs, fstype fuse.s3, and any fuse.* with a device of "+
					"objectfs have to match: internal/fuse sets Subtype \"s3\" alongside FSName "+
					"\"objectfs\", so a real mount records fuse.s3 and a grep for fuse.objectfs alone "+
					"finds nothing — which is the defect preremove.sh shipped with for several releases. "+
					"And /mnt/other and / must not match, or the uninstall refuses on an unrelated FUSE "+
					"filesystem.", got, want)
			}
		})
	}
}

// TestInstallScriptRefusesToRegisterWithoutRoot is the security half, and the case the issue did not
// mention.
//
// mount(8) exec's /sbin/mount.objectfs **as root**. A link from there into a directory whose owner is not
// root lets that owner replace the binary and run code as root the next time anyone mounts anything —
// and `sudo ./install.sh` with this script's default prefix is precisely that shape, a root-created link
// into $HOME/.local/bin. That is also the most likely way someone reaches this code, since the natural
// reading of "registering needs root" is to re-run the whole installer under sudo.
//
// The refusal is asserted in two ways because neither alone is enough. The unprivileged branch is *run*,
// because that is the branch a user without sudo hits and its message is the entire deliverable — one
// `ln -s` they can paste. The ownership branch cannot be run: reaching it needs uid 0, and a test suite
// that runs as root to check a privilege-escalation guard is a worse idea than the guard. So it is
// asserted against the source, on the narrow fact that `-O` is present on both the target and its
// parent — a root-owned file inside someone else's directory can be swapped by replacing the entry.
func TestInstallScriptRefusesToRegisterWithoutRoot(t *testing.T) {
	t.Parallel()

	// OBJECTFS_ROOT deliberately unset, because may_register short-circuits under a scratch root: there
	// is neither a real /sbin to protect nor a root to protect it from, and every other test here runs
	// unprivileged. With it unset this reaches the real check, and may_register writes nothing.
	run := sourceInstallScript(t, "", `if may_register "$TARGET"; then echo PROCEED; else echo REFUSED; fi`,
		"TARGET=/tmp/nonexistent/mount.objectfs")

	if run.exit != 0 {
		t.Fatalf("exited %d\nstderr:\n%s", run.exit, run.stderr)
	}

	if !strings.Contains(run.stdout, "REFUSED") {
		t.Fatalf("may_register allowed /sbin/mount.objectfs to be created by an unprivileged process. "+
			"`ln -s` into /sbin would fail anyway, but the failure would be reported as \"could not "+
			"create\" — an I/O problem — rather than as the one command left to run.\nstdout:\n%s",
			run.stdout)
	}

	if !strings.Contains(run.stderr, "not root") {
		t.Errorf("the refusal does not say root is what is missing.\nstderr:\n%s", run.stderr)
	}

	if !strings.Contains(run.stderr, "sudo ln -s") {
		t.Errorf("the refusal does not carry the `sudo ln -s` that finishes the job. A tarball install "+
			"without root is the common case on the login nodes this project targets, and the whole "+
			"deliverable for that user is one command they can paste.\nstderr:\n%s", run.stderr)
	}

	// The ownership check, statically. `-O` is "owned by the effective uid", which is 0 by the time it is
	// evaluated, so it asks whether root owns the path.
	body := functionBody(installScript(t), "may_register()")
	if body == "" {
		t.Fatal("scripts/install.sh has no may_register function")
	}

	for _, want := range []string{`[ ! -O "$target" ]`, `[ ! -O "$(dirname "$target")" ]`} {
		if !strings.Contains(body, want) {
			t.Errorf("may_register does not test %s.\n"+
				"mount(8) runs /sbin/mount.objectfs as root, so a link from there into a prefix whose "+
				"owner is not root is a local privilege escalation — and `sudo ./install.sh` with the "+
				"default ~/.local prefix is exactly that. Both the binary and its directory have to be "+
				"root-owned: a root-owned file inside a directory someone else owns can be swapped out "+
				"by replacing the directory entry.\nmay_register is:\n%s", want, body)
		}
	}
}

// TestInstallScriptUninstallRemovesWhatItInstalled is the counterpart scripts/preremove.sh has had all
// along and install.sh never did.
//
// A tarball install created /sbin/mount.objectfs and there was no supported way to remove it, so the
// only trace of ObjectFS an operator could not clean up by deleting a directory was the one that breaks
// `mount -a` when it dangles.
func TestInstallScriptUninstallRemovesWhatItInstalled(t *testing.T) {
	t.Parallel()

	root := rootWithMounts(t, "")
	prefix := stageTarballInstall(t, root)

	run := runScript(t, "install.sh", root, []string{"--uninstall", "--prefix", prefix}, "HOME="+root)

	if run.exit != 0 {
		t.Fatalf("exited %d\nstderr:\n%s", run.exit, run.stderr)
	}

	for _, gone := range []string{
		filepath.Join(prefix, "bin", "objectfs"),
		filepath.Join(prefix, "bin", "mount.objectfs"),
	} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s is still there after --uninstall", gone)
		}
	}

	if got := describeHelperLink(t, root); got != "" {
		t.Errorf("/sbin/mount.objectfs is still %s. Left behind after the binary it points at is "+
			"deleted it is a dangling link, and an fstab entry that worked before the uninstall then "+
			"fails at `mount -a` with mount(8)'s own \"no such file or directory\" against a helper "+
			"path — which says considerably less than \"unknown filesystem type\".", describe(got))
	}

	// The paths it must not touch, named rather than deleted. An uninstaller that removes a cache
	// directory nobody asked it to remove is the failure preremove.sh prints the same paragraph about.
	if !strings.Contains(run.stderr, "/etc/objectfs") {
		t.Errorf("the uninstall does not say which paths it left behind, so an operator who wants every "+
			"trace gone has to guess.\nstderr:\n%s", run.stderr)
	}
}

// TestInstallScriptUninstallRefusesWhileMounted is the one path in install.sh that fails rather than
// warning, and the reason is the same one preremove.sh gives for being the only scriptlet that does not
// exit 0 unconditionally.
//
// Deleting the binary out from under a live FUSE mount hangs every read against the mount point — `ls`
// blocks in the kernel — and the way out is a manual fusermount -u by someone who first has to work out
// that is what happened. `objectfs unmount` is also the only unmount path that reports which methods it
// tried and what is holding the mount open, and it is still installed right up until this function
// deletes it.
func TestInstallScriptUninstallRefusesWhileMounted(t *testing.T) {
	t.Parallel()

	root := rootWithMounts(t, "objectfs /mnt/objectfs fuse.s3 rw,nosuid,nodev 0 0\n")
	prefix := stageTarballInstall(t, root)

	before := treeState(t, root)

	run := runScript(t, "install.sh", root, []string{"--uninstall", "--prefix", prefix}, "HOME="+root)

	if run.exit == 0 {
		t.Fatalf("--uninstall succeeded with a live ObjectFS mount. Deleting the binary hangs every "+
			"read against the mount point until someone unmounts it by hand.\nstderr:\n%s", run.stderr)
	}

	if !strings.Contains(run.stderr, "/mnt/objectfs") {
		t.Errorf("the refusal does not name the mount that caused it.\nstderr:\n%s", run.stderr)
	}

	if !strings.Contains(run.stderr, "objectfs unmount /mnt/objectfs") {
		t.Errorf("the refusal does not carry the command that clears it. `objectfs unmount` is the only "+
			"unmount path that reports which methods it tried and what is holding the mount open, and it "+
			"is still installed at this point.\nstderr:\n%s", run.stderr)
	}

	// Nothing removed, not "most things removed". A refusal that had already deleted the helper would
	// leave the operator with a half-uninstalled tree and a live mount.
	if got := treeState(t, root); !sameTree(before, got) {
		t.Errorf("the refusal changed the tree:\nbefore: %v\nafter:  %v", before, got)
	}
}

// TestInstallScriptDryRunUninstallRemovesNothing is the flag meaning what it says on the path where
// getting it wrong is unrecoverable. --dry-run is documented as "download, verify and remove nothing".
func TestInstallScriptDryRunUninstallRemovesNothing(t *testing.T) {
	t.Parallel()

	root := rootWithMounts(t, "")
	prefix := stageTarballInstall(t, root)

	before := treeState(t, root)

	run := runScript(t, "install.sh", root,
		[]string{"--uninstall", "--dry-run", "--prefix", prefix}, "HOME="+root)

	if run.exit != 0 {
		t.Fatalf("exited %d\nstderr:\n%s", run.exit, run.stderr)
	}

	if got := treeState(t, root); !sameTree(before, got) {
		t.Errorf("--dry-run --uninstall changed the tree:\nbefore: %v\nafter:  %v", before, got)
	}

	if !strings.Contains(run.stderr, "would remove") {
		t.Errorf("--dry-run --uninstall did not report what it would have removed, which leaves the "+
			"flag with no output to distinguish it from a no-op.\nstderr:\n%s", run.stderr)
	}
}

// TestInstallScriptCanDeclineTheMountHelper covers --no-mount-helper, the escape hatch for a prefix that
// is only ever used interactively.
//
// A static assertion, because reaching install_mount_helper through main needs a download, a checksum and
// an extract. What is checked is that the flag is parsed into the variable the function reads and that the
// function returns before doing anything — the failure mode being a flag that is accepted, documented, and
// wired to nothing, which is #180's defect class and this repository has shipped it before.
func TestInstallScriptCanDeclineTheMountHelper(t *testing.T) {
	t.Parallel()

	script := installScript(t)

	if !strings.Contains(script, "        --no-mount-helper)") {
		t.Error("scripts/install.sh has no --no-mount-helper arm in its argument loop, so the flag " +
			"documented in usage() is rejected as an unrecognized argument")
	}

	body := functionBody(script, "install_mount_helper()")
	if body == "" {
		t.Fatal("scripts/install.sh has no install_mount_helper function")
	}

	if !strings.Contains(body, `[ "$MOUNT_HELPER" -eq 0 ]`) {
		t.Errorf("install_mount_helper does not read MOUNT_HELPER, so --no-mount-helper parses and then "+
			"does nothing — a flag accepted, documented and wired to nothing.\ninstall_mount_helper "+
			"is:\n%s", body)
	}
}

// ------------------------------------------------------------------------------------------------
// Harness.
// ------------------------------------------------------------------------------------------------

// sourceInstallScript sources scripts/install.sh and runs a snippet against its functions.
//
// OBJECTFS_INSTALL_SOURCE_ONLY is the seam, and it exists because there is no way to reach
// link_mount_helper through main: the link is made after a download, a checksum and an extract, none of
// which a unit test can or should perform. A root of "" leaves OBJECTFS_ROOT unset, which is the only way
// to reach may_register's real checks.
//
// **Values reach the snippet through the environment and never as positional arguments.** install.sh's
// `while [ $# -gt 0 ]` argument loop runs at file scope, so a sourced invocation inherits the caller's
// $@ — `bash -c '. install.sh; f "$1"' _ /some/path` feeds /some/path to that loop, which prints the
// usage text and dies on "unrecognized argument". Measured, on the first attempt at this helper.
func sourceInstallScript(t *testing.T, root, snippet string, extraEnv ...string) scriptRun {
	t.Helper()

	return sourceShell(t, "install.sh", root, snippet,
		append([]string{"OBJECTFS_INSTALL_SOURCE_ONLY=1"}, extraEnv...)...)
}

// sourceScript sources a maintainer script without running its main.
//
// postinstall.sh and preremove.sh both end in a `main "$@"` call, so this cuts the file at that line
// rather than reading it whole. Cutting rather than adding a guard to those two: a package scriptlet that
// checks an environment variable before doing its job has a way to be told not to do its job, and dpkg
// runs it with whatever environment the invoking session had.
func sourceScript(t *testing.T, name, root, snippet string) scriptRun {
	t.Helper()

	full := readFile(t, filepath.Join(repoRoot(t), "scripts", name))

	// A bare `main`, not `main "$@"`: both scriptlets read their action argument into ACTION at file
	// scope and take no parameters, which is what makes cutting here enough.
	body, _, found := strings.Cut(full, "\nmain\n")
	if !found {
		t.Fatalf("scripts/%s does not have a `main` call on a line of its own, so this helper cannot "+
			"source it without running it", name)
	}

	// 0600, not 0700: the copy is *sourced*, not executed, so it needs no execute bit at all.
	cut := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(cut, []byte(body), 0o600); err != nil {
		t.Fatalf("write the cut copy of %s: %v", name, err)
	}

	return sourceShell(t, "", root, snippet, "SCRIPT="+cut)
}

// sourceShell runs `bash -c '. "$SCRIPT"; <snippet>'` with an explicit environment.
//
// name, when non-empty, points SCRIPT at scripts/<name>; otherwise the caller supplies SCRIPT through
// extraEnv. The snippet is passed to bash as a literal string with no interpolation of test values into
// it — every value the snippet needs arrives as an environment variable it dereferences.
func sourceShell(t *testing.T, name, root, snippet string, extraEnv ...string) scriptRun {
	t.Helper()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash is not on PATH: %v", err)
	}

	// HOME and PREFIX because install.sh runs `PREFIX="${PREFIX:-$HOME/.local}"` at file scope under
	// `set -u`, so an unset HOME is a fatal error before any function is defined.
	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + t.TempDir(),
	}

	if name != "" {
		env = append(env, "SCRIPT="+filepath.Join(repoRoot(t), "scripts", name))
	}

	if root != "" {
		env = append(env, "OBJECTFS_ROOT="+root)
	}

	env = append(env, extraEnv...)

	dir := root
	if dir == "" {
		dir = t.TempDir()
	}

	// #nosec G204 -- snippet is a literal in this file; every test value reaches it through env, which is
	// the point rather than an accident: install.sh's argument loop runs at file scope, so a sourced
	// invocation inherits whatever positionals the caller passed.
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `. "$SCRIPT"`+"\n"+snippet+"\n")
	cmd.Dir = dir
	cmd.Env = env

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	run := scriptRun{stdout: stdout.String(), stderr: stderr.String()}

	if err != nil {
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) {
			t.Fatalf("sourcing %q: %v\nstdout:\n%s\nstderr:\n%s", snippet, err, run.stdout, run.stderr)
		}

		run.exit = exitErr.ExitCode()
	}

	return run
}

// stageTarballInstall builds what scripts/install.sh leaves behind on a successful run: the two binaries
// under a prefix, and the /sbin symlink pointing at the helper among them.
//
// The link target is the absolute path inside the scratch root, because that is genuinely where the file
// is — unlike the package path, whose target is the unprefixed /usr/bin/mount.objectfs.
func stageTarballInstall(t *testing.T, root string) string {
	t.Helper()

	prefix := filepath.Join(root, "opt", "objectfs")

	bin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil { // #nosec G301 -- a bin directory must be traversable
		t.Fatalf("mkdir %s: %v", bin, err)
	}

	for _, name := range []string{"objectfs", "mount.objectfs"} {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { // #nosec G306 -- an installed binary is world-executable
			t.Fatalf("stage %s: %v", path, err)
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "sbin"), 0o755); err != nil { // #nosec G301 -- /sbin's real mode
		t.Fatalf("mkdir %s/sbin: %v", root, err)
	}

	if err := os.Symlink(filepath.Join(bin, "mount.objectfs"), mountHelperLink(root)); err != nil {
		t.Fatalf("stage the /sbin link: %v", err)
	}

	return prefix
}

// describeHelperLink encodes the state of $ROOT/sbin/mount.objectfs as a string, so that the three
// outcomes the tables distinguish compare with one `!=`.
//
// "" for nothing at that path, "file:<contents>" for a regular file, and the symlink target otherwise.
// A symlink is described by its target *without* resolving it: a dangling link is a distinct and worse
// outcome than no link, and os.Stat would report both as not existing.
func describeHelperLink(t *testing.T, root string) string {
	t.Helper()

	link := mountHelperLink(root)

	info, err := os.Lstat(link)
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("lstat %s: %v", link, err)
		}

		return ""
	}

	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(link)
		if readErr != nil {
			t.Fatalf("readlink %s: %v", link, readErr)
		}

		return target
	}

	return "file:" + readFile(t, link)
}

// describe renders describeHelperLink's encoding for a failure message.
func describe(state string) string {
	switch {
	case state == "":
		return "absent"
	case strings.HasPrefix(state, "file:"):
		return fmt.Sprintf("a regular file holding %q", strings.TrimPrefix(state, "file:"))
	default:
		return fmt.Sprintf("a symlink to %q", state)
	}
}

// sameTree compares two treeState maps.
func sameTree(before, after map[string]string) bool {
	if len(before) != len(after) {
		return false
	}

	for path, state := range before {
		if after[path] != state {
			return false
		}
	}

	return true
}
