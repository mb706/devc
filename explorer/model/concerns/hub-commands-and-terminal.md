+++
id = "hub-commands-and-terminal"
title = "Commands, decisions, terminals"
summary = "What a logged-in browser can cause on a machine through the hub: session commands forwarded with the client's command ID and answered with the machine's receipt, decisions on low-risk requests only, the terminal relay, and the stream of changes to browsers."
order = 83
files = [
  "internal/hub/api.go",
  "internal/hub/stream.go",
  "internal/hub/terminal.go",
]
key = [
  "internal/daemon.hubMethods",
  "internal/hub.Server.apiCommand",
  "internal/hub.commandParams",
  "internal/hub.Server.fillRef",
  "internal/hub.machineLink.forward",
  "internal/hub.rpcStatus",
  "internal/hub.Server.apiDecide",
  "internal/hub.Server.readOlder",
  "internal/hub.Server.apiStream",
  "internal/hub.Store.changesAfter",
  "internal/hub.Server.apiTerminal",
  "internal/hub.termSink.put",
]
spec = ["doc:specs-v2.md#13", "doc:specs-v2.md#12.4"]
tests = [
  "internal/hub.TestMachineLink",
  "internal/hub.TestRPCErrors",
  "internal/hub.TestBackwardPaging",
  "internal/hub.TestStreamRetention",
  "internal/hub.TestStreamEndsWithSession",
  "internal/hub.TestWebEndpoints",
  "internal/daemon.TestHubAllowlist",
  "internal/daemon.TestHubOrigin",
  "test/integration.TestHub",
  "test/integration.TestWebApp",
]
related = ["security-model", "hub-access", "hub-machine-link", "hub-inbox-and-notifications", "input-and-receipts", "requests-and-approval", "watching-sessions"]

[[invariant]]
text = "A browser reaches a machine only through the methods in hubMethods; any other method is unknown on a hub's connection."
refs = ["internal/daemon.hubMethods", "internal/daemon.Daemon.hubHandlers"]
tests = ["internal/daemon.TestHubAllowlist"]

[[invariant]]
text = "A decision that arrives through a hub is applied only to a request of low risk; a high-risk request can be neither approved nor denied that way."
refs = ["internal/daemon.Daemon.decideHandler", "internal/requests.MayDecide", "internal/hub.Server.apiDecide"]
tests = ["internal/daemon.TestHubOrigin"]

[[invariant]]
text = "The hub never queues a command: for a machine that is not connected the request fails at once with 503."
refs = ["internal/hub.Server.apiCommand", "internal/hub.Server.offline"]
tests = ["internal/hub.TestMachineLink"]

[[invariant]]
text = "A command reaches the machine with the client's command ID and its parameters decoded strictly into the daemon method's own type."
refs = ["internal/hub.commandParams", "internal/hub.Server.fillRef"]
tests = ["internal/hub.TestMachineLink", "internal/hub.TestProxyFront"]

[[invariant]]
text = "A forwarded command whose arrival is unknown is reported as uncertain, so the client keeps its ID instead of taking it as not sent."
refs = ["internal/hub.rpcStatus", "internal/hub.writeRPCError"]
tests = ["internal/hub.TestRPCErrors"]

[[invariant]]
text = "Terminal input reaches a session only when both hub.toml and the machine's config.toml set `[terminal] control = true`."
refs = ["internal/hub.Server.apiTerminal", "internal/daemon.termManager.open", "internal/daemon.termManager.input"]
tests = ["internal/hub.TestWebEndpoints"]

[[invariant]]
text = "An event stream delivers every change after its position once and in order, or says reset."
refs = ["internal/hub.Server.apiStream", "internal/hub.Store.changesAfter"]
tests = ["internal/hub.TestStreamRetention", "internal/hub.TestMachineLink"]
+++
## What it is for

From a phone or a browser the owner wants to answer, prompt, interrupt or start sessions, approve harmless requests, and watch a session's terminal. Every such action becomes a call on the machine link, so what a browser can cause on a machine is exactly what the machine's daemon lets a hub call, with the effect the daemon gives it. A hub that is itself compromised has the same reach, no more: the limit lives on the machine.

## How it works

**What reaches a machine.** Every route below needs a device certificate and a login session ([[concern:hub-access]]); the methods are those of [[internal/daemon.hubMethods]].

| Route | Method on the machine | Effect there |
|---|---|---|
| `POST /api/commands`, type `send`, `answer`, `interrupt`, `keys` | `session.send`, `session.answer`, `session.interrupt`, `session.keys` | text, an answer, an interrupt, or up to 32 named keys for a live session, through its line of commands and the input gate ([[concern:input-and-receipts]]) |
| type `start` | `session.start` | a session of a harness the project lists, in a box that already runs, with an optional prompt, up to 64 harness arguments (passed to the harness in the box) and, if asked, a new task worktree |
| type `stop`, `seen` | `session.stop`, `session.seen` | the session ended in the box, as `devc stop` does; a turn marked seen |
| `GET /api/commands/{machine}/{id}` | `command.get` | none: a stored receipt, after waiting up to 60 seconds |
| `POST /api/requests/{machine}/{id}/decide` | `requests.decide` | a request of low risk (seal, savepoint, note) approved and carried out, or denied |
| `GET /api/terminal/{machine}/{session}` | `terminal.open`, `terminal.input`, `terminal.close` | the session's tmux window watched; typed into only in control mode |
| `GET /api/sessions/{machine}/{session}?before=` | `events.read` | none: older events |
| `GET /api/projects`, `GET /api/changes/{machine}/{session}` | `projects.list`, `changes.get` | none: worktrees asked in running boxes, a session's changes computed on the relay |

Nothing a hub may call starts or stops a box, writes the configuration, stops the daemon, or decides a high-risk request.

**Forwarding a command.** [[internal/hub.Server.apiCommand]] decodes the body strictly. The machine must be a valid name, the type one of seven, the command ID a UUID or of session-ID form, `wait` between 0 and 60. [[internal/hub.commandParams]] decodes `params` strictly into the daemon method's own type, refuses an `id` among them and sets the client's. [[internal/hub.Server.fillRef]] checks a project the client named, or fills in the one the hub holds for the session, so the daemon looks for the session in that project only ([[finding:C1-01]]). A machine that is not connected gets 503 (404 if the hub never saw it): the hub queues nothing. Otherwise [[internal/hub.machineLink.forward]] calls the machine from the request's own goroutine, not through the link's worker, for at most 90 seconds. The daemon stores a [[term:receipt]] before acting and answers with it; a repeat with the same ID gets the stored receipt, which makes retries safe while the daemon keeps it (until seven days after it became final). With `wait` the hub then asks `command.get` for the final receipt. [[flow:phone-answer]] follows an answer to the harness.

**Uncertain outcomes.** [[internal/hub.rpcStatus]] maps the machine's errors to statuses: invalid 400, not found 404, refused 403, busy 429, not offered 501. When the machine went away during the call (503), did not answer in time (504) or could not be asked or sent an invalid receipt (502), the body says `"uncertain": true`: the command may have arrived, and the client keeps its ID to ask for the receipt or to retry ([[term:uncertain-outcome]], [[finding:C2-11]]). If only the wait for the final receipt fails, the receipt so far comes back with 200.

**Decisions.** [[internal/hub.Server.apiDecide]] forwards `requests.decide` without an origin. The daemon gives every decision on a hub's connection origin `hub`, and [[internal/requests.MayDecide]] lets that origin decide low-risk requests only. A high-risk request (package, net, grant, pr, comment) is shown in the inbox but can be neither approved nor denied through a hub; `devc requests approve|deny` on the machine decides it ([[concern:requests-and-approval]]). After a decision the hub asks for a refresh of that machine's pending requests.

**Reads.** Machines, inbox, sessions and requests come from the database. A session's detail serves the hub's tail; paging back beyond it asks the machine's `events.read` ([[internal/hub.Server.readOlder]]): up to 64 reads, 16 MiB and 30 seconds, at most 1 MiB returned, ending right below the oldest event shown ([[finding:C2-08]]); `partial` says the machine could not be asked.

**The event stream.** `GET /api/stream` ([[internal/hub.Server.apiStream]]) sends the [[term:change-log]] as server-sent events with the change's ID. `session` (a summary with its inbox section), `session.removed`, `machine`, `request` and `request.removed` go to every stream; `event` and `session.reset` only for the sessions the stream names (up to 32). A client resumes with `Last-Event-ID`; with none it gets `ready` at the present; an ID the log no longer reaches, or one beyond its newest (another hub's, a new database), gets `reset`, and the client reads its snapshot again. IDs are bare numbers: another hub's ID within this hub's range passes as this hub's. IDs are consecutive, so a page that skips one shows that pruning overtook the stream: `reset` again ([[internal/hub.Store.changesAfter]], [[finding:C2-09]]). A comment goes out every 15 seconds, when the stream also checks its login session; revocation of the certificate ends it within a second, its expiry at once. A stream that names a session suppresses its notifications ([[concern:hub-inbox-and-notifications]]).

**The terminal.** [[internal/hub.Server.apiTerminal]] ([[flow:hub-terminal]]) refuses control mode unless hub.toml has `[terminal] control = true`; the machine refuses it unless its config.toml has the same ([[internal/daemon.termManager.open]]). The hub picks a random stream ID and registers a sink of 512 messages before it calls `terminal.open`, so the `terminal.data` notifications, which the link's reader handles, find it ([[internal/hub.termSink.put]]); a browser that falls that far behind loses the stream instead of stalling the reader. Output goes to the browser as binary WebSocket frames. The browser sends JSON frames: `input`, accepted only in control mode (in observe mode it ends the stream), and `visible`. A stream ends with its session, the page, the machine's link, 30 minutes hidden, the login session or the certificate; once the machine has confirmed the open, `terminal.close` goes to it however the stream ends. An open that fails, gets no answer within 30 seconds or an invalid one sends no close: a stream the daemon did open then stays there until its session ends, the link closes, the client exits or 12 hours pass. All browsers' terminals to one machine share its link, and the daemon admits four streams per connection, four terminals per machine through one hub (counted without a reservation, so opens at the same moment can pass together).

## What can go wrong, and what prevents it

- **A compromised hub.** It can read the transcripts it mirrors, type into live sessions, start sessions in running boxes, decide low-risk requests and watch terminals; it cannot stop the daemon, start or stop boxes, change policy, decide high-risk requests or reach a host shell ([[internal/daemon.hubMethods]], [[doc:docs/dev/plan.md#5]]).
- **Typing into the wrong session.** The hub names the session's project on every call, and the daemon binds an answer to the question pending now ([[flow:question-answer]]).
- **A command applied twice** after a lost answer: the command ID; the daemon returns the stored receipt, for seven days after it became final.
- **Terminal input without consent.** Control mode needs both switches; in observe mode the daemon, the only writer of the terminal it reads from, refuses input ([[concern:watching-sessions]]).
- **Malformed input from a browser.** Strict decoding, bounded bodies, the cross-site rules ([[concern:hub-access]]).
- **A slow browser.** Writes to streams and terminals have 30-second deadlines; terminal output that does not fit the sink ends the stream rather than queue without end.

## Decisions that are not obvious

- **No queue for offline machines**: a command fails at once, so nothing is typed hours after the user expected.
- **Commands bypass the worker**: a call that waits up to 90 seconds for a receipt must not hold up the projection.
- **Device-signed commands were not built** ([[doc:docs/dev/plan.md#5]]): the hub serves the [[concern:web-app|web app]]'s code, so a key in the browser would not stop a compromised hub; the allowlist and the risk classes do the work instead.

## Where to start reading

1. [[internal/daemon.hubMethods]]
2. [[internal/hub.Server.apiCommand]] and [[internal/hub.commandParams]]
3. [[internal/hub.rpcStatus]]
4. [[internal/hub.Server.apiStream]]
5. [[internal/hub.Server.apiTerminal]]
