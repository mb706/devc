# Response to review A

Chunk C03r. One paragraph per finding, in the review's order. Code references are to the working tree after C03r; tests named here are unit tests unless marked as integration tests.

**A1: fixed.** Every root exec now goes through `rootExec` in `internal/box/exec.go`. It sets the fixed `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`, `HOME=/root` and the working directory `/`. Both `devc root` and the `box fix-identity` exec after every start use it. `devc root` therefore no longer follows the host's current directory into the project. Tests: `TestRunAndRoot` and `TestUpCreatesAndIsIdempotent`. The integration test `TestBoxLifecycle` plants `~/.local/bin/id` and checks that `devc root -- id` does not run it; it also checks root's pwd, HOME and PATH.

**A2: fixed as decided.**
- Validation (`internal/config/validate.go`) rejects `home.shared` and `home.per_box` entries that are `.`, repeated, or nested. `harness.Contribute` refuses a project whose harnesses have overlapping home directories.
- `safefs.Root.MkdirAll` and `RequireDirs` (new) resolve each component beneath its parent's descriptor with `openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)` and create with `mkdirat`. `paths.Ensure`, the new `paths.EnsureBeneath` (also used for project directories below the projects root) and `prepareMountPoints` all use them. Mount points are created or checked immediately before create or start.
- The race inside realm-shared harness homes is recorded in `LEFTOVER.md` as an accepted residual.

Tests: `TestMkdirAll`, `TestEnsure`, `TestNestedMountPointSymlinkRefused`, `TestContributeRejectsNestedHomes`, plus new config cases.

**A3: fixed.** `Manager.up` (`internal/box/lifecycle.go`) first plans the box and inspects the existing container. Only then does it decide: no-op, finish a start, refuse for drift, or recreate. `publish` writes `/etc/devc`, the preparers' baselines, launch descriptions and secrets, then the mount sources and mount points. It runs only on the paths that create or start the box. One consequence: `up` on a running box no longer re-delivers secrets or baselines; the box must be restarted (see STATUS). Tests: `TestDriftWritesNothing` and `TestPreparersAndCleaners`.

**A4: fixed.** An existing `<config>/harness/<id>.toml` replaces the built-in manifest of that id whether or not it loads (`loadHarnesses`). This also holds when it is a directory, unreadable, malformed, or has a wrong id. The file is recorded in `Config.overrides`. If the manifest directory cannot be listed, `harnessDirFailed` is set. `ProblemsFor` and the new `HarnessProblems` count the failed override and the directory listing error. A project that lists the id gets its own "failed to load" problem. `devc harness install|update|login` use `HarnessProblems`. Tests: `TestBrokenOverride`, the config case "broken override blocks its users", and the lifecycle test `TestBrokenOverrideBlocksUp`.

**A5: fixed.** `netpolicy.BoxArgs` refuses `internet` and `allowlist` with a message that says to set `none` or `open`. The realm default stays `internet`, so an unconfigured realm is refused. The example realm `demo` now sets `default = "open"`, with a comment saying C07 switches it back. Tests: `TestBoxArgs` and `TestSpecRejects`. The integration test `TestMountsAndRealms` checks that `up` fails and creates no box.

**A6: fixed.** `engine.RunOptions.Args` always adds `--http-proxy=false`. That covers the installer, the tmux build, logins and the new helper container. `BuildOptions.Args` adds it to env and installer image builds. Tests: `TestRunArgs` and `TestBuildArgs`. Hosts that need a proxy for installers are listed in `LEFTOVER.md`.

**D1: fixed.**
- Each integration run draws a nonce. Realms become `itdemo<nonce>` and `itother<nonce>`, envs become `itbase<nonce>` and `ituid<nonce>`, so image tags differ per run too.
- devc honours `DEVC_TEST_RUN=<nonce>`: `engine.Podman.ExtraLabels` puts `devc.test=1` and `devc.test.run=<nonce>` on every container it creates or runs and every image it builds. The labels are added outside the box definition, so the configuration hash is unaffected.
- Cleanup removes only containers and images that carry this run's label. The shared installer-image cache is the one exception and is kept. Pulled base images are still removed only if they were absent at the start.

Test: `TestExtraLabels`. `TestBoxLifecycle` checks the labels on a box.

**C1: partly fixed; the gap is recorded.** Sources: Codex documentation (developers.openai.com, now learn.chatgpt.com; the pages "Managed configuration" and "Hooks", read 2026-11-15).
- `/etc/codex/requirements.toml` is enforced over user and project configuration. It can constrain approval policies, sandbox modes, web search, feature flags, MCP servers and filesystem deny-read rules, and it can define managed hooks. Managed hooks are trusted by policy and cannot be disabled; pinning `[features] hooks = true` keeps them on.
- `/etc/codex/managed_config.toml` holds "legacy managed defaults". Codex applies them over the user's `config.toml` at every start.
- Manifest changes: new keys `baseline.files` (rendered from a template alone) and `baseline.settings.also` (extra paths with the same rendered settings).
- The Codex manifest now renders `requirements.toml` with `allowed_approval_policies = ["never"]`, `allowed_sandbox_modes = ["danger-full-access"]`, `features.hooks = true` and the five hooks, which moved out of `config.toml`. It writes the remaining settings to both `config.toml` and `managed_config.toml`.

Remaining gap: `developer_instructions`, `check_for_update_on_startup` and `cli_auth_credentials_store` are only managed defaults. That layer is documented as legacy, and users can change it during a run. None of this could be checked against a real Codex here (`LEFTOVER.md`). Test: `TestCodexSettings`.

**A7: fixed.** `BuildSpec` fails when the controller for a configured memory or PID limit is not delegated. The message names `memory = "none"` and `pids = -1`, which now switch a limit off explicitly in `config.toml` and `project.toml`. A global `pids = 0`, which used to silently mean "no limit", is now a configuration problem. Tests: `TestSpecLimits`, the golden file `selinux-disable-open`, and new config cases.

**A8: fixed as decided.** `devc rm --home` empties the home from a helper container: `--rootfs <toolbox>/helper-rootfs:O`, network none, the box seccomp profile, no-new-privileges, all capabilities dropped except `DAC_OVERRIDE` and `FOWNER`, and only the home mounted at `/target`. Inside, `devc box rm-worker` runs `os.RemoveAll` on each entry, which follows no symlinks, and refuses to run anywhere but the helper (`boxside.RequireHelper`). The host then removes the empty directory with a single `rmdir`. `restorecon -R` stays; README rule 2 already states that exception. Tests: `TestRemoveWithHome`, `TestClearDir` and `TestRequireHelper`. The integration test `TestBoxLifecycle` deletes a home that holds a closed directory owned by in-box root and a symlink to the project's host path.

**A9: fixed.** `secrets.Deliver` checks with `statfs` that the secrets directory is on tmpfs or ramfs before it writes any secret. Otherwise it fails, unless `DEVC_ALLOW_PERSISTENT_SECRETS=1` is set (`BoxHooks.AllowPersistentSecrets`). The integration harness now creates its runtime directories below `$XDG_RUNTIME_DIR`. Tests: `TestDeliver` and `TestPrepareAndCleanup`.

**B1: fixed.** A failed creation or start is rolled back with a fresh context: a container this invocation created is removed, one it started is stopped, and the cleaners remove the delivered secrets. A running container without the readiness marker is no longer reported as "already running". `up` runs the identity fix and waits for readiness again. If that fails, it reports the error and leaves the container running, because this invocation did not start it. `EnsureRunning` (behind `sh`, `run` and `root`) checks readiness too. Tests: `TestUpRollsBack`, `TestRunningButNotReady` and `TestReadyTimeout`.

**B2: fixed.** `Toolbox.publish` holds a short publication lock while it writes `installed.json` first and switches `current` last. If the switch fails, it restores the previous metadata, or removes it after a first install. `ReadInstalled` takes the same lock shared, so readers never see the intermediate state. A history line is appended only after success. Test: `TestInstallPublication`, which injects a failure at the switch.

**B3: fixed.** Validation rejects `auth.method = "shared-home"` unless `home.strategy = "shared"`, both in manifests and in realm overrides. Config cases cover both.

**B4: fixed.** Boxes are created with `TERMINFO_DIRS=/opt/devc/terminfo:`. I checked in a bookworm container: with the variable set, Debian's ncurses still searches its compiled-in `/etc/terminfo`, `/lib/terminfo` and `/usr/share/terminfo`, so the image's own entries stay usable and no explicit list is needed. The integration test `TestTmux` no longer injects the variable; it checks the value and `tput -T linux colors`. That entry exists only in `/lib/terminfo`.

**B5: fixed.** Codex's `install.sh` pins a version with `--release VERSION` or `CODEX_RELEASE`. `{version}` now also works in `install.env` values; a variable that holds it is left out when no version is requested. The Codex manifest sets `CODEX_RELEASE = "{version}"`. `--version` is refused for any manifest that has no `{version}` placeholder. Test: `TestInstallVersion`.

**B6: fixed.** Boxes are created with `--entrypoint /opt/devc/bin/devc` and the command `box init`. The integration test `TestIdentityAndNetwork` builds an env whose image declares `ENTRYPOINT ["/bin/sh","-c","echo not devc; exit 3"]`. The box starts and PID 1 runs devc. Golden files were regenerated. Every existing box now shows as drifted once.

**C2: fixed as decided.** `gpu = true` adds `--device nvidia.com/gpu=all`. `kvm = true` adds `--device /dev/kvm --group-add keep-groups`. `nested = true` is refused as not implemented. Tests: `TestSpecDevices` and `TestSpecRejects`. Neither device could be tested here (`LEFTOVER.md`).

**E1: fixed, one addition.**
- Launch descriptions name the concrete version directory (`Toolbox.CurrentExe`).
- `devc update` rewrites the descriptions of every box that has one, under the box lock (`BoxHooks.RefreshLaunches`). Projects with configuration problems are skipped with a warning.
- Each switch of `current` is recorded in `harness/<id>/history.jsonl`.
- `harness gc` keeps current, the two newest versions, every version a description names (`harness.NamedVersions`), and every version that was current at any time since the oldest running box started (from `podman ps` and `podman inspect`). If it cannot list the boxes, it removes nothing.
- Addition: `harness install` also rewrites the descriptions. Otherwise a new install would no longer reach running boxes, as it did through `current` before.

Tests: `TestGCKeepsVersionsInUse`, `TestRefreshLaunches` and `TestDescribe`. The integration test `TestHarnessInstall` was updated.

**E2: fixed.** New package `internal/flock` provides flock with a bounded wait that respects the context. A per-box lock file `local/boxes/<realm>/<project>/lock` is held by `up`, `down`, `rm`, `rebuild`, the start inside `sh`/`run`/`root` (released before the exec) and `RefreshLaunches`. The wait is at most ten minutes, and devc says once that it is waiting. Staging names are now unique: `WriteFileAtomic`, the dotfiles staging, `ReplaceDir`, and the harness seed staging. Tests: `TestAcquire` and `TestBoxLock`.

**A10: fixed.** `execx.Cmd.WorkDir()` defaults to `/`, and `Real.Run` and `Real.Interactive` use it. The fake records the effective directory, so tests can assert it. Doctor also sets `/` explicitly. Tests: `TestWorkDir`, and `TestAllOK` (doctor) asserts `/` for every command.

**Extra item 1 (socketcall): not done as ordered.** I removed `socketcall` from the allow lists without adding a rule, as ordered. Every container then failed to start under runc with "error adding seccomp rule for syscall socket: file exists", the same failure C03 had found. The alternatives do not work either:
- An explicit ENOSYS rule is dropped by runc because it equals the default action.
- `SCMP_ACT_TRACE` returns ENOSYS when no tracer is attached, but a tracer inside the box could let the call through, because the kernel allows a traced call on re-check.

The EPERM rule therefore stays, with the reasons in `internal/toolbox/seccomp.go`, and the self-test is unchanged. 32-bit glibc has no runtime fallback from socketcall either way.

**Extra item 2 (sandbox description): fixed.** `NewData` describes a level only if `netpolicy.Enforced` says it is in force. Only `none` and `open` can reach a box now. An `open` box is told "the network is not restricted.", and an unenforced level would get a non-restrictive sentence. Test: `TestNetworkSentence`. `TestInstructions` was updated.
