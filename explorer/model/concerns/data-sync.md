+++
id = "data-sync"
title = "Moving data: pull, push, manifest"
summary = "`devc data pull` and `push` with rsync between a remote and a staging directory no box can write, the worker's merge, what each can overwrite or delete on either side, the preview and its limits, the locks, and the manifest and what it may claim."
order = 62
files = [
  "internal/data/sync.go",
  "internal/data/rsync.go",
  "internal/data/manifest.go",
  "internal/boxside/datasync.go",
  "internal/cli/datasync.go",
]
key = [
  "internal/data.Service.Pull",
  "internal/data.Service.Push",
  "internal/data.RsyncArgs",
  "internal/data.ParseItems",
  "internal/data.Service.pullStaging",
  "internal/boxside.DataWorker.pullIn",
  "internal/boxside.puller.merge",
  "internal/boxside.DataWorker.pushStage",
  "internal/boxside.Digests",
  "internal/data.AppendManifest",
  "internal/data.Service.Status",
  "internal/data.Service.lockSync",
]
spec = ["doc:specs-v2.md#14.4"]
tests = [
  "internal/data.TestRsyncArgs",
  "internal/data.TestParseItems",
  "internal/data.TestServicePull",
  "internal/data.TestServicePush",
  "internal/data.TestSyncPreviewAndLock",
  "internal/data.TestPullEntryLimits",
  "internal/data.TestPullRemoteObservation",
  "internal/data.TestStatusAndVerify",
  "internal/data.TestManifest",
  "internal/boxside.TestDataPullIn",
  "internal/boxside.TestDataPushStage",
  "internal/boxside.TestDigests",
  "test/integration.TestDataSync",
]
related = ["data-and-sealing", "savepoints", "ssh-gate", "worktrees-and-context"]

[[invariant]]
text = "rsync is never given `--delete` or `--copy-links`, and a pull writes only into a staging directory below the state directory."
refs = ["internal/data.RsyncArgs", "internal/data.Service.pullStaging"]
tests = ["internal/data.TestRsyncArgs", "internal/data.TestServicePull"]

[[invariant]]
text = "A pull never deletes from the data tree and never changes a sealed entry; what it replaces goes to the trash."
refs = ["internal/boxside.puller.merge", "internal/boxside.DataWorker.checkPullTarget", "internal/boxside.puller.displace"]
tests = ["internal/boxside.TestDataPullIn", "test/integration.TestDataSync"]

[[invariant]]
text = "Pulled files belong to the box user and carry no set-ID bits."
refs = ["internal/boxside.DataWorker.pullIn", "internal/boxside.copier.copyFile"]
tests = ["internal/boxside.TestDataPullIn"]

[[invariant]]
text = "On a push, rsync reads only the push staging directory, whose directories belong to the vault."
refs = ["internal/data.Service.Push", "internal/boxside.DataWorker.pushStage", "internal/boxside.linker.entry"]
tests = ["internal/data.TestServicePush", "internal/boxside.TestDataPushStage"]

[[invariant]]
text = "A push keeps every remote file it overwrites in a backup directory of its own, `.devc-backup/<stamp>-<12 hex>`."
refs = ["internal/data.RsyncArgs", "internal/data.BackupDir"]
tests = ["internal/data.TestRsyncArgs", "internal/data.TestServicePush"]

[[invariant]]
text = "A pull is refused before anything is transferred when its preview cannot be read whole, exceeds `--max-size` or covers more entries than a report can hold."
refs = ["internal/data.Service.Pull", "internal/data.ParseItems", "internal/data.Totals", "internal/boxside.CheckPullEntries"]
tests = ["internal/data.TestSyncPreviewAndLock", "internal/data.TestPullEntryLimits"]

[[invariant]]
text = "At most one pull or push per project and remote runs at a time."
refs = ["internal/data.Service.lockSync"]
tests = ["internal/data.TestSyncPreviewAndLock"]

[[invariant]]
text = "Nothing a box wrote reaches rsync's command line: host and remote path come from the configuration, paths from it or from your command."
refs = ["internal/data.Service.syncPlan", "internal/config.CheckRemote", "internal/boxside.CheckSyncPath"]
tests = ["internal/data.TestRsyncArgs"]

[[diagram]]
id = "pull-pipeline"
title = "Where a pull's data passes, and who writes each stage"
kind = "graph"

[[diagram.node]]
id = "remote"
label = "remote (your SSH alias)"
col = 1
row = 1
zone = "external"

[[diagram.node]]
id = "rsync"
label = "rsync on the host"
col = 2
row = 1
zone = "host"
ref = "internal/data.Service.transfer"

[[diagram.node]]
id = "staging"
label = "local/staging/… (no box)"
col = 3
row = 1
zone = "host"
ref = "internal/paths.Paths.SyncStagingDir"

[[diagram.node]]
id = "worker"
label = "pull-in worker"
col = 4
row = 1
zone = "aux"
ref = "internal/boxside.DataWorker.pullIn"

[[diagram.node]]
id = "data"
label = "data tree (box-writable)"
col = 4
row = 2
zone = "box"

[[diagram.node]]
id = "trash"
label = ".devc/trash"
col = 3
row = 2
ref = "internal/boxside.puller.displace"

[[diagram.edge]]
from = "remote"
to = "rsync"
label = "dry run, then transfer"
ref = "internal/data.RsyncArgs"

[[diagram.edge]]
from = "rsync"
to = "staging"
label = "writes, as you"
ref = "internal/data.Service.pullStaging"

[[diagram.edge]]
from = "staging"
to = "worker"
label = "mounted read-only"
ref = "internal/data.Service.work"

[[diagram.edge]]
from = "worker"
to = "data"
label = "copies, owned by dev"
ref = "internal/boxside.copier.copyEntry"

[[diagram.edge]]
from = "data"
to = "trash"
label = "what differs: rename"
ref = "internal/boxside.puller.displace"
+++
## What it is for

Results move between this machine, a cluster and a disk: inputs come in, finished runs go out. devc wraps rsync so that a pull cannot destroy what is here, a push cannot destroy what is there, the host still never walks the box-writable tree, and nothing a box wrote reaches rsync's command line. A manifest in the context repository records what each machine held.

## How it works

**Remotes.** `[data.remotes.<name>]` has a `host` (an SSH alias, empty for a directory on this machine), a `path` and default `pull` and `push` lists. [[internal/config.CheckRemote]] refuses a path starting with `-` or `~`, wildcards, control characters and `..`; a local path must be absolute. Targets are zones or paths inside one, without wildcards or control characters, never `.devc-backup` ([[internal/boxside.CheckSyncTarget]]).

**The command line** is built in one place, [[internal/data.RsyncArgs]]:

```sh
rsync --recursive --links --times --no-owner --no-group --no-devices --no-specials \
  --partial --relative --protect-args \
  [pull: --perms] [push: --no-perms --executability --chmod=D0777 --backup --backup-dir=.devc-backup/<stamp>-<id>] \
  [--dry-run --out-format='%i %l %n' | --info=progress2] [--rsh='ssh [-F …] [-S <master> -o ControlMaster=no]'] \
  pull: <host>:<path>/./<target>… <staging>/     push: <staging>/./<target>… <host>:<path>/
```

`--protect-args` keeps the remote shell from parsing paths, which is why wildcards are refused. The shell uses the master connection of `devc ssh login` when `ssh -O check` answers on it, else your ordinary SSH ([[internal/data.Service.remoteShell]], [[concern:ssh-gate]]).

**Locks.** A pull or push holds the sync lock `local/locks/sync/<realm>/<project>/<remote>.lock` throughout; a second one is refused after 3 seconds ([[internal/data.Service.lockSync]]). The box lock is taken only for the worker phases, so a transfer of hours does not hold up other devc commands on the box.

**A pull** ([[flow:data-pull]], [[diagram:pull-pipeline]]). A check worker first refuses a target that is or lies inside a sealed entry. The staging directory is `local/staging/<realm>/<project>/<stamp>-<remote>/`; one an interrupted pull from the same remote left is reused, so `--partial` continues it ([[internal/data.Service.pullStaging]]). A dry run always comes first: its listing may be at most 256 MiB and every line must be one rsync prints ([[internal/data.ParseItems]]); its total must stay under `--max-size` (default 2g) and its entries within 100,000 and the report's room ([[internal/boxside.CheckPullEntries]]). You see up to 200 lines and are asked, unless `--yes`; without a terminal it is refused. rsync then fills the staging directory, and the pull-in worker merges it, mounted read-only at `/staging`, under the box lock. After a successful merge the records go into the manifest and the staging directory is removed; after a failure it stays for the next pull.

**The merge**, [[internal/boxside.puller.merge]], after a dry pass that finds layout conflicts:

| Here | Arrived | Result |
|---|---|---|
| nothing | file, symlink, directory | copied in, owned by the box user, the remote's mode bits without set-ID |
| the same bytes, the same link target | file, symlink | left alone, counted identical |
| another file or symlink | file or symlink | the old one to the trash, the new one in |
| a directory | a directory | merged entry by entry |
| a directory | a file | the whole directory to the trash |
| a file | a directory | the file to the trash |
| a sealed entry below a target | anything | left alone, reported when it differs |
| a layout directory | a file | refused before anything changes |
| anything | FIFO, socket, device | skipped |
| an entry | nothing | stays |

What moves goes into one trash entry `<stamp>-pull-<remote>` ([[concern:data-and-sealing]]).

**A push** ([[flow:data-push]]). The push-stage worker hard-links the targets into `.devc-staging/push-<stamp>/` in the project directory: `.devc-staging` is the vault's with mode `0711`, the directories below are new and the vault's (`0755`), regular files are hard links, symlinks are new links with the same target, special files are skipped, and stages older than seven days are removed; a `.devc-staging` the box made is refused ([[internal/boxside.DataWorker.pushStage]]). The dry run and the transfer read only that directory. Afterwards the push-unstage worker removes it, also after a failure. On the remote, rsync creates and updates files below `<path>/<target>`; every file it replaces is moved into that push's backup directory; nothing is deleted.

**The manifest**, `durable/realms/<realm>/context/data/<project>/manifest.jsonl`. A record names the machine (`machine_name`, else the host name), the action (seal, save, pull, push), the path, file count, size and digest, and for a pull the remote's digest and the merge's outcome. The digest is a SHA-256 over type, path and size of every file and symlink, so it lists names and sizes; `--content` adds a hash of the contents ([[internal/boxside.Digests]]). [[internal/data.AppendManifest]] validates each record and appends beneath the context directory through safefs under the context lock; after a pull or push the CLI commits the context repository. `devc data status` compares each top-level entry with its records; a remote counts as matching only when the last pull merged the entry completely and what arrived equals the entry now, and a push only says it was sent (`>`) ([[internal/data.Service.Status]]). `devc data verify` recomputes one path.

## What can go wrong, and what prevents it

- **A pull destroying data here.** It deletes nothing and leaves sealed entries alone; everything it replaces can be brought back with `devc data trash restore`.
- **The preview misleads.** rsync compares with the staging directory, not with the data tree, so files show as new unless an interrupted pull left them; the preview says what will be transferred, never what the merge will replace here. Its sizes come from the remote's rsync.
- **What a pull trusts.** The remote and its rsync: what arrives is merged as it came; a remote symlink arrives as a link, which the worker never follows.
- **A push destroying data there.** No `--delete`; overwritten files go to the backup directory. rsync chooses files by size and modification time, which the box controls, and a staged file is the box's own file under a second name: the box can rewrite it in place until rsync has read it. It cannot add, remove or swap staged entries. A box can block pushes by creating `.devc-staging`.
- **What a record claims.** What a worker saw in a tree the box controls, at that moment: a push record describes the staged tree; a later change that keeps every name and size still looks recorded unless `--content` is used.

## Decisions that are not obvious

- **Push staging inside the project directory**, not `.devc`: with the box's ID mapping your user is the box user to the kernel, so whatever rsync may read the box may read; the vault-owned directories keep the tree fixed instead ([[chunk:C14]]).
- **No directory permissions on push**: the staging directories carry no meaningful mode; files keep theirs within the remote's umask.
- **`--max-size` limits the total**, from the dry run, rather than skipping large files silently.
- **Interrupted pulls continue** from their staging directory.

## Where to start reading

1. [[internal/data.RsyncArgs]]
2. [[internal/data.Service.Pull]] and [[internal/boxside.DataWorker.pullIn]]
3. [[internal/boxside.puller.merge]]
4. [[internal/data.Service.Push]] and [[internal/boxside.DataWorker.pushStage]]
5. [[internal/data.AppendManifest]] and [[internal/data.Service.Status]]
