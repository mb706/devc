# X9 — Explorer content: data, shares, sources

Read `docs/dev/chunks/X-content-guide.md` first. It is the work order for every content chunk and it is binding; this file only names your area. Then read `explorer/README.md` (format reference, including "Writing for the explorer").

## Area

- Packages, whole: `internal/data`, `internal/shares`, `internal/depsrc`.
- From `internal/boxside`: `datalayout.go`, `datalinks.go`, `dataproto.go`, `datasync.go`, `dataworker.go`. From `internal/toolbox`: `restic.go`, `skills.go` (comments are X4's; the files belong to your concerns too).
- From `internal/cli`: `datacmds.go`, `datasync.go`, `sharecmds.go`.

## Records to read for orientation

`STATUS.md` sections C06, C14, C16, C19r2 and C19r4 (the data, sync and source parts); findings D2-06, D2-10 to D2-14, E2-03 to E2-06, E2-08, F2-01, F2-02 with their responses.

## Concerns owed

Use these ids, and `order` values in the range 60 to 69.

- `data-and-sealing`: a project's data tree, why the host never walks it and who does instead, sealing and what it protects against, the trash.
- `savepoints`: the drivers, saving, restoring, collection.
- `data-sync`: remotes, pulling and pushing with rsync, the preview and its limits, staging, the worker's merge, the manifest and what it may claim, locks.
- `shares`: publishing a tree from a relay for other boxes of the realm, versions, mounts.
- `dependency-sources`: `devc src`: the ecosystems, the checksum rules and their exceptions, exact versions and the cache, extraction.

## Flows owed

- `data-seal`: `devc data seal` to an entry the box can no longer change.
- `data-save` and `data-restore`.
- `data-pull`: a remote directory into the data tree, with staging and the worker.
- `data-push`.
- `share-publish`: a committed tree to a mounted share.
- `src-fetch`: `devc src <package>` in a box to unpacked source.

## Diagrams to consider

The pipeline of a pull (remote, staging, worker, data tree) with who owns each stage. The states of a data entry. The checksum decision for a source download.

## Notes

- Concerns planned by other chunks are listed in `docs/dev/chunks/X-content-areas.md`. Link (`related`, `[[concern:…]]`) only to those that already exist; the last chunk adds the missing cross-links.
