+++
id = "daemon"
title = "The machine daemon"
summary = "What `devc daemon` owns and how it runs: start-up and the lock, the API socket, a worker per running box, the journal follower and its subscribers, reconciliation and housekeeping, its goroutines, locks and budgets, and the vocabulary in internal/proto."
order = 30
packages = ["internal/proto", "internal/wsconn"]
files = [
  "internal/daemon/daemon.go",
  "internal/daemon/api.go",
  "internal/daemon/client.go",
  "internal/daemon/hub.go",
  "internal/daemon/turns.go",
  "internal/cli/daemoncmds.go",
]
key = [
  "internal/daemon.Daemon.Run",
  "internal/daemon.Daemon.goSafe",
  "internal/daemon.Daemon.watchOnce",
  "internal/daemon.boxWorker",
  "internal/daemon.boxWorker.ingestLoop",
  "internal/daemon.hub",
  "internal/daemon.hub.subscribe",
  "internal/daemon.Daemon.handlers",
  "internal/daemon.Daemon.reconcileLoop",
  "internal/daemon.Daemon.housekeep",
  "internal/daemon.Client.Serves",
  "internal/proto.Conn",
]
spec = ["doc:specs-v2.md#4", "doc:docs/dev/plan.md#3.11"]
tests = [
  "internal/daemon.TestDaemon",
  "internal/daemon.TestSubscribeReplay",
  "internal/daemon.TestSubscribeLive",
  "internal/daemon.TestSubscribeRaceWithReplay",
  "internal/daemon.TestSubscriberByteBudget",
  "internal/daemon.TestReplayFailure",
  "internal/daemon.TestTurnBanking",
  "internal/proto.TestCalls",
  "internal/wsconn.TestFrames",
  "test/integration.TestDaemon",
  "test/integration.TestDaemonHousekeeping",
]
related = ["box-channel", "sessions-and-events", "input-and-receipts", "requests-and-approval", "watching-sessions", "host-setup"]

[[invariant]]
text = "At most one daemon runs per runtime directory: a second one fails at once on `daemon.lock`."
refs = ["internal/daemon.Daemon.Run", "internal/flock.Acquire"]

[[invariant]]
text = "Only the user who runs the daemon can connect to it: the socket is 0600 in a 0700 directory."
refs = ["internal/daemon.Daemon.Run"]

[[invariant]]
text = "A CLI never uses a daemon that works on other configuration or state directories than its own."
refs = ["internal/daemon.Client.Serves", "internal/cli.app.daemonClient"]
tests = ["internal/daemon.TestDaemon"]

[[invariant]]
text = "The daemon starts a box worker only for a running container that has a configured project's box name and carries that project's realm and project labels."
refs = ["internal/daemon.Daemon.watchOnce"]

[[invariant]]
text = "A box worker serves one container ID: a recreated box gets a new worker, and the old one is stopped first."
refs = ["internal/daemon.Daemon.watchOnce", "internal/daemon.boxWorker.stop"]

[[invariant]]
text = "A subscriber that falls behind (4,096 batches, 32 MiB, or 256 MiB for all together) is disconnected, never buffered further."
refs = ["internal/daemon.hub.enqueueLocked"]
tests = ["internal/daemon.TestSubscriberByteBudget"]

[[invariant]]
text = "A subscription sends each event of a session at most once and in seq order, and a session whose journal the replay could not read to its end gets `missed` before `caught_up`."
refs = ["internal/daemon.subscriber.send", "internal/daemon.subscriber.replayOne"]
tests = ["internal/daemon.TestSubscribeRaceWithReplay", "internal/daemon.TestReplayFailure"]

[[invariant]]
text = "A restart loses no session event, command or request: views come back from the journals, commands from their receipts, requests from their logs."
refs = ["internal/daemon.hub.setProjects", "internal/daemon.commander.resume", "internal/daemon.requestService.recoverInterrupted"]
tests = ["internal/daemon.TestCommandsSurviveRestart", "internal/daemon.TestRequestsInterrupted"]

[[diagram]]
id = "inside"
title = "Inside the daemon: who hands work to whom"
kind = "graph"

[[diagram.node]]
id = "api"
label = "API listener"
col = 1
row = 1
ref = "internal/daemon.Daemon.serveAPI"

[[diagram.node]]
id = "watch"
label = "Box watch (3 s)"
col = 2
row = 1
ref = "internal/daemon.Daemon.watchLoop"

[[diagram.node]]
id = "house"
label = "Housekeeping (5 min)"
col = 3
row = 1
ref = "internal/daemon.Daemon.housekeep"

[[diagram.node]]
id = "conns"
label = "API connections (≤ 64)"
col = 1
row = 2
ref = "internal/proto.Conn"

[[diagram.node]]
id = "channel"
label = "Channel (per box)"
col = 2
row = 2
ref = "internal/daemon.boxWorker.channelLoop"

[[diagram.node]]
id = "ingest"
label = "Ingester (per box)"
col = 3
row = 2
ref = "internal/daemon.boxWorker.ingestLoop"

[[diagram.node]]
id = "reconcile"
label = "Reconcile (30 s)"
col = 4
row = 2
ref = "internal/daemon.Daemon.reconcileLoop"

[[diagram.node]]
id = "requests"
label = "Requests (2 workers)"
col = 1
row = 3
ref = "internal/daemon.requestService"

[[diagram.node]]
id = "commands"
label = "Commands (a line per session)"
col = 2
row = 3
ref = "internal/daemon.commander"

[[diagram.node]]
id = "hub"
label = "Journal follower (1 s)"
col = 3
row = 3
ref = "internal/daemon.hub"

[[diagram.node]]
id = "subs"
label = "Subscribers"
col = 4
row = 3
ref = "internal/daemon.subscriber.run"

[[diagram.edge]]
from = "api"
to = "conns"
label = "accepts"

[[diagram.edge]]
from = "watch"
to = "channel"
label = "per running box"
ref = "internal/daemon.Daemon.startWorker"

[[diagram.edge]]
from = "watch"
to = "ingest"
label = "per running box"
ref = "internal/daemon.Daemon.startWorker"

[[diagram.edge]]
from = "conns"
to = "requests"
label = "requests.decide"

[[diagram.edge]]
from = "conns"
to = "commands"
label = "session.*"

[[diagram.edge]]
from = "channel"
to = "requests"
label = "host.request"

[[diagram.edge]]
from = "commands"
to = "channel"
label = "box.input.*"

[[diagram.edge]]
from = "channel"
to = "ingest"
label = "host.hint wakes"
ref = "internal/daemon.boxWorker.wake"

[[diagram.edge]]
from = "ingest"
to = "hub"
label = "poll after events"
ref = "internal/daemon.Daemon.ingest"

[[diagram.edge]]
from = "reconcile"
to = "hub"
label = "refresh, then poll"
ref = "internal/daemon.Daemon.refresh"

[[diagram.edge]]
from = "hub"
to = "commands"
label = "views changed"
ref = "internal/daemon.hub.changed"

[[diagram.edge]]
from = "hub"
to = "subs"
label = "batches"
ref = "internal/daemon.hub.enqueueLocked"
+++
## What it is for

The CLI works without a daemon: it ingests spool files and reconciles sessions whenever it shows them. Some work needs a process that stays: typing into a session when it is ready, answering a hook that waits, following events as they happen, carrying out what a box asks for, and serving the streams a box opens to the host. `devc daemon` is that process, one per runtime directory, in the foreground or as the systemd user service of [[concern:host-setup]]. What it must not lose is in files: views come from the journals, commands from their receipts, requests from their logs.

## How it works

**Start and stop.** [[internal/daemon.Daemon.Run]] makes the runtime directory 0700 and takes `daemon.lock` with a timeout of one millisecond, so a second daemon fails at once. It replaces a stale socket with `<runtime>/daemon.sock` (0600), loads the configuration, resumes the commands and requests a previous daemon left, starts the request workers, the [[concern:forwards-and-local-urls|router]], the [[concern:service-logins|services]] and the [[concern:hub-machine-link|hub links]], and then its loops. SIGTERM, SIGINT or [[rpc:daemon.stop]] cancel its context: it closes the socket, subscriptions, terminals and box workers, and waits up to 15 seconds for its goroutines. [[flow:daemon-start]] goes through this.

**What runs where.** The loops, box workers, command lines and request workers run in goroutines started by [[internal/daemon.Daemon.goSafe]], which logs a panic instead of ending the process. The handlers of a box's raw streams and the workers of `projects.list` run under a plain `go`, so a panic there ends the daemon (`docs/dev/LEFTOVER.md` part 7). [[diagram:inside]] shows how they hand work on.

| Goroutine | Does | When |
|---|---|---|
| `api` | accepts clients; each connection serves its requests one at a time | on connect |
| `watch` | reloads changed configuration, lists boxes, starts and stops workers | 3 s |
| `channel`, `ingest` (per box) | the [[concern:box-channel]]; spool ingestion ([[internal/daemon.boxWorker.ingestLoop]]) | 2 s, 250 ms while busy, on a hint |
| `journals` | the journal follower | 1 s |
| `reconcile` | ends sessions a box no longer runs | 30 s |
| `housekeeping` | prunes receipts, expires requests, runs `devc bank --all` and `devc context commit --all` | 5 min |
| `line` (per session) | that session's commands ([[concern:input-and-receipts]]) | while any wait |
| request workers (2) | approved requests ([[concern:requests-and-approval]]) | while queued |
| subscriber | one subscription's replay and stream | while connected |

**Box workers.** [[internal/daemon.Daemon.watchOnce]] lists containers labelled `devc.role=box` and keeps a [[internal/daemon.boxWorker]] for each running container that has a configured project's name and labels. A worker belongs to one container ID; its context ends with the box, ending its channel and ingester.

**The journal follower.** The daemon's view of sessions is [[internal/daemon.hub]] (not the hub server). It does not take events from ingestion: it follows the journals, whoever appended to them (the daemon's ingester, a CLI command, `devc down`), folds new lines into a [[term:session-view]] per session, wakes waiting commands and queues the events for the subscribers of [[rpc:events.subscribe]]. A subscription replays each session after the client's [[term:cursor]] (unknown sessions active within 30 days), sends `caught_up`, then live events and a heartbeat every 15 seconds. [[flow:event-ingestion]] follows one event.

**Banking after turns.** When a live session goes from working to done or idle, [[internal/daemon.turnBanker]] runs `devc bank <project> --quiet`, at most once per 30 seconds per box. Banks, housekeeping and approved requests run as child processes of the same binary, from `/`, with the daemon's directories ([[internal/daemon.Daemon.runChild]]).

**The API.** [[internal/daemon.Daemon.handlers]] is the method table ([[surface:daemon-api]]); errors are sent in full, since the only client is the same user. [[internal/daemon.Dial]] connects and asks [[rpc:daemon.info]]; the CLI uses the daemon only if it serves the same configuration and state directories, and never with `DEVC_NO_DAEMON=1`.

**Locks.** `Daemon.mu` guards the configuration in use, the box table, session windows and pending warnings. The follower's `hub.mu` guards views and subscribers and is held while journals are read. Each worker's `mu` guards its channel. Between processes, the per-box ingestion lock is shared with the CLI; the daemon waits at most two seconds for it and tries again at its next run.

**The vocabulary.** [[pkg:internal/proto]] is shared by four processes:

| Family | File | Written by | Checked by |
|---|---|---|---|
| Session events | `proto.go` | box (spool), host (journal) | [[internal/proto.ParseSpoolLine]] |
| JSON-RPC codec | `rpc.go` | every peer | [[internal/proto.ParseMessage]], strict decoding |
| Box channel | `views.go` | daemon, box agent | the daemon: [[internal/proto.HelloReply.Validate]], [[internal/session.ParseLive]] |
| API: views, commands, receipts | `views.go` | daemon, CLI, hub | each handler; a hub only through an allowlist |
| Requests | `requests.go` | box, daemon | [[internal/requests.Validate]] |
| Web app | `web.go` | the hub for the web app | the daemon's handlers |
| Machine link | `hub.go` | daemon, hub | [[internal/proto.HubHello.Validate]] |
| GitHub | `github.go` | box, daemon | the daemon's GitHub facade |

Box-written text survives validation; [[internal/proto.Printable]] is applied before it reaches a terminal. [[pkg:internal/wsconn]] carries the codec over a hub link's WebSocket, one object per text frame.

## What can go wrong, and what prevents it

- **A second daemon, another user, another configuration.** The lock; the socket's mode; [[internal/daemon.Client.Serves]].
- **A container that only looks like a box.** Without the project's labels it gets no worker and counts as absent. The channel checks the container ID again at every connection, but session starts and stops through the engine and the terminal's exec address the box by name, relying on the last observation (normally at most three seconds old): a container replaced under that name in between, which only the host's user can do, would receive them (`docs/dev/LEFTOVER.md` part 7).
- **A flood of clients.** At most 64 API connections, lines of 1 MiB, pages of 1,000 sessions or events and 512 KiB, `command.get` waits of at most 600 seconds, one subscription per connection, cursors of at most 100,000 sessions.
- **A slow subscriber.** Disconnected at its queue budgets; it resumes from its cursor ([[finding:C1-08]]). A replay that fails sends `missed`, and `caught_up` still follows, so a client must read such a session again ([[finding:C1-17]]); a window longer than 30 days loads the ended sessions it reaches ([[finding:C1-16]]); host events after a session's end still reach live views ([[finding:C1-15]]).
- **A crash or a restart.** Views, commands and requests come back from their files. What lives only in memory ends: subscriptions and terminals (clients connect again) and hooks waiting for an answer (the harness shows its own dialog).
- **A hub.** It may call only the allowlisted methods in [[internal/daemon.hubMethods]], decides with origin `hub`, and sees no host paths.

## Decisions that are not obvious

- **Follow the journals, not ingestion.** Anyone may append to a journal, and a restart rebuilds every view from them.
- **Child processes for housekeeping and requests.** The CLI's locks and fail-closed checks apply unchanged, and a crash there cannot take the daemon down ([[chunk:C08]]).
- **Reload on a fingerprint.** Names, sizes and modification times of the TOML files and their directories; also checked before answers, so a just-added project is never missed.
- **The daemon is optional.** `devc ps` without it gives the same sessions.

## Where to start reading

1. [[internal/daemon.Daemon.Run]]
2. [[internal/daemon.Daemon.watchOnce]] and [[internal/daemon.Daemon.startWorker]]
3. [[internal/daemon.hub.poll]] and [[internal/daemon.hub.subscribe]]
4. [[internal/daemon.Daemon.handlers]]
5. [[internal/proto.Conn]]
