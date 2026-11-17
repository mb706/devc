# devc user guide

This guide explains how devc is organised and how to use it day to day. The
reference for each command is `devc <command> --help`; host preparation is in
[host-setup.md](host-setup.md).

Contents: [Concepts](#concepts) · [Configuration and state](#configuration-and-state) ·
[Boxes](#boxes) · [Sessions](#sessions) · [Git](#git-relay-bank-pull-push) ·
[Data](#data-zones-sealing-savepoints) · [Network](#network-levels-exceptions-and-port-forwarding) ·
[Logins and secrets](#logins-and-secrets) · [Service logins](#service-logins) · [Shares](#shares-skills-and-reference-material-for-boxes) ·
[Dependency sources](#dependency-sources-devc-src) · [Notes and memory](#notes-and-memory) ·
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
| **Harness** | A coding agent devc knows how to install, log in and start: Claude Code, Codex, pi, the Cursor CLI and OpenCode are built in; others are described by a manifest. |
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

`build_from_repo` and `capabilities.nested` are accepted for features that
are not available yet and are refused with a message.

State lives in `~/.local/share/devc` (`--state-dir`, `DEVC_STATE_DIR` or
`state_dir`): `durable/` holds what is worth backing up (relays, journals,
context repositories with the data manifests, harness state, secrets,
published shares),
`local/` what belongs to this machine (box homes, savepoint repositories, the
staging directories of data pulls), `cache/` what can be deleted (the realms'
package caches and dependency sources), and
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

**What a running box was started with.** A box gets on demand (secrets from
the daemon, SSH hosts, the allowlist proxy, GitHub tokens, services) only
what its configuration granted when it was started and still grants; a grant
you add reaches it after `devc down` and `devc up`. Only a start through
devc records what that is. A command that finds the box running (`up`, `sh`,
`run`, `root`, a session, from the command line or the web app) never records
it again: if the box has not reported ready since its start through devc (or
something in it removed `/run/devc/ready`), or there is no record of its
start, the command refuses and asks you to restart the box with `devc down`
and `devc up`.

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
sandbox.

Journals have budgets, because a box decides how many events it writes:

```toml
# project.toml
[sessions]
session_journal_max = "128m"   # one session's journal (default)
journal_quota = "2g"           # all journals of the project (default)
```

Before devc appends events to a journal it checks both budgets and keeps
1 GiB free on the state file system. Each journal counts at least 8 KiB
toward `journal_quota` (its blocks and the session's owner record), so many
small sessions use up the budget as one large one does. Events that do not
fit stay in the box's spool (within the spool's own limits, the runtime
directory); devc writes one note into the session's journal saying which
budget was reached, `devc ps` warns about it and `devc doctor` reports it
(check `journals`). Nothing already in a journal is removed, and ingestion
goes on by itself once there is room. Make room by raising a budget, or
remove the journals of sessions that ended long ago:

```sh
devc sessions prune demo/hello --before 30d          # ended more than 30 days ago
devc sessions prune demo/hello --before 2026-10-01   # ended before that date
```

`prune` never removes a session that has not ended and does nothing without
`--before`; what a hub has archived stays there. Each harness also gets a generated description of the box (paths,
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
devc request pr --title "Fix parser"     # high risk: push the current branch and open a pull request
devc request comment 42 --body "CI is green again"   # high risk: comment on #42
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
allow` (see [Network](#network-levels-exceptions-and-port-forwarding)); an
approved pull request runs `devc pr create --yes` and a comment `devc pr
comment` (see [GitHub](#github)). Requests need the daemon; without it, `devc request` tells the agent to ask
you directly.

A savepoint a box requests is of its own class (`requested` in `devc data
ls`): at most one is made per `data.savepoints.requested_interval` (default
`10m`), and a request inside that interval, or while another savepoint
request of the project is waiting, is refused with the reason. Only the
newest `data.savepoints.requested_keep` (default 10) requested savepoints
are kept, whatever name the box gave them. They never count toward
`data.savepoints.keep`, and nothing a box requests makes your own savepoints
go.

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

### The archive on the hubs

Every hub a machine joined also keeps an archive of the machine's durable
state, so a machine can be wiped or replaced and its work brought back.
For every realm with `[sync] hub = true` (the default; `false` keeps a realm
off the hubs) the daemon uploads, in the background and rate-limited
(`[sync] upload_rate` in `config.toml`, default 4 MiB/s per hub), what each
hub lacks:

- session journals, request, network and gate logs and the savepoint index,
  as they grow (a log that was compacted or rotated is uploaded again as a
  new generation; the old one stays);
- relays and the context repository as git bundles of new objects, with a
  log of their refs;
- the shared harness state (transcripts, shared homes) except credential
  files. It is box-writable, so the daemon reads it with the same care as
  everything else a box writes: links, FIFOs and other special files, names
  outside a plain character set and files above `[sync] max_file_size`
  (realm, default 256 MiB) are left out and counted. Transcripts (the
  harness manifest's transcript globs) are uploaded as they grow, and every
  version of one is kept. Any other file there gets a new version for any
  change, at most one per `[sync] mutable_interval` (`config.toml`, default
  6 hours), copied first so a version is one state of the file (a file that
  grows beyond `max_file_size` while it is copied is left out). The hub
  keeps the newest three versions and, by its own clock, the version each
  such file had at the start of each of the last 7 days and 4 weeks (the
  hub's `[archive] keep_daily` and `keep_weekly`), so neither a box nor a
  machine can replace every archived version of a file. Only shared
  harness state can be versioned that way; the hub keeps every version of
  journals and logs whatever a machine says, and a file keeps the kind it
  was first archived as (after a manifest change, devc follows the hub).
  Permission bits travel (an executable hook stays executable; set-ID bits
  never do).

Secrets never travel in the archive. A hub only ever adds: a machine can
append to its own archive and read every machine's, never change or delete
anything (the hub's owner prunes, on the hub). Uploads use their own
connections and never hold up events or commands.

```sh
devc sync status [--now]          # per hub and realm: what is behind, what was left out, the last error
devc state pull work --from https://hub.example.org --list
devc state pull work --from https://hub.example.org [--machine old-laptop] [--merge]
devc state pull work --from https://hub.example.org --as-of 2026-11-02    # or --as-of 2026-11-02T08:00:00Z
```

`devc state pull` restores one machine's archive of a realm (by default this
machine's own, by the name the hub knows it by) into the local durable
state: journals and logs, relays and the context repository rebuilt from
their bundles, the shared harness state. Each file comes from its newest
complete version; a newer one whose upload did not finish is reported. It
refuses a realm that has durable state already, unless `--merge`, which
adds only the files and refs that are missing (a ref that exists with
another value is reported and kept). A realm is restored only under its own
name (also by `devc state import`): its journals and logs name it inside. Sessions restored this way appear among this machine's sessions.

With `--as-of <time>` (RFC 3339, or a date: the start of that day in UTC)
each file is restored as the hub held it then: the newest version it
completed at or before that time, by the hub's clock. A version completed
on a hub that did not yet record times counts as older than every version
with a time: when every timed version is newer, the newest version without
a time is restored, and the command lists such files ("restored from a
generation without a recorded time"; it may be newer than the time asked
for). A file whose versions are all newer is left out. Use it to get back a database a box broke:
the start of every one of the last 7 days and 4 weeks is kept. Journals and
logs come from the version in use then, with what was appended to it since
(they only grow). What has no versions is taken as it is now, and the
command says so: relays and the context repository (their history is all
there; refs are restored as they are now), and files archived before the
hub recorded completion times. A
hub that reports a full quota, or refuses an upload otherwise (a 4xx answer), says so in
`devc sync status` and `devc hub status`, for the realm concerned: that
realm's uploads rest for ten minutes at a time until it has room, while the
other realms go on. A quota stops all of the realm's uploads (its files and
repositories share it); another refusal of its files lets its repositories
go on.

Every enrolled machine can read every machine's archive on a hub, so the
hub holds all archived realm state of the fleet: treat it like the
machines themselves.

### Secrets on another machine

```sh
devc secret push work --to https://hub.example.org      # asks for a passphrase twice
devc secret pull work --from https://hub.example.org [--machine laptop] [--overwrite]
```

`push` encrypts every secret of the realm on this machine with a key derived
from a passphrase (argon2id, XChaCha20-Poly1305) and stores the result on
the hub, which keeps the last ten versions of each machine and can read
none of them. `pull` fetches the newest one (of one machine with
`--machine`), asks for the passphrase and stores the secrets that are
missing; existing ones with another value are kept unless `--overwrite`.
The passphrase comes from a hidden prompt, or one line on stdin when stdin
is not a terminal; it is never an argument. Harness logins of `shared-home`
harnesses are not secrets in this sense and are never carried: log in on
each machine.

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
  sends it with the same ID, so it is typed at most once. Only the receipt
  settles a prompt: a prompt in the session that looks like it (the box
  reports those) is noted next to it, but never takes it off the screen.
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
relay accepts it (an update whose bank ref cannot be written is refused); it
counts once the push has ended and git applied the update, and the record of
an update git refused (a name that clashes with an existing branch) is
removed then, or at the next push or bank if the daemon ended first. And
branches matching `git.forward` are pushed on to the upstream (the push prints
the result). A push may update at most 1000 refs (push in parts), its pack may
be at most `git.max_push_size` (default `2g`), and it is refused as a whole
when the relay would grow beyond `git.relay_quota` (default `20g`). The relay
keeps a push's objects in the pack they came in, the daemon starts a push
only with at least 1 GiB free on the state file system, and it ends a push
while git writes it once the relay would exceed its budget or less than
512 MiB would stay free (the daemon's log says why). Ending a push ends
every git process it started (index-pack, hooks) before the daemon removes
what the push left and lets the next one in. If one of them cannot be
ended (stuck in the kernel, for example on a hung file system), the daemon
logs it, leaves the relay as the push left it and refuses further pushes to
it with a message saying why; once the last of those processes has ended,
it removes what the push left and takes pushes again, without a restart.
A bank is bounded the same way: it first lists the box's branches, snapshot
refs and tags (at most 50,000), and fetches by name only those the relay
does not record yet. More than 1000 changed branches, new snapshots or new
tags refuse the bank before anything is fetched (remove some in the box).
The fetch starts only with 1 GiB free and the relay within
`git.relay_quota`, and is ended like a push when the relay would exceed its
budget or less than 512 MiB would stay free; it leaves no temporary pack,
earlier bank refs stay, and the bank reports that the work was not
preserved. Before serving a fetch, the daemon fetches the upstream into the
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

## GitHub

By default a box holds no GitHub credential: git goes through the relay, and
everything else GitHub offers reaches the box through the daemon, which uses
**your own credential**: the realm secret `github-token` (`devc secret set
<realm> github-token`, a fine-grained or classic token), otherwise whatever
`gh auth token` prints when `gh` is installed and logged in. That credential
never enters a box: not in a response, a message, a log the box can read or
an environment. The project's repository comes from its `upstream`
(`https://github.com/owner/repo`, `git@github.com:owner/repo.git`, with or
without `.git`); for projects elsewhere every command below says "not a
GitHub project".

**Reading, in the box.** `devc gh` is read-only and covers the project's own
repository plus those listed in `grants.github_read = ["owner/repo", …]`
(with `--repo`):

```sh
devc gh pr list [--state open|closed|all] [--limit n]
devc gh pr view 12 [--comments] [--diff]
devc gh issue list / devc gh issue view 7 [--comments]
devc gh run list [--branch feature/x]
devc gh run view 123456 [--log] [--failed]     # logs: the last 5 MiB
devc gh api repos/owner/repo/commits?per_page=5   # a raw GET, JSON as received
```

The daemon builds every request itself: a box names an operation, a number or
a path below `repos/<owner>/<repo>/`, never a URL, a method (only GET) or a
header; paths with `..`, empty segments, percent-encoding or another
repository are refused before anything leaves the daemon. Answers are passed
on as GitHub sent them, up to 5 MiB (logs keep their end and say what was
cut); `--json` prints the JSON, otherwise a short readable form. GraphQL is
not offered. At most 120 reads a minute per project. `grants.github_read`
is a grant like the others: a repository added while the box runs is served
after the box is brought up again.

**Pull requests and comments, from the host.**

```sh
devc pr create demo/app --branch feature/x [--title t] [--body b | --body-file f] [--base main] [--draft] [--yes]
devc pr comment demo/app 12 --body "Looks good"
```

`pr create` banks, pushes the branch to the upstream exactly as `devc push`
does (it shows what the upstream lacks and asks, unless `--yes`), then opens
the pull request and prints its URL. The title defaults to the subject of the
branch's newest commit, the base to the project's default branch. Before
anything is pushed it refuses a branch that is the base, the project's
default branch or the upstream's (the relay's `HEAD`), a branch matching
`git.protected`, and one the relay does not have. Both refuse while the
project's configuration has problems. An agent asks for the same
with `devc request pr` (its current branch unless `--branch`) or `devc
request comment <n> --body …`; both are high-risk requests, approved only
with `devc requests approve` on this machine, and an approved pull request
request publishes the branch: the approval is the confirmation.

**Tokens in the box (optional).** `grants.github` chooses what the box holds:

| Mode | In the box |
|---|---|
| `relay` (default), `none` | nothing |
| `token:<secret>` | the realm secret `<secret>`, a token you created (a fine-grained personal access token for the one repository) |
| `app` | installation tokens of a GitHub App you own, for the one repository, valid for an hour |

In both token modes the box's repository gets a second remote, `github`, at
the upstream's HTTPS URL, and devc's git credential helper (`devc box
git-credential`) for the GitHub host, so `git push github <branch>` goes to
GitHub directly while `origin` is still the relay; sessions get the token as
`GH_TOKEN`, so the real `gh` works. The helper and the launcher ask the
daemon each time, so git always gets a current token; **a long-running
process keeps the `GH_TOKEN` it started with, for at most an hour** with an
App. The token never lives in a file of the box. The mode is a capability of
the box's start, and so are the repository and, for `app`, the realm's App
and installation: changing any of them (the project's `upstream`, `app_id`,
`installation_id`) stops tokens until `devc down` and `devc up`, and
removing the mode stops them at once. Token delivery needs the daemon.

Neither `github-token` nor the App's key secret can be granted to a project
as an ordinary secret (`grants.secrets`, or a harness's credential): the
configuration check refuses it, as it refuses a service's credential, since
in the box the key would mint tokens for every repository of the
installation.

The App mode needs, in `realm.toml`:

```toml
[github.app]
app_id = 123456
key_secret = "github-app-key"   # devc secret set <realm> github-app-key < app.private-key.pem
installation_id = 7890123       # optional: otherwise looked up for each repository
```

and optionally, per project, the token's permissions (default `contents` and
`metadata` read; `workflows` is always refused):

```toml
[grants]
github = "app"

[grants.github_token]
permissions = { contents = "write", pull_requests = "write", issues = "read" }
```

devc signs a short JWT with the App's key (RS256) and first checks that the
installation covers exactly the project's repository: the installation
GitHub names for the repository must belong to its owner (and be the
configured `installation_id`, if any), and a metadata-only probe token for
the repository's name, revoked at once, must see that one repository; its
ID is noted. devc then mints a token restricted to that repository ID and
these permissions, and reuses it until five minutes before it expires.
Tokens are cached per project and per App key, so changing the key never
hands out a token minted with the old one.

**The trade-off.** With a token in the box, the relay's ratchet no longer
covers what that token can do: the agent can push to any branch the token
reaches, rewrite or delete branches on GitHub, and act on issues and pull
requests within its permissions. Protect the default branch (and release
branches) on GitHub with a ruleset, give the token only the permissions the
task needs, and prefer the default mode, where nothing reaches GitHub without
you.

**Setting up the App** (once, on github.com):

1. Settings → Developer settings → GitHub Apps → New GitHub App (for an
   organisation: the organisation's settings, so the App belongs to it).
   Name it, set any homepage URL, untick "Webhook → Active".
2. Repository permissions: the most any project will ask for, for example
   Contents read and write, Pull requests read and write, Issues read,
   Metadata read. Leave Workflows at "No access". "Where can this GitHub App
   be installed?": "Only on this account".
3. Create it, note the App ID, and under "Private keys" generate a key; store
   the downloaded `.pem` with `devc secret set <realm> github-app-key <
   file.pem` and delete the download.
4. "Install App" on your account or organisation, for all repositories or
   the ones your projects use. The installation's ID is the number at the end
   of its settings URL (optional in `realm.toml`).
5. On each repository: Settings → Rules → Rulesets → New branch ruleset,
   target the default branch, restrict deletions, block force pushes and
   require a pull request; add yourself (not the App) as a bypass actor if
   you push directly.

Nothing in devc has been tried against GitHub itself; the tests use a fake
API and git server.

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
`keep` limits the number of your unnamed savepoints; `before_session = true` takes
one before a session starts (at most one per 30 minutes; a savepoint the box
requested does not count). Savepoints a box requests are kept apart, with
their own limits (`requested_keep`, `requested_interval`; see [Requests from
a box](#requests-from-a-box)); savepoints taken before devc had these
classes count as yours. All data
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
be larger in total. The count comes from rsync's dry run; a dry run whose
output is cut (more than 256 MiB of listing) or has a line devc cannot read
is refused rather than counted in part, so pull fewer paths at a time if
that happens. A pull records at most 100,000 entries (the direct children
of each zone it names, and the other paths named), and only as many as the
report of the merge can hold with their names; a pull above that is
refused before anything is transferred (checked against the dry run) and
again before anything is merged (checked against what arrived). One pull or push per project and remote runs at a time; a
second one is refused until the first has finished. Every pull transfers the selected files in full, since the
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
`<path>/.devc-backup/<time>-<id>/` (the ID makes every push's directory its own). New files keep their permission bits, limited
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
that differs from the entry now. For a remote, a pull records what arrived
from it separately from this machine's copy after the merge, and what the
merge did with the entry (`merged`, `skipped` for a sealed entry left alone
because it differs, `partial` when something in it was left out, such as a
special file). A remote is shown as matching only when its last pull merged
the entry completely and the entry still lists as what arrived; a skipped
or partial pull, or one that kept files that exist only here, never is, even
when the listings (names and sizes) are equal. Pull records written before
C19r4 have no outcome and are not shown as matching. A `>` marks a remote that a push sent the
entry to as it is now; the remote may keep other files besides, since devc
never deletes there.

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
   changes, reaches nothing. While the host's own addresses cannot be
   listed, it connects nowhere and says why, trying again at each request;
4. the decision goes to `durable/realms/<realm>/netlog/<project>.jsonl`.

Resolving and connecting take a moment: the rules are checked again once
the connection is made, before the box gets its answer or a byte it sent
is passed on, so an exception that ends meanwhile lets nothing through.

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
  192.168/16 and 100.64/10, but never the host's own addresses, the
  engine's aliases for the host such as `host.containers.internal` and
  slirp4netns's gateway, or link-local),
  become elements of nftables sets in the box's namespace with a timeout:
  they expire in the kernel without any process. They vanish when the box
  stops; `devc up` puts back those still valid, after its checks.
- `host:<port>` makes a service listening on the host's `127.0.0.1:<port>`
  reachable at `127.0.0.1:<port>` inside the box: devc's agent listens there
  and relays over the box channel, and the daemon connects on the host only
  while the exception is in force; revoking or expiry also ends open
  connections. It needs the daemon.
- At level `allowlist`, a name and port is added to that box's allowlist for
  the duration; addresses and `lan` are refused there. Connections through
  the proxy end when what allowed them goes: an exception that expires or
  is revoked, or an allowlist entry that is removed.

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
about that. Each connection runs one small process on the host, so a
forward serves at most 64 connections at once and the router of local URLs
at most 256 to all boxes together; a connection beyond that is closed at
once (the forward's log, or the daemon's, says so once a minute).

**Local URLs.** While the daemon runs, it serves
`http://<port>.<project>.<realm>.localhost:7480/` for every port listed in
the project's `box.ports` and every port with a running `devc forward`
(which prints the URL), WebSockets included. Browsers and curl resolve
`*.localhost` to the loopback address themselves. Other ports get a 404 that
says how to allow them. The router listens on `router.listen` in
`config.toml` (a loopback address, or `"off"`), routes only by that host
name (a page that points its own name at 127.0.0.1 reaches nothing), and
reaches the box the way a forward does. Each local URL is its own origin:
the router refuses a WebSocket or a form submission (any method but GET and
HEAD) from a page of another origin, and any request a browser marks as
cross-site except following a link, so a page of one box cannot use
another box's service through your browser. A frontend on one port that
calls an API on another port of the same box is cross-origin too; serve
both from one port. Programs that are not browsers (curl) are not
affected.

## Watching a browser or a desktop

Sometimes you want to see a graphical program running in a box, usually a
browser an agent drives. There are two ways, the cheaper first.

**A browser's debugging port.** Chromium (and Chrome) can be watched and
driven from another Chromium through its debugging port. Start it in the box
with

```sh
chromium --remote-debugging-port=9222 --remote-debugging-address=127.0.0.1 --no-sandbox
```

(`--headless` works too; `--no-sandbox` is needed because Chromium's own
sandbox uses nested user namespaces, which a box blocks; the box is the
sandbox). The sandbox description tells agents this. On the host:

```sh
devc browser demo/hello                 # forwards 127.0.0.1:9222; Ctrl-C ends it
devc browser demo/hello --detach        # in the background (devc forward ls / stop)
devc browser --port 9333 --host-port 19333
```

It forwards the port like `devc forward` and prints the DevTools endpoint
(`http://127.0.0.1:9222/json/version`). In Chrome or Chromium on the host,
open `chrome://inspect`, choose *Configure…* next to *Discover network
targets*, add the printed address, and the box's pages appear under *Remote
Target*, each with *inspect* for a live, interactive view. If no browser
answers yet, devc says what to start. Whoever can connect to the forwarded
port controls that browser completely; like every forward it listens on the
host's loopback interface.

**A desktop.** For a headed browser, or any other X program, devc runs a
desktop next to the box:

```sh
devc gui up demo/hello                  # starts the box too; prints the viewer's address
devc gui status demo/hello              # absent, running, outdated, stopped or stale
devc gui down demo/hello
devc gui up demo/hello --restart        # new cookie and password
devc gui build [--rebuild]              # the image; built on first use anyway
```

`devc gui up` starts a second container, the *sidecar*, from the image
`localhost/devc-aux/desktop` (Debian with TigerVNC's server, the
matchbox window manager, noVNC and websockify; no browser, which runs in
the box). The image is built on first use, which needs the network once.
The sidecar joins the box's network namespace (and, so that the engine can
set it up there, the box's user namespace) and nothing else: no mounts but
a tmpfs, a read-only root, no capabilities, no new privileges and the box's
seccomp profile. Nothing is added to the project's image. It runs:

- an X display, `DISPLAY=127.0.0.1:1` (X on `127.0.0.1:6001`), which accepts
  only clients with its cookie;
- a VNC server on `127.0.0.1:5901`, which accepts only the password;
- the noVNC web viewer on `127.0.0.1:6080`.

All three listen on the loopback interface of the box's namespace only;
devc forwards the viewer to the host (port 6080 on `127.0.0.1`, or a free
port, `--host-port` and `--bind` as for `devc forward`) and prints its
address with the password, for example
`http://127.0.0.1:6080/vnc.html#autoconnect=1&resize=remote&password=Ab3dEf9h`.
The password is after `#`, which the browser does not send anywhere; it is
new at every start. `devc gui up` on a running desktop prints the same
address again (and forwards the viewer again if that forward has ended). A
VNC client works too: `devc forward 5901` and the same password.

Programs in the box draw on the display: the cookie is in
`/run/devc/display.xauth`, and while the desktop runs
`/etc/devc/display.env` holds `DISPLAY` and `XAUTHORITY`. Agent sessions and
`devc sh` started after `devc gui up` have both set; elsewhere use
`set -a; . /etc/devc/display.env; set +a`. Windows open full-size.
`/opt/devc/bin/devc box xcheck` in the box says whether the display
accepts.

**The cookie is readable in the box on purpose: anything in the box can
drive and read the display**, watch what you type into the viewer and take
what you paste into it. Do not type secrets into programs in the viewer that
you would not give the box.

The desktop shares the box's network namespace, so it shares the box's
level: at `none` and `allowlist` it has no network either, at `internet` the
box's rules apply to it. Joining leaves the box's start, its readiness and
its rules alone; devc checks that after the sidecar has started and stops
the sidecar otherwise. `devc down`, `devc rm`, a recreation and every start
of the box remove the sidecar first (a sidecar must not outlive the box
start whose namespace it joined); `devc gui status` calls a sidecar of an
earlier start *stale*. The desktop fails to start if a process in the box
already listens on port 6001, 5901 or 6080.

The sidecar has a cgroup of its own, beside the box's, and box programs make
it allocate (pixmaps in the X server, a relay process per connection). It
therefore gets limits of its own, set under `[gui]` in `config.toml`:
`memory = "2g"`, `pids = 512` and `cpus = 2` by default. As for boxes, a
limit that cannot be applied (its cgroup controller is not delegated) makes
`devc gui up` fail; `memory = "none"`, `pids = -1` and `cpus = -1` switch a
limit off explicitly. The X relay serves at most 64 connections at a time
and the viewer's server 16; further connections wait. A box that holds
those connections, or fills the desktop's memory, only disturbs its own
desktop.

A running desktop records the limits and the image generation it was
created with. `devc gui up` reuses it only when both are what it would
create now; after you change `[gui]`, or upgrade to a devc whose desktop
image differs (including a desktop started before these records existed),
`devc gui up` refuses the running desktop and names the difference, and
`devc gui status` calls it *outdated*. It is never replaced silently,
since programs on its display would lose it: `devc gui up --restart`
replaces it when you are ready. The same holds when devc has lost its
record of a running desktop (its password and forward).

**From a phone**, the viewer is reached like any forwarded port. Exposing it
beyond the loopback interface needs the same care as the hub
(see [docs/hub-deployment.md](hub-deployment.md)): the VNC password has
only eight characters, and noVNC over plain HTTP is readable by anyone on
the network. Put it behind an authenticating TLS proxy, or a private
network such as `tailscale serve`, rather than using `--bind` with a LAN
address.

## Logins and secrets

**Harness logins** are stored per realm, so every box of a realm uses the same
login:

```sh
devc login demo claude                 # runs `claude setup-token` in a throwaway container, then asks for the token
devc login demo claude --token-stdin   # or read the token from stdin
devc login demo codex                  # runs Codex's device login with the realm's shared Codex home
devc login demo pi                     # `pi auth login` with the realm's shared pi directory
devc login demo cursor                 # `cursor-agent login`; the login lands in the realm's shared ~/.config/cursor
devc login status demo
```

How a harness logs in is part of its manifest: `env-token` stores a token and
hands it to the harness in an environment variable, `shared-home` keeps the
vendor's own credential file in a home directory shared by the realm's boxes,
`api-key` stores a key you paste. Boxes write that shared home, so what a
login or status command prints reaches your terminal escaped: control
characters show as `�`, so a login's colours and cursor movements appear as
`�[…` while its text (a URL, a code) stays readable.

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

**pi, Cursor and OpenCode** (built-in manifests written from the vendors'
documentation; none has been tried against a real account, see
`docs/dev/LEFTOVER.md`):

- **pi** (`devc harness install pi`) needs Node.js; installing pi installs
  the toolbox's pinned Node.js first (`devc toolbox install node` does it on
  its own), and pi runs with that Node first in `PATH`. Its agent directory
  `~/.pi/agent` (login, settings, sessions) is shared across the realm. devc's
  instructions reach it with `--append-system-prompt`. pi has no approval
  prompts at all, by design; the box is its sandbox.
- **Cursor CLI** (`cursor-agent`) shares both of its directories, `~/.cursor`
  and `~/.config/cursor` (which holds the login), across the realm. Instead
  of a login, a dashboard API key works: `[harness.cursor] auth = "api-key"`
  in `realm.toml`, then `devc login <realm> cursor` stores it and sessions
  get it as `CURSOR_API_KEY`. Cursor has no instruction file outside the
  project, so it gets no baseline or sandbox description.
- **OpenCode** keeps everything in the box (each box logs in with `opencode
  auth login` inside it): its database holds sessions and credentials and is
  fragile when shared. devc's managed configuration in `/etc/opencode`
  names the instructions file.

A manifest names the credential files in its shared state with
`home.secret_files` (paths below `/home/dev`, `*` and `?` allowed; a
directory covers everything below it): Codex's `~/.codex/auth.json`, pi's
`~/.pi/agent/auth.json` and Cursor's login. They never enter the hub
archive or a state export and are never restored (see "Backups"). These
harnesses rotate their tokens, so a login copied to a second machine breaks
on one of the two: log in once per machine and realm. Claude Code's token
does not rotate; it is a realm secret, which `devc secret push` carries.

A manifest can ask for toolbox components with `install.needs = ["node"]`,
keep state in several home directories (`home.also`), and pass extra
arguments when a structured input adapter is chosen (`input.args`). pi's
structured adapter is an extension devc renders into `/etc/pi`: with your
own copy of the pi manifest saying `input.adapter = "extension"`, pi loads
it and the daemon hands messages to pi's API instead of typing them; while
no extension is connected it types them as before, and the receipt says so.

## Service logins

Some services have no tokens an agent could hold, only a login with a
password, or a token you do not want inside a box. A **service** gives
boxes an authenticated, filtered way to use such a thing without the
credential ever entering a box: the daemon holds the credential and proxies
the box's requests, adding it on the way out.

```toml
# realms/demo/services/wiki/service.toml
upstream = "https://wiki.example.org"   # scheme and host; plain http needs allow_insecure = true
port = 8601                             # loopback port in granted boxes (above 1024, unique in the realm)

[auth]
kind = "header"                         # header | cookie
header = "Authorization"
value_secret = "WIKI_TOKEN"             # realm secret; may hold a whole header value ("Bearer …")

[[allow]]                               # first match wins; everything else is refused
methods = ["GET", "HEAD"]
path = "/api/"                          # prefix at segment boundaries

[limits]
max_body = "10m"                        # each body, request and response
timeout = "60s"                         # the whole exchange, the box's side included
```

```toml
# project.toml
[grants]
services = ["wiki"]
```

```sh
devc secret set demo WIKI_TOKEN
devc service ls [realm] [--json]
devc service status demo wiki [--json]  # exit 1 while it needs a login or its secret
devc service log demo wiki [-n 50] [--json]
```

In a granted box the agent listens on `127.0.0.1:8601`, and programs use
`http://127.0.0.1:8601/…` (the sandbox description lists each service, its
address and its rules). Every connection is relayed over the box channel to
the daemon, which decides everything:

- The service must be **in force for the box**: granted when the box
  started and now, with a definition whose upstream, port, credential and
  rules have not changed since. A new grant or a changed definition needs
  `devc down` and `devc up`; removing a grant applies at once. Services work
  at every network level, `none` included (they are an explicit grant), and
  only while the daemon runs.
- The request is checked on its raw bytes first. Refused with 400: anything
  but `METHOD /path HTTP/1.1` (no absolute URLs, no `CONNECT`), `.` or `..`
  segments (also before a `;`), empty segments, encoded `/`, `\` or `%`,
  any other percent-encoded reserved character (`%3B` for `;`, `%3A`, `%40`
  and the like, which an upstream might decode before it splits the path:
  write them unencoded), a `Host` other than `127.0.0.1:<port>` or `localhost:<port>`, duplicated or
  conflicting `Content-Length` and `Transfer-Encoding`, obsolete line
  folding, bare line feeds; protocol upgrades (WebSocket) are refused with
  403. One request per connection.
- The **rules** compare the method and the normalised path (encoded letters,
  digits and `-._~` decoded). A request no rule allows gets 403 with the rule
  that would be needed, so the agent can ask you for it. Methods are the
  standard ones (no `CONNECT`, `TRACE`); `HEAD` is allowed only where
  listed.
- The daemon forwards over TLS with the host's trust store (`SSL_CERT_FILE`
  and `SSL_CERT_DIR` apply), never through a proxy, with `Host` set to the
  upstream's. It removes the box's `Authorization`, `Cookie`, `Proxy-*`,
  `Forwarded`, `X-Forwarded-*` and `Via` headers and the credential header
  before adding the credential; `Origin` and `Referer` of the local address
  become the upstream's.
- On the way back `Set-Cookie`, `Proxy-*` and hop-by-hop headers are
  dropped, `Location` headers that point at the upstream are rewritten to
  the local address, 1xx responses and trailers never pass, and every header
  is scrubbed of the credential (the value and its parts: every word except
  a leading scheme such as `Bearer`, `Basic`, `Token` or `Bot`, and the
  user:password and password of `Basic`). Bodies pass through within
  `max_body`. A stored credential is used in one normal form: spaces and
  tabs around it are removed (HTTP drops them on the wire anyway), and a
  value with control characters or bytes outside printable ASCII is
  refused. Every secret word of it must have at least 8 characters, so that
  scrubbing it cannot garble ordinary headers: a shorter header credential
  is refused (`devc service status` says `invalid-secret`, boxes get 503),
  and so is a login whose cookies have shorter values (cookie values are
  trimmed the same way). A request to a path that none of the stored
  cookies applies to goes out without a `Cookie` header.
- The whole exchange ends with `timeout` (and a few seconds' grace): a box
  that stops reading the answer, or never finishes sending its body, loses
  that connection then, and only that connection.
- Each request is logged to `durable/realms/<realm>/service-log/<name>.jsonl`:
  time, box, method, path (without the query) and status. Headers and bodies
  are never logged.

**Cookie services** (`kind = "cookie"`) need a login:

```toml
[auth]
kind = "cookie"
login = ["bash", "login.sh"]            # prints the cookies as JSON
secrets = ["INTRANET_USER", "INTRANET_PASSWORD"]
login_path = "/login"                   # a redirect there means the session expired
refresh = "12h"                         # how long a login is trusted
auto = false                            # log in again by itself
# image = "…"                           # default: devc's installer image (bash, curl)
```

`devc service login demo intranet` runs the login command in a throwaway
container: the network on, nothing mounted but a scratch directory at
`/login` (the working directory) with the other files of the service's
directory and the secrets of `auth.secrets` in `/login/secrets/`. The
command prints a JSON array of cookies (`name`, `value`, `domain`, `path`,
`expiry` in Unix seconds, 0 for none) and nothing else on standard output;
devc refuses anything else, cookies for other hosts, more than 64 cookies,
values over 4 KiB or under 8 characters (leave short cookies, such as a
language or a consent flag, out of the output). Errors about the output
name cookies by their position and never repeat what was printed, and every
error of a login is cleaned of the login's secret values before it is
shown, logged or stored. The jar is stored as the realm secret
`_jar.<service>` with the time of the login (it cannot be granted to a box,
nor can the login secrets of a service the project uses).
`examples/services/` has a worked example (a form login with curl) and notes
on what a browser-driven login would need.

When the jar is older than `refresh`, its cookies have expired, or the
upstream answers 401 or redirects to `login_path`, the service **needs a
login**: boxes get 503 with what to tell you, the live sessions of the box
get a note (once per need), and `devc service ls` and `status` say so. The
daemon logs in by itself only with `auto = true`: once at a time per service
(requests that arrive meanwhile wait for it) and at most once a minute,
through `devc service login` as a child process. Leave `auto` off for
accounts that lock after failed logins.

**Limits.** The proxy keeps the credential out of headers, logs and error
messages, not out of bodies: an upstream endpoint that reflects request
headers in its body (an "echo" or debug endpoint, some error pages) can still
show the credential to the box, so rules should not allow such endpoints.
Allowed requests act with your account: keep rules narrow (read-only where
possible). A login with a second factor cannot run unattended; a TOTP seed
stored beside the password collapses both factors into one secret. Many
services forbid automated access or sharing credentials with tools: check
the proxied service's terms of use before setting one up.

## Shares: skills and reference material for boxes

A share is a directory you publish from a project and mount read-only into
other projects' boxes: a skill, a tool, reference material. Because it puts
text in front of other agents, possibly in another realm, publishing is a
deliberate step on the host, and only committed content can be published:

```sh
devc share publish review --from demo/tools --path skills/review [--ref main] [--yes]
devc share ls [--json]                 # shares, their current version and source, who lists them
devc share log review                  # every published version with its source commit
devc share rm review [--yes]
```

`publish` reads the directory from the project's **relay** with git (bank the
box first: `devc bank demo/tools`), at the default branch or `--ref` (a
branch or a commit), never from the project directory, so uncommitted work
in the box is not published. It shows the change against the current
version (git's file list and diff, made printable; for a new share, or one
last published from another project, the list of files) and asks before
publishing; without a terminal it needs `--yes`. The new version is
unpacked into `durable/shares/<name>/<version>/` by devc's safe extractor:
symlinks that leave the directory, special files and oversized trees are
left out (and listed). Old versions are kept; `current` points at the
newest.

Boxes get the shares their project and realm list:

```toml
# project.toml (or realm.toml, whose list every box of the realm gets as well)
shares = ["review", "styleguide"]
```

In the box each share is at `/opt/shares/<name>`, read-only (its earlier
versions are visible below `/opt/shares/.v/<name>/`). A share whose
top directory holds `SKILL.md` is a skill: box init links it into
`~/.agents/skills/<name>` (read by Codex, pi, OpenCode and Cursor) and
`~/.claude/skills/<name>` (Claude Code), and removes its links to shares the
box no longer lists; your own entries there are left alone.

**When boxes see a change.** A new version of a share a box already mounts
is visible at once: the box mounts the share's whole directory and
`/opt/shares/<name>` follows its `current` link inside the box. A share that
becomes a skill only in a later version is linked at the box's next start.
Changing a project's (or realm's) `shares` list changes its box definition:
`devc up` then asks for `devc up --recreate`. `share rm` leaves running
boxes an empty `/opt/shares/<name>`.

Every box also has the built-in skill `dep-source` (at
`/opt/shares/dep-source`, from the toolbox), which tells agents how to use
`devc src`.

## Dependency sources: `devc src`

Installed packages are often not their source (an installed R package has no
`R/*.R` files, a wheel no C code, an npm package built output). In a box,
`devc src` downloads the source of a dependency into the realm's cache and
prints the path:

```sh
devc src r:dplyr                   # the installed version, from DESCRIPTION in R's library paths
devc src pypi:requests             # the version installed in the active Python environment
devc src npm:@types/node           # the version in node_modules (searched upwards from the current directory)
devc src npm:react@18.3.1          # an explicit version
devc src github:tidyverse/dplyr@v1.1.4   # a branch, tag or commit (the default branch without @)
devc src pypi:requests --json      # path, version, source URL, whether the cache answered
```

Without a version devc uses the installed one, else the current release. An
R package installed from GitHub (its `DESCRIPTION` names a GitHub remote and
commit) is fetched from that commit instead of CRAN. Sources: CRAN's source
tarballs (the current release, or an older version from CRAN's archive),
the source distribution PyPI's JSON API names, the npm registry's tarball,
GitHub's tarball endpoint. Where a registry publishes a checksum it is
required and verified: PyPI's sha256, npm's integrity value (sha512,
sha384, sha256 or sha1; the strongest is used) or, for old packages without
one, its SHA-1 shasum, and the MD5 of CRAN's current releases. A missing or
unusable checksum there refuses the download rather than skipping the
check. CRAN's package list (`PACKAGES`) is read completely for every CRAN
download; if any of it cannot be parsed, nothing is downloaded. A CRAN
version that is not the list's current one is taken only from CRAN's
archive of old versions, which (like GitHub's tarballs) has no published
checksum and is downloaded unchecked; a version that is in neither is an
error. A GitHub tarball is named by its commit. Because nothing checks an
unchecked download, it must come from where it was asked for: a redirect
from CRAN's archive is followed only to the same path, one from GitHub's
tarball only to a tarball of the same owner, repository and commit (a
renamed repository is refused); anything else stops the download. Archives are unpacked with
the same safe extractor as shares and are never built, installed or run.
An exact version that is already in the cache (a CRAN or PyPI version, an
exact npm version such as `1.2.3` or `1.2.3-rc.1`, or a GitHub commit) is
answered without any network access. An npm version counts as exact only
in the plain form that npm itself takes for a version (no leading `v`,
numbers up to 9007199254740991, at most 256 characters), since only such
names can never be dist-tags. Anything that can move is resolved
with the registry every time: "the current release", an npm dist-tag such
as `latest` or a range, `v1.2.3`, a GitHub branch or tag. (A GitHub branch or tag is
never cached under its own name: devc asks GitHub for its commit on every
request and caches the tree under the commit, so a branch that moved is
followed.)

The cache is `cache/realms/<realm>/src/` in devc's state, mounted at `/src`
in every box of the realm as `/src/<ecosystem>/<name>/<version>/`. It is
**shared by the realm's boxes and writable by them**: one box could change
what another reads there. That is acceptable within one realm, which is one
trust domain anyway; boxes of other realms never see it.

`devc src` downloads with the box's own network. At level `allowlist` the
registry must be on the allowlist; a failed download names the preset to add
(`cran`, `pypi`, `npm`, `github`), or the agent can make a `devc request net`
request. At level `none` it cannot download. The base URLs can be pointed at
mirrors with `DEVC_SRC_CRAN`, `DEVC_SRC_PYPI`, `DEVC_SRC_NPM` and
`DEVC_SRC_GITHUB_API` in the box. For GitHub, a `GH_TOKEN` or `GITHUB_TOKEN`
in the box is sent to api.github.com (unauthenticated requests are limited
to 60 an hour).

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

## Backups

A realm's durable state is `durable/realms/<realm>/` in the state directory:
append-only journals and logs, git repositories (relays and the context
repository), the shared harness state and the realm's secrets. Three ways
to keep it safe, which can be combined:

1. **Point your backup tool at the state directory** (or just `durable/`).
   A filesystem snapshot is consistent, and so is a plain copy: the files
   are append-only or git, so the worst a copy taken while devc writes can
   hold is a partial last line or an unreferenced object. This is the only
   option that includes the secrets (as 0600 files: encrypt the backup).
2. **`devc state export <realm> [--out file.tar]`** writes one tar file for
   any backup tool: journals and logs as they are, relays and the context
   repository (committed first) as full git bundles, the shared harness
   state without credential files, never secrets. `devc state import
   <file> [--realm <realm>]` restores it into a realm without durable state
   (a fresh machine, or after `rm -rf` of the realm's durable directory).
   Carry secrets separately (`devc secret push`, or your password manager).
3. **The hub archive**, for machines treated as disposable: with one or two
   hubs joined, everything above except secrets is mirrored as it grows,
   and `devc state pull` brings it back on a new machine
   (`devc secret pull` the secrets). Logins of `shared-home` harnesses are
   repeated with `devc login`.

Box homes, caches, savepoint repositories and the toolbox are not durable
state; project directories (your work tree and data zones) are backed up
like any other directory, or through the relay (banked commits) and
`devc data push`.

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
- The hub archive is append-only for machines: what a box writes into
  shared harness state reaches it only as regular files read without
  following links, credential files never, and no machine can overwrite or
  delete what is archived. Secrets reach a hub only encrypted with your
  passphrase.
- Your GitHub credential stays on the host: `devc gh` reads through the
  daemon with requests it builds itself (GET only, the project's repositories
  only), and pull requests and comments a box asks for wait for your
  approval on this machine.
- Credentials of services (`grants.services`) stay in the daemon: a box gets
  only the requests the service's rules allow, with the credential added on
  the way out and scrubbed from the headers that come back.
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
  granted to the project, and the GitHub token of `grants.github = "app"` or
  `"token:…"`, which can do on GitHub whatever its permissions allow, beyond
  the relay's branch rules (protect branches with a ruleset).
- **Within a realm**, boxes share harness state, memory, the package cache
  and the dependency sources in `/src`; one box can influence the others. Use
  separate realms for work that must stay apart.
- **Shares** put text you publish in front of every agent whose box lists
  them, in any realm. devc publishes only committed content, shows the diff
  first and unpacks safely, but what the text says is up to you: read the
  diff before you confirm.
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
- **The desktop** (`devc gui up`) is the box's: anything in the box can drive
  and read its display and see what you type or paste into the viewer, and
  the viewer's page comes from a container that shares the box's network, so
  treat it like any page a box serves. It runs with memory, process and CPU
  limits of its own (`[gui]`). A browser's debugging port (`devc browser`)
  gives whoever connects to it full control of that browser, and devc's
  probe of that port never follows a redirect.
- **Services** act with your account for every request their rules allow,
  and an upstream endpoint that reflects request headers in its body can
  show the credential to the box: keep rules narrow (see
  [Service logins](#service-logins)).
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
