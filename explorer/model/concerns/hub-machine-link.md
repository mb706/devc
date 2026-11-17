+++
id = "hub-machine-link"
title = "The machine link"
summary = "How a machine joins a hub and stays connected: the daemon dials out with its certificate and serves an allowlist of its methods, the hub subscribes from stored cursors and keeps a bounded projection that one worker per machine writes, with re-reads, refreshes, pruning, and two hubs."
order = 81
packages = ["internal/hubnode"]
files = [
  "internal/hub/link.go",
  "internal/hub/validate.go",
  "internal/hub/store.go",
  "internal/daemon/hublink.go",
  "internal/daemon/hub.go",
]
key = [
  "internal/daemon.hubMethods",
  "internal/daemon.Daemon.hubHandlers",
  "internal/daemon.hubLink.connectOnce",
  "internal/daemon.hubManager.recomputeLocked",
  "internal/hub.Server.serveLink",
  "internal/hub.machineLink.onHello",
  "internal/hub.machineLink.run",
  "internal/hub.machineLink.process",
  "internal/hub.machineLink.applyEvent",
  "internal/hub.machineLink.startResync",
  "internal/hub.machineLink.listSessions",
  "internal/hub.cleanEvent",
]
spec = ["doc:specs-v2.md#13"]
tests = [
  "internal/hub.TestMachineLink",
  "internal/hub.TestRefreshBounds",
  "internal/hub.TestResyncBounds",
  "internal/hub.TestTailBounds",
  "internal/hub.TestPruneThroughWorker",
  "internal/daemon.TestHubAllowlist",
  "internal/daemon.TestHubOrigin",
  "internal/daemon.TestHubRoles",
  "internal/hubnode.TestCredentials",
  "internal/hubnode.TestConfigEdits",
  "internal/cli.TestHubCommands",
  "test/integration.TestHub",
]
related = ["hub-access", "hub-inbox-and-notifications", "hub-commands-and-terminal", "daemon", "sessions-and-events"]

[[invariant]]
text = "Every row a machine link writes is keyed by the registry name of the link's certificate; nothing the machine sends names the machine."
refs = ["internal/hub.Server.serveLink", "internal/hub.Server.identify"]
tests = ["internal/hub.TestMachineLink"]

[[invariant]]
text = "Nothing a machine sends is stored before it passed the checks of validate.go; events are stored re-encoded from their checked fields."
refs = ["internal/hub.cleanEvent", "internal/hub.cleanView", "internal/hub.cleanRequest", "internal/hub.cleanBoxes", "internal/hub.cleanInfo"]
tests = ["internal/hub.TestMachineLink"]

[[invariant]]
text = "An event notification is applied only when its seq is the session's cursor plus one, and a gap leads to a re-read; a re-read takes the machine's view and events without checking that their seqs follow one another."
refs = ["internal/hub.machineLink.applyEvent", "internal/hub.machineLink.finishResync"]
tests = ["internal/hub.TestMachineLink"]

[[invariant]]
text = "The hub holds at most MaxMachineSessions sessions per machine, sessions being read for the first time included, and at most 500 events and 1 MiB of them per session."
refs = ["internal/hub.machineLink.used", "internal/hub.machineLink.startResync", "internal/hub.machineLink.finishResync", "internal/hub.Tx.TrimTail"]
tests = ["internal/hub.TestResyncBounds", "internal/hub.TestTailBounds"]

[[invariant]]
text = "A refresh asks for at most MaxMachineSessions sessions in pages that advance strictly, within 64 MiB and two minutes; a machine that breaks a bound loses its link."
refs = ["internal/hub.machineLink.listSessions"]
tests = ["internal/hub.TestRefreshBounds"]

[[invariant]]
text = "Only the link's worker changes a link's state; call results, request refreshes and pruning reach it through its queue."
refs = ["internal/hub.machineLink.process", "internal/hub.machineLink.async", "internal/hub.machineLink.requestPrune"]
tests = ["internal/hub.TestPruneThroughWorker"]

[[invariant]]
text = "A hub can call only the methods in hubMethods; daemon.info answers without directories and PID, and sessions.list never ingests or reconciles."
refs = ["internal/daemon.Daemon.hubHandlers", "internal/daemon.Daemon.hubInfo"]
tests = ["internal/daemon.TestHubAllowlist", "internal/daemon.TestHubOrigin"]

[[invariant]]
text = "Of a machine's connected hubs, exactly the first in config.toml's order is told it is the notifier."
refs = ["internal/daemon.hubManager.recomputeLocked", "internal/daemon.hubLink.roles"]
tests = ["internal/daemon.TestHubRoles"]

[[diagram]]
id = "holdings"
title = "What the hub holds for one machine, and the bound on each part"
kind = "graph"

[[diagram.node]]
id = "daemon"
label = "the machine's daemon"
col = 1
row = 2
zone = "host"
ref = "internal/daemon.hubLink.connectOnce"

[[diagram.node]]
id = "queue"
label = "queue: 4,096 items"
col = 2
row = 1
ref = "internal/hub.linkQueue"

[[diagram.node]]
id = "worker"
label = "link worker"
col = 2
row = 2
ref = "internal/hub.machineLink.run"

[[diagram.node]]
id = "calls"
label = "calls, a minute each"
col = 2
row = 3
ref = "internal/hub.machineLink.async"

[[diagram.node]]
id = "sessions"
label = "sessions: at most 20,000"
col = 3
row = 1
ref = "internal/hub.MaxMachineSessions"

[[diagram.node]]
id = "events"
label = "events: 500 or 1 MiB a session"
col = 4
row = 1
ref = "internal/hub.TailEvents"

[[diagram.node]]
id = "requests"
label = "pending requests"
col = 3
row = 2
ref = "internal/hub.Tx.PutRequest"

[[diagram.node]]
id = "boxes"
label = "boxes: at most 1,000"
col = 3
row = 3
ref = "internal/hub.maxBoxes"

[[diagram.node]]
id = "row"
label = "machine row: hello, role"
col = 4
row = 3
ref = "internal/hub.Tx.MachineUp"

[[diagram.edge]]
from = "daemon"
to = "queue"
label = "notifications"
ref = "internal/hub.machineLink.enqueue"

[[diagram.edge]]
from = "queue"
to = "worker"
label = "batches of 500"
ref = "internal/hub.machineLink.process"

[[diagram.edge]]
from = "worker"
to = "calls"
label = "re-reads: 256, 4 at once"
ref = "internal/hub.machineLink.startResync"

[[diagram.edge]]
from = "calls"
to = "daemon"
label = "refresh: 64 MiB, 2 min"
ref = "internal/hub.machineLink.listSessions"

[[diagram.edge]]
from = "worker"
to = "sessions"
label = "views up to 64 KiB"
ref = "internal/hub.cleanView"

[[diagram.edge]]
from = "sessions"
to = "events"
label = "trimmed every batch"
ref = "internal/hub.Tx.TrimTail"

[[diagram.edge]]
from = "worker"
to = "requests"
label = "one answer, 1 MiB"
ref = "internal/hub.machineLink.finishRequests"

[[diagram.edge]]
from = "worker"
to = "boxes"
label = "every refresh"
ref = "internal/hub.cleanBoxes"

[[diagram.edge]]
from = "boxes"
to = "row"
label = "stored with it"
ref = "internal/hub.Tx.SetBoxes"
+++
## What it is for

A hub knows about a machine only what the machine's daemon tells it, and acts on the machine only over the same connection. The machine dials out, so it needs no open port; the hub mirrors its sessions so the inbox and notifications work while nobody is at the machine, and catches up after either side was away. A hub is on the internet and a machine may be compromised, so each side bounds what the other can make it do: the daemon decides which of its methods a hub may call, and the hub bounds what a machine can make it fetch or store.

## How it works

**Joining.** `devc join <url>` stores a machine certificate, its key and optionally the CA of the hub's server certificate in `durable/node/hubs/<id>/` ([[internal/hubnode.ID]]) and appends a `[[hubs]]` block to config.toml ([[internal/hubnode.AddToConfig]]). The hub list is policy and lives only in config.toml; credentials for a URL not listed are ignored. [[flow:machine-join]] follows a join.

**The daemon's side.** [[internal/daemon.hubManager]] keeps one [[internal/daemon.hubLink]] per listed URL. It follows configuration reloads and, at every box-watch pass (three seconds), the sizes and times of the credential files ([[internal/hubnode.Stamp]]), so a new join reconnects. [[internal/daemon.hubLink.run]] connects again and again: after a loss it waits from one second, doubling to two minutes with up to 20 % added, and starts from one second again after a connection that lasted 30 seconds. [[internal/daemon.hubLink.connectOnce]] dials `wss://<hub>/machine` with the stored certificate (the server checked against the stored CA, else the system's roots), says `hub.hello` with protocol, host name, version and instance, then serves the hub's calls, eight at a time, and pings every 30 seconds. Over the link the daemon calls only `hub.hello` and `hub.role`.

**What a hub may call.** [[internal/daemon.hubMethods]] is the allowlist; [[internal/daemon.Daemon.hubHandlers]] copies those methods from the local table: reads of boxes, sessions, events and receipts, the session commands, requests, projects, changes and terminals. Three are altered: `daemon.info` loses the directories and PID, `sessions.list` loses `sync`, and `requests.decide` carries origin `hub`. Any other method is unknown on a hub's connection.

**The hub's side.** `/machine` needs a machine certificate ([[concern:hub-access]]). [[internal/hub.Server.serveLink]] replaces an older link of the same registry name once that one has marked the machine offline; the name comes from the certificate's record and keys every row the link writes. hello must come within ten seconds and is validated ([[internal/hub.machineLink.onHello]]); the machine is recorded online, not notifying, before the answer. [[internal/hub.machineLink.run]] loads the machine's sessions and pending requests from the database, asks `daemon.info` for the boxes, and subscribes ([[rpc:events.subscribe]]) with a [[term:cursor]]: the last seq of every session live or active in the last 30 days, keyed by project and ID. The daemon replays its journals from there, says `caught_up`, then sends live events and a heartbeat every 15 seconds ([[internal/daemon.subscriber.run]]).

**One worker per machine.** The goroutine serving `/machine` becomes the link's worker, the only one that touches the link's state. The codec's reader turns each notification into a [[internal/hub.linkItem]] on a queue of 4,096; the worker takes up to 500 at a time and applies them in one transaction ([[internal/hub.machineLink.process]], [[internal/hub.Store.Write]]). It never waits for the machine: [[internal/hub.machineLink.async]] runs each call in a goroutine and sends back a function for the worker to apply. An event is applied only as the successor of the session's cursor ([[internal/hub.machineLink.applyEvent]]): stored, folded into the [[term:session-view]], recorded in the change log. A duplicate is dropped; a gap, or an unknown session whose first event is not seq 1, starts a re-read.

**Re-reads and refreshes.** A re-read ([[internal/hub.machineLink.startResync]]) asks `sessions.get` for that project's session: its view and last 500 events, which replace what the hub held, unchecked for gaps; events arriving meanwhile are held back and applied after, each as the successor. At `caught_up` and every 30 seconds, [[internal/hub.machineLink.startRefresh]] pages through `sessions.list` and asks `boxes.list`: sessions the stream did not bring are read, box states and windows updated, then `requests.list` replaces the pending requests. A `missed` notice is acted on only for a session the hub holds, of the same project.

**Disconnects, restarts, pruning.** When a link ends the machine is marked offline and loses the notifier role; its rows stay and show as stale. At start the hub marks every machine offline. Everything a link resumes from is in the database; the worker's copies, re-read queue and held events are rebuilt on reconnecting. Every hour, sessions that are not live and whose last event is over 30 days old are removed (a later `seen` postpones it): through the worker for a connected machine, so its count agrees, directly for an offline one ([[internal/hub.Server.pruneSessions]], [[finding:C2-15]]).

| Goroutine | Where | What it does |
|---|---|---|
| link worker | the handler of `/machine` ([[internal/hub.machineLink.run]]) | owns sessions, cursors, pending requests and re-reads of one machine; writes all it sends |
| link reader | the link's [[internal/proto.Conn]] | decodes notifications and queues them; hands terminal output to its stream |
| calls | [[internal/hub.machineLink.async]] | one machine call each, the result back through the queue |
| API handlers | net/http | read the database; commands and terminals call the machine directly ([[internal/hub.machineLink.forward]]) |
| notifier, registry watcher, housekeeping | [[internal/hub.Server.Run]] | notifications; ending connections; hourly pruning |
| on the machine | [[internal/daemon.hubManager.sync]] | per hub a connect loop, a pinger and a role sender, eight codec workers; the subscription's goroutine |

**Two hubs.** A machine connects to every listed hub, and each builds its own projection. [[internal/daemon.hubManager.recomputeLocked]] makes the first connected hub in config.toml's order the notifier and tells every connected hub its role with `hub.role`, again whenever a link comes or goes ([[concern:hub-inbox-and-notifications]]).

[[diagram:holdings]]

## What can go wrong, and what prevents it

A compromised machine controls all it sends: hello, notifications, the answers to the hub's calls, how fast it reads, when it stops.

- **Writing another machine's data.** Rows are keyed by the registry name of the link's certificate; nothing a machine sends names a machine.
- **Hostile content.** Events are decoded strictly and checked: version, seq, session ID, a known type, timestamps, and the type's data through [[internal/proto.ValidateData]]; then re-encoded ([[internal/hub.cleanEvent]]). Views ([[internal/hub.cleanView]]: names, states, field sizes, at most 64 KiB), requests ([[internal/hub.cleanRequest]]: kind and risk agree, [[internal/requests.Validate]]) and boxes ([[internal/hub.cleanBoxes]]) likewise; what fails is dropped. A message is at most 1 MiB; a larger frame ends the connection.
- **Filling the hub.** At most 20,000 sessions per machine, first reads included ([[internal/hub.MaxMachineSessions]]); per session at most 500 events and 1 MiB ([[internal/hub.Tx.TrimTail]]); pending requests as one answer holds; the change log keeps 20,000 entries for all machines. One machine can still hold about 20 GiB of event tails.
- **Making the hub fetch without end.** A refresh asks for at most 20,000 sessions in strictly advancing pages, at most 64 MiB within two minutes ([[internal/hub.machineLink.listSessions]], [[finding:C2-01]]). Re-reads wait in a queue of 256 served four at a time; held-back events are capped at 2,000 per session and 20,000 in all ([[finding:C2-02]]). A refresh that breaks its bounds, or fails, closes the link; a full re-read queue defers the gap to the next refresh, and held events beyond their cap are dropped for another read.
- **Stalling the hub.** Every call has a minute; a worker that makes no room in its queue for a minute loses its link; a link silent for 90 seconds is closed.
- **A compromised hub.** It reaches only the allowlist ([[concern:hub-commands-and-terminal]]).

## Decisions that are not obvious

- **Cursors are keyed by project and ID** ([[finding:C1-01]]): an ID found in two projects (only from before session ownership) stays the project first learned, never mixed.
- **A machine with more sessions than the bound is truncated, not disconnected**: the hub never asks for more than it keeps ([[chunk:C12r2]], deviation 2).
- **Only pending requests are mirrored**, refreshed after `caught_up`, every 30 seconds, on a request event and after a decision.

## Where to start reading

1. [[internal/daemon.hubLink.connectOnce]] and [[internal/daemon.Daemon.hubHandlers]]
2. [[internal/hub.Server.serveLink]] and [[internal/hub.machineLink.run]]
3. [[internal/hub.machineLink.process]] and [[internal/hub.machineLink.applyEvent]]
4. [[internal/hub.machineLink.startRefresh]], [[internal/hub.machineLink.listSessions]] and [[internal/hub.machineLink.startResync]]
5. [[internal/hub.cleanEvent]] and [[internal/hub.cleanView]]
