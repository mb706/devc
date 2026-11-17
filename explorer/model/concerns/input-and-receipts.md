+++
id = "input-and-receipts"
title = "Input and receipts"
summary = "Typing into live sessions from outside: commands (send, answer, interrupt, keys, start, stop, seen) with a receipt stored before anything is typed, a line of commands per session, the wait for a session at rest and for quiet at its terminal, the input adapters and the verified paste."
order = 33
files = [
  "internal/daemon/commands.go",
  "internal/daemon/receipts.go",
  "internal/daemon/adapter.go",
  "internal/daemon/api.go",
  "internal/cli/inputcmds.go",
]
key = [
  "internal/daemon.commander.submit",
  "internal/daemon.receiptStore.create",
  "internal/daemon.commander.execute",
  "internal/daemon.atRest",
  "internal/daemon.target.gate",
  "internal/daemon.target.begin",
  "internal/daemon.adapterFor",
  "internal/daemon.paste.Send",
  "internal/daemon.commander.answerByHook",
  "internal/daemon.commander.runStart",
  "internal/daemon.commander.resume",
  "internal/proto.Receipt",
]
spec = ["doc:specs-v2.md#12.4"]
tests = [
  "internal/daemon.TestCommands",
  "internal/daemon.TestCommandsSurviveRestart",
  "internal/daemon.TestDeliveryChecks",
  "internal/daemon.TestReceiptStore",
  "internal/daemon.TestAtRest",
  "internal/daemon.TestGateDecision",
  "internal/daemon.TestPromptHashes",
  "internal/daemon.TestStartAndStop",
  "internal/daemon.TestExtensionAdapter",
  "test/integration.TestInput",
]
related = ["daemon", "launching-a-harness", "sessions-and-events", "box-channel"]

[[invariant]]
text = "A command ID acts at most once while its receipt is kept (until seven days after it became final): a repeated ID returns the stored receipt and does nothing else."
refs = ["internal/daemon.commander.submit", "internal/daemon.receiptStore.create"]
tests = ["internal/daemon.TestReceiptStore", "internal/daemon.TestCommands"]

[[invariant]]
text = "Nothing is typed into a session before `delivering` is durably stored, and a command found delivering after a restart is never typed again."
refs = ["internal/daemon.target.begin", "internal/daemon.commander.setDelivering", "internal/daemon.commander.resume"]
tests = ["internal/daemon.TestDeliveryChecks", "internal/daemon.TestCommandsSurviveRestart"]

[[invariant]]
text = "A command is bound to its session's project and ID when it is accepted, and is never looked up by ID alone again."
refs = ["internal/daemon.Daemon.commandTarget", "internal/daemon.commander.execute"]
tests = ["internal/daemon.TestCommandsSurviveRestart"]

[[invariant]]
text = "A queued send is typed only into a session at rest, checked again just before the first key."
refs = ["internal/daemon.atRest", "internal/daemon.target.begin"]
tests = ["internal/daemon.TestAtRest", "internal/daemon.TestDeliveryChecks"]

[[invariant]]
text = "An answer reaches only the question it was accepted for; a stale answer is refused and never typed."
refs = ["internal/daemon.Daemon.apiAnswer", "internal/daemon.commander.answerByHook", "internal/daemon.target.begin"]
tests = ["internal/daemon.TestDeliveryChecks"]

[[invariant]]
text = "Only one injection reaches a session at a time."
refs = ["internal/daemon.line", "internal/daemon.commander.execute"]

[[invariant]]
text = "Text with control characters other than newline and tab, invalid UTF-8 or more than 256 KiB is never typed."
refs = ["internal/proto.CheckInputText", "internal/daemon.Daemon.apiSend"]
tests = ["internal/proto.TestInputTextAndIDs"]

[[diagram]]
id = "receipt-states"
title = "A command's receipt"
kind = "graph"

[[diagram.node]]
id = "accepted"
label = "accepted"
col = 1
row = 1
ref = "internal/proto.OutcomeAccepted"

[[diagram.node]]
id = "delivering"
label = "delivering"
col = 2
row = 2
ref = "internal/proto.OutcomeDelivering"

[[diagram.node]]
id = "delivered"
label = "delivered"
col = 4
row = 1
ref = "internal/proto.OutcomeDelivered"

[[diagram.node]]
id = "tns"
label = "typed-not-submitted"
col = 4
row = 2
ref = "internal/proto.OutcomeTypedNotSubmitted"

[[diagram.node]]
id = "unknown"
label = "unknown"
col = 4
row = 3
ref = "internal/proto.OutcomeUnknown"

[[diagram.node]]
id = "refused"
label = "refused"
col = 2
row = 4
ref = "internal/proto.OutcomeRefused"

[[diagram.edge]]
from = "accepted"
to = "accepted"
label = "busy again; not stored"

[[diagram.edge]]
from = "accepted"
to = "delivering"
label = "before typing"
ref = "internal/daemon.target.begin"

[[diagram.edge]]
from = "accepted"
to = "refused"
label = "invalid, ended, stale"

[[diagram.edge]]
from = "delivering"
to = "delivered"
label = "prompt or state seen"
ref = "internal/daemon.commander.waitPrompt"

[[diagram.edge]]
from = "delivering"
to = "tns"
label = "no prompt, two keys"

[[diagram.edge]]
from = "delivering"
to = "unknown"
label = "no confirmation; restart"
ref = "internal/daemon.commander.resume"

[[diagram.edge]]
from = "delivering"
to = "refused"
label = "the box: no such pane"
ref = "internal/daemon.refusedOrUnknown"

[[diagram.edge]]
from = "delivering"
to = "accepted"
label = "start: prompt"
ref = "internal/daemon.commander.runStart"
+++
## What it is for

From the host, or a phone through a hub, the user wants to type into a session: a prompt, an answer, an interrupt, a key, or start a new session with a first prompt. The text must arrive once, in the right session, when the harness is ready for it, if possible not while someone types in the same terminal, and the user wants to know whether it arrived. Each of these is a [[term:command]] whose [[term:receipt]] the daemon stores before it acts (`seen` stores it after). This page is the daemon's half; the box's half (pasting into the pane) is in [[concern:launching-a-harness]].

## How it works

**Accepting.** `devc send`, `answer`, `interrupt`, `keys` and `seen` ([[internal/cli.newInputCmds]]) and `devc <harness> --detach --prompt` need the daemon. Each command carries an ID the client chose (session-ID form or UUID; `--id` repeats one). The API handler decodes strictly, checks the ID and session, and binds the command to its session by project and ID ([[internal/daemon.Daemon.commandTarget]]): a bare ID must be unique among the daemon's sessions ([[finding:C1-01]]). An unknown, ambiguous or ended session, or text [[internal/proto.CheckInputText]] refuses, gives a final `refused` receipt.

**Receipts.** [[internal/daemon.receiptStore]] keeps `local/receipts/<id>.json` (0600 in 0700). A new record is written to a temporary file, synced, hard-linked to its final name, which fails if the ID exists, and the directory is synced; later states replace it by rename, again with the directory synced ([[finding:D1-11]]). [[internal/daemon.commander.submit]] returns the stored receipt for a known ID and does nothing more; otherwise it stores `accepted` and answers with a copy taken before the command can run ([[finding:C1-19]]). The text is dropped once the receipt is final. [[diagram:receipt-states]] shows the outcomes; [[rpc:command.get]] waits up to 600 seconds for a final one.

**A line per session.** [[internal/daemon.commander]] keeps two queues per session, each drained by a goroutine of its own: one for commands that act now (`--steer` sends, answers, interrupts, keys, stops) and one for queued sends and a start's first prompt. They share an inject lock, so one injection at a time reaches the session, and a queued send that waits never holds up an answer or interrupt the session may be waiting for.

**Executing.** [[internal/daemon.commander.execute]] waits up to 60 seconds for the session to be known, refuses an ended one, and finds the harness's manifest. A queued command then waits until the session is [[term:at-rest]] ([[internal/daemon.atRest]]): idle, done or error with a state reported after the last prompt and no question pending; a session that never reported a state counts as at rest 60 seconds after it started. That wait has no limit while the daemon runs; a session that ends refuses the command. Then the box's channel (up to 60 seconds), the inject lock and the adapter. A stop goes through the engine instead (`devc box session stop`), followed by a reconciliation.

**The gate and the last check.** Before its first key every adapter passes the [[term:input-gate]] ([[internal/daemon.target.gate]]): it asks the agent for the clients showing the session's window (`box.input.clients`) and waits, polling every half second, while one was active in the last three seconds ([[internal/daemon.gateDecision]]). After 60 seconds, checked between calls, it types anyway. Then [[internal/daemon.target.begin]] checks once more that the session is live, that a queued send still finds it at rest (otherwise the command goes back to waiting, [[finding:C1-14]]) and that an answer's question is still the pending one ([[finding:C1-13]]), and stores `delivering`; if that cannot be stored, nothing is typed and the command is tried again ten seconds later ([[finding:C1-06]]).

**Adapters.** [[internal/daemon.adapterFor]] uses the manifest's `input.adapter`. Every built-in manifest names `paste`. `mod` and `app-server` are not built: the daemon uses paste and says so in the receipt. `extension` (pi) hands a message to devc's extension in the harness through `box.input.submit` while one is connected, and otherwise types it; interrupts and dialog answers are always typed.

- *Send* ([[internal/daemon.paste.Send]]): `box.input.paste` (a bracketed paste, the submit key 150 ms later), then up to five seconds for a `prompt` event carrying the SHA-256 of the text (also trimmed) after the seq seen before typing ([[internal/daemon.commander.waitPrompt]]). Without one: the gate again, one more submit key, three seconds, else `typed-not-submitted`. A harness without hooks gives `unknown`.
- *Interrupt*: the manifest's interrupt keys (default Escape), confirmed by any state event within five seconds.
- *Answer*: first to a hook that waits for this question ([[internal/daemon.commander.answerByHook]], waiting two seconds for one while a question is pending); a hook waiting for another question refuses it as stale. Otherwise the manifest's answer keys with `{n}`, or the text pasted, confirmed by a state change. A delivered answer is recorded as a host `answer` event, saying which way it went. [[flow:question-answer]] goes through it.
- *Keys*: one to 32 names from a fixed list.
- *Start* ([[internal/daemon.commander.runStart]]): the project must list the harness, have no configuration problems, a running box (it waits up to ten seconds for the watch to see one just started) and a launch description. After `delivering` it creates the worktree if asked (under the box lock) and runs `devc box session start --detach` with a new session ID through the engine; the first prompt is queued like a send.
- *Seen*: answered at once; it records a `seen` event first and only then, when given an ID, stores a final receipt ([[internal/daemon.Daemon.apiSeen]]).

**After a restart.** [[internal/daemon.commander.resume]] refuses unfinished commands accepted more than seven days ago, even a `delivering` one that may have typed; otherwise it turns `delivering` into `unknown` and queues `accepted` commands again in their order. Final receipts older than seven days are removed, also by housekeeping ([[internal/daemon.commander.prune]]); their IDs then count as new.

## What can go wrong, and what prevents it

- **Typed twice.** The durable `delivering` state and the receipt per ID, for as long as the receipt is kept.
- **The wrong session.** Binding at acceptance by project and ID; the box's channel is the session's project's; in the box the pane is found by its window's session tag.
- **Typed over a person.** The input gate: a wait of about a minute at most.
- **Typed into a busy harness, or into the next question.** At rest, and the last check before the first key.
- **Text that becomes commands.** [[internal/proto.CheckInputText]]; in the box the text reaches tmux on standard input and key names match a strict pattern ([[internal/boxside.Input.Paste]]).
- **A box that lies.** `delivered` means the box's hook reported a prompt with the text's hash; a compromised box can report one without typing anything. Box answers are decoded strictly and their text made printable in receipts.
- **A full disk.** A command whose state cannot be stored waits instead of typing.

## Decisions that are not obvious

- **Two queues per session.** "One command at a time per session" still holds per queue, and the inject lock keeps injections apart ([[chunk:C09]]).
- **At rest needs a state after the last prompt**, so a queued send cannot slip in between a delivered prompt and the harness's `working`.
- **Command IDs are restricted** to session-ID and UUID forms: they name files.
- **Fallback to paste** rather than refusing, with a note in the receipt.

## Where to start reading

1. [[internal/daemon.Daemon.apiSend]] and [[internal/daemon.commander.submit]]
2. [[internal/daemon.receiptStore.create]]
3. [[internal/daemon.commander.execute]]
4. [[internal/daemon.target.begin]]
5. [[internal/daemon.paste.Send]]
6. [[internal/daemon.commander.answerByHook]]
