# C06 — Data

Read `docs/dev/README.md`, `docs/dev/plan.md` (sections 3.3, 3.8, 3.13) and the `STATUS.md` notes of the earlier chunks first. Product spec: `specs-v2.md` section 14.

After this chunk a project's data directory has zones with different protection, finished artifacts can be sealed so that nothing in the box can change or remove them, the writable part can be saved and restored, and deletion goes through a trash.

Everything in this chunk that touches the project tree from the host side runs in the **helper container** (plan 3.3): the tree is box-writable, so host code must not walk it. The helper sees only what is mounted into it.

## 1. Ownership model

Verified on this host during design: a directory handed to in-box UID 2000 ("vault") with write bits removed, inside a parent that is vault-owned and sticky, cannot be deleted, overwritten, renamed or `chmod`-ed by the box user, while the box user can still create siblings; the host user can read it.

For a project that defines `[data]`:

| Path | Owner (in-box IDs) | Mode | Why |
|---|---|---|---|
| project directory | 2000:1000 | 1770 | the box can create and remove its own entries (`main`, worktrees) but not rename or remove vault-owned ones |
| `<realm>` directory above it | host user | 0700 | nobody else on the host can reach project directories |
| `.devc/` and below | 2000:2000 | 0700 | invisible to the box; holds `savepoints/`, `trash/`, `seals.jsonl` |
| data root | 2000:1000 | 1770 | zone directories cannot be renamed or removed by the box |
| input zone directories | 1000:1000 | 0755 | protected by a read-only mount, so the user can still add files from the host |
| work zone directories | 1000:1000 | 0755 | ordinary |
| sealed zone directories | 2000:1000 | 1770 | the box creates run directories inside; sealed ones become immovable |
| a sealed entry | 2000:2000, recursively | directories 0555, files without write bits, readable by all | cannot be changed from the box |

Projects without `[data]` keep an ordinary box-owned project directory and get no `.devc/`.

## 2. Helper container

Add to `internal/engine` or a small package of its own: run a worker in the helper container.

- `podman run --rm --network none --cap-drop=ALL --security-opt no-new-privileges --userns=keep-id:uid=1000,gid=1000 … --rootfs <toolbox>/helper-rootfs:O /bin/devc box <worker> …`, with the same SELinux options a box of that realm would get. The user-namespace mapping must be the box's, so that UID 2000 means the same host ID.
- Mounts and identity per worker: only the project directory (or the paths named below); `--user 0:0` with exactly the capabilities the worker needs (`CHOWN`, `FOWNER`, `DAC_OVERRIDE`, and `DAC_READ_SEARCH` where reading is required) for ownership work; `--user 1000:1000` with no capabilities for anything that only needs the box user's view.
- Workers live in `internal/boxside`. They take paths relative to the mounted root, resolve them beneath it without following symlinks (the `safefs` approach, extended with the write-side operations needed: `fchownat` with no-follow, `fchmod` on opened descriptors, `renameat2`, `mkdirat`, `unlinkat`), and never act on anything reached through a symlink. Symlinks inside a tree are handled as objects: re-owned with no-follow, copied as links, never dereferenced. Device nodes and FIFOs are skipped and reported.
- A worker prints a JSON result; the host treats it as untrusted output (parse strictly, cap the size).

## 3. Setup and mounts

- `devc up` on a project with `[data]` runs the setup worker before the box starts: create the data root, zone directories and `.devc/` if missing and apply the table above. Idempotent. It refuses to proceed if the data root or a zone path exists as something other than a directory.
- Input zones are mounted read-only over the project mount, one nested bind mount per path, through C02's extension point. They are part of the configuration hash.
- Each worktree gets a relative `data` symlink to the data root unless the repository tracks a path of that name; the link is listed in the repository's `info/exclude`. Do this in the box at `project init`, at `wt new` (C05), and at `up` for existing worktrees.
- Extend the generated sandbox description (C03) with the data paragraph: where the data root is, which paths are read-only, that sealed directories cannot be changed, where new results go, and that the user seals a finished run with `devc seal <path>`.

## 4. Commands

Paths are given relative to the data root; an absolute in-box or host path inside the data root is accepted and normalised. `ref` is optional as everywhere.

| Command | Behaviour |
|---|---|
| `devc seal <path>` | The path must be a direct child of a sealed zone directory, exist, and not be a symlink. The worker re-owns it recursively to the vault and removes write bits. Appends a record to `.devc/seals.jsonl`: path, time, file count, total size, and the relay's current tip of the default branch when the project has a relay. Sealing an already sealed entry is a no-op. |
| `devc data unseal <path>` | Gives the entry back to the box user and restores owner write bits. |
| `devc data rm <path>` | Moves the entry (from any zone) to `.devc/trash/<utc-timestamp>-<name>/` by rename. Never deletes. |
| `devc data trash ls`, `… restore <id>`, `… empty [--older-than 30d]` | List, move back (refusing if the original path is occupied), or really delete. Deletion asks for confirmation unless `--yes`. |
| `devc save [name]` | Section 5. |
| `devc restore <savepoint> [path…] [--in-place]` | Section 5. |
| `devc data ls [--json]` | Zones and their paths; sealed entries with time and size; savepoints; the number and size of trash entries. Sizes come from a worker, not from the host walking the tree. |
| `devc data gc` | Applies savepoint retention and, for restic, prunes. |

Unsealing, trash deletion and restoring are host-only actions; nothing in this chunk lets a box trigger them.

## 5. Savepoints

A savepoint covers the data root except input zones and sealed entries.

- ID: UTC timestamp, plus `-<name>` when a name is given. An index of savepoints is kept outside the box at `durable/realms/<realm>/savepoints/<project>.jsonl`: id, name, time, driver, paths, file count, size, and the relay's default-branch tip.
- Driver selection: the project's `data.savepoints.driver`. `auto` uses `reflink` when a clone probe on the project filesystem succeeds, otherwise `restic` when it is installed, otherwise fails with what to do. `none` disables savepoints.

**`reflink` driver.** A worker copies the covered trees into `.devc/savepoints/<id>.partial/` cloning every regular file with `FICLONE`, preserving modes, times and symlinks, then renames to `<id>/`. The result is vault-owned and read-only. If any clone fails with "not supported", the driver is unavailable; remove the partial copy.

**`restic` driver.**

- `devc toolbox install restic` downloads a pinned restic release for the host architecture, verifies its SHA-256, and installs it under `restic/<version>/` with a `current` link. Downloads are cached in `${DEVC_DOWNLOAD_CACHE:-~/.cache/devc/downloads}` by checksum.
- Repository: `local/savepoints/<realm>/<project>/restic`, created on first use, without a password (`--insecure-no-password`; the repository is as protected as the rest of the state directory).
- Runs in the helper container as `1000:1000` with no capabilities and no network: the data root mounted read-only, the repository and a cache directory mounted read-write, the toolbox read-only, a tmpfs for temporary files. Tag each snapshot with the savepoint ID.
- `borg` is accepted by the configuration but not implemented: fail with a clear message and add it to `LEFTOVER.md`.

**Restore.** `devc restore <id>` copies the savepoint (or the given paths) into a new directory `restored-<id>/` in the first work zone and never overwrites. With `--in-place`, each path is restored to its original location, and anything in the way is first moved to the trash. For restic, restore into a temporary directory under `local/` and move the result into the project with a worker, owned by the box user.

**Automation.**

- `data.savepoints.before_session = true`: `devc <harness>` takes a savepoint before starting the session unless one was taken in the last 30 minutes.
- `data.savepoints.keep = N`: after each save, drop the oldest unnamed savepoints beyond N. Named ones are kept until removed explicitly (`devc data gc --drop <id>`).

## 6. Tests

Unit: path normalisation and zone membership; the seal and trash record formats; driver selection; retention.

Integration. The projects filesystem on this host is ext4, so `reflink` tests must detect support and skip here; say so in `LEFTOVER.md`.

1. Setup: owners and modes match section 1 as seen from the host (the vault shows as a subordinate ID) and from the box. From the box, renaming or removing `.devc`, the data root or a zone directory fails; `.devc` cannot be listed.
2. Input zone: writing fails in the box; a file added from the host is visible in the box.
3. Seal: afterwards, from the box, `rm -rf`, `mv`, `chmod`, truncation, appending and creating a file inside all fail; reading works; a new sibling directory can be created; the host user can read the files; `seals.jsonl` has the record.
4. Seal refusals: a path outside the sealed zones, a nested path, a symlink, `..` traversal, a path that does not exist.
5. Hostile tree: the directory being sealed contains a symlink to a host file, a symlink to a directory outside, a FIFO and a file with no read permission. Sealing completes; nothing outside the tree changes owner or mode (check a marker file in a sibling directory and the link targets).
6. `unseal` makes the entry writable again. `data rm` moves it to the trash; `trash restore` brings it back; `trash empty` deletes it.
7. restic: `save`; delete and overwrite files in the work zone from the box; `restore` brings the old content back into `restored-<id>/`; `restore --in-place` puts it back and moves the conflicting files to the trash. The repository path is not reachable from the box.
8. reflink (skipped here): the same, plus the savepoint directory is not readable from the box.
9. Retention with `keep = 2` and one named savepoint.
10. `before_session`: starting two sessions in a row takes one savepoint.
11. `data ls --json` reports zones, the sealed entry, the savepoints and the trash.
12. Worktree `data` links exist, resolve in the box, and do not show in `git status`.
13. A project without `[data]` is unaffected: ordinary ownership, no `.devc/`.

## 7. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `STATUS.md` and `LEFTOVER.md` are updated, including the limits of sealing that are known and accepted: a process that already holds a writable file descriptor keeps it; sealing protects against the box, not against the host user.
