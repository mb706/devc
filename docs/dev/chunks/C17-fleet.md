# C17 — Fleet: archive, export, secrets across machines

Read `docs/dev/README.md` and the `STATUS.md` notes of C08, C11a and C11b. Product spec: `specs-v2.md` sections 1 (disposable fleet, offline use), 5 and 13.

Machines are disposable. After this chunk a realm's durable state is mirrored to the hub as it grows, a new or wiped machine can fetch it back, the state can be exported for any backup tool, and stored secrets can be carried between machines without the hub being able to read them.

## 1. What "durable state" is

Per realm, under `durable/realms/<realm>/`: session journals, request and gate logs, the savepoint index, the context repository, the relay repositories, and shared harness state (transcripts and shared homes). Shared harness state is box-writable; everything else is host-written. Secrets are handled separately (section 4) and never travel in the archive. Box homes, caches, savepoint repositories and the toolbox are not durable state.

## 2. Archive on the hub

A realm takes part when `[sync] hub = true` (the default). The daemon pushes to every connected hub; the hub only ever adds.

- **Append-only files** (journals, logs, transcript files): the daemon tracks, per hub and file, how many bytes were sent, and uploads the rest in chunks: `PUT /machine/archive/<realm>/<path>?offset=<n>` with the machine certificate. The hub appends only if the offset equals the stored length; otherwise it answers with the stored length and the daemon resumes from there. A file that shrank or changed before the sent offset is uploaded again under a new generation name; the old generation is kept.
- **Git repositories** (relays, the context repository): `git bundle` of new objects since the last upload. Relay bundles are made by host git on the relay. The context repository's bundle is made in the throwaway container that already handles it (C04). The hub stores bundles in sequence and never unpacks them.
- **Reading box-writable files.** Transcripts and shared homes are read only through `safefs`: regular files, no symlinks, size-capped per file (configurable, default 256 MiB), names validated. What does not qualify is skipped and counted.
- Hub storage: `DEVC_HUB_DIR/archive/<machine>/<realm>/…`. A machine can add to its own tree only; nothing is ever deleted or overwritten by a machine. `devc hub archive ls|du` on the hub; `devc hub archive prune --machine <m> --before <date>` is the only way to delete, and it runs on the hub.
- Uploads run in the background at low priority, are rate-limited, resume after interruptions, and never hold up event delivery. `devc sync status` on a machine shows per hub what is behind.

## 3. Restore and export

- `devc state pull <realm> --from <hub-url> [--machine <name>]`: on a machine joined to the hub, download the archive of one machine (default: this machine's name; `--list` shows what exists) into the local durable state: journals and logs as files, bundles fetched into fresh relays and a fresh context repository, transcripts into the realm's harness state. Refuses to overwrite existing state unless `--merge`, which only adds files and refs that are missing.
- Add the hub endpoints this needs, readable with a machine certificate; a machine may read any machine's archive of realms it is configured for. Document that this makes the hub a place where every enrolled machine can read all archived realm state.
- `devc state export <realm> [--out file.tar]`: one tar file of the realm's durable state, for any backup tool. Host-written parts are read directly; box-writable parts go through a helper-container worker so the host does not walk them. `devc state import <file>` is the inverse on an empty realm.
- `docs/guide.md`: a section "Backups" explaining the three options: point a backup tool at the durable directory (consistent under filesystem snapshots; a plain copy is safe because files are append-only or git), `state export`, or rely on the hub archive for machines treated as disposable.

## 4. Secrets across machines

- `devc secret push <realm> --to <hub-url>` encrypts the realm's stored secrets on the machine with a key derived from a passphrase (argon2id, then XChaCha20-Poly1305; both from `golang.org/x/crypto`, a new dependency to note) and uploads the blob. `devc secret pull <realm> --from <hub-url>` downloads and decrypts. The hub stores only ciphertext and keeps the last ten versions.
- Harness logins that rotate their tokens (shared-home harnesses such as Codex) must not be copied between machines: exclude shared harness homes' credential files from the archive by manifest (a new manifest key listing secret files) and say why in the documentation. One login per machine for those.

## 5. Second hub, end to end

C11a made the daemon connect to several hubs. Verify and finish: archive uploads go to each hub independently; `state pull` works from either; the notifier role moves when the first hub is unreachable; `devc hub status` shows all of it. Document in `docs/hub-deployment.md` how to run a second hub: same CA files, its own database, optionally its own ntfy.

## 6. Tests

Unit: offset protocol (resume, mismatch, shrink); bundle sequencing; encryption round trip and wrong passphrase; exclusion of credential files.

Integration (loopback, two hub processes, one or two machine daemons with separate state directories):

1. Journal lines and a transcript file written during a session appear in both hubs' archives, byte for byte; an interrupted upload resumes without duplication.
2. A commit banked on machine A is recoverable on machine B: `state pull` there yields a relay containing it, the journals, the context repository history and the transcript.
3. A hostile shared harness state (a symlink to a host file, a FIFO, an oversized file) is skipped; no host file content reaches the hub.
4. A machine cannot overwrite or delete archived data (attempts with a crafted offset or path are refused).
5. `state export` then `state import` on a fresh state directory reproduces the realm.
6. `secret push` and `pull` between two machines; the hub's stored blob does not contain the secret values.
7. With the first hub stopped, events, archive and notifier role continue through the second.

## 7. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `STATUS.md` and `LEFTOVER.md` are updated.
