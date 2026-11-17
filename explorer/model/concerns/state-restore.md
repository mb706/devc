+++
id = "state-restore"
title = "Restore, export and import"
summary = "devc state pull rebuilds a realm from one machine's archive on a hub, now or as of a time; devc state export writes a realm's durable state to one tar file and devc state import restores it. One restore path for both: chosen generations, files and refs only added, never replaced, and nothing outside devc's paths and ref namespaces."
order = 91
files = [
  "internal/fleet/restore.go",
  "internal/fleet/export.go",
  "internal/cli/fleetcmds.go",
]
key = [
  "internal/fleet.Restorer.Restore",
  "internal/fleet.ExistingState",
  "internal/fleet.chooseGenerations",
  "internal/fleet.chooseUsable",
  "internal/fleet.Restorer.restoreFiles",
  "internal/fleet.Restorer.restoreRepo",
  "internal/fleet.ReplayRefs",
  "internal/gitx.Relay.RestoreRefs",
  "internal/fleet.Exporter.Export",
  "internal/fleet.ReadExport",
]
spec = ["doc:specs-v2.md#5", "doc:docs/guide.md#backups"]
tests = [
  "internal/fleet.TestRestoreFilesAndHostileArchive",
  "internal/fleet.TestBundleSequence",
  "internal/fleet.TestCompleteGenerations",
  "internal/fleet.TestRestoreMixedFormats",
  "internal/fleet.TestRestoreAsOf",
  "internal/fleet.TestRestoreAsOfUntimed",
  "internal/fleet.TestRestoreRefusesReplaceRefs",
  "internal/fleet.TestExportImport",
  "internal/cli.TestFleetUsage",
  "test/integration.TestFleet",
]
related = ["fleet-archive", "git-ratchet", "worktrees-and-context", "secret-transfer"]

[[invariant]]
text = "A restore never replaces a file or moves a ref that exists: files are created exclusively, relay and context refs only where missing."
refs = ["internal/fleet.Restorer.restoreFiles", "internal/gitx.Relay.RestoreRefs", "internal/boxside.ContextWorker.Restore"]
tests = ["internal/fleet.TestRestoreFilesAndHostileArchive", "internal/fleet.TestBundleSequence"]

[[invariant]]
text = "Without --merge a restore refuses a realm holding any kind of state ExistingState looks for (all the archive carries but gate-log.jsonl.1); an import never merges."
refs = ["internal/fleet.Restorer.Restore", "internal/fleet.ExistingState", "internal/cli.newStateCmd"]
tests = ["internal/fleet.TestBundleSequence", "internal/fleet.TestRestoreFilesAndHostileArchive"]

[[invariant]]
text = "A restored file lies at a path ParsePath accepts, beneath the realm's durable directory, created without following a link."
refs = ["internal/archive.ParsePath", "internal/fleet.Restorer.restoreFiles"]
tests = ["internal/fleet.TestRestoreFilesAndHostileArchive"]

[[invariant]]
text = "Credential files, and the state of a harness without a manifest on this machine, are never restored."
refs = ["internal/fleet.Restorer.restoreFiles", "internal/archive.Excluded"]
tests = ["internal/fleet.TestRestoreFilesAndHostileArchive"]

[[invariant]]
text = "A relay restore creates refs only in devc's namespaces, for objects that came through git's fsck."
refs = ["internal/gitx.ArchiveRef", "internal/gitx.Relay.FetchBundle", "internal/gitx.Relay.RestoreRefs"]
tests = ["internal/fleet.TestRestoreRefusesReplaceRefs"]

[[invariant]]
text = "A mutable file is restored only from a generation the hub recorded as complete, at its committed length."
refs = ["internal/fleet.chooseUsable", "internal/fleet.chooseGenerations"]
tests = ["internal/fleet.TestCompleteGenerations", "internal/fleet.TestRestoreMixedFormats"]

[[invariant]]
text = "A realm is restored only under its own name."
refs = ["internal/fleet.Restorer.Restore", "internal/cli.newStateCmd"]
tests = ["internal/fleet.TestCompleteGenerations"]
+++
## What it is for

A machine is lost, reinstalled or new, and the owner wants a realm back: its journals and logs, its relays with every banked commit, its context repository and its shared harness state. `devc state pull` takes them from a hub's archive ([[concern:fleet-archive]]), from this machine's archive or another's, optionally as they were at a time. `devc state export` writes the same state into one tar file for any backup tool, and `devc state import` restores that. Whatever the source, a restore may only add: the owner must be able to run it into a realm that has data without losing any.

## How it works

**Commands** ([[internal/cli.newStateCmd]]). `devc state pull <realm> --from <hub>` restores the archive of `--machine` (default: this machine, by the name the hub knows it by); `--list` shows every machine's archive of the realm; `--merge` allows a realm that has state; `--as-of` picks a time. The realm must be configured here. `devc state import <file>` restores the realm the export names (`--realm` may only repeat it) and never merges.

**One restore path** ([[internal/fleet.Restorer.Restore]], [[flow:state-pull]]). The source is a hub ([[internal/fleet.HubSource]]: index pages through [[internal/fleet.Client.Index]], which validates every entry and requires pages to advance, at most a million entries) or an extracted export ([[internal/fleet.DirSource]]: the regular files of the tree, each a complete generation at its length). The restore:

1. refuses a source of another realm ([[finding:D1-19]]: journals, request logs and ownership records name the realm);
2. without `--merge`, refuses a realm in which [[internal/fleet.ExistingState]] finds session journals, relays, the context repository, request, savepoint or network logs, the gate log or non-empty shared harness state;
3. chooses a generation per file ([[internal/fleet.chooseGenerations]]) and writes the files ([[internal/fleet.Restorer.restoreFiles]]);
4. restores each repository from its bundles and ref log ([[internal/fleet.Restorer.restoreRepo]]).

**Which generation.** Without `--as-of`, the newest generation with a completion record, at the length it was completed with; a generation without any record (stored before records existed) counts at its stored length; one with a record that never completed is passed over while an older candidate exists. A file without any candidate is taken from its newest generation as far as it goes if it is append-only (any prefix is a state it had) and left out if mutable ([[internal/fleet.chooseUsable]]); both are reported.

**As of a time.** `--as-of` takes RFC 3339 or a date, meaning the start of that day in UTC ([[internal/cli.parseAsOf]]). Per file it takes the newest generation the hub completed at or before that time, by the hub's clock, at its committed length. An append-only generation keeps no history of its length, so it comes with what was appended to it since (counted and reported). Generations without a completion time (completed on an older hub) count as older than every timed one: when every timed generation is newer, the file is chosen among those and reported; with none, it is left out ([[finding:F1-02]]). Relays and the context repository have no generations and are taken as they are now, which the report says.

**Files.** Only paths [[internal/archive.ParsePath]] accepts, below the realm's durable directory. Harness files need a manifest on this machine and are never credential files. Each file is created beneath the durable directory component by component without following links, with `O_EXCL`: an existing file is counted and kept. At most 4 GiB per file; one that cannot be fetched whole is removed again and the restore stops. Permission bits are the archived ones without set-ID and sticky bits, always readable and writable by the owner ([[internal/fleet.restoreMode]]).

**Repositories.** A repository needs its ref log (a larger one than 64 MiB stops the restore) and complete bundles in sequence from 0; bundles after a gap are left out and reported. Each bundle (at most 16 GiB) is downloaded to `local/tmp/restore-*`. The ref log's lines are replayed only while the bundles they need are present ([[internal/fleet.ReplayRefs]]). A relay is created, or an existing one configured again ([[internal/gitx.Relay.Create]]); each bundle is fetched with fsck into `refs/devc/restore/` ([[internal/gitx.Relay.FetchBundle]]); then the refs the relay lacks are created in one transaction, only in devc's namespaces and only where their objects are present ([[internal/gitx.Relay.RestoreRefs]]), and the scratch refs are deleted. A fresh relay's HEAD becomes `main` when the archive has that branch. The context repository's bundles go to the context worker ([[internal/boxside.ContextWorker.Restore]]), which creates the missing refs and, if it created `main`, writes main's files that are missing from the working tree.

**What a restore does to what exists:**

| Part | Already there | What happens |
|---|---|---|
| a file | any file at that path | kept; counted as existing |
| a relay ref | another value | kept; reported as a conflict |
| a relay | a relay of that project | configured again; refs added |
| the context repository | refs, working tree | missing refs added; files only when `main` was created |

**Export and import.** [[internal/fleet.Exporter.Export]] writes `devc-export.json`, the host-written files read through safefs, the harness state as the archive worker packs it with every entry checked again ([[internal/fleet.Exporter.copyHarness]]), and per repository one full bundle with a one-line ref log; the context repository is committed first. The output file is created exclusively, never overwritten. [[internal/fleet.ReadExport]] extracts an export through the safe extractor ([[internal/extract.File]]: at most a million entries, 16 GiB per file, 256 GiB in all) into `local/tmp/import-*/tree` and decodes its manifest strictly.

## What can go wrong, and what prevents it

The archive a restore reads is hostile: a compromised hub, a machine whose archive is pulled, a tampered export.

- **Writing elsewhere.** Paths are parsed before they name anything and files are created beneath a directory descriptor; a planted link is not followed ([[internal/fleet.Restorer.restoreFiles]]).
- **Replacing state.** Nothing is replaced (table above); `--merge` adds only what is missing.
- **Rewriting history.** Refs are created only in devc's namespaces, never `refs/replace/` ([[finding:D1-07]]); every object passes fsck; host git runs no hook ([[concern:git-ratchet]]).
- **Costs.** Index entries, file and bundle sizes and the ref log are capped. Restores stage whole bundles and imports extract the whole export first ([[doc:docs/dev/LEFTOVER.md#5]]).
- **What remains.** Within those paths the archive chooses the content: journals and logs, harness state (which boxes write anyway) and the history in relays the restore creates. Restored journals become sessions of the restoring machine and are archived again under its name. A restore that stops part-way leaves what it wrote; the realm then needs `--merge`.

## Decisions that are not obvious

- **Only under the realm's own name** ([[finding:D1-19]]): renaming would need every journal, request and ownership record rewritten.
- **Export uses the archive's layout**, so import and `state pull` share one restore path and imported relays get devc's configuration rather than a tar file's ([[chunk:C17]]).
- **Incomplete generations** are reported, never silently used for mutable files ([[finding:D1-15]], [[finding:E1-06]]).

## Where to start reading

1. [[internal/fleet.Restorer.Restore]]
2. [[internal/fleet.chooseGenerations]]
3. [[internal/fleet.Restorer.restoreFiles]]
4. [[internal/fleet.Restorer.restoreRepo]] and [[internal/gitx.Relay.RestoreRefs]]
5. [[internal/fleet.Exporter.Export]]
