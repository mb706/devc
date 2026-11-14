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
