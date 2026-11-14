# Status

Progress log for devc v2. The implementing agent of each chunk appends a section; the orchestrator keeps the table current.

## Chunks

| Chunk | State | Commit |
|---|---|---|
| C01 Foundation | in progress | |
| C02 Boxes | pending | |
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
