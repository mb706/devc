+++
id = "sessions-and-events"
title = "Sessions and events"
summary = "The host's half of a session: how events get from a box's spool into journals no box can reach, who owns a session ID, how sessions the box no longer runs are ended, and the view of each session derived from its journal."
order = 32
packages = ["internal/session"]
files = ["internal/proto/proto.go", "internal/proto/views.go", "internal/daemon/hub.go", "internal/cli/sessionprune.go"]
key = [
  "internal/proto.Event",
  "internal/proto.ParseSpoolLine",
  "internal/session.Store.Ingest",
  "internal/session.Store.Claim",
  "internal/session.Store.Refresh",
  "internal/session.Store.AppendHostEvent",
  "internal/session.Store.Prune",
  "internal/proto.SessionView",
  "internal/proto.SessionView.Apply",
  "internal/daemon.boxWorker.ingestLoop",
  "internal/daemon.hub.poll",
]
spec = ["doc:specs-v2.md#12", "doc:docs/dev/plan.md#3.6"]
tests = [
  "internal/proto.TestParseSpoolLineInvalid",
  "internal/proto.TestSessionViewApply",
  "internal/proto.TestTurnsAndSeen",
  "internal/session.TestIngest",
  "internal/session.TestIngestHostile",
  "internal/session.TestIngestTruncated",
  "internal/session.TestIngestAppendFailure",
  "internal/session.TestJournalSessionBudget",
  "internal/session.TestJournalProjectBudget",
  "internal/session.TestJournalSpaceReserve",
  "internal/session.TestSessionOwnership",
  "internal/session.TestRefresh",
  "internal/session.TestRefreshObservesUnderLock",
  "internal/daemon.TestSubscribeEndedAndCold",
  "test/integration.TestSessions",
  "test/integration.TestJournalBudgets",
]
related = ["security-model", "launching-a-harness", "daemon", "watching-sessions", "input-and-receipts"]

[[invariant]]
text = "Nothing a box writes reaches a journal as it was written: every spool line is parsed strictly, its data validated field by field, and the event re-encoded."
refs = ["internal/proto.ParseSpoolLine", "internal/proto.ValidateData"]
tests = ["internal/proto.TestParseSpoolLineInvalid", "internal/session.TestIngestHostile"]

[[invariant]]
text = "A box cannot write the event types only the host writes: `seen`, `answer` and `request`."
refs = ["internal/proto.HostOnly", "internal/proto.ParseSpoolLine"]
tests = ["internal/proto.TestC09Events"]

[[invariant]]
text = "A box cannot add events to a session of another project: the first project to claim a session ID owns it, and that record is never replaced."
refs = ["internal/session.Store.Claim", "internal/session.Store.writeOwner", "internal/session.Store.AppendHost"]
tests = ["internal/session.TestSessionOwnership"]

[[invariant]]
text = "The host reads a spool only beneath the box's runtime directory, never through a symlink, only from regular files, and in bounded amounts per run."
refs = ["internal/session.Store.Ingest", "internal/safefs.OpenRoot"]
tests = ["internal/session.TestIngestHostile"]

[[invariant]]
text = "An event reaches a journal at most once: the spool offset advances only past events that are durably in the journal."
refs = ["internal/session.Store.ingestFile", "internal/session.appendLines"]
tests = ["internal/session.TestIngestAppendFailure"]

[[invariant]]
text = "Ingestion appends to a journal only the events that keep the session's journal within `sessions.session_journal_max`, the project's journals within `sessions.journal_quota` (each counted in whole 4 KiB blocks plus one for its owner record) and the state file system above 1 GiB free; host events a box causes (requests, push notes) are refused the same way."
refs = ["internal/session.Store.ingestFile", "internal/session.room.fits", "internal/session.journalCost", "internal/session.Store.AppendHost"]
tests = ["internal/session.TestJournalSessionBudget", "internal/session.TestJournalProjectBudget", "internal/session.TestJournalSpaceReserve", "test/integration.TestJournalBudgets"]

[[invariant]]
text = "A budget never removes what a journal holds: events that do not fit stay in the spool, and only `devc sessions prune` removes a journal, of a session that ended before the cutoff it was given."
refs = ["internal/session.Store.ingestFile", "internal/session.Store.Prune"]
tests = ["internal/session.TestJournalSessionBudget", "internal/session.TestJournalProjectBudget", "test/integration.TestJournalBudgets"]

[[invariant]]
text = "A pruned journal never comes back: prune marks the session first and keeps its owner record, ingestion removes or skips its spool file for good however the box rewrites it, the host appends nothing more to it, and no other project can claim its ID."
refs = ["internal/session.Store.Prune", "internal/session.Store.markPruned", "internal/session.Store.ingestFile", "internal/session.Store.AppendHost"]
tests = ["internal/session.TestPruneSpoolTail", "internal/session.TestJournalProjectBudget"]

[[invariant]]
text = "A spool file makes the host record a session's owner only when an event of it is appended: empty spool files, and new sessions while the project's budget is full, record nothing."
refs = ["internal/session.Store.ingestFile", "internal/session.Store.Claim"]
tests = ["internal/session.TestJournalProjectBudget"]

[[invariant]]
text = "Within a session, seq numbers are assigned once and in order, by whoever holds the box's ingestion lock."
refs = ["internal/session.Store.Lock", "internal/session.loadJournalState"]

[[invariant]]
text = "Reconciliation never ends a session that a box started after the box was observed."
refs = ["internal/session.Store.refresh"]
tests = ["internal/session.TestRefreshObservesUnderLock"]

[[invariant]]
text = "After `session.end`, a session's view changes only in the turn a user marked seen and in its event count, last seq and last-event time."
refs = ["internal/proto.SessionView.Apply"]
tests = ["internal/proto.TestTurnsAndSeen"]

[[diagram]]
id = "session-states"
title = "The state of a session's view, as the built-in hooks move it"
kind = "graph"

[[diagram.node]]
id = "unknown"
label = "unknown"
col = 1
row = 1

[[diagram.node]]
id = "idle"
label = "idle"
col = 2
row = 1
ref = "internal/proto.StateIdle"

[[diagram.node]]
id = "working"
label = "working"
col = 3
row = 1
ref = "internal/proto.StateWorking"

[[diagram.node]]
id = "done"
label = "done"
col = 4
row = 1
ref = "internal/proto.StateDone"

[[diagram.node]]
id = "blocked"
label = "blocked"
col = 3
row = 3
ref = "internal/proto.StateBlocked"

[[diagram.node]]
id = "error"
label = "error"
col = 4
row = 3
ref = "internal/proto.StateError"

[[diagram.node]]
id = "any"
label = "any live state"
col = 1
row = 3

[[diagram.node]]
id = "ended"
label = "ended (not live)"
col = 1
row = 4
ref = "internal/proto.TypeSessionEnd"

[[diagram.edge]]
from = "unknown"
to = "idle"
label = "session-start hook"

[[diagram.edge]]
from = "idle"
to = "working"
label = "prompt submitted"

[[diagram.edge]]
from = "working"
to = "done"
label = "turn ends"

[[diagram.edge]]
from = "done"
to = "working"
label = "next prompt"

[[diagram.edge]]
from = "working"
to = "blocked"
label = "permission, question"

[[diagram.edge]]
from = "blocked"
to = "working"
label = "answer via the hook"

[[diagram.edge]]
from = "blocked"
to = "done"
label = "turn ends"

[[diagram.edge]]
from = "working"
to = "error"
label = "status report (OSC)"

[[diagram.edge]]
from = "any"
to = "ended"
label = "session.end"

[[diagram]]
id = "event-path"
title = "From a spool file to the daemon's views"
kind = "graph"

[[diagram.node]]
id = "spool"
label = "spool file (box)"
col = 1
row = 1
zone = "box"
ref = "internal/paths.Paths.SpoolDir"

[[diagram.node]]
id = "ingester"
label = "ingestion (daemon or CLI)"
col = 2
row = 1
zone = "host"
ref = "internal/session.Store.Ingest"

[[diagram.node]]
id = "owners"
label = "session owners"
col = 2
row = 2
zone = "host"
ref = "internal/paths.Paths.SessionOwnersDir"

[[diagram.node]]
id = "journal"
label = "journal"
col = 3
row = 1
zone = "host"
ref = "internal/paths.Paths.JournalFile"

[[diagram.node]]
id = "reconcile"
label = "reconcile"
col = 3
row = 2
zone = "host"
ref = "internal/session.Store.Refresh"

[[diagram.node]]
id = "view"
label = "session views"
col = 4
row = 1
zone = "host"
ref = "internal/daemon.hub"

[[diagram.node]]
id = "subscribers"
label = "subscribers, commands"
col = 4
row = 2
zone = "host"

[[diagram.edge]]
from = "spool"
to = "ingester"
label = "read through safefs"
ref = "internal/safefs.Root.ReadFrom"

[[diagram.edge]]
from = "ingester"
to = "owners"
label = "claim the ID"
ref = "internal/session.Store.Claim"

[[diagram.edge]]
from = "ingester"
to = "journal"
label = "validated, with seq"
ref = "internal/session.appendLines"

[[diagram.edge]]
from = "reconcile"
to = "journal"
label = "session.end: lost"
ref = "internal/session.Store.endSessions"

[[diagram.edge]]
from = "journal"
to = "view"
label = "new lines"
ref = "internal/daemon.hub.tailLocked"

[[diagram.edge]]
from = "view"
to = "subscribers"
label = "batches, signals"
ref = "internal/daemon.hub.enqueueLocked"
+++
## What it is for

A harness works in a box, out of sight. The user wants to know, on the host or a phone, what each session is doing: working, waiting for a permission, asking a question, done. devc learns this from the harness and keeps a record of every session that no box can change. `devc ps`, the daemon's commands and the hubs rely on it, so a compromised box must not be able to rewrite it, speak for another project's session, or overwhelm the host with it.

## How it works

An event is one JSON object per line: version, session ID, timestamp, type and data ([[internal/proto.Event]]; types in [[surface:events]]). A [[term:session]] is one run of a harness. Its box's half, up to the [[term:spool]] file, is [[concern:launching-a-harness]] ([[flow:hook-to-spool]]).

**Ingestion.** [[term:ingestion]] copies new events from a box's spool into journals. The daemon runs it for every running box ([[internal/daemon.boxWorker.ingestLoop]]): at once on a hint, every two seconds, every 250 ms for five seconds after a run that brought events or left more, never while the spool's names and sizes stay the same. The CLI runs it itself in `devc ps` (without a daemon), `attach`, `stop`, `ingest`, `bank` and around `down`. Both take the box's ingestion lock, so seq numbers are never assigned twice.

[[internal/session.Store.Ingest]] opens the box's runtime directory as a [[pkg:internal/safefs]] root and lists `spool` beneath it. For each regular file named `<session ID>.jsonl` it checks the session's owner (below), reads complete lines from the offset in `spool-offsets.json`, parses each with [[internal/proto.ParseSpoolLine]], and gives each valid event the session's next `seq`, the host's time `at` and its spool offset. The batch is appended to `durable/realms/<realm>/journal/<project>/<session>.jsonl` ([[term:journal]]) in one synced write, and only then is the offset saved. [[flow:event-ingestion]] follows one line to a subscriber.

**Budgets** ([[term:journal-budget]]). Before appending, [[internal/session.Store.ingestFile]] keeps only the leading events that fit ([[internal/session.room.fits]]): the session's journal within `sessions.session_journal_max` (128 MiB), the project's journals within `sessions.journal_quota` (2 GiB; each journal counts its 4 KiB blocks plus one for its owner record, [[internal/session.journalCost]]) and 1 GiB free on the state file system. The rest stays in the spool; one note names the budget ([[internal/session.StopText]]), and the file is not read again until that budget has room. Of the host's events, `session.end`, `seen` and `answer` always go in. `devc sessions prune` removes journals of ended sessions and marks them, so their spool is never read again ([[internal/session.Store.Prune]]).

**Ownership.** A box chooses session IDs and could reuse another project's. [[internal/session.Store.Claim]] records the owner of an ID in `durable/session-owners/<id>`, created exclusively by a hard link and never removed. A spool file of another project's session is refused whole before it is read, remembered in the offending project's offsets and reported once ([[finding:C1-01]]); an unowned ID is claimed only when an event of it is appended, so an empty spool file records nothing. An ID without a record whose journals predate the records goes to the project whose journal's first event was ingested earliest.

**Host events.** `seen`, `answer` and `request` are written only by the host ([[internal/proto.HostOnly]]). [[internal/session.Store.AppendHostEvent]] appends one under the ingestion lock after one ingestion pass ([[internal/daemon.Daemon.recordHost]]). That pass is bounded and may fail, so box events can still follow the host's.

**Reconciliation.** A harness that dies without a word leaves its session open. [[term:reconciliation]] ([[internal/session.Store.Refresh]]) ingests, then observes the box under the same lock. A running box that is not ready is left alone. In a ready box, the sessions open at that moment are noted, the box is asked which sessions are alive (over the channel, or `devc box session ps --json` through the engine), the spool is ingested again, and the noted sessions the box did not report end with reason `lost`. A box that does not run ends every open session as `lost`, `devc down` as `box-down` ([[internal/session.Store.RefreshStopped]]). The daemon reconciles every 30 seconds.

**The derived view.** [[internal/proto.SessionView.Apply]] folds a journal's events into a [[term:session-view]]: harness, worktree, state, pending question, last message, turns. The state is that of the last `state` event ([[diagram:session-states]]); before one it is `unknown`, after `session.end` it is `ended`. A `prompt` adds a turn and clears the question; `done` marks the turn done; `seen` records the turn a user looked at. [[internal/proto.SessionView.NeedsYou]]: live and blocked, or done with a turn nobody marked seen. Times are the host's `at`, which a box cannot choose.

**The daemon's views.** The daemon's journal follower ([[internal/daemon.hub]]) keeps a view per journal of every configured project, reads the journals that grew and drops the views of journals that are gone; journals of sessions that ended more than 30 days ago are read only when asked for ([[concern:daemon]]).

## What can go wrong, and what prevents it

The box controls its spool completely: content, size, whether its entries are files.

- **Redirection.** A symlink, FIFO or directory in place of `spool` or a spool file is refused or skipped.
- **Malformed lines.** Unknown fields, trailing data, missing fields, another version, another session's ID, unknown and host-only types, bad timestamps and data that fails [[internal/proto.ValidateData]] are counted and reported, never stored. A line over 64 KiB is skipped whole, also across runs.
- **Raw bytes.** Journals hold re-encoded events, whose text may hold control characters; [[internal/proto.Printable]] is applied before it reaches a terminal.
- **Another project's session.** Ownership, above; and [[internal/session.Store.AppendHost]] never starts a journal for another project's session.
- **Floods.** One run takes at most 8 MiB and 20,000 events per file and looks at 10,000 spool entries; a box that floods hints gets one run per 50 ms. Over time the budgets bound `durable/`, however the box spreads its events; other devc processes' appends count within ten seconds.
- **Truncation and crashes.** A file that shrinks below the offset gets a note in the journal and is read on from its end. A torn journal line is skipped by readers. If the offset was not saved, ingestion resumes after the last spool offset the journal holds ([[finding:B6]]).
- **A lying live-session report.** [[internal/session.ParseLive]] rejects the whole report when an entry is malformed or anything but white space follows it ([[internal/proto.StrictJSON]]), and reconciliation then changes nothing. Any box can report `[]` and so end its own open sessions as `lost`.
- **Late events.** After `session.end` events are dropped; a fully ingested file of an ended session is removed.

## Decisions that are not obvious

- **Files, with a hint on the side.** The spool works without a daemon and keeps what a box wrote while none ran; the hint only shortens the wait, and a lost hint costs at most two seconds.
- **Two clocks.** `ts` is the box's claim; `at` and `seq` are the host's, and comparisons that matter use them ([[internal/session.Store.firstRecorder]]).
- **Observe under the lock, after ingesting.** A session whose start is in the journal by then started before the observation; one started later is still in the spool and untouched ([[finding:B7]]).
- **Views from journals.** The CLI and the daemon derive the same view from the same file, so `devc ps` answers alike with and without a daemon; only a last event cut off before its newline (a crash) the daemon reads not until the next append.

## Where to start reading

1. [[internal/session.Store.ingestFile]]
2. [[internal/proto.ParseSpoolLine]]
3. [[internal/session.Store.Claim]]
4. [[internal/session.Store.refresh]]
5. [[internal/proto.SessionView.Apply]]
6. [[internal/daemon.hub.poll]]
