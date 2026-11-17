+++
id = "hub-inbox-and-notifications"
title = "Inbox and notifications"
summary = "The inbox the hub derives from its projection; a durable record for each attention state of a session, published to ntfy with retries and cleared when the state ends; buttons with single-use action tokens; and which hub notifies."
order = 82
files = [
  "internal/hub/inbox.go",
  "internal/hub/notify.go",
  "internal/hub/config.go",
]
key = [
  "internal/hub.SectionOf",
  "internal/hub.BuildInbox",
  "internal/hub.notifier.run",
  "internal/hub.notifier.update",
  "internal/hub.notifyKey",
  "internal/hub.notifier.publish",
  "internal/hub.notifier.retryDue",
  "internal/hub.notifier.start",
  "internal/hub.buildNotification",
  "internal/hub.Server.apiAct",
  "internal/hub.Store.UseAction",
  "internal/hub.LoadConfig",
]
spec = ["doc:specs-v2.md#13"]
tests = [
  "internal/hub.TestSectionOf",
  "internal/hub.TestBuildInbox",
  "internal/hub.TestNotificationText",
  "internal/hub.TestNotifier",
  "internal/hub.TestNotifierRetries",
  "internal/hub.TestNotifierWatchedAfterAttempt",
  "internal/hub.TestActionTokens",
  "internal/hub.TestConfig",
  "test/integration.TestHub",
]
related = ["hub-machine-link", "hub-commands-and-terminal", "hub-access", "watching-sessions", "requests-and-approval"]

[[invariant]]
text = "A message is published only for a machine that is online and has named this hub its notifier, and never for a session a logged-in event stream follows at that moment."
refs = ["internal/hub.notifier.publish", "internal/hub.notifier.machineNotifies", "internal/hub.Server.watched"]
tests = ["internal/hub.TestNotifier"]

[[invariant]]
text = "Each attention state of a session has at most one record with one sequence ID, so a retried publish replaces its message rather than adding one."
refs = ["internal/hub.notifier.update", "internal/hub.NoteState", "internal/hub.Store.PutNoteState"]
tests = ["internal/hub.TestNotifierRetries"]

[[invariant]]
text = "When a session leaves an attention state, the buttons of its message stop working, and a message that was published, or whose publish was attempted (it may have reached ntfy although the answer was lost), is cleared, retried until ntfy confirms or the record is seven days old, also when the record was handled without a message after that attempt; every record goes through one function, and only one never attempted goes without a clear."
refs = ["internal/hub.notifier.update", "internal/hub.notifier.retire", "internal/hub.attempted", "internal/hub.notifier.publish", "internal/hub.notifier.clear", "internal/hub.Store.DeleteActions"]
tests = ["internal/hub.TestNotifier", "internal/hub.TestNotifierRetries", "internal/hub.TestNotifierLostAnswer", "internal/hub.TestNotifierWatchedAfterAttempt"]

[[invariant]]
text = "An action token answers one question of one session with one choice, once, within an hour; using it uses up the other buttons of its message."
refs = ["internal/hub.Store.UseAction", "internal/hub.Server.apiAct"]
tests = ["internal/hub.TestActionTokens"]

[[invariant]]
text = "The inbox is derived from the stored views and pending requests on every request; nothing about it is stored."
refs = ["internal/hub.BuildInbox", "internal/hub.SectionOf", "internal/hub.Server.apiInbox"]
tests = ["internal/hub.TestSectionOf", "internal/hub.TestBuildInbox"]

[[invariant]]
text = "A hub that starts for the first time announces none of the attention states it already holds."
refs = ["internal/hub.notifier.start"]

[[diagram]]
id = "note-states"
title = "A notification record"
kind = "graph"

[[diagram.node]]
id = "entered"
label = "a new attention state"
col = 1
row = 2
ref = "internal/hub.notifyKey"

[[diagram.node]]
id = "pending"
label = "pending"
col = 2
row = 2
ref = "internal/hub.NotePending"

[[diagram.node]]
id = "quiet"
label = "sent, without a message"
col = 2
row = 1
ref = "internal/hub.NoteSent"

[[diagram.node]]
id = "sent"
label = "sent"
col = 3
row = 2
ref = "internal/hub.NoteSent"

[[diagram.node]]
id = "cleared"
label = "cleared"
col = 4
row = 2
ref = "internal/hub.NoteCleared"

[[diagram.node]]
id = "gone"
label = "record removed"
col = 3
row = 3
ref = "internal/hub.notifier.drop"

[[diagram.edge]]
from = "entered"
to = "pending"
label = "news"
ref = "internal/hub.notifier.update"

[[diagram.edge]]
from = "entered"
to = "quiet"
label = "not news, first start"
ref = "internal/hub.notifier.start"

[[diagram.edge]]
from = "pending"
to = "quiet"
label = "a client follows, nothing attempted"
ref = "internal/hub.Server.watched"

[[diagram.edge]]
from = "pending"
to = "sent"
label = "a client follows after an attempt"
ref = "internal/hub.attempted"

[[diagram.edge]]
from = "pending"
to = "sent"
label = "published"
ref = "internal/hub.ntfyClient.publish"

[[diagram.edge]]
from = "pending"
to = "pending"
label = "failed: retry later"
ref = "internal/hub.backoff"

[[diagram.edge]]
from = "pending"
to = "gone"
label = "state ended before any attempt"
ref = "internal/hub.notifier.retire"

[[diagram.edge]]
from = "pending"
to = "cleared"
label = "state ended after an attempt"
ref = "internal/hub.notifier.retire"

[[diagram.edge]]
from = "quiet"
to = "gone"
label = "state ended"
ref = "internal/hub.notifier.update"

[[diagram.edge]]
from = "sent"
to = "cleared"
label = "state ended"
ref = "internal/hub.notifier.update"

[[diagram.edge]]
from = "cleared"
to = "cleared"
label = "clear failed: retry"
ref = "internal/hub.notifier.clear"

[[diagram.edge]]
from = "cleared"
to = "gone"
label = "ntfy confirmed, or 7 days"
ref = "internal/hub.notifier.clear"

[[diagram.edge]]
from = "cleared"
to = "pending"
label = "state came back"
ref = "internal/hub.notifier.update"
+++
## What it is for

The owner is away from the machines and wants to know when a session needs them, failed, or finished a turn, and to answer a simple question from the phone. The hub derives an inbox across all machines from its projection, and pushes a short message through a self-hosted ntfy server, with a button per answer when a question has one or two options. A notification must not be lost when ntfy is down or the hub restarts, nor linger once the session has moved on.

## How it works

**The inbox.** [[internal/hub.SectionOf]] places a session. A live one is `needs-you` when blocked or with a pending request, `failed` in state error, `finished` when a turn reached done that nobody marked seen, `working`, else `idle`. An ended one is `needs-you` with a pending request, gone once every turn was marked seen, `failed` when it ended as lost, else `finished`. Turns are marked seen with a `seen` command ([[concern:watching-sessions]]). [[internal/hub.summarize]] adds the count of pending requests and `stale` while the machine is offline. The inbox is built on every request from the database ([[internal/hub.BuildInbox]], newest change first in each section) and never stored; event streams carry the same summaries as `session` changes ([[concern:hub-commands-and-terminal]]).

**Records.** The [[term:notifier]] follows the [[term:change-log]] on a goroutine of its own ([[internal/hub.notifier.run]]): for each batch of changes it takes the latest summary of every session and hands it to [[internal/hub.notifier.update]]. [[internal/hub.notifyKey]] names the attention state: `needs-you/q/<question ID>`, `needs-you`, `failed`, or `finished/<done turn>/<turn>`, so a new question or another finished turn is news again. Each key gets a [[term:notification-record]] in `notify_states`, with a sequence ID chosen when it is made ([[internal/hub.NoteState]]). [[diagram:note-states]]

- A new key is `pending` and is published at once if it can be ([[internal/hub.notifier.publish]]): only while the machine is online and names this hub its notifier.
- If a logged-in event stream names the session at that moment, the record becomes `sent` with no message; it keeps its sequence ID if a publish was attempted before. A question-less needs-you right after an answered question is not news either.
- When the key ends, the record goes through [[internal/hub.notifier.retire]], the one way records go. One whose publish was never attempted ([[internal/hub.attempted]]: pending with no tries, or `sent` without a sequence ID) is dropped. Any other loses its buttons, becomes `cleared`, and its message is cleared at ntfy by sequence ID (`PUT /<topic>/<seq>/clear`); the record goes once ntfy confirms. A pending record counts its attempts in `Tries`, stored before each publish goes out, since ntfy may take a message whose answer is lost.
- A key that returns while its old message is still being cleared is published again under the same sequence ID.

Failed publishes and clears are retried: every 15 seconds the notifier looks at the records that are due ([[internal/hub.notifier.retryDue]]); after a failure a record waits 30 seconds, doubling up to 15 minutes, and a clear is given up once its record is seven days old. A change of a machine (online, role) also runs retryDue, so a hub that has just become the notifier sends what is still pending and still current.

**Restarts.** The notifier keeps its position in the change log in `meta.notify_pos`. After a restart it continues from there if the log still reaches back, else it compares the records with the projection ([[internal/hub.notifier.resync]]); a gap met while running does the same. On the very first start the states the hub holds are recorded as sent without a message ([[internal/hub.notifier.start]]).

**The message.** [[internal/hub.buildNotification]]: the title `<project> · <harness>`; as body the question's text, or the last message cut to 300 characters, at most 3,500 bytes, made printable; priority 4, or 3 for finished; a click URL `<public URL>/#/s/<machine>/<session>`; for a question with one or two options a button per option, and Open. It is posted as JSON to the root of the ntfy server with the topic, the bearer token from hub.toml when set, a ten-second timeout and three attempts on network errors, 408, 429 and 5xx ([[internal/hub.ntfyClient.do]]).

**Buttons.** An option button posts to `/api/act` with `X-Devc-Request: 1` and a 256-bit [[term:action-token]] in `X-Devc-Action`. [[internal/hub.Server.apiAct]] needs a device certificate but no login. The token, stored as its hash, is bound to machine, session, question ID and choice, valid for an hour, used once, and its use uses up the message's other buttons ([[internal/hub.Store.UseAction]]); a machine that is offline leaves it usable (503). The token is used up before the answer goes out, as `session.answer` with a command ID stored beside it, so a press whose outcome is uncertain ("Maybe not answered") cannot be repeated from the phone; the final receipt is awaited up to 20 seconds; the answer to the button is a short text with the outcome.

**Which hub notifies.** The machine decides ([[concern:hub-machine-link]]): every hub records the states, only the one the machine named notifier publishes. A hub that loses the role keeps its records; the new notifier publishes what is pending with it.

**hub.toml.** [[internal/hub.LoadConfig]] decodes the file strictly: `public_url` (which must be https and name the public name, else the hub does not start), `[ntfy] url` (http or https), `topic`, `token`, `[terminal] control` and `[archive]`. The server reads it once, at start. Without `[ntfy]` the notifier does nothing at all.

## What can go wrong, and what prevents it

- **Box text on the phone.** Question, message and option labels come from the box; they are made printable and cut. The click URL and the buttons' target are the hub's own.
- **A button pressed twice, or replayed.** One use, bound to one answer of one question, an hour; a question no longer pending is refused by the daemon ([[concern:input-and-receipts]]). Only a device certificate reaches `/api/act`, and failed tokens count against the certificate's rate limit.
- **A lost notification.** Records survive restarts and ntfy failures; a gap in the change log leads to a comparison with the projection.
- **Two notifications.** A hand-over between hubs can notify twice rather than lose one, an accepted limit ([[doc:docs/dev/LEFTOVER.md#5]]).
- **A slow ntfy server.** It holds up only the notifier's goroutine, never the machine links or the API.
- **A token in a URL.** Never: the button's token travels in a header ([[finding:C2-06]]).

## Decisions that are not obvious

- **States, not events, are notified** ([[finding:C2-13]], [[finding:C2-14]]): one record per attention state, so retries and hand-overs replace rather than repeat.
- **A followed session is not notified**, and a state suppressed that way is not sent later.
- **The notifier role is checked when publishing**, not when the state began: a hub that becomes notifier later sends only what is still current.
- **Buttons only for a question with one or two options** ([[internal/hub.actionOptions]]); a question with more gets Open alone, where spec section 13 speaks of the first two answers.

## Where to start reading

1. [[internal/hub.SectionOf]] and [[internal/hub.BuildInbox]]
2. [[internal/hub.notifier.run]] and [[internal/hub.notifier.update]]
3. [[internal/hub.notifier.publish]] and [[internal/hub.notifier.retryDue]]
4. [[internal/hub.Server.apiAct]] and [[internal/hub.Store.UseAction]]
