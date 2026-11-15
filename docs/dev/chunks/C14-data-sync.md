# C14 — Data sync and manifest

Read `docs/dev/README.md`, `docs/dev/plan.md` (section 3.8) and the `STATUS.md` notes of C06 and C10b. Product spec: `specs-v2.md` section 14.4.

Data moves between machines (a cluster, a workstation, a laptop) with rsync, wrapped so that the defaults are safe and so that nothing on the host walks or trusts the box-writable data tree. A manifest records what exists where. There is no integration with job schedulers and no provenance tracking.

## 1. Remotes

`[data.remotes.<name>]` in the project definition (schema from C01): `host` (an SSH alias; empty means a local directory, for a mounted disk or NAS), `path`, and the default path lists `pull` and `push`. Paths are relative to the data root on both sides.

When a master connection for the host exists (C10b), rsync uses it (`-e "ssh -S <socket> -o ControlMaster=no"`); otherwise it uses the user's ordinary SSH. `DEVC_SSH_CONFIG` is honoured as in C10b.

## 2. Pull

`devc data pull <remote> [path…] [--dry-run] [--yes] [--max-size 2G]`

1. rsync from the remote into a staging directory outside every box: `local/staging/<realm>/<project>/<timestamp>/`. Flags: archive mode without owner, group, devices or specials; `--partial`; symlinks kept as links, never followed; never `--delete`.
2. Unless `--yes`, show first what would be transferred (an rsync dry run with itemised changes) and ask.
3. A helper-container worker moves the staged files into the data root. Targets may be in work or input zones, never inside a sealed entry. Where a different file already exists, the existing one is moved to the trash first; identical files (same bytes) are skipped. Ownership follows the zone.
4. Record the pull in the manifest and remove the staging directory.

## 3. Push

`devc data push <remote> [path…] [--dry-run] [--yes]`

1. The source is box-writable, so the host does not read it directly. A worker hard-links the selected files into `.devc/staging/push-<timestamp>/` (vault-owned, same filesystem, no copying), keeping symlinks as links and skipping special files.
2. Host rsync sends that directory to the remote: no `--delete`; `--backup` with a dated backup directory on the remote side for anything it would overwrite; `--partial`.
3. Record the push in the manifest; remove the staging directory.

## 4. Manifest

`durable/realms/<realm>/context/data/<project>/manifest.jsonl`, appended by seal, save, pull and push. An entry: time, machine name (new global setting `machine_name`, default the host name), action, path, file count, total size, a digest of the sorted list of relative paths with sizes, and for pull and push the remote's name.

- `devc data status [--json]`: per top-level entry of each zone, its current size and digest (from a worker), whether it differs from the last record, and on which machines and remotes it was last recorded.
- `devc data verify <path> [--content]`: recompute the digest; with `--content`, also SHA-256 over file contents, compared with a stored content hash when one exists (`--content` on seal or push stores it).

The manifest lives in the context repository, so it is committed and travels with it.

## 5. Tests

Unit: rsync argument construction for every case (assert no `--delete`, the backup directory, the master socket option); digest computation; manifest records.

Integration, with a local-directory remote for most cases, and the test `sshd` of C10b for one SSH case when available:

1. Pull into a work zone: files arrive owned by the box user; a differing existing file is in the trash; an identical one is untouched; a staged symlink pointing outside is stored as a link and nothing outside the data root changes.
2. Pull refuses a target inside a sealed entry.
3. Push: the remote receives the files; a file that would be overwritten on the remote is in the remote's backup directory; nothing is deleted on the remote.
4. During a push, the host rsync process never opens a path under the data root directly (assert through the argument list: its source is the staging directory).
5. `--dry-run` changes nothing on either side.
6. `data status` shows a change after a file is modified in the box, and where the entry was last pushed.
7. One pull through the SSH path using a master connection.

## 6. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `docs/guide.md` gains a section on moving data, including advice for clusters (seal finished runs with `chmod -R a-w`, keep results on backed-up storage, pull post-processed data off).
- `STATUS.md` and `LEFTOVER.md` are updated.
