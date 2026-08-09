#%Module1.0
#
# ObjectFS — a POSIX interface over AWS S3, for research computing.
#
# TCL Modules (Environment Modules) modulefile. The Lmod equivalent is objectfs.lua in this
# directory, and the two are held to the same exported environment by
# internal/config/modulefiles_test.go — a site running Environment Modules and a site running Lmod
# get the same variables, or that test fails.
#
# INSTALLED AS:  <prefix>/share/modulefiles/objectfs/<version>
#
# With NO .tcl extension. modulecmd.tcl treats a file's name as the version, so an installed
# objectfs.tcl would be the module `objectfs/objectfs.tcl`, and `module load objectfs/<version>` would
# not resolve. The .tcl suffix on the file in this repository is for editors and for the packaging
# rule; it is not part of the installed name.
#
# The Lmod file installs into the same directory as <version>.lua. Verified against Lmod 9.3 and
# Modules 5.6.1 with both present: Lmod loads the .lua, modulecmd.tcl loads the extensionless file,
# and neither reports the other. One tree serves both systems.
#
# The `#%Module1.0` magic cookie above must be the first line of the file. Without it modulecmd.tcl
# refuses the file, and Lmod's own error message for an unloadable module says so explicitly.
#
# There is no version number written down here, and there must not be. The authority is the
# `version` constant in cmd/objectfs/main.go; this file learns the version from its own filename, so
# the number exists once, in the install path.

proc ModulesHelp { } {
    puts stderr "ObjectFS presents an AWS S3 bucket as a POSIX filesystem over FUSE."
    puts stderr ""
    puts stderr "It is NOT a POSIX-compliant filesystem. Locks are not forwarded, so they are"
    puts stderr "host-local and invisible to any other mount of the same bucket; SQLite and anything"
    puts stderr "using POSIX record locks will not work correctly across nodes. The"
    puts stderr "supported-operations table in the documentation is the authority on what works and"
    puts stderr "what fails by design."
    puts stderr ""
    puts stderr "  Mount a bucket:"
    puts stderr "    objectfs mount s3://my-bucket /mnt/data"
    puts stderr ""
    puts stderr "  With a site configuration file:"
    puts stderr "    objectfs mount --config /etc/objectfs/config.yaml s3://my-bucket /mnt/data"
    puts stderr ""
    puts stderr "  Check a configuration without mounting:"
    puts stderr "    objectfs mount --dry-run --config /etc/objectfs/config.yaml s3://my-bucket /mnt/data"
    puts stderr ""
    puts stderr "  Unmount:"
    puts stderr "    objectfs unmount /mnt/data"
    puts stderr ""
    puts stderr "Flags come before the positional arguments. Go's flag package stops parsing at the"
    puts stderr "first argument that is not a flag, so a flag written after the storage URI is not"
    puts stderr "applied — the command reports that rather than ignoring it."
    puts stderr ""
    puts stderr "ObjectFS always runs in the foreground and does not fork. Under a batch scheduler,"
    puts stderr "mount as a prologue step or background the mount in the job script, and unmount in"
    puts stderr "the epilogue so that buffered writes are flushed to S3 before the allocation ends. A"
    puts stderr "job killed with the mount still running loses whatever has not been flushed."
    puts stderr ""
    puts stderr "Credentials do not come from the configuration file. ObjectFS uses the standard AWS"
    puts stderr "credential chain: AWS_PROFILE, the environment variables, the shared credentials"
    puts stderr "file, or an instance role."
    puts stderr ""
    puts stderr "Documentation: https://github.com/scttfrdmn/objectfs"
}

module-whatis "Name:        ObjectFS"
module-whatis "Description: A POSIX interface over AWS S3, for research computing"
module-whatis "URL:         https://github.com/scttfrdmn/objectfs"

# The version is this file's own name.
#
# NOT `file tail [file dirname $ModulesCurrentModulefile]`, which is what this file's first draft
# used. That takes the tail of the *parent directory*, so for the correct install layout
# objectfs/<version> it returns "objectfs" — the name, not the version. It happens to return the
# right string for the incorrect three-level layout the same draft implied, which is how the two
# mistakes concealed each other. Measured under Modules 5.6.1 both ways.
#
# `module-info version` is not the alternative: it returns the fully-qualified "objectfs/<version>",
# not the version alone. Measured, not assumed.
set ofsVersion [file tail $ModulesCurrentModulefile]

# A modulefile installed without a version level has its own name here. Reported as unknown rather
# than exported as a version of "objectfs", which would put OBJECTFS_VERSION=objectfs into every
# job's environment.
if { $ofsVersion eq "objectfs" || $ofsVersion eq "" } {
    set ofsVersion "unknown"
}

# ---------------------------------------------------------------------------------------------
# Locating the binary. The reasoning is in objectfs.lua and is not repeated here; the summary is
# that `prepend-path PATH /usr/bin` is not the no-op it appears to be under Lmod — it hoists
# /usr/bin ahead of any site directory and does not undo that on unload — and that nothing installs
# to the /usr/lib/objectfs/<version> the first draft computed.
#
# One measured difference between the two systems, which is why this file cannot simply be a
# transliteration: Modules 5.6.1's `prepend-path` DOES skip a directory already present in PATH,
# leaving the order untouched. Lmod 9.3's does not. The static check below therefore changes nothing
# about this file's behaviour under Modules and is kept anyway, so that the two files make the same
# decision from the same input rather than relying on one implementation being forgiving.
# ---------------------------------------------------------------------------------------------

# The directories already on every login PATH. A binary in one of these needs no PATH change.
# Deliberately a static property of the derived path rather than a scan of $::env(PATH) — see the
# corresponding comment in objectfs.lua for the unload-leak that a PATH-contents test causes.
set ofsSystemBinDirs [list /bin /usr/bin /usr/local/bin /sbin /usr/sbin]

# The install prefix. OBJECTFS_MODULE_PREFIX first, for a site whose module tree is not under the
# same prefix as the software; otherwise this file's own location, split on "/share/modulefiles/".
if { [info exists ::env(OBJECTFS_MODULE_PREFIX)] && $::env(OBJECTFS_MODULE_PREFIX) ne "" } {
    set ofsPrefix $::env(OBJECTFS_MODULE_PREFIX)
    set ofsPrefixSource "OBJECTFS_MODULE_PREFIX"
} else {
    set ofsPrefixSource "this modulefile's location"
    set ofsSelf $ModulesCurrentModulefile
    set ofsCut [string first "/share/modulefiles/" $ofsSelf]

    if { $ofsCut >= 0 } {
        set ofsPrefix [string range $ofsSelf 0 [expr {$ofsCut - 1}]]
    } else {
        # Not under a share/modulefiles tree: the directory two levels above this file, which is the
        # prefix for a <prefix>/modulefiles/objectfs/<version> tree.
        set ofsPrefix [file dirname [file dirname [file dirname $ofsSelf]]]
    }
}

# Search order: the derived prefix first, so a site that installed a versioned tree gets its own
# build rather than whatever is in /usr/bin, then the standard locations.
set ofsBinDir ""
set ofsTried [list]

foreach ofsCandidate [list [file join $ofsPrefix bin] /usr/bin /usr/local/bin] {
    lappend ofsTried $ofsCandidate

    if { [file isfile [file join $ofsCandidate objectfs]] } {
        set ofsBinDir $ofsCandidate
        break
    }
}

# Fails closed, with a reason an operator can act on. A modulefile that puts a directory on PATH
# where no binary was installed reports success and defers the failure to a batch job, where the
# module system looks correct and ObjectFS looks broken.
#
# `break` inside a modulefile aborts the evaluation: it prints "ERROR: Module evaluation aborted",
# emits none of this file's environment changes — so a refused load cannot half-apply, matching
# LmodError in objectfs.lua — and emits `test 0 = 1;`, which fails when the caller evals it.
#
# The eval is the part that matters, and modulecmd.tcl's own exit status is not. Measured across two
# releases: Modules 5.6.1 exits 1 on this path, and Modules 5.4.0 exits 0 — for identical output.
# `module` is a shell function whose body is an eval of this output, so `module load objectfs || exit
# 1` branches on `test 0 = 1;` under both and an operator sees a failure either way. A test asserting
# on the exit status instead passes under one release and fails under the other against this same
# correct file, which is what happened: green locally against Modules 5.6.1, red on CI against the
# Modules 5.4.0 that Ubuntu 24.04 ships.
#
# Every version number above names the tool it belongs to, which
# TestModulefilesDoNotHardcodeAVersion requires: a bare dotted number is indistinguishable from an
# ObjectFS version, and that test exists to keep an ObjectFS version out of these files. Mind the
# line wrapping when editing — a number that ends up on the line after the word "Modules" is bare.
if { $ofsBinDir eq "" } {
    puts stderr "objectfs: no objectfs binary found. Looked in: [join $ofsTried {, }]"
    puts stderr "The prefix $ofsPrefix came from $ofsPrefixSource."
    puts stderr "This module is not loaded, because loading it would put a directory on PATH with no"
    puts stderr "binary in it — `objectfs` would then fail as 'command not found' inside a batch job."
    puts stderr "Fix: install the binary, or set OBJECTFS_MODULE_PREFIX to the prefix holding"
    puts stderr "     bin/objectfs (for a build at /sw/objectfs/$ofsVersion/bin, export"
    puts stderr "     OBJECTFS_MODULE_PREFIX=/sw/objectfs/$ofsVersion before loading)."

    break
}

if { [lsearch -exact $ofsSystemBinDirs $ofsBinDir] < 0 } {
    prepend-path PATH $ofsBinDir
}

# OBJECTFS_VERSION is informational — the module convention that lets a job script record which
# build it ran against. It is not read by the binary.
#
# What is deliberately NOT set, and why, is documented once in objectfs.lua: OBJECTFS_CONFIG (the
# binary does not read it; the systemd unit expands it into --config itself), OBJECTFS_ROOT (the
# package scriptlets read it as a filesystem prefix), and MANPATH (there is no man page).
setenv OBJECTFS_VERSION $ofsVersion
