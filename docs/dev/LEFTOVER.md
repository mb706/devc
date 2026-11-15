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
| Codex manifest values (`exe` path, `/etc/codex/config.toml` as the baseline location, per-box directory names) are taken from the work order, not checked against an installed Codex | a Codex install (no login needed) |

### From C02

| Item | Needs |
|---|---|
| SELinux box options (`label=level:<realm level>`, `label=disable`), `restorecon -R` on the project directory during `up`, and the warning when `restorecon` is missing are tested only with fakes | a Fedora machine, enforcing |
| Whether boxes start on an SELinux host in mode `labels` with the file-context rule in place (no relabelling, `/etc/devc`, toolbox and runtime mounts readable as `container_file_t`) | a Fedora machine, enforcing |
| Seccomp: the s390 variant of the `clone` rule and the profile on arm64 are unit-tested only; the high-bit AF_ALG block was verified on amd64 only | s390x / arm64 hosts |
| Seccomp gap: on amd64 a 32-bit (i386) program can still create AF_ALG sockets through `socketcall(SYS_SOCKET, …)`, whose arguments live in memory where seccomp cannot see them. Closing it means refusing `socketcall` (breaks old 32-bit binaries) or dropping the x86 sub-architecture | a decision |
| Old `toolbox/devc/<id>` builds are never removed; they need a garbage collector that knows which boxes still run an old binary | later chunk (with harness gc in C03) |
| `build_from_repo` fails with "not supported yet" | a later chunk |
| Host proxy variables are deliberately not passed into boxes (`--http-proxy=false`); machines behind a proxy need a config option for it | a decision |
| The nested-mount-point check (`/home/dev/.cache`, later harness and data mounts) runs on every start through devc; a box started with plain `podman start` skips it, and it does not cover a project directory that another running box mounts read-write (relevant for C06 data zones) | C06 design |
| `box fix-identity` runs after every start through devc only; it persists in the container's layer, so a restart outside devc keeps it, but a recreated container outside devc would not have it. `/etc/shadow` is not updated | nothing, unless tools in boxes need shadow entries |
| Host bug on the development machine: `statx()` requesting the birth time on files in an image layer hangs forever in fuse-overlayfs 1.17 with libfuse 3.18.2 (`~/.local/opt/workstation-tools`). It breaks `apt-get update` on Debian trixie (sqv) and `stat FILE` inside boxes; the integration tests build their env from bookworm. `devc doctor` could detect it with a probe container that runs `stat` under a timeout; the system fuse-overlayfs (`/usr/bin`, 1.13) was not tried | a decision (doctor check), or a fixed fuse-overlayfs on the host |
| The example `base` env (Debian trixie) has not been built on a healthy host | a host without the fuse-overlayfs bug |
