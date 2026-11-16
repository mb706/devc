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
| ~~**Login, devices, enrolment and notifications** are C11b.~~ Done in C11b (the session check is `Server.checkSession`, run by `guard` for every `accessUser` route) | — |
| **PKCS#12 bundles on real devices.** Bundles decode with go-pkcs12 (modern and `--legacy-p12`); importing one into Android, iOS, Firefox or Chrome and reaching the hub with it was not tried (no phone or desktop browser here) | a phone and a desktop browser |
| **Direct TLS with browsers.** The direct mode serves HTTP/1.1 only (the machines' WebSocket needs it; Go's server would offer h2 to browsers otherwise). A browser over HTTP/1.1 holds at most six connections per host, which an app with several event streams may hit; behind a proxy the browser side is HTTP/2. C12 decided to stay with HTTP/1.1: the app holds one event stream (reopened when the open session changes) and at most one terminal WebSocket | nothing |
| **Server certificate renewal.** A certificate from a public CA given with `--tls-cert` is read at start; renewing it needs a restart of the hub (behind a proxy the proxy renews) | a reload on SIGHUP, if wanted |
| **Machines behind an HTTP proxy.** The machine side uses `HTTPS_PROXY` from the environment (Go's `ProxyFromEnvironment`); not tried | a network that needs one |
| **Proxy configurations** were checked with nginx 1.28.0 and Caddy 2.10.0 containers (host network) on loopback, by a script, not by the integration suite (it would pull two images every run). Caddy with client authentication insists that the TLS server name equals the `Host` header, so its public name must be a DNS name | nothing (re-run by hand when the configurations change) |
| **A compromised machine** can fill its own part of the hub: at most 20,000 sessions per machine (`hub.MaxMachineSessions`), 500 events or 1 MiB per session, a change log of 20,000 entries; sessions that ended more than 30 days ago are pruned hourly. It cannot write another machine's rows | nothing |
| **Old ended sessions** outside the 30-day window are neither subscribed to nor listed; a `seen` recorded for such a session on the machine reaches a hub only if the hub still holds it | nothing |
| **Paging backwards** (`?before=`) asks the machine with `events.read` from `before − limit − 1`, which relies on a session's seqs being consecutive (they are, by construction of the journal) | nothing |
| **SELinux not tried.** Nothing new crosses the box boundary: the hub link is a TCP connection of the daemon on the host | a Fedora machine, enforcing |
| **Binary size.** modernc.org/sqlite makes `bin/devc` about 19 MB (stripped); every machine carries the hub's code | a decision, if size matters (a build tag for a machine-only binary) |

### From C11b

| Item | Needs |
|---|---|
| **Passkeys deferred.** The second factor is a password with optional TOTP, as the work order decides; passkeys need a credential provider on the phone and could not be tested here. `specs-v2.md` section 13 says so | a passkey design (go-webauthn is on the approved list) and a phone |
| **Nothing was tried on a real phone or against a real ntfy server.** Publishing follows the ntfy documentation as summarised in `../v2-research/10-unified-interface.md` section 3: JSON to the server root with `sequence_id`, clearing with `PUT /<topic>/<seq>/clear`, `http` actions with headers and `clear`. Whether a given ntfy server version honours `sequence_id` in JSON, and whether the ntfy Android app presents its client certificate for an action button's request to another host (the hub), is unverified; the tests use a fake server | a phone with ntfy from F-Droid, a self-hosted ntfy |
| **Importing an enrolled bundle** in a phone's browser by opening the URL (download, then import into the certificate store) was not tried; in direct TLS mode the hub now always asks for a client certificate (optional), and how browsers without one behave on that prompt was not looked at | a phone and a desktop browser |
| **The proxy configurations** (enrolment path without a certificate, `X-Real-IP`) were checked with nginx 1.28.0 and Caddy 2.10.0 containers by a script, not by the suite; the two images were removed afterwards. A machine daemon through the proxies was not tried again (the `/machine` location only gained headers) | re-run by hand when the configurations change |
| **TOTP enable** prints the secret once and turns TOTP on without asking for a first code; a mistyped secret is fixed with `disable` and `enable` on the hub | a confirmation step, if wanted |
| **Rate limits** count HTTP failures only; in direct TLS mode, failed handshakes (no or unknown certificates) are not counted, and a client may open TLS handshakes freely. Behind a proxy the address comes from `X-Real-IP`; a proxy that does not set it makes every client one address | a connection limit in front, if the hub is attacked |
| **A certificate holder can lock out its own device's logins** for 15 minutes with five wrong passwords (per-certificate lockout by design); another device can still log in | nothing |
| **Notification text comes from boxes** (questions, messages). It is sent as plain text (no Markdown), but the ntfy app may turn URLs in it into links, so a box could put a misleading link into a notification body; the click target and the buttons always point to the hub | nothing (or strip URLs, if it matters) |
| **No debounce:** a session that flips between a notifying state and another quickly (needs-you, working, needs-you) causes publish/clear pairs; a question-less needs-you right after an answer is skipped | a short delay before publishing, if it is noisy in practice |
| **The hub reads its CA key** to sign a machine's certificate request at enrolment; before, the server process read only the CA certificate | nothing (the alternative: a foreground `enroll` that waits for the request) |
| **`set-password` keeps existing login sessions**; it says so and points to `devc hub sessions revoke --all` | nothing |
| ~~**Web app (C12)** must handle 401 with `"login": true`, send `X-Devc-Request: 1` and the Origin on state changes, follow `GET /api/me`, and live with the CSP (`default-src 'self'`, no inline script).~~ Done in C12 | — |

### From C12

| Item | Needs |
|---|---|
| **Nothing was tried on a real phone.** The app was checked in headless Chrome 154 (`web/check.sh`) at 390×844 with touch emulation and at 1280×800, light and dark, against the development mode and, in `TestWebApp`, against a real hub, machine and fake harness through a loopback proxy that presents the device certificate (a headless browser cannot hold one). Not tried: the phone's own browser (choosing the client certificate, the soft keyboard over the composer and over the terminal in control mode, `env(safe-area-inset-*)`), installing the app to the home screen (manifest, icons, maskable icon), opening a session from an ntfy notification, the service worker's offline start, Firefox and Safari | a phone with the device bundle and ntfy; Firefox |
| **Browser support of the terminal.** The CSP shims (`web/app/cspshim.js`) need constructable stylesheets (`document.adoptedStyleSheets`: Chrome 73, Firefox 101, Safari 16.4). An older browser shows the rest of the app; the terminal would lose its colours and layout | nothing (or a fallback, if an old browser matters) |
| **xterm.js on a phone in control mode**: typing goes through xterm's hidden textarea; whether a phone keyboard's autocorrect and composition (IME) deliver what was typed was not tried | a phone |
| **The terminal shows another window briefly** if the observed window is killed while its stream is open: tmux moves the observer's grouped session to another window of the box, and the stream ends within a second when the daemon sees the session end. It is the same box, but not the session that was asked for | nothing, or a tmux hook that detaches the observer |
| **A daemon that crashes** with a terminal open leaves the box's tmux client and its grouped session (named `devc-…`) in the box until the box restarts; a normal stop kills them | nothing |
| **Slow browsers lose the terminal**: the hub queues 512 output messages per stream and then ends it ("could not keep up") rather than slowing the pane; the page offers to reconnect | nothing |
| **Terminal size** follows the window by polling it every 5 s (`podman exec` of `tmux display-message`); a stream lives at most 12 hours | nothing |
| **`projects.list` asks every running box** for its worktrees (`devc box worktree list`, one `podman exec` and git status per worktree) at most every 20 s per project, four boxes at a time; a box with many worktrees makes the Projects screen slow | nothing, unless it is slow in practice |
| **Changes of a session whose worktree is unknown** (a harness started outside a worktree) are taken from main. The branch of a worktree comes from the box (or the remembered list); with a stopped box that was never listed, devc's convention (`<branch_prefix><name>`, main the default branch) | nothing |
| **The project screen** shows sessions, worktrees and requests as the work order says; the spec's "ports and data" are not there (no API for them yet) | C14 and later |
| **The development mode** (`devc hub serve --dev-no-auth`, hidden from the help) starts only with `DEVC_DEV=1`, on loopback, over plain HTTP, and keeps the Host check; its machines are invented in the process. It is still a server without authentication on the developer's machine while it runs | nothing (it is for UI work only) |
| **Vendored files** are checked against the npm tarballs' checksums recorded in `web/vendor/README.md`, not against signatures; the hooks module carries one edited import | nothing |
| **A config.toml that does not parse** (seen in the integration test with a second `[terminal]` table; a file read half-written would do the same) makes the running daemon reload a configuration without its hub list: it disconnects from its hubs until the file is fixed. This predates C12 | a reload that keeps the last good hub list, if wanted |


### From C12r1

| Item | Needs |
|---|---|
| **The race detector was not run** on the changed packages (`./dev` builds with CGO off). C1-19's fix (a copy of the receipt taken before dispatch) holds by construction; `failSave`, the test hook of the receipt store, is atomic | `go test -race ./internal/daemon/` in the toolchain image with cgo |
| **The hub and the web app still index sessions by bare ID** and send no `ref`; the daemon accepts that while IDs are unique (STATUS, C12r1, "Changed wire formats") | C12r2: send `ref`, key cursors by project and ID; then the daemon may require `ref` on hub connections |
| **Git's CPU and memory are not limited** for a box's receive-pack (C1-07 asked for transfer and storage budgets, which exist: `receive.maxInputSize`, the fed bytes, `git.relay_quota`). index-pack and fsck of a 2 GiB pack still use the host's cores and memory for a while | cgroup limits for the daemon's children, if it matters |
| **The relay budget walks the relay** at every push (`gitx.RelayUsage`); with `gc` off, loose objects accumulate and the walk grows with them | a periodic `git gc` of relays, or a cached size |
| **Transport budgets are guesses**: 256 new streams per 10 s (every hint and in-box client opens one), 32 control frames, 128 streams. A harness that makes more than about 25 hook calls a second for a sustained time would have its channel closed and reopened with backoff | measuring real harnesses |
| **A write stall of 2 minutes ends a raw stream**: an SSH session through the gate whose box-side client is suspended (Ctrl-Z) while output is pending loses its connection | nothing, or a longer stall for the gate |
| **Boxes started before C12r1** have no capability record: daemon-delivered secrets and the SSH gate are refused until the box is restarted | a restart per box after the upgrade |
| **Journals with one ID in two projects from before C12r1** stay; the owner is the earlier one, the other stops growing, and a bare ID is ambiguous until such a journal is removed by hand | nothing |

### From C12r2

| Item | Needs |
|---|---|
| **The race detector was not run** on `internal/hub` (`./dev` builds with CGO off). The link's new state (resync queue, held events, capacity) is touched only by the worker; tests change package variables (`refreshBytes`, `MaxMachineSessions`, `bodyTimeout`) before the hub that reads them starts | `go test -race ./internal/hub/ ./internal/daemon/` in the toolchain image with cgo |
| **The hub keys its store by machine and session ID.** It sends `ref` on every call and keys its cursor by project and ID; a duplicate ID in two projects of one machine (only from before C12r1) shows as the project the hub learned first, the other is not shown | nothing, unless legacy duplicates matter |
| **The daemon still accepts calls without `ref` from hubs.** The hub now always sends it | requiring `ref` on hub connections in the daemon, if wanted |
| **Hub and machines must be upgraded together**: a daemon from before C12r1 refuses the keyed cursor, so its link to a C12r2 hub fails | nothing (one binary) |
| **The web app's logic has no unit tests** (no JS test runner; no build step). Staleness from the machine map, the merge of held-back events, superseded snapshots and the pending commands are exercised by `web/check.sh` (the unknown-then-not-arrived prompt, Send again) and `TestWebApp` only in their common paths; a machine going offline while the inbox is open, a reset during a detail read and a lost detail answer were not driven in a browser | a JS test harness, or check.sh steps that disconnect an invented machine |
| **Caddy's logging rule was only validated** (`caddy validate`, Caddy 2.10.0), not run: Caddy writes no access log unless configured, and `log_skip` applies when one is. nginx was run (no access or error log line for an enrolment request) | a run of Caddy with a `log` directive in front of a hub |
| **Hand-over can notify twice**: a state that reached both hubs is pending on the second; if the first hub published it and then went away, the second publishes it too when it becomes notifier (by design: a duplicate rather than a loss). Pending records of a hub that never becomes notifier stay until their state ends | nothing |
| **Limits are this chunk's choices**: refresh 64 MiB and 2 minutes, resync queue 256 with 4 workers, 20,000 held events, body deadline 10 s, 64 requests without login, 8 logins waiting for hashing, backward pages of 1 MiB with 64 reads and 16 MiB, notification retries from 30 s to 15 min, clears given up after 7 days, 1,000 events per open session in the app | measuring real hubs |
| **A pending command is polled every 10 s** (receipt, wait 0) while its machine is online until it is final; a send queued behind a long turn is polled for as long as the turn runs | nothing, or a longer interval |

### From C13

| Item | Needs |
|---|---|
| **Whether each real harness honours the proxy variables is unverified.** Claude Code (Node.js), Codex (Rust), pi, Cursor CLI and OpenCode were not run at level allowlist; `NODE_USE_ENV_PROXY=1` only helps Node.js 24 and later, older Node programs that do not read the variables themselves reach nothing | a box of level allowlist with each harness logged in (presets `anthropic`, `openai`) |
| **Tools that ignore the proxy variables have no network at level allowlist, by design** (raw sockets, DNS lookups, `ssh` without a gate grant, programs with their own proxy settings) | nothing; documented in the guide and the sandbox description |
| **The presets are hand-written** from the tools' documentation and were not checked against real installs (pip, uv, npm, apt, CRAN mirrors, GitHub release downloads); a preset that misses a name shows up as a refusal in `devc net log --denied` | running each tool once behind the proxy |
| **Domain fronting through CONNECT** cannot be detected: TLS is relayed unchanged, so a server that answers for many names can be asked, inside TLS, for a name that is not on the allowlist | TLS interception, which devc does not do |
| **IPv6-only upstreams** are unreachable through the proxy (IPv4 only, like C07's rules) | IPv6 private-range rules, if wanted |
| **Cookies across local URLs**: pages at `*.localhost` share the parent name, so one box's page could set cookies for another project's local URL | nothing, or one router port per project |
| **The proxy log has no session**; the field exists | the agent mapping a connection to a process and its `DEVC_SESSION`, if wanted |
| **The router listens only on the address of `router.listen` at daemon start**; a change needs a daemon restart | a listener that follows reloads, if wanted |
| **Channel budgets were raised by reasoning, not measurement** (1024 new streams per 10 s, 192 streams; agent: 64 relays, 512 relay streams per 10 s); a tool opening more than about 50 connections a second through the proxy gets connections closed by the agent, not the channel | measuring package managers behind the proxy |
| **Exceptions on boxes started before C13**: `lan` needs a restart (old rules have no `lan4`); host exceptions need a restart (the old agent does not know `box.listen`) | a restart per box |
| **SELinux**: the proxy, the relays and the router add no file or socket a box can reach; not checked on an SELinux host | a run on Fedora |
| **The race detector was not run** on the new code (`./dev` builds with CGO off) | `go test -race ./internal/netpolicy/ ./internal/daemon/ ./internal/boxside/` in the toolchain image with cgo |

### From C14

| Item | Needs |
|---|---|
| **Every pull transfers the selected files in full**: the staging directory starts empty (the host may not read the data tree to compare), so rsync's delta transfer does not apply, except when a pull continues an interrupted one | a worker listing of the target (paths, sizes, times) turned into rsync filter rules, if large repeated pulls matter |
| **A continued pull also brings what the interrupted one received**, even files removed from the remote in between | nothing, or a fresh staging directory per path set |
| **A push decides by size and modification time** (rsync's quick check); the box controls both, so a file changed in place with its size and time restored is not sent. `--checksum` is not offered | a `--checksum` flag, if wanted |
| **Staged files are hard links**: a process in the box holding one open can change it during the push, and the content hash of `push --content` describes the bytes when they were staged. Documented in the guide | nothing (seal or stop the session first for a consistent copy) |
| **The box can block pushes** by creating `.devc-staging` itself; the push then refuses (fail closed) until the user removes it | nothing, or moving a squatted directory to the trash automatically |
| **The manifest is not rotated.** The context worker leaves files above 10 MiB out of its commits (about 30,000 records); a project that records that much stops committing its manifest | rotation or compaction of old records |
| **Only rsync 3.2.7 (host) was exercised**, also as the "remote" over SSH; older remotes, rsync 3.0/3.1 and BSD or macOS openrsync were not tried (`--protect-args`, `--info=progress2`, the itemized format) | runs against a cluster's rsync |
| **SSH**: the integration test pulls over a master with key authentication only; a push over SSH, password or second-factor prompts during the dry run or transfer, and the fallback from a master that dies between the check and the transfer were not exercised | a run against a real cluster |
| **Concurrent pull and push on one project** use separate staging directories and take the box lock for their worker phases; they were not run concurrently | a test, if it matters |
| **SELinux**: the helper containers and the host staging directory get no new labels; not checked on an SELinux host | a run on Fedora |
| **A pull trusts the remote's rsync.** The host's rsync writes only into the staging directory, but a hostile rsync on the remote (a cluster where an agent with a gate grant replaced it, say) could try receiver bugs such as CVE-2024-12087 to write elsewhere as the user; spec 14.5 already treats such a cluster as the user's own | keeping rsync current on the host (3.4.0 or a patched distribution package); the guide could say so |
| **The race detector was not run** on the new code (`./dev` builds with CGO off) | `go test -race ./internal/data/ ./internal/boxside/` in the toolchain image with cgo |

### From C15

| Item | Needs |
|---|---|
| **Nothing was tried against GitHub itself.** The REST paths, headers (`Accept`, `X-GitHub-Api-Version: 2022-11-28`, bearer tokens), the shapes of the answers devc reads (`number`, `html_url`, the jobs list, the installation, `token`/`expires_at`), job log redirects and the error format come from the documentation and were exercised only against the fake API of the integration tests | a run with a real token and a throwaway repository: `devc gh …` of every kind, `devc pr create`, `devc pr comment` |
| **The App's JWT carries the App ID as a number in `iss`**, the classic documented form; GitHub now also documents the client ID (a string). Not checked against GitHub, nor whether the installation lookup `GET /repos/{owner}/{repo}/installation` is the best one for organisation-owned repositories | a real App (creation and installation as described in the guide) |
| **Rulesets, App creation and installation** are described in the guide as steps for the user and were not walked through | a GitHub account |
| **`gh auth token --hostname <host>` as the fallback credential** was never run (the tests always use the realm secret and put a stub `gh` first in PATH that records any call); GitHub Enterprise hosts were not considered beyond the base URLs | a machine with `gh` logged in |
| **`password_expiry_utc`** is printed by the credential helper for minted tokens; git before 2.41 ignores it, later versions use it to drop an expired credential. Only git 2.43 (host) and the box image's git were used | nothing |
| **Rate limits**: the facade allows 120 reads and 120 token requests per minute per project; GitHub's own limits (5000 per hour for the user's token, separate budgets per installation) are shared with the user's other tools and were not measured | nothing, or tuning after real use |
| **`grants.github_read` repositories are matched by name only**; a repository renamed or transferred on GitHub keeps whatever the configuration says | nothing |
| **Existing boxes**: a box started before C15 has no `github` field in its capability record, so token modes refuse until it is restarted; its repository gets the remote `github` and the helper at the next start or `devc project init` | a restart per box |
| **The race detector was not run** on the new code (`./dev` builds with CGO off) | `go test -race ./internal/github/ ./internal/daemon/` in the toolchain image with cgo |

### From C16

| Item | Needs |
|---|---|
| **pi was never installed or run.** Assumed from the research report and pi's docs: `https://pi.dev/install.sh` runs under bash, honours `PI_CODING_AGENT_DIR` (set to the version directory's `home/.pi/agent` during the install) and puts the launcher at `<agent dir>/bin/pi`; `--version`, `pi auth login`, `pi auth check`, `--append-system-prompt <file>`, `-e <file>`, `--session <id>`, `PI_SKIP_VERSION_CHECK=1`, `PI_TELEMETRY=0`; sessions below `sessions/*/*.jsonl` | the optional network test (`DEVC_TEST_NETWORK_HARNESS=1`), then a real login |
| **pi's launcher may look for its release through `PI_CODING_AGENT_DIR`.** At launch that variable names the realm's shared agent directory, not the toolbox's version directory; if `bin/pi` resolves `install/releases/…` through the variable rather than its own location, it will not find the release (then point `install.exe` at the release's entry point, or set the variable only for the installer) | a run of the installed pi |
| **pi's self-update is not switched off**, only the version check (`PI_SKIP_VERSION_CHECK`). The installation is in the read-only toolbox, so `pi update` fails, but pi may write update state into the shared agent directory | a run of pi; `PI_SELF_UPDATE_COMMAND` if it helps |
| **pi has no hooks for devc**: no prompt, state or session events reach the journal, so the inbox shows a pi session only as live, `devc send` confirms delivery only through the extension (paste: "unknown"), and `launch.resume` never has pi's session ID | a hook kind for pi built as part of the extension (its `agent_start`/`agent_settled`/`input` events) |
| **pi's input extension was never loaded by pi.** Assumed: extensions are TypeScript loaded with jiti (`export default function (pi)`), `pi.sendUserMessage(text)` when idle and `{deliverAs: "steer" | "followUp"}` when busy, the events `session_start`, `session_shutdown`, `agent_start`, `agent_settled`, and that `node:net` reaches the abstract socket `"\0devc-agent"`. The agent side, the daemon adapter and the protocol are unit-tested with a stand-in client; interrupts and answers still go through the terminal | pi with `input.adapter = "extension"` in a user manifest |
| **Cursor CLI was never installed or run.** Assumed: `https://cursor.com/install` (bash) installs to `~/.local/share/cursor-agent/versions/<v>/` and links `~/.local/bin/cursor-agent` (a link with an absolute target stays valid, the installer runs at the in-box path), `--version`, `cursor-agent login` (with `NO_OPEN_BROWSER=1`), `cursor-agent status`, the login file `~/.config/cursor/auth.json`, `CURSOR_API_KEY`, `--resume <id>`, transcripts below `projects/*/agent-transcripts/` | the optional network test, then a login or an API key |
| **Cursor in a box**: its own sandbox needs unprivileged user namespaces, which the box's seccomp profile refuses, so it falls back to asking for approval; its background auto-update may write into the box home (`~/.local/share/cursor-agent`); its chats and worktrees (`~/.cursor/chats`, `~/.cursor/worktrees`) are shared with the rest of `~/.cursor` across the realm; "too many computers" limits apply to a shared login; `/tmp/cursor-agent-persist-*` (needed by `--resume`) is per box and not kept across restarts. Cursor gets no baseline or sandbox description (no instruction file outside the project) | a run of Cursor; `--sandbox disabled` or `sandbox.json` once confirmed |
| **OpenCode was never installed or run.** Assumed: `https://opencode.ai/v2/install` installs to `$HOME/.opencode/bin/opencode`, `--version`, `--session <id>`, the managed configuration `/etc/opencode/opencode.json` outranks every other layer and accepts `instructions` with an absolute path. Updates are not switched off (the `update` key was not confirmed; the toolbox is read-only, so an update fails). `~/.cache/opencode` lands in the realm's shared package cache (`~/.cache` is a realm mount). Its fixed service port may clash between two OpenCode sessions in one box | the optional network test, then a run |
| **The optional network test** now also installs pi (with node), Cursor and OpenCode and runs their `--version`; it was not run (vendor downloads) | `DEVC_TEST_NETWORK_HARNESS=1 ./dev itest -run TestHarnessNetwork -v` |
| **Node.js**: only the amd64 release was downloaded and run (24.22.3, in the integration test); the arm64 checksum is copied from the release's `SHASUMS256.txt` and not exercised. Old node versions are never removed (launch descriptions name the version directory) | an arm64 machine; a gc for toolbox components, if it matters |
| **`devc src` never talked to the real registries.** CRAN (`src/contrib/PACKAGES`, the archive layout), PyPI's JSON API (`urls[].packagetype == "sdist"`, `digests.sha256`), the npm registry (`/<name with %2F>/<version>`, `dist.integrity`) and GitHub (`/repos/o/r/commits/<ref>` with `Accept: application/vnd.github.sha`, `/tarball/<sha>` redirecting to codeload) were exercised only against local stand-ins | a box with network: `devc src r:jsonlite`, `pypi:requests`, `npm:@types/node`, `github:cran/git2r@0.36.2` |
| **Installed versions** are found through `R_LIBS*`, `Rscript` (which also sees renv) and the usual library directories; `VIRTUAL_ENV`, `CONDA_PREFIX` and `python3`'s `sys.path`; `node_modules` upwards from the working directory and `NODE_PATH`. pipx/uv tool environments, npm's global prefix and Python packages installed from VCS (`direct_url.json`) are not used; GitHub Enterprise remotes in DESCRIPTION fall back to CRAN | nothing, or more finders when needed |
| **The source cache is never pruned** and is writable by every box of the realm (documented); two concurrent fetches of the same version both download, one wins the rename | deleting `cache/realms/<realm>/src` by hand, or a prune command |
| **Share versions are kept until `devc share rm`**; there is no gc of old versions, and boxes see old versions below `/opt/shares/.v/<name>/` (documented). Shares are not yet part of state export or sync (C17) | C17 |
| **`.gitattributes` in a published tree is honoured by `git archive`**: entries marked `export-ignore` are listed in the preview (from `git ls-tree`) but not published; a box can only make a share smaller that way | nothing |
| **SELinux**: the share and source mounts get no labels of their own (the share directories are devc's, read-only); not checked on an SELinux host | a run on Fedora |
| **Existing boxes**: every box definition gained the `/src` and `/opt/shares` mounts, so boxes created before C16 report drift (`devc up --recreate`) | a recreation per box |
| **The race detector was not run** on the new code (`./dev` builds with CGO off) | `go test -race ./internal/extract/ ./internal/shares/ ./internal/depsrc/ ./internal/boxside/ ./internal/daemon/` in the toolchain image with cgo |

### From C17

| Item | Needs |
|---|---|
| **Credential file names of Cursor** (`~/.config/cursor/auth.json`, `~/.cursor/auth.json`) and pi (`~/.pi/agent/auth.json`) are assumed, not observed; Codex's `~/.codex/auth.json` is documented by OpenAI. A harness that keeps its login elsewhere would have it archived | a login with each harness, then `find` in the realm's harness state |
| **SELinux**: the archive worker's helper container gets the realm's `label=level:` like the data workers; not run on an SELinux host | a run on Fedora |
| **Uploads are rate-limited but not niced**: `git bundle` of a large relay and the helper container run at normal CPU and I/O priority | `nice`/`ionice` for the daemon's archive children, if it matters in practice |
| **Restores stage whole bundles** in `local/tmp/` before fetching them, and `state import` extracts the whole export first, so a restore needs up to twice the archive's size on disk | streaming, if large relays make it a problem |
| **The hub counts a machine's usage by walking its archive** once per start (and after a prune); a machine with the maximum 200,000 files makes its first upload after a restart wait for that walk | nothing, or a usage table in the database |
| **Shares are not archived or exported** (`durable/shares/` is not part of a realm's durable state, section 1 of the work order); they are published again from their relays | a decision whether shares belong to the fleet state |
| **Restored journals** are shown as sessions of the restoring machine and archived again under its name; a restored session that was live on the lost machine is ended as lost by reconciliation | nothing (documented) |
| **`--merge` into the context repository** keeps the local `main`; the archived history is fetched but only reported when `main` differs (no merge commit) | merging notes and memory of two machines, if wanted |
| **Hub-side checks of bundles**: the hub stores bundles without looking inside; a hostile machine can archive garbage under its own name (bounded by its quota), which a restore then refuses at `git fetch` (fsck) | nothing |
| **The race detector was not run** on the new code (`./dev` builds with CGO off) | `go test -race ./internal/fleet/ ./internal/hub/ ./internal/daemon/` in the toolchain image with cgo |

### From C18

| Item | Needs |
|---|---|
| **No real service was contacted.** The proxy, the login and the example's `login.sh` ran only against the test upstream (Go's TLS server, which speaks HTTP/2). Real login forms (CSRF tokens, redirects through an identity provider), other servers' framing and HTTP/2 implementations were not tried | a real intranet or wiki behind a password |
| **The cookie integration test reaches the upstream through the host's private LAN address** from the login container (rootless Podman with slirp4netns here). With pasta networking the container may share the host's address, so that address can mean the container itself; the test skips only when the host has no private address | a run on a pasta host; `host.containers.internal` if needed |
| **Body reflection**: an upstream endpoint that echoes request headers in its body shows the credential to the box (documented; only headers are scrubbed) | nothing, or a body scrubber for small text responses if wanted |
| **Second factors and browser logins** are not supported by devc itself: the login runs without standard input, and no browser is shipped (notes in `examples/services/README.md`) | a user-built `auth.image` with a browser |
| **No hub notification** when a service needs a login: the note lands in the live sessions of the box that met the need (none when it has no live session), the daemon's log and `devc service ls` | an inbox item or ntfy message, if wanted |
| **SELinux**: the login container bind-mounts a scratch directory below `local/tmp` like the harness logins (no relabel; `label=disable` only in mode `disable`); not run on an SELinux host | a run on Fedora |
| **Existing boxes**: a box started before C18 has no services in its capability record and an agent without `box.services` | a restart per box that gets a service |
| **The race detector was not run** on the new code (`./dev` builds with CGO off) | `go test -race ./internal/svclogin/ ./internal/daemon/ ./internal/boxside/` in the toolchain image with cgo |

### From C19

| Item | Needs |
|---|---|
| **Android emulator** (work order C19, out of scope): the emulator needs `/dev/kvm`, which this host (a KVM guest without nested virtualisation) does not have | a host with KVM; a separate work order |
| **No real browser was watched.** The tests drive the display with devc's own X handshake (`devc box xcheck`), xdpyinfo, xclock and xeyes (by hand), and `devc browser` with a fake DevTools endpoint; Chromium in a box (headed on the display, and `chrome://inspect` on the host against it) was not tried, nor noVNC in a real browser | Chromium in a test image; a look at the viewer in a browser and on a phone |
| **SELinux**: the sidecar repeats the box's `label=` options (`level:` or `disable`) and joins its namespaces; whether an SELinux host lets a second container with the same MCS level join the box's network and user namespaces was not run | a run on Fedora |
| **pasta networking**: the sidecar joins whatever namespace the box has; tested with slirp4netns only | a run on a pasta host |
| **The image is large** (about 470 MB): Debian's TigerVNC server pulls in Mesa and LLVM through libgl1, websockify needs NumPy, and the server's scripts need Perl | a slimmer base or a statically built X server, if it matters |
| **Stale sidecars** of a box stopped outside devc keep running (in the old, now unconnected network namespace) until `devc gui down`, `devc down`, `devc rm` or the next start of the box removes them; `devc gui status` reports them as stale | nothing (documented), or a daemon sweep |
| **`devc run` does not get `DISPLAY`** (only agent sessions through `devc box launch` and the login shell of `devc sh` read `/etc/devc/display.env`); scripts can source the file | nothing, or host-side `--env` for `devc run` while a desktop runs |
| **Clipboard**: the viewer's clipboard reaches programs in the box and back (TigerVNC's defaults); documented as visible to the box, not switched off | `-SendCutText=0 -AcceptCutText=0` if it should be |
| **The race detector was not run** on the new code (`./dev` builds with CGO off) | `go test -race ./internal/gui/ ./internal/boxside/` in the toolchain image with cgo |
