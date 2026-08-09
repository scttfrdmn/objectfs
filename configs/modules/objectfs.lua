-- ObjectFS — a POSIX interface over AWS S3, for research computing.
--
-- Lmod modulefile. The TCL Modules equivalent is objectfs.tcl in this directory, and the two are
-- held to the same exported environment by internal/config/modulefiles_test.go — a site running
-- Environment Modules and a site running Lmod get the same variables, or that test fails.
--
-- INSTALLED AS:  <prefix>/share/modulefiles/objectfs/<version>.lua
--
-- Not as objectfs/<version>/objectfs.lua, which is what this file's first draft did. Lmod reads a
-- module tree as name/version, so a third level makes the *version* the name: with the file at
-- objectfs/<version>/objectfs.lua, `module load objectfs` fails with "the following module(s) are
-- unknown", `module avail` lists it as objectfs/<version>/objectfs, and inside the file
-- myModuleVersion() returns "objectfs". Measured against Lmod 9.3, not reasoned about.
--
-- The TCL file installs alongside this one, in the same directory, as <version> with no extension.
-- That is deliberate and it was verified rather than assumed: with both present, Lmod loads the
-- .lua and modulecmd.tcl loads the extensionless one, and a directory holding only .lua files is
-- invisible to modulecmd.tcl. So one tree serves both systems and neither sees the other's file.
--
-- There is no version number written down here, and there must not be. The authority is the
-- `version` constant in cmd/objectfs/main.go; this file learns the version from its own filename
-- via myModuleVersion(), so the number exists once, in the install path. Five files in this
-- repository once claimed five different versions.

whatis("Name:        ObjectFS")
whatis("Description: A POSIX interface over AWS S3, for research computing")
whatis("URL:         https://github.com/scttfrdmn/objectfs")

-- myModuleVersion() returns the boolean false — not a string, not nil — for a modulefile installed
-- without a version directory level. Concatenating that raises a Lua error from inside Lmod, which
-- an operator sees as an internal traceback rather than as a usable message. Coerced here instead.
local ofsVersion = myModuleVersion()
if type(ofsVersion) ~= "string" or ofsVersion == "" then
    ofsVersion = "unknown"
end

whatis("Version:     " .. ofsVersion)

help([[
ObjectFS presents an AWS S3 bucket as a POSIX filesystem over FUSE.

It is NOT a POSIX-compliant filesystem. Locks are not forwarded, so they are host-local and
invisible to any other mount of the same bucket; SQLite and anything using POSIX record locks
will not work correctly across nodes. The supported-operations table in the documentation is
the authority on what works and what fails by design.

  Mount a bucket:
    objectfs mount s3://my-bucket /mnt/data

  With a site configuration file:
    objectfs mount --config /etc/objectfs/config.yaml s3://my-bucket /mnt/data

  Check a configuration without mounting:
    objectfs mount --dry-run --config /etc/objectfs/config.yaml s3://my-bucket /mnt/data

  Unmount:
    objectfs unmount /mnt/data

Flags come before the positional arguments. Go's flag package stops parsing at the first
argument that is not a flag, so a flag written after the storage URI is not applied — the
command reports that rather than ignoring it.

ObjectFS always runs in the foreground and does not fork. Under a batch scheduler, mount as a
prologue step or background the mount in the job script, and unmount in the epilogue so that
buffered writes are flushed to S3 before the allocation ends. A job killed with the mount still
running loses whatever has not been flushed.

Credentials do not come from the configuration file. ObjectFS uses the standard AWS credential
chain: AWS_PROFILE, the environment variables, the shared credentials file, or an instance role.

Documentation: https://github.com/scttfrdmn/objectfs
]])

-- ---------------------------------------------------------------------------------------------
-- Locating the binary.
--
-- The first draft of this file did `prepend_path("PATH", "/usr/bin")` while separately computing a
-- versioned base of /usr/lib/objectfs/<version>. Those two disagree about where the binary lives,
-- and only one of them can be right. Worse, both are wrong in a way a modulefile can detect:
--
--   * Nothing installs to /usr/lib/objectfs/<version>. The systemd unit this repository ships
--     invokes /usr/bin/objectfs, and that is where the package puts it.
--   * /usr/bin is already on PATH everywhere. Prepending it is not the no-op it looks like — it
--     HOISTS /usr/bin to the front, ahead of any site directory earlier in PATH. Measured: PATH
--     "/opt/site/bin:/usr/local/bin:/usr/bin:/bin" becomes
--     "/usr/bin:/opt/site/bin:/usr/local/bin:/bin". At an HPC site, where /opt/site/bin is exactly
--     how a center shadows a distro tool with its own build, that reorders every command in the
--     user's environment as a side effect of loading ObjectFS. And it does not come back: Lmod's
--     unload removes the entry it thinks it added, so /usr/bin stays hoisted after
--     `module unload objectfs`. That was measured too.
--
-- So the prefix is derived from where this file actually is, and PATH is touched only when it needs
-- to be.
-- ---------------------------------------------------------------------------------------------

-- parent strips one trailing path component. Wrapped in parentheses because gsub returns two
-- values and a bare return would leak the substitution count into the caller.
local function parent(path)
    return (path:gsub("/[^/]*$", ""))
end

-- systemBinDirs are the directories already on every login PATH. A binary in one of these needs no
-- PATH change, and making one anyway is the hoisting defect described above.
--
-- Membership is decided on a static property of the derived path — is this a system bindir — and
-- deliberately NOT by scanning $PATH for it. A PATH-contents test looks more precise and is a trap:
-- Lmod records what a modulefile did at load time and replays the inverse at unload, so a prepend
-- that was conditional on the loading shell's PATH gets removed from a shell whose PATH differs,
-- and the entry leaks. Verified by measurement: the PATH-scanning form of this check leaves
-- /opt/objectfs/bin on PATH after `module unload objectfs`, and this form round-trips clean.
local systemBinDirs = {
    ["/bin"] = true,
    ["/usr/bin"] = true,
    ["/usr/local/bin"] = true,
    ["/sbin"] = true,
    ["/usr/sbin"] = true,
}

-- installedPrefix returns the install prefix, and how it was established.
--
-- Two sources, in priority order:
--
--   1. OBJECTFS_MODULE_PREFIX, for a site whose module tree is not under the same prefix as the
--      software — a center that keeps every modulefile in /sw/modulefiles and every build in
--      /sw/objectfs/<version>. That layout cannot be derived from this file's location by any
--      arithmetic, so it has to be stated. It is read from the environment rather than patched into
--      this file so that the shipped file is the file that runs.
--   2. This file's own location, split on "/share/modulefiles/". String-matched rather than
--      counted-up-four-levels, because counting silently produces a wrong answer for a tree that is
--      not the expected shape — /opt/modulefiles/objectfs/<version>.lua would yield "/" — whereas a
--      match either applies or does not, and "does not" falls through to the search below.
local function installedPrefix()
    local override = os.getenv("OBJECTFS_MODULE_PREFIX")
    if override ~= nil and override ~= "" then
        return override, "OBJECTFS_MODULE_PREFIX"
    end

    local self = myFileName()
    local cut = self:find("/share/modulefiles/", 1, true)
    if cut ~= nil then
        return self:sub(1, cut - 1), "this modulefile's location"
    end

    -- Not under a share/modulefiles tree. Fall back to the directory two levels above this file,
    -- which is the prefix for a <prefix>/modulefiles/objectfs/<version>.lua tree.
    return parent(parent(parent(self))), "this modulefile's location"
end

local prefix, prefixSource = installedPrefix()

-- candidateBinDirs are the places to look for the binary, in order: the derived prefix first,
-- because a site that installed a versioned tree wants its own build and not whatever is in
-- /usr/bin, then the standard locations the package and `make install` use.
local candidateBinDirs = {
    pathJoin(prefix, "bin"),
    "/usr/bin",
    "/usr/local/bin",
}

local binDir, tried = nil, {}
for _, dir in ipairs(candidateBinDirs) do
    tried[#tried + 1] = dir
    if isFile(pathJoin(dir, "objectfs")) then
        binDir = dir
        break
    end
end

-- Fails closed, with a reason an operator can act on.
--
-- This is the defect class the whole file exists to avoid: a modulefile that puts a directory on
-- PATH where no binary was installed. It reports success, `module list` shows objectfs loaded, and
-- the failure surfaces later as "command not found" in a batch job — at which point the module
-- system looks correct and ObjectFS looks broken. A load that refuses, naming the paths it looked
-- in, costs the operator one minute and tells them exactly what to fix.
--
-- LmodError, not LmodWarning: this is a correctness property, not a performance one. Measured —
-- LmodError exits 1 and emits no environment changes at all, so a refused load cannot half-apply.
if binDir == nil then
    LmodError(
        "objectfs: no objectfs binary found. Looked in: " .. table.concat(tried, ", ") .. "\n" ..
        "The prefix " .. prefix .. " came from " .. prefixSource .. ".\n" ..
        "This module is not loaded, because loading it would put a directory on PATH with no\n" ..
        "binary in it — `objectfs` would then fail as 'command not found' inside a batch job.\n" ..
        "Fix: install the binary, or set OBJECTFS_MODULE_PREFIX to the prefix holding bin/objectfs\n" ..
        "     (for a build at /sw/objectfs/" .. ofsVersion .. "/bin, export\n" ..
        "     OBJECTFS_MODULE_PREFIX=/sw/objectfs/" .. ofsVersion .. " before loading).\n"
    )
end

if not systemBinDirs[binDir] then
    prepend_path("PATH", binDir)
end

-- OBJECTFS_VERSION is informational — the module convention that lets a job script record which
-- build it ran against. It is not read by the binary, and that is the point of saying so here.
--
-- What is deliberately NOT set:
--
--   * OBJECTFS_CONFIG. configs/systemd/objectfs@.service sets it, which makes it look like
--     something the binary reads. It is not: the unit expands it into `--config ${OBJECTFS_CONFIG}`
--     itself, and cmd/objectfs takes its config file from that flag only. Exporting it here would
--     be a variable that looks like it selects a configuration and selects nothing.
--   * OBJECTFS_ROOT. scripts/postinstall.sh and scripts/preremove.sh read it as a prefix for every
--     path they touch, so a login shell that exported OBJECTFS_ROOT=/usr would send the next
--     `apt upgrade` scriptlet at /usr/usr/share and /usr/etc/objectfs.
--   * MANPATH. There is no man page in this repository. A MANPATH entry pointing at a directory
--     with no pages in it is the same defect as a PATH entry with no binary.
setenv("OBJECTFS_VERSION", ofsVersion)
