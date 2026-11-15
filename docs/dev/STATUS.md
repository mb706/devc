# Status

Progress log for devc v2. The implementing agent of each chunk appends a section; the orchestrator keeps the table current.

## Chunks

| Chunk | State | Commit |
|---|---|---|
| C01 Foundation | done | e7adb01 |
| C02 Boxes | in progress | |
| C03 Harnesses | pending | |
| Review A + C03r | pending | |
| C04 Sessions | pending | |
| C05 Git | pending | |
| C06 Data | pending | |
| C07 Network and finish | pending | |
| Review B + C07r | pending | |
| C08 Daemon core | pending | |
| C09 Status and input | pending | |
| C10 Relay service, gate, requests | pending | |
| C11 Hub | pending | |
| C12 Web app | pending | |
| Review C + C12r | pending | |
| C13 Gateway | pending | |
| C14 Data sync | pending | |
| C15 GitHub | pending | |
| C16 Skills and sources | pending | |
| C17 Fleet | pending | |
| C18 Service logins | pending | |
| C19 GUI | pending | |
| Review D + C19r | pending | |

## Resuming

To continue after an interruption: read `docs/dev/plan.md`, this file, and the work order of the first chunk that is not done. Work orders are written one at a time, just before the chunk starts; a missing work order means the chunk has not been specified yet.

## Chunk notes

(Each chunk adds a section below: what exists, deviations from the work order, how to exercise it.)

### C01 Foundation

**Toolchain.** `./dev` runs `docker.io/library/golang:1.27.4` (Go 1.27.4, pinned in `GO_IMAGE` at the top of the script) with `CGO_ENABLED=0`, `GOTOOLCHAIN=local`, `-buildvcs=false`; caches in `${DEVC_DEV_CACHE:-~/.cache/devc-dev}`. `go.mod` says `go 1.27.4`. Dependencies: `spf13/cobra` v1.10.2 (pulls `spf13/pflag`, `inconshreveable/mousetrap` indirectly), `BurntSushi/toml` v1.6.0, `golang.org/x/sys` v0.50.0. Nothing outside the approved list.

**What exists.**

- `cmd/devc`: dispatch on argv[0]; `devc` and `devc-*` run the CLI, `git-remote-devc` and any other name print "not implemented" and exit 2.
- `internal/cli`: cobra root with `--config-dir`, `--state-dir`, `-v` (count: warn, info, debug), hidden empty `box` group; `version`, `doctor [--json]`, `config check`, `config show [ref] [--json]`, `realm ls`, `project ls [--realm r]`, `env ls` (all listings take `--json`). Exit codes 0/1/2; flag errors, unknown subcommands and wrong argument counts exit 2.
- `internal/names`: name rule, `Check` with a precise reason, `ProjectRef`, `BoxName`, `BoxID`, session IDs.
- `internal/paths`: every layout path of spec 5 and plan 3.2, in-box constants and UIDs, `Ensure` (0700 below the state, runtime or config root only).
- `internal/safefs`: `OpenRoot`, `Open`, `ReadFile`, `ReadFrom`, `List` via openat2 with `RESOLVE_BENEATH|NO_SYMLINKS|NO_MAGICLINKS`; directory listing parses getdents itself so `DT_UNKNOWN` never triggers a path-based lstat.
- `internal/execx`: `Runner` (`Run`, `Interactive`), `Real`, `Fake`, typed `*ExitError`, `Redact`, output cap.
- `internal/config`: all schemas of the work order, strict decoding (unknown keys and type errors reported with file and key), loader-applied defaults, all validation rules, lookups, `ResolveProject`, `Resolved`, `View`, `ProblemsFor`. Built-in manifests `assets/harness/{claude,codex}.toml` embedded through package `assets`.
- `internal/engine`: `Engine` interface (`Binary`, `Version`, `Info`, `Ps`, `Inspect`, `Exists`), `Podman` over execx, `Fake`; parsing tested against JSON recorded on this host (`internal/engine/testdata`, user paths sanitised).
- `internal/doctor`: the 14 checks of work order section 9, table and JSON output; all branches unit-tested with a fake root filesystem, fake runner and fake engine.
- `examples/config`: realms `demo` (`hello`, `analysis` with all three zones, savepoints `auto`, a mount of `hello`, a data remote) and `other` (`tools`, plain image); env `base` (Debian trixie-slim Containerfile).
- `test/integration`: helper package `itest` (temporary config/state/runtime/projects dirs from `examples/config`, runs `DEVC_BIN`) and two tests (smoke; broken config rejected).

**How to exercise.** `./dev build && bin/devc doctor`, `bin/devc --config-dir examples/config config check`, `… project ls`, `… config show demo/analysis`; `./dev test`, `./dev itest -v`, `./dev vet`.

**`devc doctor` on this host:** ok engine (5.8.3 from `~/.local/opt/workstation-tools/bin`), rootless, subids, runtime-dir, tools, storage (fuse-overlayfs), selinux (disabled), cgroups, gpu (`/etc/cdi/nvidia.yaml`); warn linger (off), netbackend (slirp4netns), apparmor-userns (restriction on and the conda-installed podman is not covered by any profile in `/etc/apparmor.d`, although rootless podman evidently works), projects-fs (ext4, no reflink); skip kvm.

**Deviations and choices.**

1. `./dev itest` always rebuilds `bin/devc` and `bin/itest` (the work order says build if missing): a stale binary would silently test old code, and the build cache makes it cheap. It translates `go test` flags (`-v`, `-run X`) to test-binary flags and runs from `test/integration`. Added `./dev go …` as a passthrough; `./dev vet` includes `-tags integration`; `./dev test` passes extra flags to `go test` and adds `./...` when no package is named.
2. Session IDs use lower-case base32hex (`0-9a-v`), because only that alphabet sorts lexically in numeric order: 50-bit millisecond time plus 50 random bits, strictly increasing within a process.
3. `config show` without an argument prints the global view (effective directories, engine, box defaults, hubs); it does not infer a project from the working directory, so its output schema does not depend on where it runs. With an argument it accepts `realm/project` or a unique bare name.
4. Read-only commands (`ls`, `show`, `doctor`) run despite configuration problems and print a one-line warning. `Config.ProblemsFor(ref)` returns the problems affecting one project (global, realm, project, its env, its manifests) so lifecycle commands from C02 on can refuse to act. Files with syntax or type errors drop their entity; unknown keys are reported but keep it. A realm directory without `realm.toml` (or a project directory without `project.toml`) is a problem and is skipped. `config check` fails if the config directory does not exist.
5. State directory precedence: `--state-dir` > `DEVC_STATE_DIR` > `state_dir` in `config.toml` > default. `projects_root` and `state_dir` must be absolute or start with `~/`.
6. Validation beyond the work order, all for argument safety or obvious mistakes: `upstream` refuses `<transport>::` URLs (`ext::` runs commands) and unknown schemes; image references, SSH aliases, branch names and globs cannot start with `-`; data paths have no component starting with `-`; `data.root` is one name (name rules) other than `main`; harness `bin` values are unique and not `devc`, `devc-*` or `git-remote-devc` (argv[0] dispatch); a user manifest's file name must equal its `id`; a realm override to `env-token`/`api-key` needs `auth.env` in the manifest; `home.shared` only with `private`, `home.per_box` only with `shared`; `launch.resume` must contain `{id}`; duplicates in harnesses, grants, ports, mounts and zones are errors.
7. Types and defaults not fixed by the work order: `box.ports` is a list of integers; `[[mounts]] mode` defaults to `ro`; `savepoints.keep = 0` means keep all; project `box.pids = 0` inherits; manifest defaults are `install.method = "none"`, `auth.method = "none"`, `home.strategy = "private"`, `input.adapter = "paste"`, `submit_key = "Enter"`, `seed.when = "missing"`, `display = id`.
8. `paths` getters panic on a name that did not pass `names` (a programming error, never user input), rather than returning errors from every getter.
9. `execx.Interactive` has no default timeout (attached sessions last hours); `Run` defaults to 2 minutes, sends SIGTERM on timeout and SIGKILL 5 s later. The fake picks the longest matching prefix, the latest registration among equals.
10. Doctor details: `subids` also accepts the engine's actual ID mapping (covers subordinate IDs from LDAP/sssd); `cgroups` reads delegated controllers from `podman info` when rootless, else from `user@<uid>.service`; `linger` falls back to `/var/lib/systemd/linger/<user>`; "distribution path" for AppArmor means a profile in `/etc/apparmor.d` that attaches to the resolved engine binary and grants `userns`; `projects-fs` probes the nearest existing ancestor when the projects root does not exist yet (and says so); a missing `ssh` is a warning; `vfs` storage and CNI also warn; SELinux permissive counts as enabled.

**Notes for later chunks.**

- Box names `devc-<realm>-<project>` can collide (realm `a-b` + project `c` vs realm `a` + project `b-c`). C02 should verify the `devc.realm`/`devc.project` labels whenever it finds a container by name, and refuse to create a box whose name is taken by another project.
- `Config.ProblemsFor` exists for fail-closed lifecycle commands; nothing uses it yet.
- `engine.Engine` and `engine.Fake` must grow together when methods are added.

### C02 Boxes

**What exists.**

- **C01 amendments.** `[[root_log]]` is gone from `env.toml` (an old entry is now an "unknown key" problem); `devc root` appends `{"time","project","argv"}` lines to `<config>/envs/<env>/root-log.jsonl` (`paths.EnvRootLog`), the only file devc writes below the config directory. Global `[selinux] mode = "labels" | "disable"` (default `labels`, shown by `config show`). Realm names are `^[a-z0-9]{1,32}$` (`names.ValidRealm`, `names.Check(KindRealm)`, `paths` getters, config loading); the example realms already complied. Every container found by name is checked for `devc.realm`, `devc.project` and `devc.role=box` (`box.Manager.Find`); a foreign one is never touched and shows as `conflict` in `ls`. `up`, `rebuild`, `sh`, `run` and `root` refuse while `Config.ProblemsFor(ref)` is non-empty and print the problems.
- **`internal/engine`.** Image inspect/exists, build, pull (both stream progress), create, init, start, stop, remove, exec (captured and interactive; user, workdir, env, privileged, `-i`/`-t` with `--detach-keys=`), unshare; inspect now parses mounts, `HostConfig` (security options, caps, memory, pids, network, init), effective/bounding caps, user, env. Every engine command runs with working directory `/`. `engine.Fake` implements all of it in memory (create parses `--name`, `--label`, `--mount`, …). `execx.Cmd` gained `Stdout`/`Stderr` for `Interactive`.
- **`internal/toolbox`.** `Sync(exe, version)` (flock-protected, idempotent): `devc/<version>-<sha12>/devc`, `devc/current`, `bin/devc -> ../devc/current/devc`, `helper-rootfs/bin/devc` (hard link, else copy; anything else in the helper rootfs is removed), `seccomp.json`; everything 0755/0644 because in-box root without capabilities maps to a subordinate UID. `List()`. Commands `devc toolbox sync|ls [--json]`.
- **Seccomp.** `assets/seccomp/default.json` is containers/common `common/v0.67.1` (the version Podman 5.8.3 uses), source and licence in `assets/seccomp/SOURCE.txt`. `toolbox.DeriveProfile` removes `clone`, `clone3`, `unshare` from the unconditional allow rule, adds `clone`/`unshare` allowed only with `(flags & CLONE_NEWUSER) == 0` (s390 variant on argument 1), leaves `clone3` to the default action (ENOSYS), replaces each "allow socket if domain != x" rule by range rules, and adds `socket` with `(domain & 0xffffffff) == 38` → EAFNOSUPPORT. Strict decoding and self-checks make the derivation fail loudly if a future base changes shape. Unit tests evaluate the rules for both capability sets and three architectures and check that undoing the documented changes gives the base back.
- **Env images.** `localhost/devc-env/<env>:latest` built from the env directory with labels `devc.role=env`, `devc.env=<env>` and the build args; base-only envs and `image =` projects are pulled when missing; `build_from_repo` fails with "not supported yet". `devc env build <env> [--no-cache] [--pull]`; `devc env ls` shows whether the image exists and when it was built.
- **`internal/box`.** `BuildSpec` (golden-file tests in `internal/box/testdata`, regenerate with `./dev test ./internal/box/ -run Golden -args -update`), `Contributor`/`Spec.AddMount`/`AddEnv`/`AddLabel` for later chunks, host directories, `/etc/devc` (`box.json` + a copy of `<config>/dotfiles/`), image selection, `Manager` with `Up`, `Down`, `Remove`, `Rebuild`, `List`, `Shell`, `Run`, `Root`. After every start: `podman exec --user 0 … devc box fix-identity`, then a wait of up to 20 s for `<runtime>/<box-id>/ready` (removed before each start; the wait also notices a box that stopped).
- **`internal/netpolicy`.** `BoxArgs(level)`: `none` → `--network none`; `open` → engine default; `internet`/`allowlist` → engine default plus a "not enforced yet" warning.
- **`internal/boxside`.** `box init` (home skeleton from `/etc/skel` when the home holds only mount points, dotfile links, `~/.local/bin`, `/run/devc/spool`, `ready`, waits for SIGTERM/SIGINT, ignores SIGHUP), `box fix-identity`, `box shell`, `box selftest [--json]` (12 items, see below).
- **Commands.** `devc up [ref] [--recreate] [--build]`, `down`, `rm [--home]`, `rebuild`, `ls [--json]`, `sh`, `run [ref] -- cmd…`, `root [ref] -- cmd…`, `env build`, `toolbox sync|ls`, hidden `box init|fix-identity|shell|selftest`.
- **Integration tests** (`test/integration/box_test.go`): `TestBoxLifecycle` (items 1–6), `TestBoxDrift` (7), `TestIdentityAndNetwork` (8 and 10), `TestMountsAndRealms` (9). The whole suite takes about 80 s here; the base env image is built once per run and all test images are removed at the end.

**Box arguments**, in this order: `--name --hostname --init --tz local --userns=keep-id:uid=1000,gid=1000 --user 1000:1000 --passwd-entry dev:*:1000:1000:dev:/home/dev:/bin/sh --http-proxy=false --cap-drop=ALL --security-opt no-new-privileges --security-opt seccomp=<toolbox>/seccomp.json [label=level:… | label=disable] [--pids-limit] [--memory] [--network none] --workdir --env… --mount… --label… <image> /opt/devc/bin/devc box init`. `devc.confighash` is SHA-256 over the arguments (without the hash label), the image reference and command, the image ID and the SHA-256 of the seccomp profile.

**Deviations and choices.**

1. **Extra creation flags.** `podman create` has no `--passwd=false` (only `run` does), so boxes get `--passwd-entry` as in spec 6.2; it covers the moment before `fix-identity`, which then makes the files canonical (Podman adds nothing once UID 1000 exists). `--http-proxy=false`: the host's proxy variables are host environment, not policy, and may carry credentials. Engine-default network levels pass no `--network` at all.
2. **Seccomp beyond the work order.** The kernel truncates the socket domain to 32 bits while seccomp compares 64, so `socket(0x100000026, …)` creates an AF_ALG socket under the base profile (verified here). Every allow range therefore requires the upper 32 bits to be zero (aligned power-of-two blocks with `SCMP_CMP_MASKED_EQ`, about 70 rules), and the AF_ALG refusal matches the low 32 bits. The no-`CAP_AUDIT_WRITE` ranges also exclude 40, which removes the base profile's overlap of "allow domain != 16" with "refuse domain == 40". No rule overlaps another with a different action, so nothing depends on libseccomp's tie-breaking.
3. **Hash inputs** include the seccomp digest (the engine reads the profile only at creation), so a devc upgrade that changes the profile shows as drift.
4. **`up --recreate`** always recreates; `rebuild` builds the env (if it has a Containerfile) and recreates; image projects are not re-pulled. On an already running box, `sh`, `run` and `root` exec even if it has drifted; a stopped box is started through `up`, so drift stops them.
5. **Fail-closed scope.** `down`, `rm` and `ls` still work with configuration problems (they only stop or remove, or read); all commands that create, start or exec refuse.
6. **Nested mount points.** Before every create/start, each mount that lies inside another mount (today `/home/dev/.cache` in the home) must be a real directory at that place in the parent's host source; a symlink planted by the box (for example `.cache -> /opt/devc`) would otherwise redirect the mount inside the box and shadow the toolbox. Missing mount points are created; anything else is refused.
7. **`devc box shell`** (not in the work order) chooses the shell inside the box (`box.shell` from `box.json`, else zsh, bash, sh in `/bin`, `/usr/bin`, `/usr/local/bin`) and execs it as a login shell; `devc sh` runs it with `-i` and `-t` only when stdin is a terminal. All `devc box` commands refuse unless `DEVC_BOX=1` and `/etc/devc/box.json` exist, so `fix-identity` can never rewrite a host's `/etc/passwd`.
8. **`fix-identity`** replaces entries matching UID/GID 1000 or the name `dev` (and 2000/`vault`) with canonical lines at the position of the first match, keeps a replaced group's member list, keeps every other line byte for byte, writes by rename (falling back to in-place on `EBUSY`/`EXDEV`), and leaves `/etc/shadow` alone. `vault` has home `/nonexistent` and a nologin shell.
9. **`box init` and dotfiles.** Dotfile links replace existing symlinks and skip real files; links to removed dotfiles are deleted. The host copies only regular files and directories from `<config>/dotfiles/` (symlinks and special files are skipped with a log line, `.git` is skipped), so a symlink in the dotfiles repository cannot copy an arbitrary host file into a box.
10. **`box selftest`** items: identity, capabilities (CapEff), no-new-privileges, `unshare(CLONE_NEWUSER)` and `clone(CLONE_NEWUSER|CLONE_FS)` must fail with ENOSYS (the kernel alone would say EINVAL, so ENOSYS proves the seccomp rule), `clone3` ENOSYS, AF_ALG EAFNOSUPPORT with and without upper bits, ordinary sockets still work, `/opt/devc` and `/etc/devc` read-only, project directory writable. Under the unmodified base profile all six seccomp items fail.
11. **Integration-test naming** deviates from README ("containers named `devc-it-…`, labelled `devc.test=1`"): box names and labels are fixed by the plan, so the harness renames the copied example configuration instead: realms `itdemo`/`itother`, env `itbase` (boxes `devc-itdemo-hello` etc.). Containers labelled with these realms are removed before and after every test; `localhost/devc-env/it*` images are removed at the end of the run, pulled Debian images only if they were absent at the start.
12. **Host bug worked around in tests.** On this host, `statx()` asking for the birth time on a file in an image layer hangs forever in fuse-overlayfs 1.17 with libfuse 3.18.2 (the process sits in `request_wait_answer`). Debian trixie's `apt-get update` verifies signatures with `sqv`, which does exactly that, so the example `base` env cannot be built here; `stat FILE` hangs too, while `ls -l` and `stat -c %s` work. The test harness builds the base env from `debian:bookworm-slim` (apt uses gpgv); the example configuration stays on trixie. Not a devc problem; see LEFTOVER.
13. **AppArmor.** Nothing box-specific: rootless Podman applies no AppArmor profile (`podman info` reports it disabled), and devc never passes `apparmor=…`. The host-side user-namespace restriction remains a `doctor` check (unchanged; it warns here, as expected).
14. **SELinux.** Mode `labels`: `label=level:<realm level>` when the realm sets one, never `:z`/`:Z`, and `restorecon -R -- <project dir>` (from `/`) during `up` when the tool exists, otherwise a warning. Mode `disable`: `label=disable` plus a one-line warning. Both only when `/sys/fs/selinux/enforce` exists; tested with fakes only.

**How to exercise.** `./dev build`, then with a config directory: `bin/devc up demo/hello`, `bin/devc sh demo/hello`, `bin/devc run demo/hello -- /opt/devc/bin/devc box selftest`, `bin/devc root demo/hello -- apt-get install -y …`, `bin/devc ls`, `bin/devc down|rm [--home] demo/hello`, `bin/devc toolbox ls`, `bin/devc env ls`. Tests: `./dev test`, `./dev itest -v`.

**Notes for later chunks.**

- C03: add mounts and environment through a `box.Contributor` appended to `Manager.Contributors` (the CLI builds the manager in `internal/cli/boxcmds.go`); nested mounts below the home get the mount-point check automatically. `/etc/devc/harness/…` can be written next to `box.json` in `writeEtc` (`internal/box/dirs.go`). `boxside.Info` is the box's description of itself (schema 1).
- C07: the place for firewall rules is marked in `createAndStart` (between create and start; use `Engine.Init`); `netpolicy.BoxArgs` is the single decision point and its warning text must go.
- C06: data-zone mounts nested inside a project directory are checked like `.cache`, but that directory may be mounted read-write into another running box (`[[mounts]] mode = "rw"`), which could race the check; consider refusing nested zones under rw-shared projects or checking inside the helper container.
- `Manager.Find` is the only way to get a box container; use it.
