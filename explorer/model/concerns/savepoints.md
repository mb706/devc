+++
id = "savepoints"
title = "Savepoints"
summary = "Copies of the data tree that no box can reach: what a savepoint holds and what it leaves out, the reflink and restic drivers, when savepoints are taken and dropped, and how a copy or in-place restore works."
order = 61
files = [
  "internal/data/savepoints.go",
  "internal/toolbox/restic.go",
  "internal/boxside/dataworker.go",
  "internal/cli/datacmds.go",
]
key = [
  "internal/data.Service.Save",
  "internal/boxside.DataWorker.reflinkSave",
  "internal/boxside.SavedModes",
  "internal/data.Service.resticSave",
  "internal/data.Service.restic",
  "internal/data.Retention",
  "internal/data.Service.Restore",
  "internal/boxside.DataWorker.restore",
  "internal/boxside.DataWorker.restoreInPlace",
  "internal/data.Service.resticRestore",
  "internal/data.Service.GC",
  "internal/data.Service.BeforeSession",
]
spec = ["doc:specs-v2.md#14.3"]
tests = [
  "internal/boxside.TestDataSavepointRestore",
  "internal/boxside.TestDataReflinkModes",
  "internal/boxside.TestDataReflinkUnsupported",
  "internal/boxside.TestDataStagingRestore",
  "internal/data.TestIndexAndRetention",
  "internal/data.TestServiceRestic",
  "internal/data.TestServiceAutoAndBeforeSession",
  "internal/toolbox.TestInstallRestic",
  "test/integration.TestData",
]
related = ["data-and-sealing", "data-sync", "requests-and-approval", "toolbox-and-harnesses"]

[[invariant]]
text = "A savepoint covers the data root without its input zones and without the entries that were sealed when it was taken."
refs = ["internal/boxside.DataWorker.sealedEntries", "internal/boxside.DataWorker.reflinkSave", "internal/data.Service.resticSave"]
tests = ["internal/boxside.TestDataSavepointRestore", "internal/data.TestServiceRestic"]

[[invariant]]
text = "No box can read, change or remove a savepoint's copy: reflink copies are vault-owned inside `.devc`, restic repositories in the state directory; approved save requests can make retention drop unnamed ones."
refs = ["internal/boxside.DataWorker.reflinkSave", "internal/paths.Paths.ResticRepo", "internal/data.Retention"]
tests = ["test/integration.TestEscape"]

[[invariant]]
text = "A copy restore overwrites nothing: it writes only into a new `restored-<id>` directory of the first work zone."
refs = ["internal/boxside.DataWorker.restore"]
tests = ["internal/boxside.TestDataSavepointRestore"]

[[invariant]]
text = "An in-place restore looks for the refusals it can foresee before it replaces any entry, moves what it replaces into one trash entry and deletes nothing; it is not atomic."
refs = ["internal/boxside.DataWorker.restoreInPlace", "internal/boxside.inPlace.displace"]
tests = ["internal/boxside.TestDataSavepointRestore"]

[[invariant]]
text = "Retention drops only unnamed savepoints; a named one goes only with `devc data gc --drop`."
refs = ["internal/data.Retention", "internal/data.Service.GC"]
tests = ["internal/data.TestIndexAndRetention", "test/integration.TestData"]

[[invariant]]
text = "With `before_session` and a driver other than `none`, a session `devc <harness>` starts does not start when its savepoint fails; sessions the daemon starts take none."
refs = ["internal/cli.app.saveBeforeSession", "internal/data.Service.BeforeSession", "internal/daemon.commander.runStart"]

[[invariant]]
text = "restic runs as the box user with no capability and no network, and sees the project directory only read-only."
refs = ["internal/data.Service.restic"]
tests = ["internal/data.TestServiceRestic"]
+++
## What it is for

A work zone belongs to the agent: it can delete or overwrite days of results in a second. A savepoint is a copy of the data tree that no box can reach, taken when you ask, before sessions if the project wants that, or when the box asks. You restore it as a copy beside the data or back in place, and what the restore replaces goes to the trash rather than away.

## How it works

**What a savepoint holds.** The data root without its input zones (read-only for the box anyway) and without the entries sealed at that moment ([[concern:data-and-sealing]]): directories, regular files with their bytes, modes and times, and symlinks as links. FIFOs, sockets and devices are skipped and reported. Owners are not kept: restored files belong to the box user. The box may be writing while the copy is made, so each file is as it was when it was copied, not the whole tree at one instant. The index entry also records the relay's default-branch tip, and the manifest gets a record with path `.` ([[concern:data-sync]]).

**Drivers** (`data.savepoints.driver`, default `auto`; [[internal/data.Service.Save]]):

- `reflink`. The worker first clones a one-byte file in `.devc/savepoints` ([[internal/boxside.DataWorker.probeClone]]); `FICLONE` makes a copy that shares the original's blocks until either changes, so a savepoint costs space only as files diverge. [[internal/boxside.DataWorker.reflinkSave]] copies the tree into `<id>.partial/`, owned by the vault, directories `0555`, files without write bits, keeps the original modes in `.devc/savepoints/.modes/<id>` ([[internal/boxside.SavedModes]]) and renames the copy to `<id>`. It lives on the project's filesystem, inside the project directory.
- `restic`. The setup worker checks the layout and reports what to leave out and the digest; then [[internal/data.Service.restic]] runs `restic backup --json --quiet --host devc --tag <id> --exclude=… /project/<root>` in a container from the helper root filesystem as the box user, without capabilities or network, the project directory read-only at `/project`, the repository `local/savepoints/<realm>/<project>/restic` at `/repo`, a cache, the toolbox read-only and a tmpfs `/tmp`. The repository has no password; it is in the state directory. restic is the pinned 0.19.1 that `devc toolbox install restic` puts into the toolbox after checking its SHA-256 ([[internal/toolbox.Toolbox.InstallRestic]]).
- `auto` uses reflink when the probe succeeds, else restic if installed, else fails and says what to do. `none` and `borg` refuse.

**The index**, `durable/realms/<realm>/savepoints/<project>.jsonl`, holds `save` and `drop` records ([[internal/data.ReadIndex]]). IDs are `YYYYMMDDTHHMMSSZ[-name]`; a taken ID moves on by a second ([[internal/data.NewID]]).

**When.** `devc save [name]`, under the box lock ([[flow:data-save]]). Before a session that `devc <harness>` starts when `before_session = true`, unless a savepoint is younger than 30 minutes or the driver is `none`; if it fails the session does not start ([[internal/cli.app.saveBeforeSession]]). A session the daemon starts (from the web app or the hub, [[internal/daemon.commander.runStart]]) takes none ([[doc:docs/dev/LEFTOVER.md#7]]). And when the box asks with `devc request savepoint [name]`, which the default `requests.auto` approves at once ([[concern:requests-and-approval]]).

**Retention.** After each save, [[internal/data.Retention]] drops the oldest unnamed savepoints beyond `keep` (default 10; 0 keeps all). Named ones stay until `devc data gc --drop`. A reflink copy is deleted by the savepoint-drop worker; a restic snapshot is forgotten, and its space comes back only when `devc data gc` runs `restic prune` ([[internal/data.Service.GC]]).

**Restore.** `devc restore <savepoint> [path…] [--in-place]` ([[flow:data-restore]]). The host refuses paths in input zones and paths that were sealed when the savepoint was taken ([[internal/data.Service.Restore]]). Without `--in-place` the worker copies into a new `restored-<id>[-N]/` in the first work zone, owned by the box user with the saved modes; nothing is overwritten. In place, [[internal/boxside.DataWorker.restoreInPlace]] walks twice. The dry pass only looks for refusals: an entry sealed now (a full restore skips it, an explicit path is refused), a layout conflict. Missing layout directories are made before it, and the box keeps running, so the second pass can still fail half-way, leaving what it did. The second pass leaves alone what is already identical (same type and symlink target, or a box-owned file with the restored mode and the same bytes; times do not count) and moves anything else in the way into one trash entry `<stamp>-restore-<id>` before copying. What was created since the savepoint stays, except inside a directory the savepoint has as a file or symlink: that directory goes to the trash whole. For restic, [[internal/data.Service.resticRestore]] has restic restore into a new `local/tmp/restore-*` as the box user; the worker copies from it mounted read-only at `/staging`; `devc box rm-worker` empties it in a helper container and the host removes the empty directory.

## What can go wrong, and what prevents it

- **The box reaching a savepoint.** Reflink copies are inside `.devc`, which is the vault's and `0700`; the restic repository and the index are in the state directory, which no box mounts. Neither copy can be changed through the data tree: a reflink copy shares blocks, not files.
- **The box pushing savepoints out.** A box can ask for unnamed savepoints, and the default `requests.auto` approves them; every save applies retention, so about ten such requests drop every older unnamed savepoint, including those taken before sessions. Named requests escape retention and accumulate instead ([[doc:docs/dev/LEFTOVER.md#7]]). Name the savepoints you rely on, or take `savepoint` out of `requests.auto`.
- **Files restic cannot read.** restic reads as the box user, so a file the box made unreadable is missing; restic's exit status 3 still records the savepoint, with a warning. The reflink worker reads as root with `DAC_OVERRIDE` and misses nothing.
- **What no savepoint holds.** Input zones, entries sealed at the time, special files. An entry unsealed later and then damaged is in no savepoint taken while it was sealed.
- **Not a backup.** A reflink copy shares the disk and the project directory with the data; the restic repository shares the state directory's disk. Neither survives the loss of that disk.
- **The restored tree.** It came from box-written data; the host never walks it: workers copy it, the rm worker deletes the restic staging tree.

## Decisions that are not obvious

- **Modes in a separate record.** The copy is stored read-only and vault-owned, so its own modes cannot say what the saved files had; the record keeps every mode that differs from the default, and a restore applies it ([[finding:B9]]).
- **restic sees the whole project directory.** Podman follows symlinks in a bind source; the project directory's path is host-made, so nothing the box planted changes what is mounted, and `.devc` is unreadable to restic's user ([[chunk:C06]]).
- **Identical means the same bytes**, never the same times: the box sets times at will.
- **Prune only in `gc`**: forgetting is quick, pruning rewrites the repository.

## Where to start reading

1. [[internal/data.Service.Save]]
2. [[internal/boxside.DataWorker.reflinkSave]] and [[internal/data.Service.resticSave]]
3. [[internal/data.Retention]] and [[internal/data.Service.GC]]
4. [[internal/boxside.DataWorker.restore]] and [[internal/boxside.DataWorker.restoreInPlace]]
5. [[internal/data.Service.resticRestore]]
