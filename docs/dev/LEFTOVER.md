# Leftover checks and deferred work

Things that could not be verified during autonomous development, and work deliberately postponed. Each entry says what is needed to close it.

## Needs a real account or login

| Item | Needs |
|---|---|
| `claude setup-token`: the token works from several boxes and machines; how to revoke it; whether usage display works | one browser approval |
| Claude Code `autoMemoryDirectory`: whether the path is used exactly or gets a per-project subfolder | a Claude session |
| Claude Code `WorktreeCreate` hook redirecting worktrees to sibling directories | a Claude session in a box |
| Claude Code plugin interface for posting a user message into a live session | a Claude session in a box |
| Codex shared home with per-box daemon directories; app-server with an attached TUI | a Codex login |
| Codex hook trust for hooks supplied through `/etc/codex/config.toml` | a Codex login |

## Needs another machine

| Item | Needs |
|---|---|
| SELinux: file-context rule, per-realm level, box-to-host channel direction, `restorecon` on `devc up` | a Fedora machine, enforcing |
| Seccomp profile and nested user-namespace block on a host without the AppArmor restriction | a Fedora machine |
| Host-applied firewall rules with pasta on Podman 6 | a Podman 6 host |
| `reflink` savepoint driver | project storage on XFS or btrfs |
| GPU and KVM capabilities | hardware |

## Decisions waiting for the user

| Item | Context |
|---|---|
| Remote approval of high-risk requests | `docs/dev/plan.md` section 5: not built; a WebAuthn-bound design is recorded there |

## Deferred work

(Chunks append here.)

### From C01

| Item | Needs |
|---|---|
| `doctor` SELinux branch (`matchpathcon`, `semanage` hint) is tested only with fakes | a Fedora machine, enforcing |
| `doctor` reflink-supported branch and `ReflinkProbe` success | XFS or btrfs storage |
| `doctor` KVM accessible branch | hardware with KVM |
| `doctor apparmor-userns` warns on this host because the conda-installed podman is not covered by a profile, yet rootless podman works; whether the hint profile is ever needed here is unverified | root access to load a profile, or a host where podman fails without one |
| `doctor linger` with lingering enabled, on a real host (only the fake branch is tested) | `loginctl enable-linger` (root) |
| `safefs` behaviour when openat2 is missing (`ErrUnsupported`) | a kernel older than 5.6 |
| `safefs.List` with `DT_UNKNOWN` entries (fstatat fallback) | a filesystem without d_type |
| `bin/devc-linux-arm64` is built but never run | an arm64 machine |
| Codex manifest values (`exe` path, `/etc/codex/config.toml` as the baseline location, per-box directory names) are taken from the work order, not checked against an installed Codex | `exe` (now `home/.local/bin/codex`) verified by installing codex-cli 0.169.0 in C03; `/etc/codex/config.toml` is the documented system layer; per-box directory names still unverified (need a Codex login) |

### From C02

| Item | Needs |
|---|---|
| SELinux box options (`label=level:<realm level>`, `label=disable`), `restorecon -R` on the project directory during `up`, and the warning when `restorecon` is missing are tested only with fakes | a Fedora machine, enforcing |
| Whether boxes start on an SELinux host in mode `labels` with the file-context rule in place (no relabelling, `/etc/devc`, toolbox and runtime mounts readable as `container_file_t`) | a Fedora machine, enforcing |
| Seccomp: the s390 variant of the `clone` rule and the profile on arm64 are unit-tested only; the high-bit AF_ALG block was verified on amd64 only | s390x / arm64 hosts |
| Seccomp gap: on amd64 a 32-bit (i386) program can still create AF_ALG sockets through `socketcall(SYS_SOCKET, …)`, whose arguments live in memory where seccomp cannot see them. Closing it means refusing `socketcall` (breaks old 32-bit binaries) or dropping the x86 sub-architecture | closed in C03: `socketcall` is refused with EPERM (see the C03 entries) |
| Old `toolbox/devc/<id>` builds are never removed; they need a garbage collector that knows which boxes still run an old binary | later chunk (with harness gc in C03) |
| `build_from_repo` fails with "not supported yet" | a later chunk |
| Host proxy variables are deliberately not passed into boxes (`--http-proxy=false`); machines behind a proxy need a config option for it | a decision |
| The nested-mount-point check (`/home/dev/.cache`, later harness and data mounts) runs on every start through devc; a box started with plain `podman start` skips it, and it does not cover a project directory that another running box mounts read-write (relevant for C06 data zones) | C06 design |
| `box fix-identity` runs after every start through devc only; it persists in the container's layer, so a restart outside devc keeps it, but a recreated container outside devc would not have it. `/etc/shadow` is not updated | nothing, unless tools in boxes need shadow entries |
| Host bug on the development machine: `statx()` requesting the birth time on files in an image layer hangs forever in fuse-overlayfs 1.17 with libfuse 3.18.2 (`~/.local/opt/workstation-tools`). It breaks `apt-get update` on Debian trixie (sqv) and `stat FILE` inside boxes; the integration tests build their env from bookworm. `devc doctor` could detect it with a probe container that runs `stat` under a timeout; the system fuse-overlayfs (`/usr/bin`, 1.13) was not tried | a decision (doctor check), or a fixed fuse-overlayfs on the host |
| The example `base` env (Debian trixie) has not been built on a healthy host | moot since C03: the example and the installer image use bookworm; trixie still untested on a healthy host |

### From C03

Vendor formats that could not be confirmed without an account or a live session (keys taken from the vendors' documentation as of 2026-11 unless noted):

| Item | Needs |
|---|---|
| Claude `.claude.json` seed keys `hasCompletedOnboarding`, `bypassPermissionsModeAccepted`, `projects.<path>.hasTrustDialogAccepted`, `hasCompletedProjectOnboarding`: not documented, taken from observation of earlier versions; only the location (`$CLAUDE_CONFIG_DIR/.claude.json`) is documented | a Claude session in a box: no onboarding, trust or bypass prompt on first start |
| Claude managed settings: `permissions.defaultMode = "bypassPermissions"` and `skipDangerousModePermissionPrompt` from `/etc/claude-code/managed-settings.json` (documented scopes allow managed); whether bypass mode works for UID 1000 without further confirmation | a Claude session in a box |
| Claude command hooks for `SessionStart`, `UserPromptSubmit`, `Stop`, `Notification`, `SessionEnd` from managed settings (documented format); they call the C03 placeholder `devc box hook`, which ignores them | C04, then a Claude session |
| Claude runtime with the native binary under `/opt/devc/harness/claude/v/<stamp>/home/.local/share/claude/versions/…` while `HOME=/home/dev`: whether it warns about its install location or tries to self-manage (`DISABLE_UPDATES=1` is set, which also blocks `claude install`) | a Claude session in a box |
| Codex `/etc/codex/config.toml` is the documented system layer, the lowest-precedence file layer: the box can override `approval_policy` and the rest from `$CODEX_HOME/config.toml` in the realm-shared, box-writable home. Admin-enforced `/etc/codex/requirements.toml` would bind; its keys were not verified | a decision, then a Codex install |
| Codex hooks: inline `[[hooks.<Event>]]` tables in the system layer, events `SessionStart`, `UserPromptSubmit`, `Stop`, `PermissionRequest` (used as the counterpart of Claude's `Notification`) and `SessionEnd`; the documentation says system-layer hooks count as managed and need no trust review | a Codex session in a box |
| Codex `developer_instructions`, `cli_auth_credentials_store = "file"`, `check_for_update_on_startup = false` (documented keys) take effect | a Codex session |
| `codex login --device-auth` in the login container with the realm's shared home at `/home/dev/.codex`, and `codex login status` as the status command (its exit code is reported as is) | a Codex login |
| `devc login` with an interactive vendor command and the hidden token prompt on a real terminal (tests cover `--token-stdin` and a non-interactive shared-home login with the fake harness) | a manual run |

Other checks and deferred work:

| Item | Needs |
|---|---|
| **Host bug hits Claude Code.** On this host's default storage (fuse-overlayfs 1.17, libfuse 3.18.2) Claude Code's Bun binary hangs in `statx()` with the birth-time mask on image-layer paths (`/opt`, `/etc`): the installer hung in `statx("/etc")`, and Claude in a box is expected to hang the same way. Native rootless overlay (`mount_program` unset in `storage.conf`; kernel 6.8) does not hang: the network test passed there. Installer containers already put tmpfs on `/home`, `/opt`, `/tmp` | a decision: switch this host's podman storage to native overlay (or another fuse-overlayfs), and/or a `devc doctor` probe (run `stat /` in a container under a timeout) |
| 32-bit programs that use `socketcall` (32-bit glibc does) have no sockets in boxes; ENOSYS was not possible (runc drops rules equal to the default action, and libseccomp then collides socket rules on x86) | a decision if 32-bit networking is ever needed (drop the x86 sub-architecture instead, or accept the AF_ALG gap) |
| The socketcall self-test probe and the profile on a kernel without IA-32 emulation (the probe then reports the entry as unreachable) and on arm64 (reported as not applicable) | such hosts |
| Static tmux build and installers on arm64 (same scripts; Alpine and the vendor installers support aarch64) | an arm64 machine |
| Auxiliary containers (installer, tmux build, logins) on an SELinux host: they mount toolbox and state directories without relabelling and add `label=disable` only in mode `disable` | a Fedora machine, enforcing |
| Same-realm race: per-box mount points inside a realm-shared harness home (Codex) are checked with `lstat` before start, but another running box of the realm can swap one for a symlink between the check and the mount, redirecting the mount inside the starting box. Same trust domain, but a residual risk | a design that creates per-box mount points inside the helper container or refuses nested mounts in shared homes |
| A realm-shared harness home is box-writable: a box can plant configuration (Codex hooks, MCP servers) that runs in other boxes of the realm and in the shared-home login container (which is throwaway, with only the toolbox, a temporary home and that directory) | inherent to `strategy = "shared"`; document for users (C07) |
| `harness gc` removes old versions even if a running box still executes one (spec: "only when no box uses them"); old `toolbox/devc/<id>` builds are still never removed | a reference check against running boxes |
| Codex's standalone installer edits shell profiles in the installer's temporary home and uses `lsof` if present; harmless in the throwaway container but unverified across installer versions | each Codex installer update |

