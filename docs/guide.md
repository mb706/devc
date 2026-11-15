# devc user guide

This guide explains how devc is organised and how to use it day to day. The
reference for each command is `devc <command> --help`; host preparation is in
[host-setup.md](host-setup.md).

Contents: [Concepts](#concepts) · [Configuration and state](#configuration-and-state) ·
[Boxes](#boxes) · [Sessions](#sessions) · [Git](#git-relay-bank-pull-push) ·
[Data](#data-zones-sealing-savepoints) · [Network](#network-levels-and-port-forwarding) ·
[Logins and secrets](#logins-and-secrets) · [Notes and memory](#notes-and-memory) ·
[Editors](#editors) · [What is protected](#what-is-protected-and-what-is-not)

## Concepts

| Term | Meaning |
|---|---|
| **Realm** | A trust domain, such as `work` or `private`. Boxes of one realm share harness logins and history, notes and memory, and a package cache. Nothing writable is shared between realms. |
| **Project** | A git repository, optionally with a data directory, that belongs to one realm. Written `realm/project`, for example `demo/hello`. |
| **Env** | An image recipe: a directory with a `Containerfile` and an `env.toml`. A project uses an env or a plain image reference. |
| **Box** | The container of one project, named `devc-<realm>-<project>`. It runs as user `dev` (UID 1000) whatever your own UID is. |
| **Worktree** | A checkout inside the project directory: `main/`, and one sibling directory per task. |
| **Session** | One agent (or shell) running in a tmux window in a box, with an ID such as `s01k7etp7bc05l9nn1t9l`. |
| **Harness** | A coding agent devc knows how to install, log in and start: Claude Code and Codex are built in; others are described by a manifest. |
| **Toolbox** | A directory mounted read-only at `/opt/devc` in every box: the devc helper, a static tmux, the installed harnesses. |
| **Relay** | A bare git repository on the host, one per project, between the box and the upstream repository. |

Inside every box the paths are fixed, so they are the same on every machine:

| In the box | What |
|---|---|
| `/w/<realm>/<project>/` | the project directory: `.repo/` (git directory), `main/`, task worktrees, `data/` |
| `/home/dev` | the box home, kept when the box is removed |
| `/home/dev/.cache` | the realm's package cache |
| `/ctx/notes`, `/ctx/memory` | notes of this project and memory of its memory group (see [Notes and memory](#notes-and-memory)) |
| `/opt/devc` | the toolbox (read-only) |
| `/etc/devc` | the box's description and your dotfiles (read-only) |
| `/run/devc-secrets` | the secrets granted to the project (read-only) |

## Configuration and state

devc reads its configuration only from the configuration directory,
`~/.config/devc` by default (`--config-dir` or `DEVC_CONFIG_DIR` to change
it). Nothing a box can write is ever read as configuration. The directory is
meant to be a git repository of its own, so a new machine is set up with
`devc init <url>`.

```
~/.config/devc/
  config.toml                      global settings: projects_root, default_env, box limits
  dotfiles/                        linked into every box home (.zshrc, .tmux.conf, .gitconfig, ...)
  baseline/                        settings and instructions applied to every harness
  harness/<id>.toml                your own harness manifests
  envs/<env>/Containerfile         image recipes, with env.toml
  realms/<realm>/realm.toml        realm settings: network level, git identity, harness overrides
  realms/<realm>/projects/<project>/project.toml
```

[examples/config](../examples/config/) shows every key with a comment. `devc
config check` reports every problem; `devc config show <realm>/<project>`
prints a project's settings with all defaults applied. A command that would
create or start a box refuses while that project's configuration has a
problem.

Some keys are accepted for features that are not available yet and have no
effect: `grants.ssh`, `grants.github`, `box.ports`, `[data.remotes]`,
`[sync]`, `[[hubs]]`. `build_from_repo`, `network = "allowlist"` and
`capabilities.nested` are refused with a message.

State lives in `~/.local/share/devc` (`--state-dir`, `DEVC_STATE_DIR` or
`state_dir`): `durable/` holds what is worth backing up (relays, journals,
context repositories, harness state, secrets), `local/` what belongs to this
machine (box homes, savepoint repositories), `cache/` what can be deleted, and
`toolbox/` the toolbox. Project directories live below `projects_root`
(default `~/devc/<realm>/<project>`). Runtime files are in
`$XDG_RUNTIME_DIR/devc`.

`devc realm add`, `devc project add` and `devc project adopt` write new
realm and project files for you.

## Boxes

```sh
devc up demo/hello            # create or start the box (building or pulling its image if needed)
devc ls                       # all boxes: state, image, drift, sessions
devc sh demo/hello            # a login shell as dev in the box
devc run demo/hello -- make   # run one command as dev
devc root demo/hello -- apt-get install -y ripgrep   # as root, with all capabilities
devc down demo/hello          # stop the box (banks its commits first)
devc rm demo/hello            # remove the container; home, project and state are kept
devc rm --home demo/hello     # ... and delete the box home too
devc rebuild demo/hello       # rebuild the env image and recreate the box
devc up --all / devc down --all   # start every existing box / stop every running one
```

Without a `realm/project` argument, commands use the project that contains the
current directory.

**Images.** An image must be glibc-based and contain `sh`, `git` and an
OpenSSH client; everything else devc needs comes from the toolbox. `devc env
build <env>` builds `localhost/devc-env/<env>`. A project names either `env =
"<env>"` or `image = "<reference>"` (or neither, when `default_env` is set).
The example configuration contains three envs: `base` (Debian with git), and
`r-base` and `python-base`, which are larger images ported from devc v1.

**Root.** Inside a box nobody can become root. `devc root` runs a command as
root with all capabilities from the host, and appends it to
`envs/<env>/root-log.jsonl`, so you can fold it into the Containerfile
later. Changes made this way live in the container and are lost when it is
recreated.

**Drift.** When the definition of an existing box changes (configuration,
image, or devc's hardening after an upgrade), `devc up` says so and stops;
`devc up --recreate` recreates it. Home, project files and all state are kept.

**Limits and capabilities.** `[box] memory` and `pids` set limits; they need
the memory and pids cgroup controllers to be delegated to your user, and
`devc up` fails if a configured limit cannot be applied (switch a limit off
with `memory = "none"` or `pids = -1`). `[capabilities] gpu = true` passes the
NVIDIA GPUs (CDI) and `kvm = true` passes `/dev/kvm`; both weaken isolation.

**Self-test.** `devc run <ref> -- /opt/devc/bin/devc box selftest` tries what
the box must refuse (capabilities, privilege gain, user namespaces, `AF_ALG`
sockets, changing the firewall, writing read-only parts) and reports each
item.

## Sessions

Every installed harness gets a command of its own:

```sh
devc claude demo/hello                  # new session, attached
devc claude demo/hello -w fix-login     # in the worktree fix-login (created if needed)
devc codex demo/hello --detach          # keep it in the background; prints the session ID
devc claude demo/hello -- --resume      # arguments after -- go to the harness
devc ps                                 # sessions of all projects and what they do
devc attach demo/hello [session]        # attach to the box's tmux
devc stop <session>                     # end a session
```

Sessions run in one tmux server per box; each session is a window, and each
attached terminal gets its own view. Detach with the tmux prefix and `d`
(`C-b d` unless your `.tmux.conf` changes the prefix). From another machine:
`ssh <machine> -t devc attach demo/hello`.

Harnesses report through hooks that devc installs in their managed settings:
prompt submitted, question asked, final message, start and end. devc keeps
these events in a journal per session in its state directory, which the box
cannot write; `devc ps` and the SESSIONS column of `devc ls` show them. A
harness runs with its permission prompts switched off: the box is the
sandbox. Each harness also gets a generated description of the box (paths,
network level, git and data rules) in its instructions.

Harness installs: `devc harness install claude` (or `codex`) installs the
vendor's release into the toolbox in a throwaway container; `devc update`
installs newer versions; `devc harness ls` shows what is installed; `devc
harness gc` removes old versions. In-box self-updates are switched off.

## Git: relay, bank, pull, push

Each project has a relay, a bare repository on the host. In the box, the
repository's `origin` is that relay (`devc::<realm>/<project>`); the upstream
(GitHub or any git server) is reached only from the host, with your own
credentials.

```sh
devc project add demo/app --from git@github.com:you/app.git --harness claude
devc project adopt demo/app --path ~/src/app   # use an existing clone instead
devc bank demo/app      # copy the box's work into the relay
devc log demo/app       # history in the relay (git log arguments after --)
devc diff demo/app      # changes in the relay (git diff arguments after --)
devc pull demo/app      # fetch the upstream into the relay and hand it to the box
devc push demo/app      # push the branch checked out in main, after showing what it sends
devc wt new fix-login demo/app      # a task worktree with branch <prefix>fix-login
devc wt ls demo/app / devc wt rm fix-login demo/app
```

**Banking** is how commits leave a box. `devc bank` takes a snapshot of
uncommitted work in every worktree (`refs/devc/snap/<worktree>/<time>`),
fetches the box's branches and tags, updates the relay's branches, and records
every new tip under `refs/devc/bank/<branch>/<time>`. The box can add commits
and even rewrite its branches, but a tip that was banked once stays in the
relay. Branches listed in `git.protected` are never updated from the box; a
branch deleted in the box stays in the relay. Branches matching `git.forward`
are pushed to the upstream by each bank. Banking happens on `devc bank`,
before `devc down` and `devc rm`, and every five minutes once you run `devc
service install` (a systemd user timer).

**In the box**, `git fetch origin` and `git pull` work after `devc pull` has
handed new upstream commits to the box; `git push origin` changes nothing and
says that commits are banked by devc. Use `git commit` for work in progress
instead of `git stash`.

**Pushing** is always your decision: `devc push [branch...]` banks, shows the
commits and the diffstat the upstream lacks, and asks before it pushes (`--yes`
skips the question, `--force` allows a non-fast-forward with a lease, `--tag
t` pushes a tag created in the box).

Host-side git runs only against relays, with hooks disabled and the box's
configuration ignored. The project directory itself is box-writable: do not
run git, build tools or editors on it from the host (see
[What is protected](#what-is-protected-and-what-is-not)). Host git reports
"not a git repository" there on purpose.

## Data: zones, sealing, savepoints

A project may declare a data directory (default `data/`, beside `main/`),
shared by all worktrees through a `data` link in each, and divided into
zones:

```toml
[data]
inputs = ["raw"]          # read-only in the box; you put files there from the host
work = ["scratch"]        # ordinary writable directories, covered by savepoints
sealed = ["results"]      # one directory per run; finished runs are sealed
```

**Sealing.** `devc seal results/run1` hands a finished run (a direct child of
a sealed zone) to a second in-box user, the vault, and removes write
permission recursively. From the box it can still be read, but not changed,
renamed, removed or have its permissions changed; new runs can still be
created beside it. The zone directories, the data root and the project's
`.devc/` directory cannot be moved or removed from the box either.

```sh
devc seal results/run1 demo/analysis
devc data ls demo/analysis           # zones, sealed entries, savepoints, trash
devc data unseal results/run1        # give it back to the box user (host only)
devc data rm results/run1            # move it to the trash in .devc/ (host only)
devc data trash ls | restore <id> | empty [--older-than 30d]
```

Limits of sealing: a process that already holds a file open for writing can
keep writing until it closes it; sealing protects against the box only, not
against you; FIFOs, sockets and device files inside an entry are skipped; a
file hard-linked from elsewhere in the project is sealed under every name.

**Savepoints** copy the data directory (without input zones and sealed
entries) and record the current commit of `main`:

```sh
devc save before-refactor demo/analysis
devc restore <savepoint> [path...] -p demo/analysis        # into restored-<id>/ in the first work zone
devc restore <savepoint> --in-place -p demo/analysis       # back in place; what is in the way goes to the trash
devc data gc demo/analysis [--drop <id>]
```

The driver is set by `data.savepoints.driver`: `reflink` makes instant copies
inside the project when its filesystem can clone files (XFS, btrfs); `restic`
keeps an incremental repository on the host (install it with `devc toolbox
install restic`); `auto` picks `reflink` when a probe succeeds, else `restic`.
`keep` limits the number of unnamed savepoints; `before_session = true` takes
one before a session starts (at most one per 30 minutes). All data
operations run in a throwaway helper container that sees only the project.

## Network levels and port forwarding

A realm sets `network.default` in `realm.toml`; a project may override it with
`network.level` in `project.toml`.

| Level | What the box can reach |
|---|---|
| `internet` (default) | The internet, and the box's DNS resolver on port 53. Not the host, the LAN, link-local, carrier-grade NAT, multicast or other private addresses, and no IPv6. |
| `none` | Nothing: the box has only a loopback interface. |
| `open` | Everything the engine's default network reaches, including the LAN and services on the host. |
| `allowlist` | Not available yet; `devc up` refuses it. |

**How `internet` works.** devc creates the box with IPv6 switched off, lets
the engine set up the box's network namespace (`podman init`), installs
nftables rules into that namespace from the host, and only then starts the
box. The rules reject every private destination with an error, so attempts
fail at once. No process in the box has the capability to list or change
them. Two checks guard the rules:

- when the box starts, devc's init connects to a sentinel address that only
  the rules answer; if the answer is not the expected refusal, the box stops
  before it reports ready. A box started behind devc's back with `podman
  start` therefore stops again instead of running unfiltered;
- after the start, the host listens on all its addresses for a moment while
  the box tries to reach them and the engine's host alias; if anything gets
  through, the box is stopped and `devc up` fails.

The rules live in the box's network namespace and disappear when the box
stops; every start through devc (`up`, `sh`, `run`, a session) installs them
again.

**Limits of `internet`.** Anything the box can read, it can send to the
internet: code, notes, and the harness credential. DNS queries to the resolver
are a channel too. If the host's resolver is itself on the LAN (common with
home routers), that one address is reachable on port 53. The host's own
public addresses are blocked as they are when the box starts; an address the
host gets later is not.

**Forwarding a port.** To use a server running in a box (a web app, Jupyter),
forward its port to the host:

```sh
devc forward 8888 demo/analysis                  # until Ctrl-C; prints the address
devc forward 8888 demo/analysis --detach         # in the background
devc forward ls
devc forward stop <id>
devc forward 3000 --host-port 13000              # another port on the host
```

The forward listens on `127.0.0.1` with the same port number (a free port is
chosen and printed if that one is taken) and relays each connection to
`127.0.0.1:<port>` inside the box. The server in the box should listen on
`127.0.0.1`; this works at every level, including `none`, and needs nothing
in the image. A forward ends when its box stops. A loopback port can be used
by every user and program on the host; `--bind <address>` with a
non-loopback address makes it reachable from the network, and devc warns
about that.

## Logins and secrets

**Harness logins** are stored per realm, so every box of a realm uses the same
login:

```sh
devc login demo claude                 # runs `claude setup-token` in a throwaway container, then asks for the token
devc login demo claude --token-stdin   # or read the token from stdin
devc login demo codex                  # runs Codex's device login with the realm's shared Codex home
devc login status demo
```

How a harness logs in is part of its manifest: `env-token` stores a token and
hands it to the harness in an environment variable, `shared-home` keeps the
vendor's own credential file in a home directory shared by the realm's boxes,
`api-key` stores a key you paste.

**Secrets** are per realm and given to a project by name:

```sh
devc secret set demo PYPI_TOKEN        # from stdin when piped, else a hidden prompt
devc secret ls demo
devc secret rm demo PYPI_TOKEN
```

```toml
# project.toml
[grants]
secrets = ["PYPI_TOKEN"]
```

At `devc up`, the granted secrets and the harness token are written as files
into `/run/devc-secrets` of that box only (read-only, mode 0400), and the
harness launcher exports them to the harness as environment variables. They
are stored in `durable/realms/<realm>/secrets/` as 0600 files and delivered
through `$XDG_RUNTIME_DIR`, which must be a tmpfs (devc refuses otherwise,
unless `DEVC_ALLOW_PERSISTENT_SECRETS=1`). A box that is running when you
change a secret sees the change after `devc down` and `devc up`. Everything
in the box can read the secrets granted to it.

**Harness state.** Claude Code keeps its home in the box home and shares its
`projects` directory (session history) across the realm. Codex uses one home
shared by all boxes of the realm. A shared home is writable by every box of
the realm, so configuration planted there by one box (hooks, servers) can run
in the others: a realm is one trust domain.

## Notes and memory

Each realm has a context repository in devc's state. In a box, `/ctx/notes`
holds that project's notes and `/ctx/memory` the memory of its memory group
(`memory_group` in `project.toml`, default the project name); Claude Code is
pointed at it for its automatic memory. The notes are not part of the
project's repository. devc commits the context repository when a box goes
down, on the banking timer, and on `devc context commit [realm]`; `devc
context log` and `devc context diff` show it. The commit is made in a
throwaway container; symbolic links, special files and anything named like
git metadata are left out.

## Editors

`devc cursor [ref]` and `devc code [ref]` start the box and open Cursor or VS
Code attached to it, at the `main` checkout. The editor's command-line tool
must be on your PATH.

**An attached editor is a deliberate hole in the sandbox.** The editor installs
its server in the box, and that server talks to the editor on your desktop:
extensions, terminals and tasks act with your rights there, and code in the
box can reach the host through them. Attach only to boxes whose code you
would also run on your desktop.

Emacs reaches a box through TRAMP's podman method, which runs commands in the
box as `dev`:

```
/podman:devc-<realm>-<project>:/w/<realm>/<project>/main/
```

The files are box-written: keep Emacs from applying `.dir-locals.el` and
file-local variables without asking (`enable-local-variables` set to `:safe`
or `nil`).

## What is protected and what is not

A box is a container on your kernel. devc makes it a hardened one and keeps
everything that matters outside it, but it is not a virtual machine.

**What devc enforces:**

- Box processes run as `dev` without capabilities, with no-new-privileges
  (setuid programs gain nothing) and a seccomp profile that also blocks
  creating user namespaces and `AF_ALG` sockets.
- A box sees its own project directory, read-only mounts of other projects
  of the same realm that you configure, its home, and the realm's shared
  state. Never mounted: your home, SSH keys or agent, the Podman socket, the
  configuration directory, or anything of another realm.
- Policy (mounts, network level, grants) comes only from the configuration
  directory; network rules, sealing and branch policy are applied from the
  host.
- The host does not run anything the box wrote: host git works only on relays
  with hooks and the box's configuration ignored; spool files and other
  box-written files are read without following links and parsed strictly;
  data operations and context commits run in throwaway containers that see
  only the tree they work on.
- Banked commits, sealed data, session journals and notes commits are kept
  where the box cannot change them.
- At level `internet`, the box cannot reach the host, the LAN or other private
  addresses, and a box whose rules are missing does not start.

**What devc does not protect against:**

- **Kernel bugs.** A kernel exploit escapes any container. Keep the host
  patched.
- **Leaks to the internet.** At level `internet` (and `open`) the box can send
  anything it can read, including the harness credential, to any internet
  host. Use `none` for work that must not leave the machine.
- **The harness credential** is visible to the agent; so is every secret
  granted to the project.
- **Within a realm**, boxes share harness state, memory and the package cache;
  one box can influence the others. Use separate realms for work that must
  stay apart.
- **The project directory is writable by the box.** Running git, direnv, an
  editor, a build tool or a test runner on it from the host runs whatever the
  box planted there (hooks, `.envrc`, `.vscode/tasks.json`, `Makefile`). Use
  `devc sh`, `devc run`, `devc log` and `devc diff` instead.
- **Attached editors** (`devc cursor`, `devc code`) and **level `open`** are
  holes you open on purpose.
- **Forwarded ports** can be used by every user and program on the host.
- **`devc root`** gives full root inside the box; changes it makes to the
  image persist in the container.
- **Box-written text** in `devc ps --json` and the journals is escaped for
  terminals by devc, but other programs that read these files must treat it as
  untrusted input.
- **Sealing** protects against the box only (see [Data](#data-zones-sealing-savepoints)
  for its limits).
