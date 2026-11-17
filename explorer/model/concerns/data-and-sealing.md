+++
id = "data-and-sealing"
title = "Data and sealing"
summary = "A project's data tree: its zones and who owns what, why the host never walks it and which helper container does, sealing and exactly what it protects against and what not, and the trash."
order = 60
files = [
  "internal/data/data.go",
  "internal/data/box.go",
  "internal/data/helper.go",
  "internal/data/ops.go",
  "internal/boxside/datalayout.go",
  "internal/boxside/datalinks.go",
  "internal/boxside/dataproto.go",
  "internal/boxside/dataworker.go",
  "internal/cli/datacmds.go",
]
key = [
  "internal/data.Service.work",
  "internal/boxside.DataRequest",
  "internal/boxside.DataResult",
  "internal/boxside.DataWorker.Run",
  "internal/boxside.DataWorker.layoutDirs",
  "internal/boxside.DataWorker.seal",
  "internal/boxside.DataWorker.ownTree",
  "internal/data.DecodeResult",
  "internal/data.BoxHooks",
  "internal/boxside.DataLayout.SealTarget",
  "internal/data.Normalize",
  "internal/boxside.DataWorker.trashRestore",
]
spec = ["doc:specs-v2.md#14.1", "doc:specs-v2.md#14.2"]
tests = [
  "internal/boxside.TestDataSetup",
  "internal/boxside.TestDataSealHostile",
  "internal/boxside.TestDataSealSwap",
  "internal/boxside.TestDataTrash",
  "internal/boxside.TestReadDataRequest",
  "internal/data.TestDecodeResult",
  "internal/data.TestServiceWorkers",
  "internal/data.TestNormalize",
  "test/integration.TestData",
  "test/integration.TestEscape",
]
related = ["security-model", "savepoints", "data-sync", "isolation", "host-foundations", "requests-and-approval", "box-lifecycle"]

[[invariant]]
text = "Host code never opens, walks or changes the data tree (the SELinux relabel of the project before a start aside); every operation on it runs in a helper container without network, where the project is the only box-writable tree."
refs = ["internal/data.Service.work", "internal/data.Service.helperBase", "internal/data.Service.restic"]
tests = ["internal/data.TestServiceWorkers"]

[[invariant]]
text = "No process in a box can open a sealed entry for writing, truncate, chmod, rename, remove or add to it, nor move it or its zone; a descriptor it opened for writing before the seal still works."
refs = ["internal/boxside.DataWorker.seal", "internal/boxside.DataWorker.ownTree", "internal/boxside.DataWorker.layoutDirs"]
tests = ["test/integration.TestEscape", "test/integration.TestData"]

[[invariant]]
text = "Sealing never follows a symlink and changes only the layout's directories and inodes it finds inside the entry it names; a hard-linked file changes under every name."
refs = ["internal/boxside.DataWorker.ownTree", "internal/safefs.Dir.Sub", "internal/safefs.Dir.Lchown"]
tests = ["internal/boxside.TestDataSealHostile", "test/integration.TestData"]

[[invariant]]
text = "A seal is recorded, and its digest taken, only for the inode the worker opened and sealed: afterwards the entry's name must lead to the same device and inode as the open descriptor (a directory's, or a regular file's, opened once), and a regular file's digest is read from that descriptor."
refs = ["internal/boxside.DataWorker.seal", "internal/boxside.ownOpen", "internal/boxside.digestOpen"]
tests = ["internal/boxside.TestDataSealSwap"]

[[invariant]]
text = "The box can neither enter nor rename `.devc`, and a `.devc` the box created is refused rather than used."
refs = ["internal/boxside.DataWorker.layoutDirs", "internal/boxside.DataWorker.openDevc"]
tests = ["internal/boxside.TestDataSetup", "test/integration.TestEscape"]

[[invariant]]
text = "No worker deletes an entry of the data root: rm, a pull and an in-place restore move what they replace into the trash; only `devc data trash empty` deletes."
refs = ["internal/boxside.DataWorker.rm", "internal/boxside.puller.displace", "internal/boxside.inPlace.displace", "internal/boxside.DataWorker.trashEmpty"]
tests = ["internal/boxside.TestDataTrash", "internal/boxside.TestDataPullIn"]

[[invariant]]
text = "Every worker report is decoded strictly within 64 MiB, with nothing after it, and its IDs, paths and digests are validated before the host uses them."
refs = ["internal/data.DecodeResult", "internal/proto.StrictJSON", "internal/data.Service.work"]
tests = ["internal/data.TestDecodeResult", "internal/data.TestDecodeDigests", "internal/boxside.TestReportBounds"]

[[invariant]]
text = "A seal path a box sends in a request is read against the data root as the box sees it, and its refusal names no host path."
refs = ["internal/data.NormalizeInBox", "internal/daemon.requestService.create"]
tests = ["internal/data.TestNormalizeInBox", "internal/daemon.TestRequestHostPaths"]

[[diagram]]
id = "entry-states"
title = "What happens to an entry of the data tree"
kind = "graph"

[[diagram.node]]
id = "box"
label = "in a zone, the box's"
col = 1
row = 2
zone = "box"

[[diagram.node]]
id = "sealed"
label = "sealed: vault, read-only"
col = 1
row = 1
ref = "internal/boxside.DataWorker.seal"

[[diagram.node]]
id = "trash"
label = "in .devc/trash"
col = 2
row = 2
ref = "internal/boxside.DataWorker.newTrashEntry"

[[diagram.node]]
id = "gone"
label = "deleted"
col = 3
row = 2

[[diagram.edge]]
from = "box"
to = "sealed"
label = "devc seal"
ref = "internal/data.Service.Seal"

[[diagram.edge]]
from = "sealed"
to = "box"
label = "devc data unseal"
ref = "internal/data.Service.Unseal"

[[diagram.edge]]
from = "box"
to = "trash"
label = "rm, pull, restore"
ref = "internal/boxside.DataWorker.rm"

[[diagram.edge]]
from = "trash"
to = "box"
label = "trash restore"
ref = "internal/boxside.DataWorker.trashRestore"

[[diagram.edge]]
from = "sealed"
to = "trash"
label = "devc data rm"
ref = "internal/data.Service.Rm"

[[diagram.edge]]
from = "trash"
to = "gone"
label = "trash empty"
ref = "internal/boxside.DataWorker.trashEmpty"

[[diagram.edge]]
from = "box"
to = "gone"
label = "the box deletes it"
+++
## What it is for

A project's data tree holds what took days of compute: raw inputs, intermediate results, finished runs. The agent in the box must be able to work there, and it can delete or overwrite anything it owns. devc gives the tree zones with different protection, lets you hand a finished run to an owner the box is not (sealing), and keeps what devc itself replaces in a trash instead of deleting it. Savepoints ([[concern:savepoints]]) and moving data between machines ([[concern:data-sync]]) build on the same machinery.

## How it works

**Zones.** `[data]` in project.toml names a data root (default `data`, beside `main/`) and zone paths below it: `inputs`, `work`, `sealed`. Zones may not overlap; a project without zones has no data tree ([[internal/data.ProjectOf]]). The layout travels with every request and is checked again on both sides ([[internal/boxside.DataLayout.Validate]]). Path arguments are relative to the data root or absolute inside it, as the box or the host sees it (a box's request: as the box sees it, [[internal/data.NormalizeInBox]]); `..` is refused anywhere ([[internal/data.Normalize]]).

**Owners.** The [[term:vault]] is in-box UID 2000, an ID that no process in a box runs as or can take on, since boxes hold no capability ([[concern:isolation]]). A sticky directory lets only an entry's owner, or the directory's, rename or remove that entry, so the box cannot move anything the vault owns out of one. [[internal/boxside.DataWorker.layoutDirs]] makes the tree look like this:

| Path | Owner | Mode | For the box |
|---|---|---|---|
| project directory | vault, group dev | `1770` | creates entries; cannot move `.devc`, `.devc-staging` or the data root |
| `.devc/`, `.devc/savepoints/`, `.devc/trash/` | vault | `0700` | cannot enter or list |
| data root, parents of zone paths | vault, group dev | `1770` | the same, one level down |
| input zone | dev | `0755` | read-only: a bind mount over it |
| work zone | vault, group dev | `0775` | writes anything inside; cannot remove the zone |
| sealed zone | vault, group dev | `1770` | creates entries; cannot move the vault's |
| sealed entry | vault | directories `0555`, files without write bits | reads only |

**Who touches the tree.** Host code never does (README rule 2). [[internal/data.Service.work]] starts a [[term:helper-container]] for each operation:

```sh
podman run --rm --http-proxy=false --label devc.role=helper … --network none \
  --security-opt no-new-privileges --security-opt seccomp=<toolbox>/seccomp.json \
  --cap-drop=ALL --cap-add=<the operation's set> --userns=keep-id:uid=1000,gid=1000 \
  --mount type=bind,source=<project directory>,destination=/target \
  [--mount type=bind,source=<staging>,destination=/staging,ro] \
  --env DEVC_HELPER=1 --user 0:0 -i --detach-keys= \
  --rootfs <toolbox>/helper-rootfs:O /bin/devc box data-worker
```

| Operations | Capabilities |
|---|---|
| setup, seal, unseal, rm, trash restore, reflink save, restore, pull-in, push-stage | `CHOWN FOWNER DAC_OVERRIDE` |
| trash empty, savepoint drop, push-unstage | `DAC_OVERRIDE FOWNER` |
| ls, digest | `DAC_READ_SEARCH` |

The worker is the toolbox's devc, run as the container's root, which the box's mapping sends to one of your subordinate IDs; its capabilities count only in the container's user namespace, and the project directory (plus a read-only staging directory for restic restores and pulls) is all it can reach. restic runs from the same root filesystem but as the box user without capabilities ([[concern:savepoints]]). Only the data links are made in the box itself, by `devc box data-links` through `podman exec` as the box user ([[internal/data.Service.Links]]).

**The protocol.** The host writes one [[internal/boxside.DataRequest]] as JSON to the worker's standard input; [[internal/boxside.ReadDataRequest]] reads at most 1 MiB, strictly, version 1. [[internal/boxside.DataWorker.Run]] validates the layout and the time again, and each operation its own fields (a seal target, trash and savepoint IDs, a staging name, sync targets). It prints one compact [[internal/boxside.DataResult]]; a failure is a `devc: ` line on standard error, which the host shows made printable ([[internal/data.workerError]]). The host captures at most 64 MiB, and [[internal/data.DecodeResult]] refuses unknown fields, anything but white space after the report ([[internal/proto.StrictJSON]]), another version or operation, negative counts, invalid IDs, paths that are not data paths, malformed digests and more than 100,000 of them. The worker caps the lists that grow with the tree ([[internal/boxside.capReport]]).

**Setup** runs before a box is created or started, for the project and every data project it mounts read-write ([[internal/data.BoxHooks]]), and at the start of every worker operation that changes data (not of trash empty, savepoint drop and push-unstage). It creates what is missing, sets the owners and modes above, and refuses a layout path that is not a directory and a `.devc` that belongs to neither the vault nor the worker's root, which a crashed worker may leave. Input zones are mounted read-only over the project mount.

**Sealing.** `devc seal <path>` takes a direct child of a sealed zone, a directory or a regular file ([[internal/boxside.DataLayout.SealTarget]]). The worker opens it without following links and hands a directory to the vault and makes it `0555` before it lists it; then [[internal/boxside.DataWorker.ownTree]] does the same top-down, removes the write and set-ID bits of each file through a descriptor, re-owns symlinks as links, and skips FIFOs, sockets and devices. A regular file is opened once, and its state, owner, mode and digest all go by that descriptor ([[internal/boxside.ownOpen]], [[internal/boxside.digestOpen]]). Then the name must lead to the sealed descriptor's inode, else no record. A line goes to `.devc/seals.jsonl` with the relay's default-branch tip, and a digest to the manifest ([[concern:data-sync]]). Sealing a sealed entry walks it again, which completes an interrupted seal; it writes no seal record, and a manifest record only when `--content` asks for a content hash. `devc data unseal` walks bottom-up and gives the tree back with owner write bits. [[flow:data-seal]]

**The trash.** `devc data rm`, a pull and an in-place restore move entries by rename into `.devc/trash/<stamp>-<label>[-N]/payload/<path>`, with a `meta.json` of what moved; owners stay, so a sealed entry comes back sealed. `trash restore` checks every path first and refuses an occupied place ([[internal/boxside.DataWorker.trashRestore]]), but one that fails half-way cannot be repeated ([[doc:docs/dev/LEFTOVER.md#7]]); `trash empty` deletes, asking on a terminal. [[diagram:entry-states]]

**Data links.** Every worktree gets `data -> ../<root>` unless its index tracks `data` or something is there; `/data` goes into `.repo/info/exclude` ([[internal/boxside.Repo.EnsureDataLinks]]).

## What can go wrong, and what prevents it

A box controls everything it owns in the tree, the names (any byte but NUL), symlinks, special files, its files' modes, and the timing: it keeps running while workers work.

- **Tricking a worker.** Every step is one path component beneath a directory descriptor, never through a symlink ([[internal/safefs.Dir]]): `openat` with `O_NOFOLLOW`, `fchownat` without following, `fchmod` on descriptors, `renameat2` with `RENAME_NOREPLACE`, new files with `O_EXCL`. Trees deeper than 2,048 levels are refused.
- **A swap during the seal.** A directory that belongs to the vault and is `0555` no longer changes, so the walk below it sees a still tree. An entry renamed or swapped before the check is sealed wherever it went, and the seal fails without a record or digest; a file left at the name is the one sealed.
- **What sealing does not do.** It is against the box, not you: `unseal`, `rm` and `trash empty` are your commands, and a command run with `devc root` (also an approved `package` request) has every capability in the box. A file a process opened for writing before the seal stays writable through that descriptor, so seal finished runs. The box can still read and copy sealed data. A hard-linked file is sealed under every name. Special files stay as they were. A worker killed half-way leaves a partly sealed entry until the next seal. Work zones have no protection beyond savepoints.
- **Asking from the box.** A box can request a seal or a savepoint, approved by default (`requests.auto`), which the daemon carries out as `devc seal` and `devc save` ([[concern:requests-and-approval]]); no request unseals, removes, restores or empties the trash.
- **Worker output.** The host uses report paths only after validation; skipped texts are made printable, and the CLI makes paths printable before printing them.

## Decisions that are not obvious

- **Work zones belong to the vault**, writable through the group: a box-owned zone in a sticky root could still be renamed by its owner ([[chunk:C06]]).
- **Operations that change data apply the layout first**, so they also work on a project whose box has not started since, and always on a checked layout.
- **The worker comes from the toolbox**, never from the box's image.

## Where to start reading

1. [[internal/data.Service.work]] and [[internal/data.DecodeResult]]
2. [[internal/boxside.DataWorker.Run]] and [[internal/boxside.DataWorker.layoutDirs]]
3. [[internal/boxside.DataWorker.seal]] and [[internal/boxside.DataWorker.ownTree]]
4. [[internal/data.BoxHooks]]
5. [[internal/boxside.DataWorker.trashRestore]]
