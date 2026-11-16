+++
id = "host-foundations"
title = "Host foundations"
summary = "How host code touches files and processes: where everything lives, how it reads in places a box controls, how devc processes keep out of each other's way, how commands run, and how untrusted archives are unpacked."
order = 11
packages = ["internal/paths", "internal/safefs", "internal/flock", "internal/execx", "internal/extract", "internal/display"]
key = [
  "internal/paths.Resolve",
  "internal/paths.Paths",
  "internal/paths.Paths.Ensure",
  "internal/safefs.OpenRoot",
  "internal/safefs.Root.Open",
  "internal/safefs.Root.ReadFrom",
  "internal/safefs.Dir",
  "internal/flock.Acquire",
  "internal/execx.Runner",
  "internal/execx.Cmd",
  "internal/extract.Tar",
]
spec = ["doc:specs-v2.md#2", "doc:specs-v2.md#5"]
tests = [
  "internal/paths.TestLayout",
  "internal/paths.TestEnsure",
  "internal/safefs.TestRefused",
  "internal/safefs.TestSwapToSymlinkAfterOpen",
  "internal/safefs.TestFIFODoesNotBlock",
  "internal/execx.TestKillGroup",
  "internal/extract.TestTarHostile",
  "internal/extract.TestTarLimits",
  "test/integration.TestEscape",
]
related = ["configuration"]

[[invariant]]
text = "Below a safefs root, host code never follows a symlink, never leaves the root through `..`, and reads only regular files."
refs = ["internal/safefs.Root.openat2", "internal/safefs.checkRel", "internal/safefs.Root.Open"]
tests = ["internal/safefs.TestRefused", "internal/safefs.TestOpenRootRefusesSymlink", "internal/safefs.TestSwapToSymlinkAfterOpen"]

[[invariant]]
text = "A FIFO planted where host code expects a file never blocks it."
refs = ["internal/safefs.Root.Open", "internal/safefs.Dir.OpenFile"]
tests = ["internal/safefs.TestFIFODoesNotBlock"]

[[invariant]]
text = "Every read of a box-writable file or directory has a size or entry limit set by the caller."
refs = ["internal/safefs.Root.ReadFile", "internal/safefs.Root.ReadFrom", "internal/safefs.Root.List"]
tests = ["internal/safefs.TestOversized", "internal/safefs.TestList"]

[[invariant]]
text = "A layout getter never returns a path built from a name that failed the name rules; it panics instead."
refs = ["internal/paths.must"]
tests = ["internal/paths.TestUnvalidatedNamesPanic"]

[[invariant]]
text = "Directories devc creates below its roots are made one component at a time, and a symlink on the way is refused, not followed."
refs = ["internal/paths.Paths.Ensure", "internal/paths.EnsureBeneath", "internal/safefs.Root.MkdirAll"]
tests = ["internal/paths.TestEnsure", "internal/safefs.TestMkdirAll"]

[[invariant]]
text = "An external command runs with an argument vector and from `/`, unless the caller names another working directory."
refs = ["internal/execx.Cmd.WorkDir", "internal/execx.Real.command"]
tests = ["internal/execx.TestWorkDir"]

[[invariant]]
text = "Nothing in an archive is written outside the target directory or through a symlink, and no extracted file is set-ID."
refs = ["internal/extract.newExtractor", "internal/extract.extractor.name", "internal/extract.extractor.parent"]
tests = ["internal/extract.TestTarHostile", "internal/extract.TestTargetMustBeEmpty"]

[[invariant]]
text = "When a command started with KillGroup returns, no process of its group still runs, or the error says so."
refs = ["internal/execx.run", "internal/execx.GroupLeftError"]
tests = ["internal/execx.TestKillGroup", "internal/execx.TestKillGroupLeft"]
+++
## What it is for

Host code reads and writes files all the time, and many of them sit in places a box can write: project directories, box homes, harness state, a box's runtime directory with its spool. It also starts programs (Podman, git, ssh, nft) on behalf of users. Rule R2 of the spec says the host must not trust or execute box-writable content. This concern is the handful of small packages every other part uses to stay within that rule: one place that knows where everything lives, one way to read beneath a directory a box controls, locks between concurrent devc processes, one way to run a command, and an extractor for archives no one vouches for.

## How it works

**Where everything lives.** [[internal/paths.Resolve]] decides three roots, each in the order flag, environment variable, default: the configuration directory (`~/.config/devc`), the state directory (`~/.local/share/devc`, also movable by `state_dir` in config.toml) and the runtime directory (`$XDG_RUNTIME_DIR/devc`). The projects root comes from config.toml (default `~/devc`). Every path below them is a getter of [[internal/paths.Paths]]; the in-box paths and IDs (`/w/<realm>/<project>`, `/home/dev`, `/opt/devc`, `/run/devc`, `/etc/devc`, UID 1000) are constants in `inbox.go`. Getters take names that already passed [[pkg:internal/names]] and panic otherwise ([[internal/paths.must]]). They create nothing; [[internal/paths.Paths.Ensure]] creates a directory with mode 0700 below one of the roots, every component without following a symlink ([[internal/paths.EnsureBeneath]]). The state view shows the trees.

**Reading what a box controls.** [[pkg:internal/safefs]] opens a trusted directory as a [[internal/safefs.Root]] and resolves every name beneath its descriptor with `openat2(2)`, a Linux call that takes resolution flags: RESOLVE_BENEATH (no escape), RESOLVE_NO_SYMLINKS and RESOLVE_NO_MAGICLINKS (no symlink anywhere on the way, nor `/proc` magic links). [[internal/safefs.Root.Open]] accepts only regular files, checked with `fstat` on the open descriptor, so a swap after the check changes nothing. Reads are capped ([[internal/safefs.Root.ReadFile]]); append-only files are tailed from an offset ([[internal/safefs.Root.ReadFrom]]); directories are listed by parsing `getdents64` ([[internal/safefs.Root.List]]). The write side, [[internal/safefs.Dir]], acts one path component at a time relative to a directory descriptor; the helper container's workers, the extractor and a few host writers use it. The tour [[flow:safe-read]] goes through it in order.

**Locks.** [[internal/flock.Acquire]] takes an advisory `flock(2)` lock, shared or exclusive, on a 0600 file opened without following a symlink, retrying every 50 ms up to a timeout (one minute unless the caller sets one). Lock files sit in devc's own directories: a box's lifecycle lock beside its home, one per realm for the context repository, one per box for spool ingestion, and the daemon's in the runtime directory with a timeout of one millisecond, so a second daemon fails at once. The toolbox has its own lock without a time limit ([[internal/toolbox.lock]]).

**Running commands.** Everything goes through the [[internal/execx.Runner]] interface: [[internal/execx.Real]] in production, [[internal/execx.Fake]] in tests, which answers from scripted responses. A [[internal/execx.Cmd]] is an argument vector, never a shell string; its working directory is `/` unless given ([[internal/execx.Cmd.WorkDir]]). `Run` captures standard output and error, each up to 8 MiB ([[internal/execx.capped]]), with a default timeout of two minutes: the context's end sends SIGTERM and, five seconds later, SIGKILL. `Interactive` connects the terminal and has no default timeout. A non-zero exit is an [[internal/execx.ExitError]] with the command line and the first line of standard error, both with the values in `Cmd.Redact` replaced.

With `KillGroup` the command runs in a process group of its own. When it exits, the rest of the group is killed and waited for, and the command itself, the group's leader, is reaped only then, because until it is reaped its ID, which is the group's, cannot be reused ([[internal/execx.run]], [[finding:F1-U1]]). A group that outlives 30 seconds is handed to a watcher and reported as a [[internal/execx.GroupLeftError]].

**Archives.** [[pkg:internal/extract]] unpacks tar (plain, gzip, bzip2) and zip into a new, empty directory: published shares on the host, dependency sources in a box, the Node.js release of the toolbox, and exports read back by the fleet commands. Names are cleaned and checked; files and directories are created beneath the target's descriptor without following a symlink; symlinks are created last, only when relative and inside the tree, and each is then resolved by the kernel beneath the target and removed if it escapes; hard links, devices and FIFOs are left out; modes are 0644 or 0755, never set-ID. Limits on entries, file sizes, total size and decompressed input stop an extraction with `ErrLimit`.

**Display.** [[pkg:internal/display]] is a leaf of wire formats for the desktop sidecar (Xauthority files, the X11 connection setup, VNC authentication), with strict, bounded parsers because the box side reads files and answers the box controls.

## What can go wrong, and what prevents it

A box controls the content and the shape of its trees: it can plant symlinks, FIFOs, devices, huge files and deep directories, and swap them while the host looks.

- **A symlink redirects a read or a write.** No safefs operation follows one below the root, and `..` is refused before the kernel sees it ([[internal/safefs.checkRel]]). The root itself must be reached by a host-controlled path, and its last component must not be a link ([[internal/safefs.OpenRoot]]).
- **A FIFO stalls the host.** Files are opened with `O_NONBLOCK`, so opening a FIFO returns at once, and the type check then refuses it.
- **A huge file or directory exhausts memory.** Every read takes a limit; a file that grows past it during the read is refused too.
- **A file is truncated or replaced while being tailed.** [[internal/safefs.Root.ReadFrom]] reports `ErrShrunk` instead of reading from the wrong place.
- **An old kernel.** Without `openat2` (Linux before 5.6) every safefs operation fails with `ErrUnsupported`; there is no path-based fallback.
- **A command runs inside a project.** Host tools would then read configuration and hooks from a box-writable directory. execx never inherits the caller's working directory.
- **A secret appears in an error or log.** Values listed in `Cmd.Redact` are replaced in `Cmd.String`, in error messages and in the debug log; the package's rule is that secrets go on standard input, never into arguments or the environment.
- **An archive escapes its directory.** See the extractor's rules above; the [[internal/extract.Report]] counts every left-out entry and lists up to 100 with the reason.

## Decisions that are not obvious

- **getdents parsed by hand.** `os.File.ReadDir` falls back to a path-based `lstat` for entries of unknown type, which a box could race; [[internal/safefs.direntType]] asks with `fstatat` relative to the directory's descriptor instead.
- **Getters panic.** An unvalidated name reaching a getter is a programming error, never user input, so it panics rather than every getter returning an error ([[chunk:C01]]).
- **The secrets directory is a sibling.** `/run/devc-secrets` is not below `/run/devc`: that directory is box-writable, and a mount nested in it could be redirected by a planted symlink ([[internal/paths.InBoxSecret]], [[chunk:C03]]).
- **Process groups, leader last.** `KillGroup` exists because git's receive-pack left index-pack writing into a quarantine that was being removed ([[finding:E1-16]]). The leader is reaped last because signalling a group by the ID of a reaped leader could hit an unrelated group that took the ID over ([[finding:F1-U1]]).

## Where to start reading

1. [[internal/paths.Resolve]] and [[internal/paths.Paths.Ensure]]
2. [[internal/safefs.Root.openat2]] and [[internal/safefs.Root.Open]]
3. [[internal/execx.Cmd]] and [[internal/execx.run]]
4. [[internal/flock.Acquire]]
5. [[internal/extract.Tar]]
