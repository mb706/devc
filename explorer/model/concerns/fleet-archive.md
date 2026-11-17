+++
id = "fleet-archive"
title = "The archive on the hubs"
summary = "What a daemon uploads to every hub it joined: the durable state of each realm with [sync] hub, as generations of append-only or mutable files and as git bundles; how the hub stores it as a ratchet with budgets, records completion by its own clock and drops only mutable generations outside its retention window; and what a box, a machine or the hub can do to it."
order = 90
files = [
  "internal/archive/paths.go",
  "internal/archive/wire.go",
  "internal/fleet/client.go",
  "internal/fleet/uploader.go",
  "internal/fleet/state.go",
  "internal/fleet/workers.go",
  "internal/boxside/archiveworker.go",
  "internal/daemon/archive.go",
  "internal/hub/archive.go",
  "internal/hub/retention.go",
  "internal/cli/fleetcmds.go",
]
key = [
  "internal/fleet.Uploader.Pass",
  "internal/fleet.kindError",
  "internal/fleet.Uploader.syncAppend",
  "internal/fleet.Uploader.syncMutable",
  "internal/fleet.Uploader.syncRepo",
  "internal/archive.ParsePath",
  "internal/archive.ClassOf",
  "internal/archive.Excluded",
  "internal/boxside.ArchiveWorker.Run",
  "internal/hub.archiveStore.putFile",
  "internal/hub.archiveStore.checkQuota",
  "internal/hub.retentionDrops",
  "internal/hub.archiveStore.retain",
]
spec = ["doc:specs-v2.md#5", "doc:specs-v2.md#13", "doc:docs/hub-deployment.md#10", "doc:docs/guide.md#the-archive-on-the-hubs"]
tests = [
  "internal/fleet.TestRealmRefusalContained",
  "internal/hub.TestArchiveRatchet",
  "internal/hub.TestArchiveBudgets",
  "internal/hub.TestArchiveGenerations",
  "internal/hub.TestArchiveClasses",
  "internal/hub.TestArchiveDurableCompletion",
  "internal/hub.TestRetentionDrops",
  "internal/hub.TestRetentionHostile",
  "internal/hub.TestCheckpoints",
  "internal/hub.TestArchiveAdmission",
  "internal/hub.TestArchiveNamesAndDirectories",
  "internal/fleet.TestOffsetProtocol",
  "internal/fleet.TestFileClasses",
  "internal/fleet.TestSnapshotConsistency",
  "internal/fleet.TestBundleSequence",
  "internal/fleet.TestRefLogChunks",
  "internal/boxside.TestArchiveWorker",
  "test/integration.TestFleet",
]
related = ["security-model", "state-restore", "secret-transfer", "hub-access", "hub-machine-link", "git-ratchet", "secrets", "worktrees-and-context"]

[[invariant]]
text = "No upload changes stored bytes: it appends at the stored length, a bundle's .part is renamed without replacing anything, and of what was stored before, an upload removes only what retention chooses and secrets bundles beyond the newest ten."
refs = ["internal/hub.archiveStore.put", "internal/hub.archiveStore.putFile", "internal/hub.appendFile", "internal/hub.archiveStore.retain", "internal/hub.Server.apiSecretsPut"]
tests = ["internal/hub.TestArchiveRatchet"]

[[invariant]]
text = "A machine writes only below archive/<its registry name>/ and secrets/<its registry name>/; nothing it sends selects another tree."
refs = ["internal/hub.Server.apiArchivePut", "internal/hub.Server.apiSecretsPut", "internal/archive.ParsePath"]
tests = ["internal/hub.TestArchiveRatchet"]

[[invariant]]
text = "Journals, request, network and gate logs are never mutable, and every new generation of a file keeps the class the hub recorded for it."
refs = ["internal/archive.MutableAllowed", "internal/hub.Server.apiArchivePut", "internal/hub.recordedClass", "internal/hub.archiveStore.putFile"]
tests = ["internal/hub.TestArchiveClasses", "internal/fleet.TestClassAdoption"]

[[invariant]]
text = "The hub removes a stored generation on its own only when it is recorded as mutable and retentionDrops chooses it, after another generation of the file first completed and its record is durable."
refs = ["internal/hub.archiveStore.retain", "internal/hub.retentionDrops", "internal/hub.writeMeta"]
tests = ["internal/hub.TestRetentionDrops", "internal/hub.TestArchiveDurableCompletion"]

[[invariant]]
text = "For every checkpoint of the window the newest generation completed at or before it is kept; completion times are the hub's and never decrease with the generation number."
refs = ["internal/hub.retentionDrops", "internal/hub.completionTime", "internal/hub.archiveStore.putFile"]
tests = ["internal/hub.TestRetentionHostile", "internal/hub.TestCheckpoints", "internal/hub.TestArchiveClasses"]

[[invariant]]
text = "An honest machine uploads no file at a path its manifests name in home.secret_files; the hub does not check this, and content a box copies to another name is archived."
refs = ["internal/archive.Excluded", "internal/boxside.archiveWalk.walk", "internal/fleet.Uploader.realmPass", "internal/archive.ValidRel"]
tests = ["internal/boxside.TestArchiveWorker", "internal/fleet.TestOffsetProtocol"]

[[invariant]]
text = "The host never walks box-writable harness state or follows a link in it: the worker lists it in the helper container, and each listed file is opened through safefs."
refs = ["internal/boxside.ArchiveWorker.Run", "internal/boxside.DecodeArchiveListing", "internal/fleet.Uploader.syncFile"]
tests = ["internal/boxside.TestArchiveWorker"]

[[invariant]]
text = "A refusal the hub answers (507, or a 4xx) stays with its realm: a reached quota ends that realm's pass, another refusal only the kind it hit (the files, or one repository); the realm's uploads rest for ten minutes with the refusal in `devc sync status`, and the other realms go on in the same pass. A server error or a network error ends the pass, and nothing rests."
refs = ["internal/fleet.Uploader.Pass", "internal/fleet.Uploader.realmPass", "internal/fleet.realmRefusalOf", "internal/fleet.kindError", "internal/cli.newSyncCmd"]
tests = ["internal/fleet.TestRealmRefusalContained"]

[[invariant]]
text = "Every archive request is admitted before its bounded body is read, and every upload is checked against the quotas, file and realm limits and the free-space reserve before a file is created or extended."
refs = ["internal/hub.Server.admitted", "internal/hub.archiveStore.admit", "internal/hub.archiveStore.planNew", "internal/hub.archiveStore.checkQuota"]
tests = ["internal/hub.TestArchiveBudgets", "internal/hub.TestArchiveAdmissionNoWait", "internal/hub.TestArchiveNamesAndDirectories"]

[[diagram]]
id = "generation"
title = "The life of a file generation on the hub"
kind = "graph"

[[diagram.node]]
id = "none"
label = "not on the hub"
col = 1
row = 1

[[diagram.node]]
id = "uploading"
label = "uploading (incomplete)"
col = 2
row = 1
zone = "hub"
ref = "internal/hub.appendFile"

[[diagram.node]]
id = "complete"
label = "complete (append-only)"
col = 3
row = 1
zone = "hub"

[[diagram.node]]
id = "grown"
label = "grown since it completed"
col = 4
row = 1
zone = "hub"

[[diagram.node]]
id = "sealed"
label = "complete, sealed (mutable)"
col = 3
row = 2
zone = "hub"

[[diagram.node]]
id = "dropped"
label = "dropped by retention"
col = 3
row = 3
ref = "internal/hub.archiveStore.retain"

[[diagram.edge]]
from = "none"
to = "uploading"
label = "first PUT: class, mode"
ref = "internal/hub.archiveStore.putFile"

[[diagram.edge]]
from = "uploading"
to = "uploading"
label = "PUT at stored length"

[[diagram.edge]]
from = "uploading"
to = "complete"
label = "final=1: time recorded"
ref = "internal/hub.completionTime"

[[diagram.edge]]
from = "complete"
to = "grown"
label = "more bytes"

[[diagram.edge]]
from = "grown"
to = "complete"
label = "final=1 again"

[[diagram.edge]]
from = "uploading"
to = "sealed"
label = "final=1 (mutable)"

[[diagram.edge]]
from = "sealed"
to = "dropped"
label = "outside the window"
ref = "internal/hub.retentionDrops"

[[diagram.edge]]
from = "uploading"
to = "dropped"
label = "older than newest complete"
ref = "internal/hub.retentionDrops"
+++
## What it is for

The archive is the owner's backup of what a realm cannot recreate: session journals and logs, the shared harness state (transcripts, harness homes), the relays and the context repository. A daemon uploads it to every hub it joined while it runs, a pass a minute, so little goes down with a machine. Boxes write part of what is archived and an enrolled machine may be compromised, so neither may be able to empty the backup: the hub decides what it keeps.

## How it works

**What is archived**, for each realm with `[sync] hub = true` ([[config:realm.sync.hub]]):

| What | Archive path | Class |
|---|---|---|
| session journals | `files/journal/<project>/<session>.jsonl` | append-only |
| request logs, savepoint index, network and gate logs (with `.1`) | `files/requests/…`, `savepoints/…`, `netlog/…`, `gate-log.jsonl` | append-only |
| shared harness state | `files/harness/<harness>/…` | append-only where the manifest's transcript globs match, else mutable |
| relays, the context repository | `bundles/relay/<project>/`, `bundles/context/`: bundles and `refs.jsonl` | bundles in sequence |

[[internal/archive.ValidRel]] names every file path a hub accepts. Left out: secrets ([[concern:secret-transfer]]); credential files a manifest lists in `home.secret_files` ([[internal/archive.Excluded]], applied on the machine by worker, uploader, export and restore, never by the hub); state of a harness without a manifest on this machine; links, special files, names outside the archive's character set and harness files above `[sync] max_file_size` (default 256 MiB; host-written files up to 1 TiB), which are counted as skipped; service logs, session-owner records and shares.

**The uploader.** [[internal/daemon.hubLink.archiveLoop]] runs one uploader per listed hub beside the machine link, with its own client ([[internal/fleet.Client]]), every minute or at once for `devc sync status --now`. A pass ([[internal/fleet.Uploader.Pass]], [[flow:archive-upload]]) takes a lock per hub and goes through the realms in name order. Host-written files are listed directly ([[internal/fleet.hostFiles]]); the box-writable harness state is listed by the archive worker in the helper container ([[internal/boxside.ArchiveWorker.Run]], at most every five minutes per realm), and each listed file is opened through safefs. The bookkeeping in `local/archive/<hub-id>/` only saves questions: the hub's stored lengths, index and prefix hashes are the truth ([[internal/fleet.Uploader.fileConflict]]).

**Generations and classes.** A file is stored as generations `files/<rel>` (0) and `files/<rel>~<n>`, each appended to at the hub's stored length. A generation's first upload proposes its class, taken from this machine's manifests, never from the box ([[internal/archive.ClassOf]]). The hub refuses mutable outside `files/harness/` ([[internal/archive.MutableAllowed]]) and any class other than the one it recorded for the file ([[internal/hub.recordedClass]]), answering with the class to use.

- An append-only file is uploaded from the file itself ([[internal/fleet.Uploader.syncAppend]]). When the bytes the hub holds are no longer the file's prefix (it shrank, or the prefix hashes differently), a new generation begins, at most one an hour per file.
- A mutable file is copied to a snapshot and uploaded from that ([[internal/fleet.Uploader.syncMutable]]): when its inode, size, modification time or mode changed, a new generation begins, at most one per `[sync] mutable_interval` (default six hours).

**Completion.** Once a generation is uploaded, and for an append-only file only if it did not change while it was read, the machine sends `final=1` at its length. The hub writes the record `<file>~meta` and syncs it before answering ([[internal/hub.archiveStore.putFile]], [[internal/hub.writeMeta]]). The time is the hub's clock, never earlier than an older generation's ([[internal/hub.completionTime]]), and an older generation cannot complete once a newer one has. An append-only generation may grow and be marked again (the time stays); a complete mutable one takes no more bytes. [[diagram:generation]]

**Repositories.** After the files come the relays and the context repository ([[internal/fleet.Uploader.syncRepo]]). When the refs the archive carries ([[internal/gitx.ArchiveRef]]) differ from those the hub's ref log records, host git (the context worker, for the context repository) bundles the objects those tips do not reach. The bundle is uploaded as `<seq>-<id>.bundle.part` and made final; the hub takes bundle n only after bundle n−1 is complete. A line `{bundles, set, del}` then goes to `refs.jsonl`.

**Retention** ([[flow:retention]]). When a mutable generation completes for the first time, [[internal/hub.archiveStore.retain]] removes the file's generations that [[internal/hub.retentionDrops]] chooses: those recorded as mutable that are complete with a time and neither among the newest three complete ones nor, for some checkpoint, the newest one completed at or before it; and incomplete ones older than the newest complete one. Checkpoints are the starts of the last `keep_daily` days (7) and `keep_weekly` weeks (4), UTC, by the hub's clock ([[internal/hub.checkpoints]]). Nothing is dropped while a time recorded for the file lies after the hub's now. With `keep_daily = 2` and `keep_weekly = 1`, when g9 completes on Wednesday 14:00:

| Generation, completed | Kept? | Why |
|---|---|---|
| g1 Sat 12:00, g3 Mon 10:00, g5 Tue 09:00 | dropped | no checkpoint's newest |
| g2 Sun 18:00, g4 Mon 22:00, g6 Tue 23:00 | kept | newest by Mon, Tue, Wed 00:00 |
| g7, g8, g9 (Wednesday) | kept | the newest three |

Besides retention, the hub deletes secrets bundles beyond the newest ten per machine and realm, and what a failed upload created; `devc hub archive prune` ([[internal/hub.PruneArchive]]) deletes from one machine's archive, never its secrets bundles, what the owner asks for.

## What can go wrong, and what prevents it

- **A box**, through an honest machine, writes the shared harness state. It cannot make the host read outside that tree or follow a link, choose a file's class, or get a file at a path the manifest names as a credential archived, though a copy under another name is archived. It can fill the archive: 200,000 files of up to `max_file_size`, and a transcript it rewrites gets a new generation, kept for good, every hour. Once the realm's quota (20 GiB) or the machine's (50 GiB) is reached, the hub answers 507 ([[internal/hub.archiveStore.checkQuota]]). That ends this realm's pass, its repositories included, which share the quota; its uploads rest ten minutes, `devc sync status` names it, and the other realms go on ([[internal/fleet.kindError]], [[doc:docs/dev/LEFTOVER.md#fleet-and-the-web-app-x12]], item 1).
- **A malicious machine** writes only below its own registry name ([[internal/hub.Server.apiArchivePut]]) and cannot overwrite, truncate or rename stored bytes. In its own archive it can append garbage and start generations; by completing mutable ones it pushes its older mutable generations out, but it cannot name a time, so the state at each checkpoint stays. It can read every machine's archive (index, files, prefix hashes: [[internal/hub.Server.apiArchiveFile]]). Towards others it holds at most two of eight upload and two of eight read places ([[internal/hub.archiveStore.admit]]), and on a hub whose disk is smaller than the quotas it can push free space down to the 2 GiB reserve, after which every machine gets 507.
- **The hub** holds all of it unencrypted (secrets aside; credential files only as far as machines left them out); a compromised hub can read, alter or delete it and serve a hostile archive to a restore ([[concern:state-restore]]).
- **Cost on the hub.** A body of at most `max_request` (8 MiB), two minutes to send it, 30 seconds per write of an answer; names checked against the file system's limit and every directory counted before anything is created; a failed upload removes what it created, except a generation whose completion record alone failed, which stays incomplete ([[internal/hub.archiveStore.planNew]], [[internal/hub.archiveStore.create]]).

## Decisions that are not obvious

- **The machine numbers generations**: the hub only appends and never needs to know a file was rewritten ([[chunk:C17]]). Numbers only grow: the hub refuses a generation older than one it holds.
- **Snapshots for mutable files**: a database that keeps changing could otherwise never complete ([[finding:D1-14]]).
- **The hub decides classes and keeps time** ([[finding:E1-04]]): a machine's word would let it mark a journal mutable and have it dropped.
- **"At or before" a checkpoint**, the same rule as `state pull --as-of`, so restoring as of a checkpoint gives the state retention kept ([[chunk:C19r3]]).
- **Every machine reads every archive**: the hub cannot know which realms a machine is configured for ([[chunk:C17]]).

## Where to start reading

1. [[internal/fleet.Uploader.Pass]] and [[internal/fleet.Uploader.realmPass]]
2. [[internal/fleet.Uploader.syncAppend]] and [[internal/fleet.Uploader.syncMutable]]
3. [[internal/hub.archiveStore.putFile]]
4. [[internal/hub.retentionDrops]] and [[internal/hub.archiveStore.retain]]
5. [[internal/fleet.Uploader.syncRepo]]
