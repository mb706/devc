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
| Host proxy variables are deliberately not passed into boxes (`--http-proxy=false`); machines behind a proxy need a config option for it | a decision (since C03r also for auxiliary containers and builds) |
| The nested-mount-point check (`/home/dev/.cache`, later harness and data mounts) runs on every start through devc; a box started with plain `podman start` skips it, and it does not cover a project directory that another running box mounts read-write (relevant for C06 data zones) | C06 design |
| `box fix-identity` runs after every start through devc only; it persists in the container's layer, so a restart outside devc keeps it, but a recreated container outside devc would not have it. `/etc/shadow` is not updated | nothing, unless tools in boxes need shadow entries |
| Host bug on the development machine: `statx()` requesting the birth time on files in an image layer hangs forever in fuse-overlayfs 1.17 with libfuse 3.18.2 (`~/.local/opt/workstation-tools`). It breaks `apt-get update` on Debian trixie (sqv) and `stat FILE` inside boxes; the integration tests build their env from bookworm. `devc doctor` could detect it with a probe container that runs `stat` under a timeout; the system fuse-overlayfs (`/usr/bin`, 1.13) was not tried | doctor check `overlay-statx` added in C07 (fails here, as expected); still needs a fixed fuse-overlayfs or native overlay on the host |
| The example `base` env (Debian trixie) has not been built on a healthy host | moot since C03: the example and the installer image use bookworm; trixie still untested on a healthy host |

### From C03

Vendor formats that could not be confirmed without an account or a live session (keys taken from the vendors' documentation as of 2026-11 unless noted):

| Item | Needs |
|---|---|
| Claude `.claude.json` seed keys `hasCompletedOnboarding`, `bypassPermissionsModeAccepted`, `projects.<path>.hasTrustDialogAccepted`, `hasCompletedProjectOnboarding`: not documented, taken from observation of earlier versions; only the location (`$CLAUDE_CONFIG_DIR/.claude.json`) is documented | a Claude session in a box: no onboarding, trust or bypass prompt on first start |
| Claude managed settings: `permissions.defaultMode = "bypassPermissions"` and `skipDangerousModePermissionPrompt` from `/etc/claude-code/managed-settings.json` (documented scopes allow managed); whether bypass mode works for UID 1000 without further confirmation | a Claude session in a box |
| Claude command hooks for `SessionStart`, `UserPromptSubmit`, `Stop`, `Notification`, `SessionEnd` from managed settings (documented format); they call `devc box hook claude <event>` (the real receiver since C04) | a Claude session |
| Claude runtime with the native binary under `/opt/devc/harness/claude/v/<stamp>/home/.local/share/claude/versions/…` while `HOME=/home/dev`: whether it warns about its install location or tries to self-manage (`DISABLE_UPDATES=1` is set, which also blocks `claude install`) | a Claude session in a box |
| Codex `/etc/codex/config.toml` is the documented system layer, the lowest-precedence file layer: the box can override `approval_policy` and the rest from `$CODEX_HOME/config.toml` in the realm-shared, box-writable home. Admin-enforced `/etc/codex/requirements.toml` would bind; its keys were not verified | requirements and managed defaults added in C03r (see there); still unverified |
| Codex hooks: inline `[[hooks.<Event>]]` tables in the system layer, events `SessionStart`, `UserPromptSubmit`, `Stop`, `PermissionRequest` (used as the counterpart of Claude's `Notification`) and `SessionEnd`; the documentation says system-layer hooks count as managed and need no trust review | a Codex session in a box |
| Codex `developer_instructions`, `cli_auth_credentials_store = "file"`, `check_for_update_on_startup = false` (documented keys) take effect | a Codex session |
| `codex login --device-auth` in the login container with the realm's shared home at `/home/dev/.codex`, and `codex login status` as the status command (its exit code is reported as is) | a Codex login |
| `devc login` with an interactive vendor command and the hidden token prompt on a real terminal (tests cover `--token-stdin` and a non-interactive shared-home login with the fake harness) | a manual run |

Other checks and deferred work:

| Item | Needs |
|---|---|
| **Host bug hits Claude Code.** On this host's default storage (fuse-overlayfs 1.17, libfuse 3.18.2) Claude Code's Bun binary hangs in `statx()` with the birth-time mask on image-layer paths (`/opt`, `/etc`): the installer hung in `statx("/etc")`, and Claude in a box is expected to hang the same way. Native rootless overlay (`mount_program` unset in `storage.conf`; kernel 6.8) does not hang: the network test passed there. Installer containers already put tmpfs on `/home`, `/opt`, `/tmp` | doctor probe added in C07 (`overlay-statx`); still a decision: switch this host's podman storage to native overlay (or another fuse-overlayfs) |
| 32-bit programs that use `socketcall` (32-bit glibc does) have no sockets in boxes; ENOSYS was not possible (runc drops rules equal to the default action, and libseccomp then collides socket rules on x86). Re-verified in C03r: removing socketcall from the allow lists makes every container fail to start; SCMP_ACT_TRACE would be bypassable by a tracer in the box | a decision if 32-bit networking is ever needed (drop the x86 sub-architecture instead, or accept the AF_ALG gap) |
| The socketcall self-test probe and the profile on a kernel without IA-32 emulation (the probe then reports the entry as unreachable) and on arm64 (reported as not applicable) | such hosts |
| Static tmux build and installers on arm64 (same scripts; Alpine and the vendor installers support aarch64) | an arm64 machine |
| Auxiliary containers (installer, tmux build, logins) on an SELinux host: they mount toolbox and state directories without relabelling and add `label=disable` only in mode `disable` | a Fedora machine, enforcing |
| Same-realm race: per-box mount points inside a realm-shared harness home (Codex) are checked with `lstat` before start, but another running box of the realm can swap one for a symlink between the check and the mount, redirecting the mount inside the starting box. Same trust domain, but a residual risk | accepted in C03r (see there) |
| A realm-shared harness home is box-writable: a box can plant configuration (Codex hooks, MCP servers) that runs in other boxes of the realm and in the shared-home login container (which is throwaway, with only the toolbox, a temporary home and that directory) | inherent to `strategy = "shared"`; documented in `docs/guide.md` (C07) |
| `harness gc` removes old versions even if a running box still executes one (spec: "only when no box uses them"); old `toolbox/devc/<id>` builds are still never removed | harness versions: closed in C03r (E1); old `toolbox/devc/<id>` builds are still never removed |
| Codex's standalone installer edits shell profiles in the installer's temporary home and uses `lsof` if present; harmless in the throwaway container but unverified across installer versions | each Codex installer update |


### From C03r

| Item | Needs |
|---|---|
| **Accepted residual (A2).** Per-box mount points inside a realm-shared harness home (Codex's `per_box` directories) are created beneath a directory descriptor without following symlinks, immediately before the box starts; another running box of the same realm can still swap a component for a symlink between that check and the engine's mount. Same trust domain (one realm); not closed on purpose | nothing, unless realms stop being one trust domain |
| Codex enforcement (C1): `/etc/codex/requirements.toml` (allowed approval policies and sandbox modes, `[features] hooks = true`, managed hooks) and `/etc/codex/managed_config.toml` are written from the documentation (2026-11); whether Codex 0.169+ honours them, treats the hooks as managed without a trust prompt, and ignores a conflicting `$CODEX_HOME/config.toml` is unverified | a Codex session in a box, with a conflicting `config.toml` in the shared home |
| Codex gap (C1): `developer_instructions`, `check_for_update_on_startup` and `cli_auth_credentials_store` cannot be enforced by requirements; they are managed defaults (`managed_config.toml`, a "legacy" layer users can change during a run) plus the system layer. A box can therefore weaken them for the realm through the shared home at the next start if Codex drops the managed-defaults layer | a Codex release with an enforced instructions setting, or a private Codex home |
| Codex option not taken: `allow_managed_hooks_only = true` in `requirements.toml` would stop hooks planted in the realm-shared home from running in other boxes, at the price of the user's own Codex hooks | a decision |
| Codex version pinning through `CODEX_RELEASE` (`devc harness install codex --version x.y.z`) is taken from the installer script; not run here | one networked install |
| GPU (`--device nvidia.com/gpu=all`, needs the CDI spec from `nvidia-ctk cdi generate`) and KVM (`--device /dev/kvm --group-add keep-groups`) are unit-tested only; `keep-groups` is documented as crun-only, so with runc the KVM case may fail at create | hardware with a GPU / KVM, and a check with runc |
| `capabilities.nested = true` is refused as not implemented | a design for nested containers |
| Hosts that need an HTTP proxy for installers, the tmux build or image builds: all devc containers now run with `--http-proxy=false` (A6), so such hosts cannot install harnesses yet | an explicit proxy option that keeps credentials out of inspect data |
| Resource limits now fail closed (A7): on hosts without cgroup delegation every box needs `memory = "none"` and `pids = -1` set explicitly | nothing; documented in the error message |
| The helper container (`--rootfs <toolbox>/helper-rootfs:O`) for `rm --home` works on this host (fuse-overlayfs, runc); untested on SELinux hosts (it mounts a box home without relabelling) and with crun | a Fedora machine, enforcing |
| `devc up` on a running box no longer re-delivers secrets, baselines or launch descriptions (A3); `devc update`/`harness install` still refresh launch descriptions of running boxes. A command to refresh secrets of a running box does not exist | a decision (C08 delivers secrets on demand anyway) |
| `harness gc` treats every running devc box on the host (any state directory) as a possible user of a version; boxes started before the history file existed fall back to the stamp times | nothing |

### From C04

Hook field names that could not be confirmed against a live harness (taken from the vendors' hook references as of 2026-11):

| Item | Needs |
|---|---|
| Claude Code hook inputs: `session_id`, `transcript_path`, `cwd`, `source` (SessionStart), `prompt` (UserPromptSubmit), `last_assistant_message` (Stop), `message`, `title`, `notification_type` (Notification), `reason` (SessionEnd). Mapped notification types: `permission_prompt` → blocked/permission, `elicitation_dialog`, `elicitation_url_dialog`, `agent_needs_input` → blocked/question (the last two from the matcher table only), `idle_prompt` → done, everything else → note | a Claude session in a box (one permission prompt, one question, one idle notification, `/exit`) |
| Claude: an empty stdout and exit 0 from every hook is harmless (SessionStart and UserPromptSubmit stdout becomes model context); the SessionEnd hook finishes within Claude's 1.5 s budget | a Claude session in a box |
| Codex hook inputs: `session_id`, `transcript_path`, `cwd`, `source`, `prompt`, `turn_id`, `last_assistant_message`, `tool_name`, `tool_input.description` (may be null; `tool_input.command` is used as a fallback), `reason` (documented as always `other`); whether `PermissionRequest` fires at all under `approval_policy = "never"` | a Codex session in a box |
| Claude Code and Codex TUIs inside devc's tmux (passthrough, extended keys, focus events, mouse) and inside a grouped client session | real sessions in a box |

Other checks and deferred work:

| Item | Needs |
|---|---|
| `ssh <machine> -t devc attach <ref>`: tested only under a local pseudo-terminal (creack/pty), not over SSH | a second machine (or sshd on localhost) |
| The numbered session chooser of `devc attach` on a real terminal (unit-tested with scripted input) | a manual run |
| A harness that exits within milliseconds of its window's creation (before the window is tagged) gets no `session.end` from tmux and is reconciled as `lost`; a launcher error in an attached window disappears with the pane, because the hook removes dead panes at once (`box session start` checks the description and the executable first, which catches the usual causes) | a decision (keep failed panes for a few seconds?) |
| Periodic context commits (spec 8.3): C04 commits at `down`/`rm` and on demand only | C05's banking timer |
| `box session ps` counts every process whose initial environment names a session, including background processes an agent left behind; they keep a session started from a shell "live" after its harness died | a decision (count only processes with `DEVC_HARNESS`?) |
| Journals are never rotated or pruned; diff snapshots leave unreachable objects in the context repository (`gc.auto = 0`, no `git gc`) | a retention design (with C17 state sync) |
| The context container on SELinux hosts mounts the context directory and the toolbox without relabelling, like the other auxiliary containers | a Fedora machine, enforcing |
| Text fields in `devc ps --json` and the journal are box-written; devc escapes control characters for terminals, but other consumers must treat them as untrusted | documented in `docs/guide.md` (C07) |

### From C05

| Item | Needs |
|---|---|
| Claude Code's `WorktreeCreate`/`WorktreeRemove` hooks in the baseline call `devc box worktree-hook create|remove`. Assumed hook input: `name` (or `worktree_name`) for create, `worktree_path` (or `path`, or `name`) for remove; create prints the absolute path on stdout. Unverified: the field names, that the printed path is used, and how a failure (non-zero exit) is reported | a Claude session in a box using `--worktree` |
| Upstream operations attached to a terminal (SSH passphrase prompts, credential helpers) are exercised only without a terminal (`GIT_TERMINAL_PROMPT=0`) against `file://` upstreams | a real SSH/HTTPS remote with credentials, interactively |
| `devc service install|uninstall|status` is tested against a temporary unit directory with a fake systemctl; the units were never installed or run by a real systemd user manager | a manual `devc service install` on a workstation (and `loginctl enable-linger` for runs while logged out) |
| Box images with old git: the box side uses `git worktree list --porcelain` (with `prunable`, git ≥ 2.31), `git checkout -b … --track`, `ls-files -z`, `update-index -z --stdin`; tested only with Debian bookworm's git 2.39 | an image with an older git, or a minimum version in the image contract |
| The stub `git-remote-devc` answers `fetch` with nothing; git normally skips the call because every object is already present. A box whose refs/remotes/origin/* point at objects it does not have (deleted packs) would fail its fetch with a confusing message | C10's real transport |
| `devc pull` on a stopped box updates the relay only; the box sees github/* and new tags at the next `devc pull` with the box running (bank reflects only origin/*) | a decision (reflect everything at `up`?) |
| Relay tags are refs/tags = the upstream as last fetched; `pull` does not prune deleted upstream tags (it prunes deleted branches) | nothing |
| Relay garbage: `gc.auto = 0`, nothing ever runs `git gc` on relays, and bank refs are never pruned | a retention design (with C17) |
| A bank applies at most 1000 changed branches and 1000 new tags (the rest wait for the next bank); a box can still make the fetch itself large | a size limit on fetched packs, if needed |

### From C06

| Item | Needs |
|---|---|
| The `reflink` savepoint driver (FICLONE copies in `.devc/savepoints/`, probe, read-only vault-owned result, restore from it) is unit-tested with a substitute clone function only; the projects filesystem here is ext4, so `TestData/reflink` skips and `auto` always falls back to restic | a btrfs or XFS projects filesystem |
| `borg` is accepted by the configuration but `devc save` refuses it with a clear message | an implementation (auxiliary image) if anyone wants it |
| Helper containers and restic runs get the box's SELinux options (`label=level:<realm level>`, or `label=disable` in disable mode); never run on an SELinux host | a Fedora machine, enforcing |
| Known and accepted limits of sealing: a process in the box that already holds a writable descriptor of a file keeps it after sealing (and can write until it closes it); sealing protects against the box only, never against the host user, who can unseal, delete or change anything; FIFOs, sockets and devices inside a sealed entry are skipped (left to the box user, but immovable inside the read-only directory); a file hard-linked from elsewhere in the project is sealed under every name | documented in `docs/guide.md` (C07) |
| Sealing changes ownership in place and has no undo log: a worker killed half-way leaves the top of the entry sealed and part of the tree unsealed; sealing again (a no-op on the record) re-walks and completes it | nothing (documented) |
| Savepoints do not keep owner write bits of reflink copies (the copy is read-only); a restore gives every file owner write and every directory owner rwx, and the box user owns everything restored | nothing (documented) |
| restic exit status 3 (some files unreadable) still records the savepoint, with a warning; files the box made unreadable even to itself (mode 000) are therefore missing from restic savepoints | nothing (documented) |
| A project whose `[data]` section is removed keeps its vault-owned project directory, `.devc/` and data root; devc does not hand them back | a decision (`devc data teardown`?) |
| A project directory whose `.devc` was created by the box (possible before C06, when the project directory was box-owned) is refused by setup until the user moves it away | nothing (documented) |
| Data commands hold the box lock: a long `devc save` or `restore` makes `up`, `bank` and other commands on that box wait (up to 10 minutes, then they fail) | a decision, if it bothers anyone |
| restic prunes only in `devc data gc`; retention after `save` only forgets snapshots, so the repository grows until the next gc | the banking timer could run `data gc` (C08) |

### From C07

| Item | Needs |
|---|---|
| **Tried with slirp4netns only.** The whole level-internet path (namespace path from `SandboxKey`, rules installed between `podman init` and `podman start` with `podman unshare nsenter --net`, IPv6 sysctls next to the network command's own IPv6 setup, resolvers from the box's resolv.conf, host alias from its hosts file, sentinel, canary) and `devc forward` were exercised only with Podman 5.8.3 and slirp4netns. With pasta the resolver is expected to be a link-local forwarder (169.254.1.1) and the host alias a mapped address (169.254.1.2); both are handled by the code (resolvers allowed on port 53 whatever their address, every host alias address tested by the canary) but untried. Podman 6 removes slirp4netns | a Podman 6 host (pasta) |
| **Sentinel assumption.** Box init treats an immediate "connection refused" from 192.0.2.1:9 as proof that the rules are loaded. Without rules, slirp4netns lets the connection time out (verified); a network stack that answered unreachable TEST-NET addresses with a TCP reset would let an unfiltered box pass this check (the canary after `devc up` would still catch a missing rule set for host and LAN addresses, but not for a plain `podman start`) | a check with pasta and with netavark bridge networks |
| **DNS remains a channel** at level internet: UDP and TCP port 53 to the box's resolvers are open, so a box can tunnel data through DNS queries | nothing (documented); the gateway of C13 can restrict it |
| **Resolver on a private address.** Every IPv4 nameserver of the box's resolv.conf is allowed on port 53, including private ones. On this host Podman lists `10.0.2.3` (slirp4netns) and `192.168.122.1` (the LAN router, from systemd-resolved's upstream list), so boxes can reach that one LAN host on port 53. Allowing only the network command's own forwarder would close it but needs to know which address that is (engine-specific) | a decision (documented for users) |
| Host public addresses are rejected as they are at the box's start; an address the host acquires later (DHCP, a new interface) is not covered until the box restarts | nothing, unless servers with changing public addresses matter |
| The canary only tests the host's addresses and the host alias on one random TCP port; other protocols and ports rely on the rules (verified by listing them from the host) | nothing |
| `devc forward` runs `nsenter -U -n --preserve-credentials` against the box's PID: verified rootless on this host (keep-id user namespace nested in the engine's); with SELinux enforcing, the dial process keeps the host's label inside the box's network namespace, which may be refused | a Fedora machine, enforcing |
| `doctor overlay-statx` cleanup kills the container's fuse-overlayfs process; verified with fuse-overlayfs 1.17 here. With native overlay the probe returns at once (expected `ok`, not run here since this host's storage is fuse-overlayfs); with other storage drivers (`vfs`, btrfs) the probe just passes | a host with native overlay storage |
| `devc cursor` and `devc code` build the attached-container URI as v1 did; never run here (no editor CLI on this host) | a desktop with Cursor or VS Code |
| `devc init <git-url>` is unit-tested with a fake git only | a real configuration repository |
| The example envs `r-base` and `python-base` were not built (not part of the tests); `r-base` uses `COPY --chmod`, which needs a recent buildah | one `devc env build r-base` and `python-base` on a host with network |
| The root script of `doctor --fix` (usermod, loginctl, semanage, AppArmor profile, cgroup drop-in) was printed and reviewed, never run (no root here) | a fresh host with sudo |
| The XFS loop-file recipe in `docs/host-setup.md` is untested | root access |
