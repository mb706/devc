+++
id = "requests-and-approval"
title = "Requests and approval"
summary = "What a box may ask the host for with `devc request`, the risk classes, who may decide which, how approved requests are carried out with the user's own commands, and the request log."
order = 35
packages = ["internal/requests"]
files = [
  "internal/daemon/requests.go",
  "internal/proto/requests.go",
  "internal/cli/requestcmds.go",
]
key = [
  "internal/requests.Validate",
  "internal/requests.MayDecide",
  "internal/requests.EffectiveOrigin",
  "internal/requests.Store",
  "internal/daemon.requestService.create",
  "internal/daemon.requestService.decideLocked",
  "internal/daemon.requestService.execute",
  "internal/daemon.requestCommands",
  "internal/daemon.requestService.status",
  "internal/cli.newRequestCmd",
]
spec = ["doc:docs/dev/plan.md#3.11", "doc:docs/dev/plan.md#5"]
tests = [
  "internal/requests.TestValidate",
  "internal/requests.TestRisk",
  "internal/requests.TestMayDecide",
  "internal/requests.TestStore",
  "internal/requests.TestStoreCompaction",
  "internal/daemon.TestRequests",
  "internal/daemon.TestRequestFlood",
  "internal/daemon.TestRequestCommands",
  "internal/daemon.TestRequestsInterrupted",
  "internal/daemon.TestRequestExecutor",
  "internal/daemon.TestRequestLogs",
  "internal/daemon.TestSavepointRequestWindow",
  "internal/daemon.TestRequestHostPaths",
  "test/integration.TestRequests",
  "test/integration.TestRequestedSavepoints",
]
related = ["security-model", "daemon", "box-channel", "sessions-and-events"]

[[invariant]]
text = "Nothing a box asks for runs before an approval, and a request that fails validation is stored nowhere."
refs = ["internal/daemon.requestService.create", "internal/requests.Validate", "internal/daemon.requestService.decideLocked"]
tests = ["internal/daemon.TestRequests", "internal/requests.TestValidate"]

[[invariant]]
text = "A high-risk request is decided only through the daemon's local socket; the auto list and a hub decide low-risk requests only."
refs = ["internal/requests.MayDecide", "internal/requests.EffectiveOrigin"]
tests = ["internal/requests.TestMayDecide"]

[[invariant]]
text = "A box sees and creates requests only of the project its channel belongs to."
refs = ["internal/daemon.requestService.create", "internal/daemon.requestService.status", "internal/requests.Store.Load"]
tests = ["internal/daemon.TestRequests"]

[[invariant]]
text = "No box-chosen value becomes an option or shell syntax in the commands that carry a request out."
refs = ["internal/daemon.requestCommands", "internal/daemon.Daemon.runChild"]
tests = ["internal/daemon.TestRequestCommands"]

[[invariant]]
text = "An approved request runs at most once: one found approved at start fails instead of running again."
refs = ["internal/daemon.requestService.recoverInterrupted"]
tests = ["internal/daemon.TestRequestsInterrupted"]

[[invariant]]
text = "An outcome or an expiry replaces only the state it was computed from."
refs = ["internal/daemon.requestService.transitionLocked", "internal/daemon.requestService.status"]
tests = ["internal/daemon.TestRequestLogs"]

[[invariant]]
text = "What host.request tells a box names paths as the box sees them: a seal path's refusal names the in-box data root, and a request's result has the project's host directory as /w/<realm>/<project> and devc's other host directories and the home as <host>."
refs = ["internal/data.NormalizeInBox", "internal/daemon.boxView", "internal/daemon.requestService.status"]
tests = ["internal/daemon.TestRequestHostPaths"]

[[diagram]]
id = "request-states"
title = "A request's status"
kind = "graph"

[[diagram.node]]
id = "pending"
label = "pending"
col = 1
row = 2
ref = "internal/proto.RequestPending"

[[diagram.node]]
id = "approved"
label = "approved"
col = 2
row = 2
ref = "internal/proto.RequestApproved"

[[diagram.node]]
id = "done"
label = "done"
col = 3
row = 1
ref = "internal/proto.RequestDone"

[[diagram.node]]
id = "failed"
label = "failed"
col = 3
row = 3
ref = "internal/proto.RequestFailed"

[[diagram.node]]
id = "denied"
label = "denied"
col = 1
row = 4
ref = "internal/proto.RequestDenied"

[[diagram.node]]
id = "expired"
label = "expired"
col = 2
row = 4
ref = "internal/proto.RequestExpired"

[[diagram.edge]]
from = "pending"
to = "approved"
label = "approved, with a place"
ref = "internal/daemon.requestService.decideLocked"

[[diagram.edge]]
from = "pending"
to = "done"
label = "a note, approved"

[[diagram.edge]]
from = "pending"
to = "denied"
label = "denied"

[[diagram.edge]]
from = "pending"
to = "expired"
label = "seven days"
ref = "internal/requests.Expired"

[[diagram.edge]]
from = "approved"
to = "done"
label = "commands succeed"
ref = "internal/daemon.requestService.execute"

[[diagram.edge]]
from = "approved"
to = "failed"
label = "a command fails; restart"
ref = "internal/daemon.requestService.recoverInterrupted"
+++
## What it is for

An agent in a box sometimes needs something it cannot do from inside: seal a finished run, take a savepoint, install a system package, open the network, be granted an SSH host or a secret, open a pull request or comment on one, or just tell the user something. It asks with `devc request`; the user decides, or for low-risk kinds the project's settings do; devc then carries the request out on the host with the commands the user would run. A box must not be able to make anything happen beyond what was approved, approve its own requests, or flood the host.

## How it works

**Kinds.** Each [[term:request]] has a kind with a fixed [[term:risk-class]] ([[internal/proto.RequestRisk]]) and is carried out by [[internal/daemon.requestCommands]] with the commands of [[concern:data-and-sealing]], [[concern:savepoints]], [[concern:allowlist-proxy]] and [[concern:github]]:

| Kind | Risk | Carried out as |
|---|---|---|
| `seal` | low | `devc seal -- <path> <project>` |
| `savepoint` | low | `devc save --request <id> -- [<name>] <project>` |
| `note` | low | nothing: done when approved |
| `package` | high | `devc root <project> -- …` with `apt-get update` and `apt-get install -y --no-install-recommends`, `dnf install -y` or `apk add` |
| `net` | high | `devc net allow --request <id> --for <d> -- <target> <project>` |
| `grant` | high | not available: grants are edited in `project.toml` |
| `pr` | high | `devc pr create --yes --branch=… --title=… --body=… -- <project>` |
| `comment` | high | `devc pr comment --body=… -- <project> <n>` |

**Asking.** In the box, [[internal/cli.newRequestCmd]] validates the request as the host will, adds the session from `DEVC_SESSION`, and sends [[rpc:host.request]] through the agent. The daemon's handler ([[internal/daemon.requestService.create]]) has the project of the channel the stream arrived on. It decodes strictly and runs [[internal/requests.Validate]]: only the fields of the kind; package names, managers, host aliases, secret and branch names matched against patterns; text without control characters (newline and tab only where multi-line) and within limits; paths without `..`; `host:port` with a host name or IPv4 address, or `lan`; durations from one minute to seven days. The project's configuration must be free of problems, and a seal path is resolved against its data root. A refusal goes back to the box and is stored nowhere.

Under the service's lock, the project's log must be below 16 MiB, fewer than 100 requests outstanding (pending or approved) and fewer than 20 made in the last minute. A savepoint request is refused, with the reason, while another savepoint request of the project is outstanding or a savepoint made on request is younger than `data.savepoints.requested_interval` ([[internal/daemon.requestService.savepointRefusal]]); the save, run with `--request`, is of the class requested ([[concern:savepoints]]). The request is stored as `pending` with a new ID (`r` and 20 time-ordered characters) and a `request` event goes into the session's journal when the session has one in this project. A kind in the project's `[requests] auto` (default `seal`, `savepoint`, `note`; configuration refuses high-risk kinds there) is approved at once with origin `auto`.

**Deciding.** `devc requests` lists requests; `devc requests approve|deny <id>` sends [[rpc:requests.decide]]. The origin is the connection's: `local` on the daemon's socket, `hub` on a hub link; a local client may declare `hub`, never the reverse ([[internal/requests.EffectiveOrigin]]). [[internal/requests.MayDecide]] is the only place that says who decides what: `local` decides anything, `auto` and `hub` low-risk requests only. [[internal/daemon.requestService.decideLocked]] decides only a pending request; an approval first takes a place in the executor (two workers, 32 waiting, for the whole daemon), and without one it is refused as busy and stays pending ([[finding:C1-11]]).

**Carrying out.** A worker runs the commands one after another as child processes of the same binary with the daemon's directories, from `/`, 30 minutes each ([[internal/daemon.Daemon.runChild]]), so the box lock, the configuration checks and the root log apply as for the user. The outcome keeps the last 2,000 characters of the output and replaces the request's state only while it is still `approved` ([[finding:C1-18]]). [[flow:request-approve]] goes through a package request.

**The box learns.** [[rpc:host.request.status]] returns a request of the channel's own project, waiting up to 600 seconds for a final status, with its result in the box's view of paths ([[internal/daemon.boxView]]); `devc request status --wait` asks until it is final.

**Expiry and restart.** Pending requests expire after seven days: checked at start, at every housekeeping pass, whenever a log is read to create, list or decide requests, and when a box asks about an expired one. A request found `approved` at start was interrupted and fails without running again ([[internal/daemon.requestService.recoverInterrupted]]).

**The log.** [[internal/requests.Store]] keeps `durable/realms/<realm>/requests/<project>.jsonl` (0600 in 0700), one line per change holding the whole request; the last line of an ID wins and torn lines are skipped. Only the daemon writes it. Past 4 MiB it is rewritten, by rename, with every open request and the 200 most recently changed closed ones. The service's lock serialises every change; it is held while the request event is appended to the journal, which takes the box's ingestion lock.

## What can go wrong, and what prevents it

- **A box approving its own request.** The box channel has no decide method; the origin comes from the connection; `MayDecide`.
- **A compromised hub approving a package install.** High-risk kinds are refused for origin `hub` (plan section 5).
- **Forged or extra fields.** Strict decoding and the per-kind field list.
- **Box-chosen values as options.** They come after `--` or as `--flag=value`, in argument vectors.
- **A seal outside the data.** The path is resolved against the project's data root as the box sees it before the request is stored; the refusal names only that root ([[internal/data.NormalizeInBox]]).
- **Host paths in answers.** A request's result is what devc printed on the host; the box gets it with the project's host directory written as it sees it and devc's other directories and the home as `<host>` ([[internal/daemon.boxView]]). The user's listing keeps the text as printed.
- **Floods.** The per-project limits, the log cap and the executor's places; for savepoints also the interval and the separate retention of requested ones, so requests cannot push out the owner's savepoints.
- **Another project's requests.** The channel names the project; a log line naming another project is skipped.
- **A decision racing an expiry.** Expiry reads the log again under the lock and changes only a request still pending.
- **Text on a terminal.** Summaries and results are made printable.

## Decisions that are not obvious

- **The user's own commands.** No second implementation of sealing, package installation or pull requests: the request runs exactly what the user would, with its checks ([[chunk:C10a]]).
- **Approvals need a place.** An approval that cannot run soon stays pending instead of piling up child processes.
- **Interrupted means failed.** A request that may have taken effect is never repeated.
- **Without a daemon** `devc requests` reads the logs as they are: nothing expires, and nothing can be decided.

## Where to start reading

1. [[internal/requests.Validate]]
2. [[internal/daemon.requestService.create]]
3. [[internal/requests.MayDecide]]
4. [[internal/daemon.requestService.decideLocked]]
5. [[internal/daemon.requestService.execute]] and [[internal/daemon.requestCommands]]
