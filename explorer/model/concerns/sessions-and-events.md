+++
id = "sessions-and-events"
title = "Sessions and events"
summary = "How what a harness does in a box becomes events, and how they get from the box's spool into the host's journals and the daemon's view of every session."
order = 10
packages = ["internal/session"]
files = ["internal/proto/proto.go", "internal/boxside/hook.go", "internal/boxside/spool.go", "internal/daemon/hub.go"]
key = [
  "internal/proto.Event",
  "internal/boxside.RunHook",
  "internal/boxside.AppendEvents",
  "internal/boxside.SendHintTo",
  "internal/daemon.boxWorker.ingestLoop",
  "internal/session.Store.Ingest",
  "internal/proto.ParseSpoolLine",
  "internal/session.Store.Claim",
  "internal/session.Store.Refresh",
  "internal/daemon.hub.poll",
]
spec = ["doc:specs-v2.md#12", "doc:docs/dev/plan.md#3.6"]
tests = [
  "internal/boxside.TestHookMappings",
  "internal/proto.TestParseSpoolLineInvalid",
  "internal/session.TestIngestHostile",
  "internal/session.TestSessionOwnership",
  "internal/session.TestIngestAppendFailure",
  "test/integration.TestSessions",
]

[[invariant]]
text = "Nothing a box writes reaches a journal as it was written: every spool line is parsed strictly, its data validated field by field, and the event re-encoded."
refs = ["internal/proto.ParseSpoolLine", "internal/proto.ValidateData"]
tests = ["internal/proto.TestParseSpoolLineInvalid", "internal/session.TestIngestHostile"]

[[invariant]]
text = "A box cannot write the event types only the host writes: `seen`, `answer` and `request`."
refs = ["internal/proto.HostOnly", "internal/proto.ParseSpoolLine"]

[[invariant]]
text = "A box cannot add events to a session of another project: the first project to record a session ID owns it."
refs = ["internal/session.Store.Claim"]
tests = ["internal/session.TestSessionOwnership"]

[[invariant]]
text = "The host reads a spool only beneath the box's runtime directory, never through a symlink, only from regular files, and in bounded amounts per run."
refs = ["internal/session.Store.Ingest", "internal/safefs.OpenRoot"]
tests = ["internal/session.TestIngestHostile"]

[[invariant]]
text = "An event is appended to a journal at most once: the spool offset advances only past events that are durably in the journal."
refs = ["internal/session.Store.ingestFile"]
tests = ["internal/session.TestIngestAppendFailure"]

[[invariant]]
text = "A hook never disturbs the harness: the receiver does not fail and prints nothing but an answer to a question."
refs = ["internal/boxside.RunHook"]
tests = ["internal/boxside.TestHookMappings"]

[[diagram]]
id = "event-path"
title = "From a hook to the daemon's view"
kind = "graph"

[[diagram.node]]
id = "harness"
label = "harness"
col = 1
row = 1
zone = "box"

[[diagram.node]]
id = "hook"
label = "devc box hook"
col = 2
row = 1
zone = "box"
ref = "internal/boxside.RunHook"

[[diagram.node]]
id = "spool"
label = "spool file"
col = 3
row = 1
zone = "box"
ref = "internal/boxside.AppendEvents"

[[diagram.node]]
id = "agent"
label = "box agent"
col = 2
row = 2
zone = "box"
ref = "internal/boxside.Agent.relay"

[[diagram.node]]
id = "ingester"
label = "ingester"
col = 3
row = 2
zone = "host"
ref = "internal/daemon.boxWorker.ingestLoop"

[[diagram.node]]
id = "journal"
label = "journal"
col = 3
row = 3
zone = "host"
ref = "internal/paths.Paths.JournalFile"

[[diagram.node]]
id = "view"
label = "session views"
col = 2
row = 3
zone = "host"
ref = "internal/daemon.hub.poll"

[[diagram.node]]
id = "subscribers"
label = "subscribers"
col = 1
row = 3
zone = "host"

[[diagram.edge]]
from = "harness"
to = "hook"
label = "hook input (JSON)"

[[diagram.edge]]
from = "hook"
to = "spool"
label = "append events"
ref = "internal/boxside.AppendEvents"

[[diagram.edge]]
from = "hook"
to = "agent"
label = "host.hint"
ref = "internal/boxside.SendHintTo"

[[diagram.edge]]
from = "agent"
to = "ingester"
label = "wake"
ref = "internal/daemon.boxWorker.acceptStreams"

[[diagram.edge]]
from = "spool"
to = "ingester"
label = "read through safefs"
ref = "internal/session.Store.Ingest"

[[diagram.edge]]
from = "ingester"
to = "journal"
label = "validated, with seq"
ref = "internal/session.Store.ingestFile"

[[diagram.edge]]
from = "journal"
to = "view"
label = "new lines"
ref = "internal/daemon.hub.poll"

[[diagram.edge]]
from = "view"
to = "subscribers"
label = "events.subscribe"
+++
## What it is for

A harness works in a box, out of sight. From the host, and from a phone through the hub, the user wants to know what each session is doing: working, waiting for a permission, asking a question, done. devc learns this from the harness itself, through its hooks, and keeps a record of every session that no box can change. That record is what `devc ps`, the daemon's commands and the hub rely on, so a compromised box must not be able to rewrite it, speak for another project's session, or overwhelm the host with it.

## How it works

An event is one JSON object per line: version, session ID, timestamp, type and data ([[internal/proto.Event]]). The types are in [[surface:events]]. A session is one run of a harness ([[term:session]]).

**In the box.** A harness's manifest installs hooks that run `devc box hook <kind> <event>` with the hook's JSON on standard input. [[internal/boxside.RunHook]] maps the input to events: for Claude Code ([[internal/boxside.mapClaude]]), a submitted prompt becomes a `prompt` event with the hash and length of the text and a `state` event "working". The session is the one in `DEVC_SESSION`, which the launcher set; without a valid one the hook does nothing. [[internal/boxside.AppendEvents]] validates each event as the host will, and appends them in one write to `/run/devc/spool/<session>.jsonl`, which is `<runtime>/<box-id>/spool` on the host ([[internal/paths.Paths.SpoolDir]]). Then the hook sends `host.hint` through the box agent ([[internal/boxside.SendHintTo]]), a notification that only means "look now". Events themselves never travel over the [[term:box-channel]].

**Ingestion.** The daemon runs a worker for every running box with an ingester of its own ([[internal/daemon.boxWorker.ingestLoop]]). It runs at once on a hint, every 250 ms while events arrive and every 2 s otherwise, and skips a run when no spool file changed ([[internal/session.Store.SpoolSnapshot]]). [[internal/session.Store.Ingest]] takes the box's ingestion lock, opens the box's runtime directory through [[pkg:internal/safefs]], and reads each spool file from the offset it reached last time. Every line passes [[internal/proto.ParseSpoolLine]]. A valid event gets the next `seq` of its session and the host's time (`at`), and the batch is appended to the session's journal, `durable/realms/<realm>/journal/<project>/<session>.jsonl` ([[term:journal]]).

The CLI ingests the same files itself, under the same lock, when it shows or acts on sessions (`devc ps`, `devc attach`, `devc ingest`, `devc bank`): [[internal/session.Store.Refresh]] ingests and then reconciles, ending with reason `lost` every session the journal shows open but the box no longer runs.

**The daemon's view.** The journals are the source of truth. The daemon's hub ([[internal/daemon.hub.poll]]) reads their new complete lines, after an ingestion run that brought events and on a timer, folds them into a view per session (state, pending question, last message, turns) and hands them to the subscribers of `events.subscribe`: `devc events --follow`, `devc ps --watch`, and the hubs over the machine link. The daemon's commands that wait for a prompt or a state change re-check the views whenever one changes ([[internal/daemon.commander.waitPrompt]]). Everything in the view can be rebuilt from the journals.

## What can go wrong, and what prevents it

The box controls its spool completely: the content of the files, their size, whether they are files at all.

- **Redirection.** A symlink, FIFO or directory in place of a spool file is refused: the spool is resolved beneath the runtime directory's descriptor without following symlinks, and only regular files are read ([[internal/safefs.OpenRoot]]).
- **Malformed lines.** [[internal/proto.ParseSpoolLine]] refuses unknown fields, trailing data, missing fields, another version, another session's ID, unknown types, host-only types and bad timestamps, and [[internal/proto.ValidateData]] checks each type's data field by field with size limits. Invalid lines are counted and reported, never stored. A line longer than 64 KiB is skipped as a whole.
- **Raw bytes.** The journal holds the re-encoded event, never the box's bytes. Text may still contain control characters; [[internal/proto.Printable]] is applied before any of it reaches a terminal.
- **Another project's session.** A box could name its spool file after a session ID it saw elsewhere. [[internal/session.Store.Claim]] records the owner of every session ID in `durable/session-owners/` when the ID first appears, and a spool file for a session of another project is refused and reported ([[finding:C1-01]]).
- **Floods.** One run takes at most 8 MiB and 20,000 events from a file and comes back for the rest; at most 10,000 spool entries are looked at. A box that floods hints still gets at most one run per 50 ms.
- **Truncation and crashes.** A spool file that shrinks below the offset gets a note in the journal, and ingestion continues at its end. The offset advances only past events that are durably in the journal; if it was not saved, ingestion resumes after the last event the journal holds ([[finding:B6]]).
- **Late events.** After `session.end`, further events of that session are dropped; a spool file whose session has ended and that holds nothing more is removed.

## Decisions that are not obvious

- **Files, with a hint on the side.** The spool works without a daemon and keeps what a box wrote while the daemon was not running; the hint only shortens the wait. The daemon still polls, so a lost hint costs at most two seconds.
- **The box validates too.** [[internal/boxside.AppendEvents]] replaces an event the host would refuse by a note, so a mapping bug shows up in the journal instead of disappearing.
- **The hook never fails.** Some harnesses add hook output to the model's context, and a failing hook must not disturb a session; [[internal/boxside.RunHook]] recovers from everything and prints only an answer to a question.
- **Two clocks.** `ts` is the box's claim; `at` is when the host ingested the event, and `seq` is the host's order. Where the host compares sessions by time, as in resolving which project owns a session ID recorded before ownership records existed, it uses `at` ([[internal/session.Store.firstRecorder]]).

## Where to start reading

1. [[internal/boxside.RunHook]] and [[internal/boxside.mapClaude]]
2. [[internal/boxside.AppendEvents]]
3. [[internal/session.Store.ingestFile]]
4. [[internal/proto.ParseSpoolLine]]
5. [[internal/daemon.boxWorker.ingestLoop]]
6. [[internal/daemon.hub.poll]]
