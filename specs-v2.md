# devc v2 — specification

Draft 1, 2026-11-14. Nothing here is implemented yet.

Markers used below:

- **[tested]** — reproduced on Ubuntu 24.04 with rootless Podman 5.8.3 during the design work.
- **[verify]** — rests on documentation or a short probe; needs a test before code depends on it.

Background research (twelve reports) is in `../v2-research/`.

## 1. Purpose

devc runs AI coding agents (Claude Code, Codex, pi, others) inside sandboxed containers on one developer's fleet of Linux machines, and gives that developer one interface to all sessions.

Goals:

- **Isolation.** An agent's mistakes, a malicious dependency or a prompt injection stay inside a box.
- **Persistence.** Logins, history, memory and work survive container recreation, image changes and wiped machines.
- **Ratchets.** A box can add work but cannot take away what is already banked.
- **Harness independence.** Adding a harness means writing a manifest, not code.
- **A disposable fleet.** A new machine is usable after four commands; UIDs and installed software differ between machines.
- **Offline use.** Everything except model inference, package installation and sync works without a network.
- **Explicit host requirements.** `devc doctor` lists them; nothing is assumed silently.

Non-goals:

- Multi-user operation, macOS, Windows.
- An agent harness of its own.
- Data provenance, pipelines, or integration with job schedulers.
- Defending against kernel exploits. Boxes are containers; a VM-backed realm is a later option.

## 2. Security rules

| | Rule |
|---|---|
| R1 | **Policy lives outside the box.** Mounts, network, grants and capabilities are defined in host-side config that no box can write. Image content may come from the project repository if the project opts in; runtime policy never does. |
| R2 | **The host does not execute or trust box-writable content.** No git, R, editor integration or build tool runs on the host inside a project directory. Host-side file operations on box-writable trees run in a throwaway helper container that sees only that tree. |
| R3 | **Ratchets.** Commits, sealed artifacts and transcripts are banked where the box cannot write. Nothing leaves a machine except through a host-side command. |
| R4 | **Capabilities, not credentials.** Where possible a box gets the use of something (a relay, an SSH connection) rather than the secret behind it. |
| R5 | **The realm is the sharing boundary.** Boxes in a realm share login, history, memory and caches. Nothing writable crosses realms. |
| R6 | **Enforcement sits outside the box.** Network rules, sealing and branch policy are applied by the host and cannot be changed from inside. |

Accepted risks:

- The kernel is shared; patch lag on the host is the dominant container risk.
- The model credential (Claude token, Codex login) is visible to the agent.
- With open internet, anything the agent can read it can send somewhere.
- The hub is a trusted intermediary. It sits behind client certificates, and high-risk approvals are not accepted through it (section 13).

## 3. Concepts

| Term | Meaning |
|---|---|
| Machine | A host running devc: workstation, laptop or server. |
| Realm | A trust domain such as `work` or `private`. |
| Project | A git repository plus optional data, belonging to one realm. |
| Env | An image recipe (Containerfile plus recorded packages). |
| Box | The container for one project. |
| Worktree | A checkout inside the project directory: `main` or a task. |
| Session | One agent (or shell) running in a tmux window in a box. |
| Relay | A host-side bare repository per project, between the box and GitHub. |
| Gate | A host-side endpoint giving boxes use of an authenticated SSH connection. |
| Zone | A part of a project's data directory with its own protection. |
| Toolbox | A read-only directory of harness binaries and helpers mounted into every box. |
| Machine daemon | A per-machine user service: journal, relay, gate, forwards. |
| Hub | An always-on server: inbox, web app, notifications, archive. |

## 4. Components

One static Go binary, `devc`, in several roles:

- **CLI** on the host. All lifecycle operations work without any daemon.
- **Machine daemon** (`devc daemon`, a systemd user service). Phase 2.
- **Hub** (`devc hub serve`). Phase 2.
- **In-box helper**, the same binary mounted from the toolbox: init, launcher, status wrapper, hook receiver, git remote helper.

Plus two kinds of auxiliary container:

- **Helper container**: started from a bare directory containing only the static binary, with one project mounted and no network. Used for sealing, savepoints and moving received data **[tested]**.
- **Gateway container** per realm, for network policy. Phase 3.

```
phone: browser + ntfy app
        │ HTTPS, client certificate
        ▼
hub (server): inbox, web app, ntfy, archive
        ▲ outbound TLS from each machine
machine daemon: journal, relay, gate, forwards
        │ channel opened from the host side
box: tmux → launcher → claude | codex | pi
```

## 5. Host layout

```
~/.config/devc/                        config, in git, no secrets
  config.toml                          global settings, hub list
  baseline/<harness>/                  settings and instructions applied in every box
  dotfiles/                            shell and tmux config linked into every box home
  harness/<id>.toml                    user-defined harness plugins
  envs/<env>/Containerfile, env.toml   image recipes
  realms/<realm>/                      may be its own git repository
    realm.toml
    projects/<project>/project.toml

<state>/                               default ~/.local/share/devc
  durable/realms/<realm>/
    harness/<id>/                      shared harness state
    context/                           git repo: notes/<project>/, memory/<group>/, data/<project>/manifest.jsonl
    relay/<project>.git
    journal/<project>/<session>.jsonl
    secrets/                           0600 files unless another backend is configured
  durable/node/                        this machine's hub certificate
  local/boxes/<realm>/<project>/home/  box home
  local/savepoints/                    for savepoint drivers that need a repository
  cache/realms/<realm>/pkg/            package caches
  toolbox/                             harness binaries, tmux, helper rootfs

<projects_root>/<realm>/<project>/     box-writable; the host runs no tools here
  .repo/                               git directory
  main/                                main worktree
  <task>/                              linked worktrees
  data/                                data zones
  .devc/                               owned by the vault UID: savepoints/, trash/
```

Three state classes:

- **durable**: synced to the hub and worth backing up. It consists of append-only logs and git repositories, so a filesystem snapshot or a plain copy is consistent.
- **local**: persistent on this machine, not synced.
- **cache**: safe to delete.

Sockets live in `$XDG_RUNTIME_DIR/devc/<box-id>/`, because Unix socket paths are limited to 108 bytes.

## 6. Boxes

### 6.1 Identity

Every box runs as user `dev`, UID and GID 1000, home `/home/dev`, whatever the host UID is. A second in-box UID, 2000 ("vault"), owns sealed data. On the host it appears as a subordinate UID.

### 6.2 Creation flags

```
podman create --name devc-<realm>-<project> --hostname <project> --init
  --userns=keep-id:uid=1000,gid=1000 --user 1000:1000
  --passwd-entry 'dev:*:1000:1000:dev:/home/dev:/bin/sh' --env HOME=/home/dev
  --cap-drop=ALL --security-opt=no-new-privileges
  --security-opt seccomp=<toolbox>/seccomp.json
  --security-opt label=level:<realm level>         (SELinux hosts)
  --pids-limit=<n> --memory=<n>
  --network=<per section 11>
  --label devc.realm=… --label devc.project=… --label devc.confighash=…
```

- The seccomp profile is Podman's default minus user-namespace creation and `AF_ALG` sockets. Blocking nested user namespaces removes the largest kernel attack surface **[tested for the namespace part on Ubuntu; verify on Fedora]**.
- Harness sandboxes are off; every harness runs in its bypass-permissions mode. Chromium inside a box needs `--no-sandbox`.
- `devc.confighash` lets `devc up` detect that the definition changed and offer to recreate the box.

### 6.3 Mounts

| In the box | Source | Mode |
|---|---|---|
| `/w/<realm>/<project>` | project directory | rw |
| `/home/dev` | box home | rw |
| harness shared paths | realm harness state (section 8) | rw |
| `/home/dev/.cache` | realm package cache | rw |
| `/ctx/notes`, `/ctx/memory` | realm context repository | rw |
| `/opt/devc` | toolbox | ro |
| harness baseline paths, `/etc/devc` | rendered baseline, sandbox description, dotfiles | ro |
| `/run/devc` | runtime directory | rw |
| data input zones | nested over the project mount | ro |

Never mounted: the host home, SSH keys or agent sockets, the Podman socket, another realm's anything.

The in-box path is fixed, so sessions and worktrees are valid on every machine. On the host the project sits under `projects_root`, and its git directory is referenced by the in-box path, so host-side git reports "not a repository". Creating a `/w` symlink on the host re-enables host git and gives up R2 for git.

### 6.4 Lifecycle

- `devc up` creates or starts the box; `devc down` stops it; `devc rm` removes the container but keeps home and state.
- `devc rebuild` builds the image and recreates the container. Home, state and project are untouched; running sessions end and can be resumed.
- `devc root -- <cmd>` runs a command as root with full capabilities from the host (`podman exec --user 0 --privileged`) on an otherwise hardened box **[tested]**. Commands are logged in the env's `env.toml` so they can be folded into the Containerfile.

### 6.5 Image contract

A glibc-based image with `sh`, `git` and an OpenSSH client. Everything else devc needs comes from the toolbox: the helper, a static tmux, harness binaries, a Node runtime for harnesses that need one.

### 6.6 Capabilities

Off by default, switched on per project, each documented as weaker isolation: `gpu` (NVIDIA CDI device), `nested` (rootless Podman inside the box), `kvm`.

## 7. Toolbox and harness plugins

### 7.1 Toolbox

`<state>/toolbox/` holds one directory per harness version and a `current` symlink each. `devc update <harness>` runs the vendor installer in a throwaway container with the toolbox writable, then swaps the symlink. Running sessions keep their binary; old versions are removed only when no box uses them. In-box updaters are disabled.

### 7.2 Plugin manifest

Built-in manifests ship in the binary; files in `~/.config/devc/harness/` add or override.

```toml
id = "claude"
bin = "claude"

[install]
script = "https://claude.ai/install.sh"

[home]
strategy = "private"                    # private | shared
env = { CLAUDE_CONFIG_DIR = "~/.claude" }
shared = ["projects/"]                  # directory mounts from realm state
seed = "claude.json.tmpl"               # onboarding and trust flags

[auth]
method = "env-token"                    # env-token | shared-home | api-key
login = ["claude", "setup-token"]
env = "CLAUDE_CODE_OAUTH_TOKEN"

[baseline]
settings = "/etc/claude-code/managed-settings.json"
instructions = "/etc/claude-code/CLAUDE.md"

[memory]
setting = "autoMemoryDirectory"

[launch]
resume = ["--resume", "{id}"]

[status]
osc7501 = true
hooks = "claude-hooks.json"

[input]
adapter = "mod"                         # mod | app-server | extension | paste
submit_key = "Enter"
```

### 7.3 Launcher

Each harness binary name resolves to a devc shim first in `PATH`. The shim fetches the credential, applies launch settings, registers the session and starts the real binary. Sessions are therefore registered whether started by `devc claude` on the host or by typing `claude` in a box shell.

## 8. State

### 8.1 Sharing strategy

Harness homes mix durable and per-machine state, so whole homes are not shared by default.

| Harness | Home | Shared across the realm | Login |
|---|---|---|---|
| Claude Code | private, on the box home | transcript directory | one-year token from `claude setup-token`, per realm |
| Codex | shared | whole home except daemon and socket directories, which stay per box | device-code login per machine and realm |
| pi | shared | whole home | per provider |
| OpenCode | private | nothing | per box |

- A Claude token does not refresh, so it is safe to use from many boxes and many machines **[verify: revocation, usage display]**.
- Different realms may use different accounts.
- The baseline sets Claude's transcript retention far above its 30-day default.

### 8.2 Baseline and instructions

`~/.config/devc/baseline/<harness>/` is rendered into the harness's managed-settings location and mounted read-only. It applies in every box and the agent cannot weaken it. Each box also gets a generated description of its sandbox: paths, what is banked automatically, how to request things.

### 8.3 Context

The realm's context repository holds what agents may write but collaborators should not see:

- `notes/<project>/`, mounted at `/ctx/notes`, injected through each harness's own mechanism;
- `memory/<group>/`, mounted at `/ctx/memory`. Projects naming the same `memory_group` share it. Claude's `autoMemoryDirectory` points there **[verify: whether it appends a per-project subfolder]**.

The host commits this repository periodically, so memory can be reviewed and reverted.

## 9. Git

### 9.1 Layout in the box

The project directory holds `.repo/` (git directory), `main/` and one sibling directory per task worktree. `devc wt new <name> [--from <ref>]` adds a worktree and branch; Claude Code's own `--worktree` is redirected to the same layout through its `WorktreeCreate` hook **[verify]**.

Box git config sets `gc.auto=0` and `maintenance.auto=false`. The sandbox description tells agents to commit work in progress rather than stash, because the stash is shared across worktrees.

### 9.2 Relay

One bare repository per project in realm state. The box sees it as `origin`.

| Refs in the relay | Written by | Meaning |
|---|---|---|
| `refs/heads/*` | the box, and host sync from GitHub when it can fast-forward | what the box sees as `origin/*` |
| `refs/remotes/github/*` | host only | GitHub as last fetched |
| `refs/devc/bank/<branch>/<time>` | host only | every tip a branch ever had |
| `refs/devc/snap/<worktree>/<time>` | host only | snapshot commits of uncommitted work |

Policy on updates from the box, enforced on the host:

- branches may be created and updated, including non-fast-forward;
- deletions are refused;
- tags may be created but not moved;
- `refs/devc/*` and `refs/remotes/*` are refused;
- branches listed as `protected` in the project definition are refused.

Because every tip is recorded under `refs/devc/bank`, an amend, reset or deleted clone loses nothing.

### 9.3 Movement

| Command | Direction | Notes |
|---|---|---|
| `devc bank` | box → relay | Fetches all branches and a snapshot commit per worktree. `git-upload-pack` runs inside the box, so no box config or hook executes on the host **[tested]**. Snapshot commits skip files above a size limit. |
| `devc pull` | GitHub → relay → box | The host fetches with its own credentials and writes remote-tracking refs into the box. |
| `devc push [branch]` | relay → GitHub | Shows ahead/behind and a diffstat first. |
| automatic forward | relay → GitHub | Only for patterns listed in `git.forward`, default none. |
| `devc log`, `devc diff` | read the relay | Safe on the host: bare repository, host-controlled config. |

Phase 1 is host-driven: banking runs on `devc push`, `devc down` and a timer. From phase 2 the daemon banks after every agent turn, and a git remote helper lets `git push` and `git fetch` inside the box reach the relay directly. The relay is always served through a host process, never mounted.

### 9.4 Adding projects

`devc project add <realm>/<name> --from <url>` fetches into a new relay and populates the box repository from it. `devc project adopt <path>` does the same from an existing clone and leaves the original untouched.

## 10. Credentials and brokered access

### 10.1 GitHub

Default: the box holds no GitHub credential.

- **Push and fetch** through the relay (section 9).
- **Reading CI runs, issues and pull requests** through a host-side read-only facade limited to the project's repositories. Phase 3.
- **Opening a pull request or commenting** through host commands the agent can request. Phase 3.

Optional, per project: a token handed into the box.

- **GitHub App installation token**: one repository, chosen permissions, one hour, bot identity. The App starts as "only on this account" on the personal account. For an organisation, prefer an App owned by that organisation over making the personal App public. Never grant the `workflows` permission.
- **Fine-grained PAT**: the fallback where no App can be installed.

### 10.2 SSH gate

Phase 2.

- `devc ssh login <host>` authenticates on the host, including 2FA, and keeps a master connection in a directory no box can reach.
- A box with a grant for that host runs an ordinary OpenSSH client. Its connection ends at a gate on the host (`sshd -i` with a forced command and a per-box key), which runs the command over the master **[verify the combination]**.
- The grant is full use of the account: no per-command approval.
- Port forwarding is disabled at the gate. A bind-mounted control socket would allow forwards into the host's namespace, which is an escape **[tested]**.
- Every command is logged. Closing the master revokes access.
- An optional read-only profile restricts a grant to an allowlist of commands.

### 10.3 Other secrets

`devc secret set <realm> <NAME>` stores a value; a project grant delivers it to that project's sessions at launch. Such secrets are visible to the agent and should be scoped accordingly. Storage defaults to 0600 files under realm state; `pass` and systemd-creds are optional backends.

### 10.4 Later

Service logins (section 17) and cloud credentials through `credential_process`.

## 11. Network

| Level | Meaning |
|---|---|
| `none` | no network |
| `allowlist` | listed domains only, through a proxy |
| `internet` | internet, but no LAN, no host services, no link-local or metadata addresses. **Default.** |
| `open` | Podman's default networking |

- **Phase 1**: `internet` is implemented by firewall rules that the host installs into the box's network namespace before the first process starts **[tested]**. They do not survive a restart done outside devc, so `devc up` always reapplies them and checks from inside that a private address is unreachable.
- **Phase 3**: boxes sit on an internal network per realm, and a gateway container forwards and filters. This fails closed. `allowlist` adds a domain-filtering proxy with no resolver in the box.
- **Exceptions**: `devc net allow <box> <host:port | lan | host:port-on-host> --for <duration>` adds a firewall entry with built-in expiry.
- **Ports**: `devc forward <port>` listens on host loopback and dials `127.0.0.1:<port>` inside the box's network namespace **[tested by the research pass]**. It reaches services bound to loopback and needs nothing in the image. Later, the daemon serves `http://<port>.<project>.<realm>.localhost`.

IPv6 is disabled in boxes.

## 12. Sessions

### 12.1 Model

A session is one harness process in a tmux window in a box, bound to a worktree, with a stable ID. One tmux server per box. `devc attach` attaches a terminal; from another machine, `ssh <machine> -t devc attach`.

### 12.2 State and content

| Source | Provides | Phase |
|---|---|---|
| Harness hooks | prompt submitted, question asked, final message, session start and end | 1 |
| OSC 7501 status reports, answered by a small PTY wrapper around the harness | `idle`, `working`, `blocked`, `done`, `error` | 2 **[verify wrapper transparency]** |

Hooks are installed through the baseline, so the agent cannot remove them.

### 12.3 Journal

Hooks write events to a spool in the box's runtime directory. The host copies them into `journal/<project>/<session>.jsonl`, which the box cannot write. Each event has a sequence number, time, session ID, type and data. The machine's journals are the source of truth; the hub holds projections.

### 12.4 Commands

`send` (queue or steer), `answer`, `interrupt`, `keys`, `start`, `stop`, `seen`, `attach`. Each carries a client-issued ID and produces a stored receipt, so a retry never types twice. Receipt outcomes: `delivered`, `typed-not-submitted`, `unknown`, `refused`.

Delivery, in order of preference:

1. the harness's own interface for posting a user message into a live session: a Claude Code plugin, the Codex app-server with the TUI attached, a pi extension **[verify each in a box]**;
2. a bracketed paste into tmux, a separate submit key, and confirmation through the prompt-submitted hook.

Nothing is injected while a human is typing in that terminal.

### 12.5 Channel between box and host

The in-box helper listens on a Unix socket in the runtime directory and the host connects to it. That direction works under SELinux without a policy module **[verify on Fedora]**. The box never gets a socket that can control the daemon.

## 13. Hub

Phase 2.

- **Reachability.** Machine daemons dial out over TLS on port 443. Machines need no inbound port and no VPN.
- **Authentication.** The reverse proxy in front of the hub requires a client certificate from a small private CA before any HTTP is spoken. Inside that, devices are paired and use a passkey.
- **Role.** Stores the latest projection per session and a bounded event log, derives the inbox, serves the web app, publishes to a self-hosted ntfy server, and receives the archive of durable state.
- **Notifications.** One ntfy message per event that needs attention: the question as body, up to three buttons (the first two answers and Open), a link to the session. It is cleared when answered elsewhere.
- **Web app.**
  - Home is an inbox across machines: needs you, failed, finished and unseen, working, idle. Machine, realm and project are filters.
  - A session view has the pending question, a transcript with tool calls folded, a reply box with queue, steer and stop, an observing terminal, and a changes tab.
  - A project view has branches against relay and GitHub, requests, ports and data.
  - New work starts from one draft screen.
- **Several hubs.** A machine may list more than one hub and connects to all of them. Each hub builds its own view from the machines' journals, so no hub-to-hub replication is needed for live state. The first reachable hub in the list sends notifications. Receipts live on the machine, so a command sent through two hubs is applied once.
- **Offline.** A machine without a hub connection works fully and syncs when it reconnects. Sessions belong to one machine, so journals do not conflict; the context repository merges through git.
- **Exposure.** A compromised hub could read mirrored transcripts and inject prompts into online sessions. The daemon therefore accepts only box-scoped commands from the hub: no host shell, no policy change. Signing commands with a key held by the web app would not help, because the hub serves the web app's code; that idea is dropped (`docs/dev/plan.md` section 5).

Requests are the agent's way to ask for something outside its box: a package, a network exception, a seal, a pull request. They appear in the inbox; approving one applies and records it. Each request has a risk class:

- **Low risk** (a seal, a savepoint, restarting a session): may be approved through the hub.
- **High risk** (a network exception, a new grant, a root command, a push to GitHub): approved only with the CLI on a machine, until a remote approval that a compromised hub cannot forge is designed and agreed.

## 14. Data

devc protects and moves data. It does not track provenance or integrate with job tools.

### 14.1 Zones

A project's data directory is shared by all worktrees and divided into zones in the project definition.

| Zone | In the box | Protection |
|---|---|---|
| inputs | read-only mount | cannot be changed from the box |
| work | writable | savepoints |
| sealed | writable parent, sealed children | sealing |

### 14.2 Sealing

`devc seal <path>` hands a finished directory to the vault UID and removes write permission; the zone directory is sticky and vault-owned. From inside the box, delete, overwrite, `chmod` and rename all fail, while new directories can still be created beside it **[tested]**. The host user can read sealed data. `devc data unseal` and `devc data rm` (which moves to a dated trash in `.devc/`) are host-only. From phase 2 an agent can request a seal; it can never undo one.

### 14.3 Savepoints

`devc save [name]`, and automatically before a session starts if configured. A savepoint records the current git commit.

| Driver | Where | Cost |
|---|---|---|
| `reflink` | btrfs or XFS project storage | instant; no space until files diverge |
| `borg`, `restic` | any filesystem | first run copies everything; later runs are incremental |

Hard-link copies are not offered: they do not survive overwriting in place. `devc restore <savepoint> [path]` restores all or part.

### 14.4 Sync

Phase 3. `devc data pull <remote> [paths]` and `devc data push` wrap rsync with safe defaults: no deletion, overwritten files kept in a backup directory, preview first. Received data lands in a staging directory outside box reach and is moved into place by the helper container. The project definition says which paths each machine normally holds. A manifest in the context repository records path, size, checksum and which machines hold each artifact.

### 14.5 Clusters

On a cluster the agent acts as the developer, so devc can only help against accidents: seal finished runs with `chmod -R a-w`, keep results that matter on backed-up storage rather than scratch, and pull post-processed data off the cluster.

### 14.6 Other tools

git-annex, DataLad and `targets` remain usable. Their object stores can be declared as zones so they are sealed or included in savepoints.

## 15. Command overview

```
devc init <config-repo>            devc doctor [--fix]             devc join <hub>
devc realm add|ls                  devc project add|adopt|ls       devc env build|ls
devc up|down|rm|rebuild|ls         devc sh        devc run -- …    devc root -- …
devc login <realm> <harness>       devc update [harness]           devc secret set|ls|rm
devc claude|codex|pi [-w task]     devc attach    devc send        devc answer
devc wt new|ls|rm                  devc bank|pull|push|log|diff
devc seal    devc save    devc restore    devc data ls|rm|unseal|pull|push
devc forward <port>                devc net allow|ls               devc ssh login|ls|close
devc daemon                        devc hub serve                  devc state export
```

The project is inferred from the current directory when omitted.

## 16. Host requirements

Checked by `devc doctor`; `--fix` prints or applies the root commands.

- Rootless Podman 5.x or 6.x with a working network backend; subordinate UID and GID ranges of at least 65,536.
- User lingering enabled, or boxes die at logout.
- `nsenter` and `nft`.
- SELinux hosts: a file-context rule labelling the projects root and state directory as container files, so mounts need no relabelling. Files moved in from elsewhere keep foreign labels; `devc up` repairs them. Optionally one category pair per realm.
- Ubuntu with the AppArmor user-namespace restriction: a profile for the Podman binaries if they are not at the distribution's paths.
- For the gate: an `sshd` binary.
- For `reflink` savepoints: project storage on XFS or btrfs. For `borg` or `restic` savepoints: nothing extra, the tool runs in a container.
- Optional: KVM, NVIDIA CDI.

## 17. Phases

1. **One machine, CLI only.**
   - Config, env build, box lifecycle, hardening, SELinux and AppArmor handling, `doctor`.
   - Toolbox, harness plugins for Claude Code and Codex, launcher, `login`, baseline, context.
   - tmux sessions, hooks, journal, `ls` with session state, `attach`.
   - Project add and adopt, worktrees, relay, `bank`, `pull`, `push`, snapshot commits.
   - Zones, sealing, savepoints, trash.
   - `internet` network level by host-applied rules, `forward`.
   - *Done when* v1 can be retired on one machine, a box can be recreated without losing login or history, and a test suite run from inside a box fails to destroy sealed data, rewrite banked commits, reach the LAN or read another realm.
2. **Interface.** Machine daemon, hub, web app, ntfy; status wrapper and structured input; SSH gate; relay reachable from inside the box; requests.
3. **Workflow.** Gateway network levels and exceptions; data sync and manifest; GitHub read facade and host commands; optional App tokens; published skills; dependency-source helper.
4. **Later.**
   - *Second hub.*
   - *Service logins*: for services that offer only a password login, a separate container holds the password, performs the login, and exposes an authenticated, filtered proxy to boxes.
   - GUI sidecar.
   - Not planned for now: VM-backed realms (waiting for a machine with KVM), and remote approval of high-risk requests (undecided; see `docs/dev/plan.md` section 5).

## 18. Repository and build

- Go module at the repository root: `cmd/devc`, `internal/…`, web assets embedded later.
- Built inside a container (`golang` image), statically linked, for amd64 and arm64. No Go toolchain is needed on the host.
- v1 moves to `legacy/` when phase 1 can replace it.
- Tests: unit tests, integration tests against real rootless Podman, and the escape suite from phase 1.

## 19. To verify early

| Item | Needs |
|---|---|
| `claude setup-token`: revocation, usage display, use from several machines | one browser approval |
| `autoMemoryDirectory` path behaviour | a Claude session |
| SELinux: file-context rule, per-realm level, box-to-host channel direction | a Fedora machine |
| Seccomp profile and nested-namespace block on Fedora | a Fedora machine |
| Claude `WorktreeCreate` redirection; plugin interface for posting a message | a Claude session in a box |
| Codex shared home with per-box daemon directories; app-server with attached TUI | a Codex login |
| Status wrapper transparency (resize, mouse, extended keys) | prototype |
| Gate built from `sshd -i` with forced command | prototype |
| Host-applied firewall rules on Podman 6 with current passt | a Podman 6 host |
