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

### From C07r

| Item | Needs |
|---|---|
| **Supersedes earlier entries.** C06 "Savepoints do not keep owner write bits…": reflink restores now use the savepoint's mode record and restic restores keep restic's modes. C07 "Sentinel assumption": box init now checks the network binding first, so a reset from upstream no longer lets a plain `podman start` pass. C07 "`devc forward` runs `nsenter -U -n … against the box's PID`": it now enters namespaces pinned by descriptor (`--user=/proc/self/fd/3 --net=/proc/self/fd/4`); the SELinux question stays. C07 "Every IPv4 nameserver of the box's resolv.conf is allowed": only those the host trusts are; on this host that is still `10.0.2.3` and `192.168.122.1` | nothing |
| **Binding assumptions.** The record compares the start time of the box's PID 1 as the host sees it (`/proc/<pid>/stat`) with the box's own view (`/proc/1/stat`). The two differ if the box ever gets a time namespace with a boot-time offset, and then every start fails closed with a clear message. Podman gives containers no time namespace, and devc asks for none. On hosts that hide other processes' namespaces from the user (`EACCES` on `/proc/<pid>/ns/net`), the host-side comparison with the engine's namespace path is skipped; box init still compares its own namespace with the record | a host with a different OCI runtime (crun) or hidepid |
| **Trusted resolvers tried with slirp4netns only.** Untried: pasta (`169.254.1.1` taken as its built-in resolver), netavark bridge networks (gateways from `NetworkSettings.Networks`), a slirp4netns `cidr=` option. DNS servers configured in `containers.conf` (`dns_servers`) or by `--dns` are not trusted; boxes then get no DNS exception, which fails closed (`up` warns) | a Podman 6 host (pasta) and a bridge-network setup |
| **B13 result is host-specific.** On this host the engine writes `/etc/resolv.conf` again at every `podman start` and `podman init`, so a change made as root in the box does not survive; other engine versions may differ. The trusted set makes that irrelevant for the rules | nothing |
| A running box of level internet that has also drifted is not checked by `up`, which reports the drift and leaves the box running. If the box was started behind devc's back it can therefore keep running unfiltered until `devc up --recreate` or `devc down`. Box init's binding check still stops a plain `podman start` | a decision (stop drifted boxes whose rules fail?) |
| Branch-tip recording failures (for example a directory/file conflict between a branch kept in the relay and a new one in the box) count as preservation failures. `down`, `rm` and `rebuild` then refuse until the conflict is resolved or `--no-bank` is given. `wt rm` is affected only by its own worktree's snapshot | a decision, if it bothers anyone |
| An in-place restore does not reset the mode of a directory that still exists (it recurses into it); entries it puts back get their saved modes | nothing |
| Box tips (`refs/devc/boxtip/*`) are never pruned. A branch deleted in the box and later recreated at the very tip last seen keeps the relay's head, which a pull may have moved | nothing |

### From C08

| Item | Needs |
|---|---|
| **Box listens, host connects: not tried under SELinux.** The daemon connects (through `/proc/self/fd/<n>` of an `O_PATH` descriptor) to a socket that box init creates in `/run/devc`, which is the box's runtime directory bind-mounted from the host. Spec 12.5 expects this direction to work without a policy module because the socket is created with the box's label and the host process is unconfined; never verified, and neither was reading `/proc/<pid>/cgroup` and `/proc/<pid>/ns/pid` of a container process from the host under SELinux | a Fedora machine, enforcing |
| **Peer check depends on Podman's cgroup naming.** The daemon accepts the channel only when the socket's listener has a cgroup path component `libpod-<container id>` or `libpod-<container id>.scope` (verified on this host: cgroup v2, systemd manager, `…/user.slice/libpod-<id>.scope`; conmon sits in `libpod-conmon-<id>.scope` and does not match). Rootless Podman on cgroup v1 puts containers in no cgroup of their own; there the check fails closed and boxes never get a channel (ingestion from the spool and reconciliation through `podman exec` still work) | a cgroup v1 host; a crun host |
| **The agent's host check assumes separate PID namespaces.** A connection on `agent.sock` is taken as the host's only when `SO_PEERCRED` reports PID 0, that is, a peer outside the box's PID namespace. devc never shares the host's PID namespace with a box; a future option that did would have to replace this check | nothing |
| A box process that deletes `/run/devc/agent.sock` cuts its box off from the daemon until the box restarts: the agent re-creates the socket only when its listener fails, not when the file disappears. Only the box itself is affected | nothing (documented) |
| Warnings about invalid spool lines that the daemon meets while ingesting on its own are kept and handed to the next client that asks (`devc ps` prints them); with several clients, only one sees them. The daemon's log has them all | nothing |
| `devc service install` (devc.service with `Restart=on-failure`, `--timer-only`, `uninstall`) was tested against a temporary unit directory with a stand-in `systemctl`; no real unit was installed on this machine. Lingering is still needed for the daemon to run without a login session | one real `devc service install` and a reboot with lingering on |
| `devc ps --watch` was exercised without a terminal (it prints each redraw after a blank line); the screen-clearing redraw on a terminal was not looked at | a look at a terminal |
| The daemon keeps the derived view of every session it has read (a few KiB each); `ps --all` reads every journal ever written once. Subscriptions that fall more than 4096 batches behind are disconnected and must resubscribe with their cursor | nothing unless journals grow into the tens of thousands |
| Not done from earlier notes: `data gc` on the housekeeping timer (C06 note), daemon-owned forwards and `*.localhost` routing (C07 note), banking after turns (C10a). `Command` and `Receipt` are types only (used from C09) | later chunks |

### From C09

| Item | Needs |
|---|---|
| **Program Status Protocol, as assumed.** Implemented from the specification revision 0.4 (2026-10-10, superlogical.com/rex/docs/build/program-status), fetched on 2026-11-15: report `ESC ] 7501 ; key=value[:key=value…] ST`, query `ESC ] 7501 ; ? ST`, ST = `ESC \` or BEL, whole sequence ≤ 4096 bytes, `msg`/`title` base64 (padding optional) of UTF-8 without control characters (≤ 2048/192 bytes decoded), `id` addresses child records. Choices the specification leaves open: only 7-bit `ESC ]` is recognised (no 8-bit C1 OSC or ST); a control character other than BEL, or an escape that does not start ST, inside the sequence aborts it and the bytes pass through untouched; the wrapper answers `ESC ] 7501 ; ? : states=idle,working,done,blocked,error : kinds=permission,question,auth` (without spaces) with the terminator the query used; reports with an `id` (child records) and `state=clear` produce no event; `progress` and `title` are validated and dropped; consecutive identical reports produce one event | a Claude Code ≥ 2.1.295 and a Codex session in a box: `devc ps` shows their states with `devc events <session> --json` listing `"source":"osc"` |
| **Wrapper with real TUIs.** Transparency was tested with random bytes, a resize, a kill and the exit status through `podman exec -t` and with the fake harness under tmux; never with Claude Code's or Codex's own TUI (mouse, kitty keyboard protocol, bracketed paste, alternate screen, focus events). A possible start of a status report (`ESC`, `ESC ]`, … `ESC ] 7501 ;`) at the end of the output is held back up to 30 ms waiting for the next byte; a started report waits for its terminator (at most 4 KiB) | a session of each in a box: type, paste, resize, use the mouse, quit; compare with `DEVC_NO_PTY=1` |
| **Flooded echo through `podman exec -t` deadlocks in conmon.** Writing 256 KiB into a terminal whose program echoes everything back hung (conmon blocked writing the container's terminal while not reading its output); it happens with any echoing program, wrapper or not. The integration test sends 1 KiB at a time and waits for the echo. tmux, the wrapper's real outer terminal, is event-driven and not affected | nothing (recorded) |
| **Claude Code `AskUserQuestion` answered from the hook.** Written from the hook reference (fetched 2026-11-15): a `PreToolUse` hook with matcher `AskUserQuestion` returns `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{…original input…,"answers":{"<question text>":"<option label>"}}}}`; free text is passed as the answer value; multi-select answers would join labels with commas (devc sends one label). Only a call with a single question is answered remotely; several questions show the dialog. The baseline sets the hook's timeout to `input.question_wait + 60` seconds. The question's ID is `tool_use_id` | a Claude session: ask it to use AskUserQuestion with nobody attached, `devc answer <session> 2`, check the turn continues with that choice; then attach and check the dialog appears at once |
| **Paste adapter with real harnesses.** Claude Code collapses pastes over 800 characters or three lines into a placeholder and tells the model the paste may hold instructions it did not write (research 10 section 6); its `UserPromptSubmit` hook receives the expanded text, which is what devc hashes (devc also accepts the hash of the text with surrounding whitespace or trailing newlines removed). Codex submits with Tab (manifest), Enter steers. Neither was tried | a session of each: `devc send`, `--steer` during a turn, a long multi-line prompt; check the receipts say delivered |
| **Interrupt and on-screen answers with real harnesses.** Escape (manifest `input.interrupt_keys`) confirmed by any state event within 5 s; dialogs answered with the option number and Enter (`input.answer_keys`). Claude Code's and Codex's dialogs were not looked at; their keys may differ (arrow selection) | a session of each with a permission or question dialog |
| **`mod` adapter (Claude Code): not available.** The mods API documentation (code.claude.com/docs/en/plugins/mods/api, fetched 2026-11-15) gives `$.prompt.submit({text, asUser: true})` (waits until idle, then starts a turn), `turn.*` events and `$.process.spawn`, but a mod has no sockets or Node APIs, only `$.process`, `$.fs`, `$.http` (http/https) and `$.clock`. A workable design: the mod's `session.start` hook spawns `devc box mod-bridge` (to be written), which connects to the box agent and prints commands as JSON lines; the mod reads them from the spawned process's output, calls `$.prompt.submit` or aborts the turn, and reports results by running `devc box mod-ack`. Missing for confidence: how a mod is packaged and loaded through managed settings (plugin directory, manifest, enabling it without user consent prompts), the exact abort call (`$.turn.abort` per research 10 vs. the reference), whether `$.process.spawn` streams output line by line to a hook, and behaviour after an interrupt (calls refused until the next user input). The daemon falls back to paste and says so in the receipt | the mods reference and types for the installed build, then a Claude session: write the mod and bridge, set `input.adapter = "mod"` in a user manifest, run the C09 integration items 3–8 against Claude |
| **`app-server` adapter (Codex): not available.** From the app-server documentation (learn.chatgpt.com/docs/app-server, fetched 2026-11-15): `codex app-server --listen unix://PATH` serves JSON-RPC (without the `jsonrpc` member) as WebSocket messages over the Unix socket; the TUI attaches with `codex --remote unix://PATH`; a client sends `initialize` (clientInfo) then `initialized`, and uses `turn/start {threadId, input:[{type:"text",text}]}`, `turn/steer {threadId, input, expectedTurnId}`, `turn/interrupt {threadId, turnId}`, with notifications `turn/started`, `turn/completed`, `thread/status/changed`. Missing for confidence: how a second client finds the thread the attached TUI shows (`thread/loaded/list` returns loaded threads, but nothing ties one to a TUI), whether turns started by another client render in the TUI, and who owns the app-server process (the launcher would start it beside the TUI and must stop it when the TUI exits). Would use `coder/websocket` (approved) | a Codex login: start `codex app-server --listen unix:///run/devc/codex-<session>.sock` and `codex --remote unix://…` by hand in a box, list loaded threads from a second client, start a turn and watch the TUI; then build the adapter and set `input.adapter = "app-server"` |
| **`extension` adapter (pi): not available.** pi documents `pi.sendUserMessage(text, {deliverAs: "steer"})` and `ctx.abort()` for extensions (research 10); pi has no manifest yet (C16) | C16, then a pi session |
| The wrapper is used only when the launcher's standard input and output are terminals (tmux windows, `devc sh`); a harness started with pipes (`claude -p … > file`) runs directly, so it never gets a pseudo-terminal it did not ask for | nothing (choice) |
| Receipts keep the command's text until the command is final (then it is removed); they live in `local/receipts/` (0700, files 0600). A queued command older than seven days is refused at the next daemon start | nothing |
| `session.start` (and `devc <harness> --detach --prompt`) needs a running box and does not create worktrees itself; the CLI prepares both before it asks the daemon. A missing worktree makes the session start in the project directory | C12, if the hub needs to start sessions in new worktrees |

### From C10a

| Item | Needs |
|---|---|
| **Fetch-through and forwards only tried with `file://` upstreams.** Background mode (no terminal, no askpass, `GIT_TERMINAL_PROMPT=0`) should make an SSH upstream without an agent key, or an HTTPS upstream without a stored credential, fail within the 20 s instead of asking; an `ssh-agent` key and credential helpers should keep working. A credential helper that pops up a window (or a hardware key that wants a touch) still might | a GitHub upstream over SSH (agent and no agent) and over HTTPS (helper and none) |
| **Git over `connect` uses protocol v0.** The daemon does not pass `GIT_PROTOCOL`, so upload-pack and receive-pack advertise every visible ref (the hidden ones excluded). Fine for ordinary repositories; a relay with tens of thousands of refs makes every fetch advertise them all. Partial clones (`--filter`) are not offered | a large repository, if fetches get slow |
| **Turn banking with real harnesses** depends on their state events (hooks or OSC 7501) going working → done or idle; only the fake harness was used | a Claude Code and a Codex session: a turn that edits without committing, then a snapshot in `refs/devc/snap/` within ~30 s |
| **On-demand secrets with real harnesses.** Only the fake harness's env-token was delivered through `host.secret`; Claude Code (`CLAUDE_CODE_OAUTH_TOKEN`) and api-key manifests take the same path | a Claude login with `secrets.delivery = "daemon"` |
| **Requests not carried out yet:** `net` (C13), `grant` (needs a decision how a grant is written: devc never edits `project.toml` today), `pr` (C15). They are stored and fail with "not available yet" when approved | C13, C15, a decision for grants |
| **Hub decisions** are only exercised through a local client declaring origin `hub`; the hub transport (C11) must use the same `requests.decide` path with its own origin (`decideHandler(proto.OriginHub)`) and nothing else | C11a |
| Old boxes keep the agent they were started with (yamux close timeout 1 minute): a push whose receive-pack checks and hooks take longer than a minute after the pack was sent breaks until the box restarts. Projects initialised before C10a lack the box's `github/*` fetch refspec and `tagOpt` until `devc project init` | nothing (upgrade note) |
| A box can make the host store as much as it pushes (no `receive.maxInputSize`), as it already could through banking; receive-pack checks every object (`receive.fsckObjects`) | a quota decision, if needed |
| `devc requests` without a daemon reads the logs as they are; nothing expires until the daemon runs | nothing |
| The relay service and `host.secret` under SELinux: nothing new crosses the box boundary (the daemon spawns git on the host; the stream runs over the existing channel), but it was not tried | a Fedora machine, enforcing |

### From C10b

| Item | Needs |
|---|---|
| **Password and second-factor logins not exercised.** `devc ssh login` runs ssh attached to the terminal, so prompts should work as with plain ssh; only key authentication was used (the integration test's stand-in remote accepts only a key). Nothing in the gate depends on how the master authenticated | a host with password + OTP login: `devc ssh login <host>`, then the integration items 2–5 by hand from a box |
| **SELinux not tried.** New across the box boundary: the file mount of `/etc/devc/ssh_config` at `/etc/ssh/ssh_config.d/50-devc.conf` (its source lies in the box's etc directory, which is already mounted). The gate's sshd runs on the host, unconfined, and talks to the box only over the existing channel | a Fedora machine, enforcing |
| **Root in a box with a grant cannot run ssh at all.** OpenSSH refuses an included configuration file owned by anyone but root or the caller ("Bad owner or permissions on /etc/ssh/ssh_config.d/50-devc.conf", verified); in the box the file belongs to `dev`. `dev` is unaffected. A fix would chown the host file to the subordinate UID that is root in the box (`podman unshare chown 1:1` under the keep-id mapping) after every write; not done because it assumes the mapping's layout | a decision, if root in a box needs ssh |
| **rsync over the gate not tried**: the test env image has no rsync (the test checks for it and skips that step). It uses an ordinary command through the gate, like `scp -O`, which was tried | an image with rsync |
| **Forced command and the user's login shell.** sshd runs `devc gate-exec` through the host user's login shell (`$SHELL -c`). A `nologin` shell disables the gate; shell start-up files that print something (bash sources `~/.bashrc` for sshd sessions on Debian, zsh always reads `~/.zshenv`) corrupt sftp and scp through the gate, as they would for direct SSH. Only zsh was tried | nothing (documented in the guide) |
| **OpenSSH versions.** The gate's sshd configuration uses `DisableForwarding` (7.4), `PermitListen` (7.8) and `AuthenticationMethods`; gate-exec's ssh uses `RemoteCommand` (7.6). An older sshd fails on the unknown keyword and the box's ssh reports the connection closed (the daemon logs sshd's message). Tried with OpenSSH 9.6 on the host and 9.2 in the box only; OpenSSH ≥ 9.8 splits sshd into `sshd-session`, which `sshd -i` should still handle | a host with OpenSSH 10 |
| **Interactive shells through the gate** were tried without a terminal (`ssh -T host` reading commands from standard input) and as `ssh -tt host tty`; a full interactive login with a terminal (resizing, `~.` escapes in the box's client) was not looked at | a look at a terminal: `devc sh <ref>`, `ssh <host>`, resize, exit |
| The gate log grows without bound and holds whatever the agent typed (truncated to 4 KiB per command), possibly secrets it put on a command line; it is 0600 in the durable state | a rotation decision |
| A grant added while a box runs needs `devc up --recreate` (the configuration mount changes the box definition); a removed grant closes the gate at once. `devc request grant ssh <host>` is still "not available yet" (C10a) | the grant decision of C10a |
| If the gate's host key is deleted, the next `devc up` of a box with a grant makes a new one, and boxes that are running keep the old one in their known-hosts file until they restart (their ssh then refuses the gate with a host key warning) | nothing |
| When the daemon stops (or the box's channel goes) it sends SIGTERM to the gate sshd processes it started; the remote command should then see its input close, as when a direct SSH connection drops. Not tried with a session open | a long-running `ssh <host> sleep 600` from a box while the daemon stops |

### From C11a

| Item | Needs |
|---|---|
| **Login, devices, enrolment and notifications** are C11b. Until then any valid device certificate may use the whole API; the session check goes into `hub.Server.checkUser`, the one place every `/api/` request passes. `hub.role` is recorded (`machines.notifier`) but nothing notifies yet | C11b |
| **PKCS#12 bundles on real devices.** Bundles decode with go-pkcs12 (modern and `--legacy-p12`); importing one into Android, iOS, Firefox or Chrome and reaching the hub with it was not tried (no phone or desktop browser here) | a phone and a desktop browser |
| **Direct TLS with browsers.** The direct mode serves HTTP/1.1 only (the machines' WebSocket needs it; Go's server would offer h2 to browsers otherwise). A browser over HTTP/1.1 holds at most six connections per host, which an app with several event streams may hit; behind a proxy the browser side is HTTP/2 | C12: decide whether to enable h2 for browsers in direct mode |
| **Server certificate renewal.** A certificate from a public CA given with `--tls-cert` is read at start; renewing it needs a restart of the hub (behind a proxy the proxy renews) | a reload on SIGHUP, if wanted |
| **Machines behind an HTTP proxy.** The machine side uses `HTTPS_PROXY` from the environment (Go's `ProxyFromEnvironment`); not tried | a network that needs one |
| **Proxy configurations** were checked with nginx 1.28.0 and Caddy 2.10.0 containers (host network) on loopback, by a script, not by the integration suite (it would pull two images every run). Caddy with client authentication insists that the TLS server name equals the `Host` header, so its public name must be a DNS name | nothing (re-run by hand when the configurations change) |
| **A compromised machine** can fill its own part of the hub: at most 20,000 sessions per machine (`hub.MaxMachineSessions`), 500 events or 1 MiB per session, a change log of 20,000 entries; sessions that ended more than 30 days ago are pruned hourly. It cannot write another machine's rows | nothing |
| **Old ended sessions** outside the 30-day window are neither subscribed to nor listed; a `seen` recorded for such a session on the machine reaches a hub only if the hub still holds it | nothing |
| **Paging backwards** (`?before=`) asks the machine with `events.read` from `before − limit − 1`, which relies on a session's seqs being consecutive (they are, by construction of the journal) | nothing |
| **SELinux not tried.** Nothing new crosses the box boundary: the hub link is a TCP connection of the daemon on the host | a Fedora machine, enforcing |
| **Binary size.** modernc.org/sqlite makes `bin/devc` about 19 MB (stripped); every machine carries the hub's code | a decision, if size matters (a build tag for a machine-only binary) |
