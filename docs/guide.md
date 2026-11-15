# devc user guide

This guide explains how devc is organised and how to use it day to day. The
reference for each command is `devc <command> --help`; host preparation is in
[host-setup.md](host-setup.md).

Contents: [Concepts](#concepts) · [Configuration and state](#configuration-and-state) ·
[Boxes](#boxes) · [Sessions](#sessions) · [Git](#git-relay-bank-pull-push) ·
[Data](#data-zones-sealing-savepoints) · [Network](#network-levels-exceptions-and-port-forwarding) ·
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
effect: `grants.github`, `[sync]`. `build_from_repo` and
`capabilities.nested` are refused with a message.

State lives in `~/.local/share/devc` (`--state-dir`, `DEVC_STATE_DIR` or
`state_dir`): `durable/` holds what is worth backing up (relays, journals,
context repositories with the data manifests, harness state, secrets),
`local/` what belongs to this machine (box homes, savepoint repositories, the
staging directories of data pulls), `cache/` what can be deleted, and
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
devc down demo/hello          # stop the box (banks its work first; --no-bank skips that)
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
session ID belongs to the project whose journal recorded it first: a box
cannot write into, or take over, another project's session by reusing its ID
(devc warns when one tries). Commands that take a session accept its ID, a
unique prefix, or `<realm>/<project>/<id>`. A
harness runs with its permission prompts switched off: the box is the
sandbox. Each harness also gets a generated description of the box (paths,
network level, git and data rules) in its instructions.

### The daemon

`devc daemon` is an optional background process, one per machine (or per
runtime directory); `devc service install` runs it as a systemd user service
(`devc service status`, `devc service uninstall`). Without it everything
works as described above. With it:

- each running box has a channel to the daemon: the box listens on
  `/run/devc/agent.sock`, the daemon connects to it, checks that the other end
  really is a process of that box, and the hooks tell it at once when they have
  written an event;
- session events reach the journal within moments instead of at the next
  `devc ps`; sessions whose box no longer runs them are marked lost every 30
  seconds;
- every five minutes it banks every running box and commits the context
  repositories (what the banking timer does without a daemon);
- `devc ps` and `devc ls` ask it instead of reading the spools themselves, and
  `devc ps --watch` keeps the list on the screen, redrawn as events arrive;
- `devc events <session>` prints a session's events; with `--follow` it keeps
  printing new ones (`devc events --follow` follows every session).

```sh
devc daemon                 # run it in the foreground (--log-level debug for detail)
devc daemon status          # is it running, and its channel to every box
devc daemon stop
devc service install        # run it as a systemd user service
devc service install --timer-only   # the banking timer instead, without a daemon
```

In a box, `devc box agent-status` says whether the daemon is connected. A box
started before the daemon existed has no agent; restart it with `devc down` and
`devc up`. The daemon serves its API on `daemon.sock` in the runtime directory
(mode 0600); only your own user can talk to it. Setting `DEVC_NO_DAEMON=1`
makes a command ignore a running daemon.

### Talking to a running session

With the daemon running, you can type into a session without attaching:

```sh
devc send <session> "fix the failing test"   # waits until the session is idle or done
devc send <session> --steer "stop, use the other API"   # delivers now
git diff | devc send <session> -              # the text from stdin
devc answer <session> 2                       # choose option 2 of the pending question
devc answer <session> "use Postgres"          # or answer with text
devc interrupt <session>                      # stop the current turn (Escape)
devc keys <session> Down Enter                # named keys: Enter Escape Tab arrows C-c 0-9 y n
devc seen <session>                           # mark the session's turns as seen (on every device)
devc claude demo/hello --detach --prompt "review the open TODOs"   # start with a first prompt
```

A session's argument may be a unique prefix of its ID. Each command prints a
receipt: its ID and an outcome (`accepted`, then `delivered`,
`typed-not-submitted`, `unknown` or `refused`); `--wait` waits for the final
outcome, `--json` prints the receipt as JSON. A command carries an ID (`--id`
to choose it): sending the same ID again returns the stored receipt and never
types the text a second time. Receipts are kept for seven days, and queued
commands survive a restart of the daemon.

Text is pasted into the session's terminal as one block, then the harness's
submit key is pressed, and devc waits for the harness to report the prompt.
Nothing is typed while someone is typing in that terminal: devc waits until
the attached terminal has been quiet for three seconds (at most a minute).
When a harness asks a question through Claude Code's question tool and nobody
has used its terminal in the last minute, the question waits (up to
`input.question_wait`, ten minutes) for `devc answer`; otherwise the harness
shows its own dialog and `devc answer` types the choice into it.

Harnesses that support the Program Status Protocol (Claude Code, Codex) run
under a small terminal wrapper (`devc box pty`) that answers the protocol's
query and turns the harness's own status reports into the session's state;
it passes every other byte unchanged. `DEVC_NO_PTY=1` in the box starts a
harness without it.

Harness installs: `devc harness install claude` (or `codex`) installs the
vendor's release into the toolbox in a throwaway container; `devc update`
installs newer versions; `devc harness ls` shows what is installed; `devc
harness gc` removes old versions. In-box self-updates are switched off.

### Requests from a box

An agent asks for things outside its box with `devc request` (inside the
box; the sandbox description tells it how):

```sh
devc request seal results/run7           # seal a finished run
devc request savepoint before-refactor
devc request note "the test data looks truncated"
devc request package apt libpq-dev       # high risk
devc request net pypi.org:443 --for 2h   # high risk: a network exception
devc request net host:5432               # high risk: a service on the host
devc request grant ssh cluster1          # high risk (not available yet)
devc request pr --title "Fix parser"     # high risk (not available yet)
devc request status <id> --wait
```

A request does nothing by itself. The daemon checks every field, stores the
request in `durable/realms/<realm>/requests/<project>.jsonl` and notes it in
the session's events. Low-risk kinds listed in the project's `[requests] auto`
(default `["seal", "savepoint", "note"]`) are approved at once; everything
else waits for you:

```sh
devc requests [--all] [--json]       # pending (or all) requests
devc requests approve <id> [--wait]  # devc carries it out (seal, save, devc root …)
devc requests deny <id> [--reason "…"]
```

High-risk requests can be approved only with the CLI on this machine, never
from a hub. Pending requests expire after seven days. An approved package
request runs the manager's install command through `devc root`, so it is
recorded in the env's root log; an approved network request runs `devc net
allow` (see [Network](#network-levels-exceptions-and-port-forwarding)).
Requests need the daemon; without it, `devc request` tells the agent to ask
you directly.

### Hubs

A hub is a server your machines report to, so that you can see and steer
the sessions of every machine from one place (the web app, below). Machines
dial out to it; it needs no access to them. Setting one up, with or without a
reverse proxy, is described in [hub-deployment.md](hub-deployment.md). On a
machine:

```sh
devc join https://hub.example.org --token <token> [--ca ca.crt]   # token from 'devc hub enroll machine <name>'
devc join https://hub.example.org --cert laptop.crt --key laptop.key [--ca ca.crt]
devc hub status            # each hub, connected or not, and which one notifies
devc leave https://hub.example.org
```

With `--token` the machine makes its key itself and the hub signs a
certificate for it; with `--cert` and `--key` it takes files the hub issued.
`devc join` stores the certificate under `durable/node/hubs/` and adds the hub
to `config.toml`; the daemon connects within seconds and keeps the connection
while it runs. The first connected hub in the list sends notifications to
your phone through ntfy. A hub can read the sessions' events, type into sessions, start
and stop them, and decide low-risk requests; it cannot stop the daemon, run
anything on the host, change the configuration, or approve high-risk
requests.

### The web app

The hub serves a small web app at its address, made for a phone first. Open
`https://hub.example.org/` in the browser that holds the device certificate
(from `devc hub enroll device <name>`), log in with the hub's password (and
the code from your authenticator when TOTP is on), and add it to the home
screen if you like: it installs as an app and opens offline with the last
data it saw. Notifications come from ntfy, not from the browser; tapping one
opens the session in the app.

- **Inbox** (home): every session across machines in the hub's sections:
  needs you, failed, finished, working, idle. A row shows project, worktree,
  harness, machine, state, the question or last message, and when it last
  changed; a machine that is offline dims its rows. Filter by machine, realm
  and project. The tab bar counts the sessions that need you.
- **Session**: the header says project, worktree, harness, machine, state and
  how long it runs. A pending question sits above the composer as a card with
  a button per option and a field for your own words; a pending request has
  Approve and Deny when it is low risk (a high-risk one shows the command to
  run on the machine). The timeline shows your prompts, the agent's messages,
  tool calls folded into one line per run (tap to expand), notes, questions
  and requests. The composer has **Send** (typed when the agent is ready),
  **Steer** (typed now, even mid-turn) and **Stop** (interrupts the turn, as
  Escape does); ending the session is in the ⋯ menu. Opening a session marks
  its finished turns seen. The timeline keeps the latest 1,000 events (**Load
  earlier** pages back from there). When the hub or the machine goes away
  while a command is on its way, the app cannot know whether it arrived: the
  prompt (or command) stays on screen as *unknown* and the app asks the
  machine for its receipt when it can; if it never arrived, **Send again**
  sends it with the same ID, so it is typed at most once.
- **Terminal** tab: the session's tmux window, live. It only observes: the
  phone never resizes the agent's pane, and the font shrinks to show the
  whole width (Zoom shows it larger, scrolling sideways). When both the hub
  and the machine allow it, **Take control** lets you type into the pane,
  with a key bar for Esc, Tab, Ctrl and the arrows. A terminal closes when the
  session ends and after 30 minutes in the background.
- **Changes** tab: what the session's worktree changed, computed from the
  machine's relay: **Uncommitted** is the work of the last bank's snapshot,
  **Branch vs upstream** the worktree's branch against the upstream's default
  branch. A diffstat, then the diff file by file.
- **Projects**: each machine's projects with their box, sessions, worktrees
  (uncommitted work; commits ahead of or behind the banked branch and the
  upstream) and pending requests.
- **New**: start a session: machine, project, worktree (an existing one, or a
  new name, which creates the worktree and its branch), harness and the first
  prompt. The box must be running (`devc up`).
- **Machines**: online, or when last seen, and how many sessions run.

Typing into a terminal is off by default. It needs `[terminal] control =
true` in the hub's `hub.toml` and in the machine's `config.toml`:

```toml
[terminal]
control = true
```

Everything else the app does goes through the hub's API with the same rules
as before: a login bound to the device certificate, box-scoped commands
only. The app has no build step; its files and the libraries it uses
(Preact, htm, xterm.js, listed with their versions in
`web/vendor/README.md`) are built into `devc`.

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
devc ingest --all       # copy session events into the durable journal (the daemon or the timer does this)
```

**Banking** is how commits leave a box. `devc bank` takes a snapshot of
uncommitted work in every worktree (`refs/devc/snap/<worktree>/<time>`),
fetches the box's branches and tags, updates the relay's branches, and records
every new tip under `refs/devc/bank/<branch>/<time>`. The box can add commits
and even rewrite its branches, but a tip that was banked once stays in the
relay. Branches listed in `git.protected` are never updated from the box; a
branch deleted in the box stays in the relay. Branches matching `git.forward`
are pushed to the upstream by each bank. A pull that fast-forwards a relay
branch is not taken back by the next bank: the relay remembers the box's tip
of every branch (`refs/devc/boxtip/<branch>`) and follows the box only when
the box moves the branch. Banking happens on `devc bank`, before `devc down`,
`devc rm`, `devc rebuild` and `devc up --recreate`, and every five minutes
while the daemon runs (`devc service install`; see "The daemon" above). On a
machine without the daemon, `devc service install --timer-only` installs a
systemd user timer that banks every five minutes and also copies session
events into the durable journal with `devc ingest --all`.

Banking is refused while the project's configuration has problems (see `devc
config check`); so are `pull`, `push` and the worktree commands. `down` and
`rm` then stop the box anyway and say that nothing was banked. When work
cannot be banked (a snapshot fails, the box cannot be fetched, a ref cannot be
recorded), `down`, `rm`, `rebuild` and `up --recreate` leave the box running
and say why; `--no-bank` stops it without banking (its files stay in the
project directory). `devc wt rm` removes a worktree only after its
uncommitted work is banked, also with `--force` (which allows a dirty worktree
or live sessions); `--discard` removes it without banking, and its uncommitted
work is lost.

**In the box**, with the daemon running, `git push origin <branch>` and `git
fetch origin` go to the relay through the daemon (the relay service): the
daemon runs `git receive-pack` or `git upload-pack` on the relay of the box's
own project, whatever URL `origin` has. A push follows the bank's rules:
branches can be created and rewritten, deletions and protected branches are
refused, tags can be created but not moved, `refs/devc/*` and
`refs/remotes/*` are refused. Every pushed tip gets a bank ref before the
relay accepts it (an update whose bank ref cannot be written is refused), and
branches matching `git.forward` are pushed on to the upstream (the push prints
the result). A push may update at most 1000 refs (push in parts), its pack may
be at most `git.max_push_size` (default `2g`), and it is refused as a whole
when the relay would grow beyond `git.relay_quota` (default `20g`). Before serving a fetch, the daemon fetches the upstream into the
relay (at most once a minute, 20 seconds at most, without prompts; turn it off
with `git.fetch_through = false`), so `git fetch origin` brings new upstream
commits as `origin/*` and `github/*` without `devc pull`. Without the daemon,
`git fetch origin` does nothing new until `devc pull`, and `git push origin`
changes nothing and says that commits are banked by devc. The daemon also
banks a box shortly after each agent turn ends (at most every 30 seconds), so
commits and uncommitted work reach the relay without waiting for the
five-minute pass. Projects set up before this version need `devc project init`
once so the box's `origin` also fetches `github/*` and tags. Use `git commit`
for work in progress instead of `git stash`.

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

## Moving data between machines

Data moves between this machine and others (a cluster, a workstation, a
laptop, a mounted disk) with rsync, wrapped by devc so that the defaults are
safe: nothing is ever deleted, what is replaced is kept, and you see what
would be transferred before anything happens. A remote is declared in
`project.toml`:

```toml
[data.remotes.cluster1]
host = "cluster1"                 # an SSH host alias of your own ssh configuration
path = "/scratch/me/analysis"     # its data root: absolute, or relative to the remote home
pull = ["results"]                # default paths of 'devc data pull cluster1'
push = ["raw"]                    # default paths of 'devc data push cluster1'

[data.remotes.nas]
path = "/mnt/nas/analysis"        # no host: a directory on this machine (absolute)
```

Paths are relative to the data root on both sides and must lie in a zone (or
be one). rsync runs on the host with your own SSH configuration, keys and
agent; when `devc ssh login cluster1` has a connection open, the transfer
goes through it (its control socket never leaves the host). The remote needs
rsync 3.0 or later.

```sh
devc data pull cluster1 [path...] -p demo/analysis [--dry-run] [--yes] [--max-size 2g]
devc data push cluster1 [path...] -p demo/analysis [--dry-run] [--yes] [--content]
```

Both first list what would be transferred (an rsync dry run) and ask; `--yes`
skips the list and the question (needed without a terminal), `--dry-run`
shows the list and changes nothing on either side.

**Pull.** rsync fetches into a staging directory in devc's state (no box sees
it), keeping symlinks as links and never following them; owners, groups,
devices and special files are not transferred. A helper container then moves
the files into the data root, owned by the box user (set-ID bits dropped):

- a file that differs from the one already there replaces it, and the old one
  goes to one trash entry for the pull (`devc data trash ls`, `restore`);
- identical files (same bytes) are left alone;
- sealed data is never changed: a pull of a path inside a sealed entry is
  refused, and sealed entries inside a pulled directory are left as they are
  (reported when the remote's copy differs);
- new entries in a sealed zone arrive unsealed; seal them with `devc seal`.

`--max-size` (default 2g, `0` for no limit) refuses a pull whose transfer would
be larger in total. Every pull transfers the selected files in full, since the
staging directory starts empty: pull the paths you need rather than whole
zones. When a transfer is interrupted, what arrived is kept and the next pull
from the same remote continues from it (and also brings what that interrupted
pull received).

**Push.** The data tree is writable by the box, so rsync does not read it. A
helper container first hard-links the selected files into
`.devc-staging/push-<time>/` in the project directory, whose directories
belong to the vault: the box can read them but cannot add, remove, rename or
replace anything there. rsync sends only that directory. Nothing is deleted on
the remote; a file the push replaces is kept there in
`<path>/.devc-backup/<time>/`. New files keep their permission bits, limited
by the remote's umask; a file that replaces another keeps that one's
permissions (and takes over the executable bit); directories are created with
the remote's defaults.

The staged files are hard links, not copies: a process in the box that holds
a file open can still change it while it is being sent, and rsync decides
what to send by size and modification time, which the box controls too. For a
consistent copy, seal the entry (or stop the session) before pushing.

**The manifest.** Seals, savepoints, pulls and pushes are recorded in
`durable/realms/<realm>/context/data/<project>/manifest.jsonl`: the time, the
machine (`machine_name` in `config.toml`, default the host name), the action,
the path, the number of files, the total size, a digest of the sorted list of
files with their sizes, the remote, and with `--content` (on `devc seal` and
`devc data push`) a SHA-256 hash over the contents. The manifest is part of
the realm's context repository, committed after every pull and push and with
the context's other commits, so it travels with it.

```sh
devc data status demo/analysis [--json]   # each top-level entry: size, digest, state, where it was last recorded
devc data verify results/run7 [--content] # recompute and compare with the manifest (exit 1 when it differs)
devc seal results/run7 --content          # seal and store a content hash
```

`data status` shows each top-level entry of each zone as `new` (no record),
`recorded` (as last recorded), `changed` or `absent` (recorded but not here),
with the machines and remotes it was last recorded on; a `*` marks a record
that differs from the entry now.

**Clusters.** On a cluster the agent acts as you, with your account, and devc
cannot sandbox it there: it can only help against accidents. Seal finished
runs on the cluster with `chmod -R a-w <run>`; keep results that matter on
backed-up storage rather than on scratch; and pull post-processed data off the
cluster (`devc data pull cluster1 results/run7`, then `devc seal
results/run7`) rather than raw output.

## Network levels, exceptions and port forwarding

A realm sets `network.default` in `realm.toml`; a project may override it with
`network.level` in `project.toml`.

| Level | What the box can reach |
|---|---|
| `internet` (default) | The internet, and the box's DNS resolver on port 53. Not the host, the LAN, link-local, carrier-grade NAT, multicast or other private addresses, and no IPv6. |
| `allowlist` | Only the names on the project's allowlist, through a proxy that the devc daemon runs on the host. The box has no network interface besides loopback and no DNS. |
| `none` | Nothing: the box has only a loopback interface. |
| `open` | Everything the engine's default network reaches, including the LAN and services on the host. |

Exceptions (below) open more for a while; they are created on the host only.

**How `internet` works.** devc creates the box with IPv6 switched off, lets
the engine set up the box's network namespace (`podman init`), installs
nftables rules into that namespace from the host, and only then starts the
box. The rules reject every private destination with an error, so attempts
fail at once. No process in the box has the capability to list or change
them. Two checks guard the rules:

- between installing the rules and starting the box, devc writes a record
  into the box's read-only `/etc/devc` that names the filtered network
  namespace, the box's main process and the boot. devc's init in the box
  compares it with its own and stops the box before it reports ready when
  they differ, so a box started behind devc's back with `podman start` stops
  again instead of running unfiltered. Then init connects to a sentinel
  address that only the rules answer and expects a refusal;
- `devc up`, `sh`, `run`, a session or a forward on a running box checks from
  the host that the record matches the running box and that the rules are in
  its namespace; a box that fails is stopped and started properly. The box's
  own readiness marker is not trusted;
- after the start, the host listens on all its addresses for a moment while
  the box tries to reach them and the engine's host alias; if anything gets
  through, the box is stopped and `devc up` fails.

The rules live in the box's network namespace and disappear when the box
stops; every start through devc (`up`, `sh`, `run`, a session) installs them
again.

DNS exceptions go only to resolvers devc computes on the host: the host's own
nameservers (systemd-resolved's upstream servers when `/etc/resolv.conf` names
its local stub) and the network backend's built-in resolver (slirp4netns,
pasta, or a named network's gateway). A nameserver in the box's
`/etc/resolv.conf` that is not among them gets no exception, and `devc up`
warns about it.

**Limits of `internet`.** Anything the box can read, it can send to the
internet: code, notes, and the harness credential. DNS queries to the resolver
are a channel too. If the host's resolver is itself on the LAN (common with
home routers), that one address is reachable on port 53. The host's own
public addresses are blocked as they are when the box starts; an address the
host gets later is not.

**How `allowlist` works.** The box is created without any network
(`--network none`). Its environment points `HTTP_PROXY` and `HTTPS_PROXY`
(and the lower-case forms, plus `NODE_USE_ENV_PROXY=1` for Node.js) at
`http://127.0.0.1:3128`, where devc's agent in the box listens and relays
every connection, unread, over the box channel to the daemon. The daemon is
the proxy. For every request (`CONNECT host:port`, or plain HTTP with an
absolute URL; several requests on one connection are each decided):

1. the host must be a name on the allowlist; IP addresses are refused;
2. the port must be 80 or 443, unless the entry names another;
3. the daemon resolves the name itself and connects only to public IPv4
   addresses: never to the private ranges of level `internet` or to an
   address of the host, and it checks the address of the socket it actually
   connects, so a name that resolves to a private address, or whose answer
   changes, reaches nothing;
4. the decision goes to `durable/realms/<realm>/netlog/<project>.jsonl`.

```toml
# realm.toml (projects add their own [network.allowlist] to it)
[network]
default = "allowlist"

[network.allowlist]
presets = ["anthropic", "github", "pypi"]
domains = ["example.org", "*.cdn.example.net", "git.example.org:8443"]
```

`*.example.org` matches every name below `example.org`, not `example.org`
itself. The presets are short explicit lists (`anthropic`, `openai`,
`github`, `pypi`, `npm`, `cran`, `debian`), never a wildcard over a hosting
provider; `devc config show <realm>/<project>` shows the merged list. The
allowlist is part of what a box was started with: an entry you add needs
`devc down` and `devc up` (or an exception, below); an entry you remove is
refused at once.

```sh
devc net log demo/hello            # the proxy's last 50 decisions
devc net log demo/hello --denied   # refused requests only
```

Without a running daemon the proxy does not answer, so the box has no
network at all; when the daemon starts, the proxy works again without
restarting the box. Programs that ignore the proxy variables have no network
at this level. TLS is relayed unchanged, so the proxy sees the name a
`CONNECT` asks for, not what is said inside the encrypted connection: a
server that answers for many names can be asked for any of them.

**Exceptions.** An exception opens one more thing for a while (default 2
hours, at most 30 days, or `--for forever`):

```sh
devc net allow 192.168.1.20:5432 demo/hello            # a LAN host and port (level internet)
devc net allow nas.local:445 demo/hello --for 30m      # names are resolved now, on the host
devc net allow lan demo/hello --for 1h                 # the whole LAN (level internet)
devc net allow host:5432 demo/hello                    # a service on this host's loopback (every level)
devc net allow pkg.example.org:443 demo/hello          # at level allowlist: one more name
devc net ls                                            # exceptions in force, with the time left
devc net revoke x1a2b3c
```

- At level `internet`, a host and port, or `lan` (10/8, 172.16/12,
  192.168/16 and 100.64/10, but never the host's own addresses or link-local),
  become elements of nftables sets in the box's namespace with a timeout:
  they expire in the kernel without any process. They vanish when the box
  stops; `devc up` puts back those still valid, after its checks.
- `host:<port>` makes a service listening on the host's `127.0.0.1:<port>`
  reachable at `127.0.0.1:<port>` inside the box: devc's agent listens there
  and relays over the box channel, and the daemon connects on the host only
  while the exception is in force; revoking or expiry also ends open
  connections. It needs the daemon.
- At level `allowlist`, a name and port is added to that box's allowlist for
  the duration; addresses and `lan` are refused there.

An agent can ask for an exception with `devc request net <target> [--for 1h]`
in the box; it is a high-risk request, approved only on this machine
(`devc requests approve <id>`), which runs `devc net allow` for it.
`devc net ls` and `devc ls` show an exception without expiry prominently.
Exceptions survive `devc down` and `devc up` while valid; `devc rm` drops
them.

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

**Local URLs.** While the daemon runs, it serves
`http://<port>.<project>.<realm>.localhost:7480/` for every port listed in
the project's `box.ports` and every port with a running `devc forward`
(which prints the URL), WebSockets included. Browsers and curl resolve
`*.localhost` to the loopback address themselves. Other ports get a 404 that
says how to allow them. The router listens on `router.listen` in
`config.toml` (a loopback address, or `"off"`), routes only by that host
name (a page that points its own name at 127.0.0.1 reaches nothing), and
reaches the box the way a forward does.

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

With `[secrets] delivery = "daemon"` in `config.toml`, nothing is written
into boxes (except the SSH gate's key, see below): each time a harness starts, its launcher asks the daemon for the
harness token and the granted secrets, and the daemon answers only for names
the project is granted now and was granted when the box was started:
removing a grant takes effect at the next launch, a new grant once the box
has been brought up again (`devc down` and `devc up`). Without a
running daemon the harness starts without them and says so. In such a box,
`devc box secret <NAME>` prints a granted secret for programs started outside
a harness.

**SSH to other machines (the SSH gate).** A box can use an SSH connection you
authenticated on the host, password and second factor included, without ever
holding a credential for it:

```sh
devc ssh login cluster1 [--ttl 12h]   # ssh to cluster1 on this terminal; the connection stays as a master
devc ssh ls [--json]                  # masters and whether they answer
devc ssh close cluster1               # end it, and with it every box's access
```

```toml
# project.toml
[grants]
ssh = ["cluster1", "login2:ro"]

[grants.ssh_allow]
login2 = ["squeue", "sinfo", "ls"]
```

`<host>` is an alias of your own SSH configuration (`~/.ssh/config`, or the
file named by `DEVC_SSH_CONFIG`). In a box with the grant, plain
`ssh cluster1 …`, `scp`, `sftp`, `rsync -e ssh` and `git clone cluster1:…`
work. The box's connection ends at a gate on the host: an `sshd -i` that the
daemon starts for each connection, which accepts only that box's key (made
anew at every start of the box and delivered as `/run/devc-secrets/gate_key`,
also with `delivery = "daemon"`), has every kind of forwarding switched off,
and runs each command over your master connection; the remote shell parses
exactly what the box typed. The master's control socket stays in
`$XDG_RUNTIME_DIR/devc/gate/`, which no box can reach: through a control
socket a client could ask the master for forwards into the host. Every
session is logged in `durable/realms/<realm>/gate-log.jsonl` (time, box,
host, command, exit status, duration). The gate needs the daemon and an
`sshd` binary on the host (the service itself need not run); it logs in as
your user, whose login shell runs the gate's forced command.

**A grant is full use of the remote account**, with no per-command approval:
whatever the agent runs there runs as you. `"<host>:ro"` restricts a box to
commands whose first word is listed in `[grants.ssh_allow]` and that contain
no shell metacharacters (`;`, `|`, `&`, `$`, backquote, `<`, `>`,
parentheses, newline), with no interactive shell and no sftp. It is a
convenience, not a strong boundary: an allowed command that runs programs or
scripts (`sbatch`, `find -exec`, an editor) can still do anything the account
can. Removing a grant from `project.toml` closes the gate for that host at
once, and so does narrowing it to `:ro` or shortening its `ssh_allow` list;
a new grant, or a wider one, needs a restart of the box (`devc up
--recreate`), which
writes `/etc/devc/ssh_config` (also mounted at
`/etc/ssh/ssh_config.d/50-devc.conf` and included from the box's
`~/.ssh/config`).

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
down, every five minutes while the daemon (or the banking timer) runs, and on
`devc context commit [realm]`; `devc
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
  only the tree they work on. `devc data pull` and `push` run rsync on the
  host only between a remote and a staging directory no box can change. The daemon's channel is opened from the host to
  a socket in the box's runtime directory without following links, and only
  to a process of that box; what arrives on it is bounded and checked.
- Banked commits, sealed data, session journals and notes commits are kept
  where the box cannot change them.
- At level `internet`, the box cannot reach the host, the LAN or other private
  addresses, and a box whose rules are missing does not start. At level
  `allowlist`, it has no network interface at all; the host's proxy decides
  every connection by name and never connects to a private address. Network
  exceptions are created on the host only.

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
- **Forwarded ports** and **local URLs** can be used by every user and program
  on the host. Pages served at local URLs of different projects share the
  parent name `localhost`, so a box's page could set cookies for another
  box's local URL.
- **The allowlist** limits where connections go, not what they carry: an
  allowed host that relays (a paste site, a cloud storage API, a server that
  answers for many names) can receive anything the box can read.
- **`devc root`** gives full root inside the box; changes it makes to the
  image persist in the container.
- **Box-written text** in `devc ps --json` and the journals is escaped for
  terminals by devc, but other programs that read these files must treat it as
  untrusted input.
- **Sealing** protects against the box only (see [Data](#data-zones-sealing-savepoints)
  for its limits).
