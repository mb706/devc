# C02 — Boxes

Read `docs/dev/README.md`, `docs/dev/plan.md` (sections 3.1–3.4, 3.9, 3.13) and the `STATUS.md` notes of C01 first. Product spec: `specs-v2.md` sections 5, 6 and 16.

After this chunk a project's box can be built, created, entered and recreated, with the hardening of spec section 6 in place. Harnesses, sessions, git and data come later; leave clean extension points for them.

## 1. Amendments to C01

- **Root log.** Remove `[[root_log]]` from the env schema. `devc root` appends JSON lines to `<config>/envs/<env>/root-log.jsonl` instead (time, project, argv). This keeps user-written TOML untouched.
- **SELinux mode.** Add to the global config:

  ```toml
  [selinux]
  mode = "labels"        # labels | disable
  ```

  `labels` (default) expects the file-context rule from `devc doctor` and mounts without relabelling. `disable` adds `--security-opt label=disable` to boxes; it is the escape hatch for machines where the rule has not been set up, and `devc up` prints a one-line warning when it is used.

- **Realm names.** Box names `devc-<realm>-<project>` are ambiguous when a realm name contains `-`. Restrict realm names to `^[a-z0-9]{1,32}$` (letters and digits only), in `names` and in config validation, and update the example configuration if needed. In addition, whenever a container is found by name, verify its `devc.realm` and `devc.project` labels and refuse to act on a container that devc did not create for that project.
- **Fail closed on configuration problems.** Commands that create, start or change a box must refuse when `Config.ProblemsFor(ref)` reports anything, and print those problems.

## 2. Engine wrapper

Extend `internal/engine` with what this chunk needs: image exists / inspect (ID, environment, created), build, pull, create, init, start, stop, remove, exec (captured and interactive, with user, working directory, environment, privileged flag), unshare (run a command in the user namespace), and container inspect fields for state, PID, labels, mounts and security options. Keep the fake in step.

## 3. Toolbox base

`internal/toolbox`. The toolbox root is `<state>/toolbox/`, mounted read-only at `/opt/devc` in every box. Everything inside uses relative symlinks so it resolves identically on the host and in a box.

- `Sync()` is idempotent and is called by `devc up` and by `devc toolbox sync`:
  - copy the running executable to `devc/<id>/devc`, where `<id>` is the build version plus the first 12 hex characters of the file's SHA-256; point `devc/current` at it; `bin/devc` is a relative symlink to `../devc/current/devc`;
  - `helper-rootfs/bin/devc`: a hard link to the same file, or a copy if linking fails. This directory is the root filesystem of the helper container (plan 3.3) and contains nothing else;
  - `seccomp.json` (section 4).
- Old `devc/<id>` directories are kept; removing unused ones is deferred (note it in `LEFTOVER.md`).
- `devc toolbox ls` prints what is installed.

## 4. Seccomp profile

- Embed the default seccomp profile of containers/common at a pinned release under `assets/seccomp/default.json`, with a sibling text file naming the source URL, version and licence.
- Derive the devc profile in code from that base and write it to the toolbox:
  1. **No new user namespaces.** `clone` and `unshare` are allowed only when the `CLONE_NEWUSER` bit (0x10000000) is clear in the flags argument (masked-equality rule on argument 0); `clone3` returns `ENOSYS` so libc falls back to `clone`. Leave the capability-gated variants of the base profile as they are.
  2. **No `AF_ALG` sockets.** `socket` with domain 38 is refused with `EAFNOSUPPORT`. libseccomp cannot AND two comparisons on one argument in a single rule, and separate allow rules are ORed, so express the allowed domains as ranges that exclude 38 and whatever the base profile already excludes.
- Unit-test the derivation on the embedded base (the rules above are present; nothing else changed). Its real effect is tested in section 9.

## 5. Env images

- An env with a `Containerfile` is built with the env directory as context: tag `localhost/devc-env/<env>:latest`, labels `devc.role=env` and `devc.env=<env>`, build arguments from `env.toml`.
- An env with only `base` uses that image as is; a project with `image = …` uses the literal reference. Both are pulled if absent.
- `build_from_repo` is not implemented in this chunk: fail with "not supported yet" and note it in `LEFTOVER.md`.
- `devc env build <env> [--no-cache] [--pull]`; `devc env ls` now also shows whether the image exists and when it was built.

## 6. Box specification

`internal/box` turns `config.Resolved` plus host facts into a deterministic argument list for `podman create`. Golden-file unit tests cover the variants.

Flags (spec 6.2), in a fixed order:

```
--name devc-<realm>-<project>  --hostname <project>  --init  --tz local
--userns=keep-id:uid=1000,gid=1000  --user 1000:1000
--cap-drop=ALL  --security-opt no-new-privileges
--security-opt seccomp=<toolbox>/seccomp.json
[--security-opt label=level:<level>]      realm sets a level, SELinux enabled, mode "labels"
[--security-opt label=disable]            SELinux enabled, mode "disable"
[--pids-limit N] [--memory M]             only when the controller is delegated; otherwise warn once
--network …                               section 7
--workdir /w/<realm>/<project>
--env …  --mount …  --label …
<image>  /opt/devc/bin/devc box init
```

Environment set at creation: `HOME=/home/dev`, `USER=dev`, `LOGNAME=dev`, `DEVC_BOX=1`, `DEVC_REALM`, `DEVC_PROJECT`, and `PATH=/opt/devc/shims:/opt/devc/bin:/home/dev/.local/bin:<PATH from the image>`.

Mounts in this chunk (all `type=bind`, no relabel options):

| Target | Source | Mode |
|---|---|---|
| `/w/<realm>/<project>` | host project directory | rw |
| `/w/<realm>/<other>` | each `[[mounts]]` project of the same realm | as configured |
| `/home/dev` | `local/boxes/<realm>/<project>/home` | rw |
| `/home/dev/.cache` | `cache/realms/<realm>/pkg` | rw |
| `/ctx/notes` | `durable/realms/<realm>/context/notes/<project>` | rw |
| `/ctx/memory` | `durable/realms/<realm>/context/memory/<memory_group>` | rw |
| `/opt/devc` | toolbox | ro |
| `/etc/devc` | `local/boxes/<realm>/<project>/etc` | ro |
| `/run/devc` | box runtime directory | rw |

Provide a function later chunks call to contribute mounts and environment (harness state in C03, data zones in C06) so the argument list stays in one place.

Labels: `devc.realm`, `devc.project`, `devc.role=box`, `devc.confighash`. The hash covers the full argument list (without the hash label) and the image ID.

Host directories are created on demand with mode 0700 under the state directory and 0755 for the project directory. Never create or modify anything under the config directory, except the root log.

`/etc/devc` content, regenerated on every `up`: `box.json` (realm, project, paths, devc version) and `dotfiles/` (a copy of `<config>/dotfiles/`, so the config directory itself is never mounted).

## 7. Network placeholder

Level `none` gives `--network none`. Levels `open`, `internet` and `allowlist` use the engine default network for now. For `internet` and `allowlist`, print a clearly worded warning on `up` that the level is **not enforced yet** (C07 replaces this). Put the decision in a small `netpolicy` function so C07 has one place to change.

## 8. Lifecycle

`ref` is optional everywhere and resolved from the current directory (C01).

| Command | Behaviour |
|---|---|
| `devc up [ref] [--recreate] [--build]` | Sync the toolbox; make sure the image exists (build the env when `--build` is given or the image is missing); create directories and `/etc/devc`; on SELinux hosts in mode `labels`, run `restorecon -R` on the project directory if the tool exists. Then: container absent → create and start; stopped → start; running → nothing. If the container exists and its hash differs from the current specification, say what to do and exit 1 unless `--recreate` is given. After start, run the identity fix (below) and wait up to 20 s for `/run/devc/ready`. |
| `devc down [ref]` | Stop the container. |
| `devc rm [ref] [--home]` | Stop and remove the container. With `--home`, also delete the box home (through `podman unshare`, because it may contain files owned by subordinate IDs). |
| `devc rebuild [ref]` | Build the env image if the project uses one, then recreate. |
| `devc ls [--json]` | One row per configured project: ref, state (`running`, `stopped`, `absent`), image, drift (`yes`/`no`), started-at. |
| `devc sh [ref]` | `up` if needed, then an interactive login shell: `podman exec -it --detach-keys= -e TERM -e COLORTERM`. Shell: `box.shell` from config, else the first of zsh, bash, sh found in the box. Working directory: the in-box equivalent of the host's current directory when that is inside the project, else the project root. |
| `devc run [ref] -- cmd…` | Run a command as `dev`; allocate a TTY only when stdin is one; propagate the exit code. |
| `devc root [ref] -- cmd…` | Same as root with full capabilities: `podman exec --user 0 --privileged`. Append the command to the env's root log when the project uses an env. |

**Identity fix.** Images differ: some have no UID 1000, some have it under another name and home. Right after start, `devc up` runs `podman exec --user 0 <box> /opt/devc/bin/devc box fix-identity`, which rewrites `/etc/passwd` and `/etc/group` so that UID/GID 1000 is `dev` with home `/home/dev` and the chosen shell, and adds `vault` for 2000. Root without capabilities can do this because it owns those files. It must be idempotent and must not disturb other entries. Whether to also pass `--passwd-entry` or `--passwd=false` at creation is your call; test with an image that has no UID 1000 and one that has a different user there.

## 9. In-box commands (`internal/boxside`)

| Command | Behaviour |
|---|---|
| `devc box init` | The container's main process (under the engine's init). As `dev`: create a minimal home skeleton if the home is empty; link each entry of `/etc/devc/dotfiles/` into the home as a symlink unless a non-symlink already exists there; create `/run/devc/spool`; write `/run/devc/ready`; then wait for `SIGTERM` or `SIGINT` and exit 0. Needs no shell. |
| `devc box fix-identity` | See above. |
| `devc box selftest [--json]` | Reports, by trying: effective UID and GID; effective capability set empty; no-new-privileges set; `unshare(CLONE_NEWUSER)` refused by the syscall itself; `socket(AF_ALG, …)` refused; `/opt/devc` and `/etc/devc` not writable; `/w/<realm>/<project>` writable. Used by tests here and by the escape suite later. |

## 10. Integration tests

Against real Podman, using the example configuration. Build the `base` env image once per test run and reuse it.

1. `up` creates a running box; `ls --json` shows it; a second `up` is a no-op.
2. Container inspection shows: all capabilities dropped, no-new-privileges, the seccomp profile, user 1000, the mounts of section 6 with the right modes, the labels.
3. In the box: `id` gives `dev` (1000); `HOME` is `/home/dev`; `getent passwd dev` and `getent passwd 1000` agree; a file created under the project directory is owned by the invoking user on the host.
4. `box selftest` passes every item. The user-namespace item must fail at the syscall, which shows the seccomp rule is active independently of the host's AppArmor restriction.
5. `run -- sh -c 'touch /usr/local/x'` fails; `root -- sh -c 'touch /usr/local/x'` succeeds, and the root log gained a line.
6. `down` then `up`, and `rm` then `up`: a file in the home survives both. `rm --home` removes it.
7. Changing the project's memory limit makes `up` report drift and exit 1; `up --recreate` applies it.
8. Identity fix on two images: one without UID 1000, one where UID 1000 has another name.
9. A project with `[[mounts]]` sees the other project read-only; a mount naming another realm is rejected by `config check` (C01) — assert it stays rejected.
10. Network level `none`: no network inside.

## 11. Done when

- All of the above passes through `./dev test` and `./dev itest`.
- On this host, `devc up` followed by `devc sh` on an example project gives a working shell as `dev`.
- `STATUS.md` and `LEFTOVER.md` are updated, including everything SELinux-related that could not be exercised here.
