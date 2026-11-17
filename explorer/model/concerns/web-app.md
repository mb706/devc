+++
id = "web-app"
title = "The web app"
summary = "The hub's web app in plain JavaScript: how the hub embeds and serves it under a strict content security policy, how it starts, its single store fed by snapshots and one event stream, how it resynchronises after a gap, what keeps it bounded, commands whose outcome is not known, the terminal, how box-made text is rendered, and the development mode that drives it."
order = 84
packages = ["web"]
files = [
  "internal/hub/static.go",
  "internal/hub/web.go",
  "internal/hub/devmode.go",
  "internal/hub/devmachine.go",
  "internal/hub/devterm.go",
]
key = [
  "internal/hub.Server.serveFile",
  "internal/hub.contentSecurityPolicy",
  "internal/hub.Store.changesAfter",
  "internal/hub.Server.apiCommand",
  "internal/hub.rpcStatus",
  "internal/hub.Server.apiProjects",
  "internal/hub.cleanChanges",
  "internal/hub.newDev",
  "internal/hub.Server.runDevMachines",
]
spec = ["doc:specs-v2.md#13", "doc:docs/guide.md#the-web-app", "doc:docs/hub-deployment.md#9"]
tests = [
  "internal/hub.TestStaticFiles",
  "internal/hub.TestAppReferences",
  "internal/hub.TestNoInlineCode",
  "internal/hub.TestWebRoutes",
  "internal/hub.TestWebEndpoints",
  "internal/hub.TestDevMode",
  "internal/hub.TestStreamRetention",
  "internal/hub.TestRPCErrors",
  "test/integration.TestWebApp",
]
related = ["hub-commands-and-terminal", "hub-access", "hub-inbox-and-notifications", "input-and-receipts", "watching-sessions"]

[[invariant]]
text = "Every response of the hub carries a content security policy that allows scripts, styles and connections from its own origin only, and no inline code."
refs = ["internal/hub.contentSecurityPolicy", "internal/hub.Server.setHeaders"]
tests = ["internal/hub.TestStaticFiles", "internal/hub.TestNoInlineCode"]

[[invariant]]
text = "The app's files are the embedded ones, served with a type per extension; any other path below them is 404."
refs = ["internal/hub.loadStatic", "internal/hub.Server.serveFile"]
tests = ["internal/hub.TestStaticFiles", "internal/hub.TestAppReferences"]

[[invariant]]
text = "Text that originates in a box reaches the page only as text nodes and attribute values: the app builds no HTML from strings."
refs = ["file:web/app/lib.js~export const html", "internal/hub.cleanEvent", "internal/hub.cleanChanges"]
tests = ["internal/hub.TestNoInlineCode"]

[[invariant]]
text = "An open session keeps at most 1,000 events and draws at most 300 rows until asked for more; a folded tool run holds at most 200 calls."
refs = ["file:web/app/timeline.js~export const MAX_EVENTS", "file:web/app/session.js~const RENDER_STEP"]

[[invariant]]
text = "A command keeps its client-made ID through every retry; one whose outcome is unknown stays pending (an uncertain `seen` is dropped) until a receipt settles it, the user dismisses it, or, for a prompt, a matching prompt event arrives."
refs = ["file:web/app/api.js~export async function command", "file:web/app/store.js~export async function run(", "file:web/app/store.js~function settleOutbox", "internal/hub.rpcStatus"]
tests = ["internal/hub.TestRPCErrors", "test/integration.TestWebApp"]

[[invariant]]
text = "A stream message or a detail read never replaces a session summary with one older by stream position; a snapshot replaces the summaries without that check."
refs = ["file:web/app/store.js~function putSession", "file:web/app/store.js~export async function loadSnapshot"]

[[invariant]]
text = "The development mode starts only with DEVC_DEV=1 and a loopback address, and no machine certificate reaches it."
refs = ["internal/hub.newDev"]
tests = ["internal/hub.TestDevMode"]

[[diagram]]
id = "data-flow"
title = "The web app's data flow"
kind = "graph"

[[diagram.node]]
id = "hubread"
label = "hub: inbox, sessions"
col = 1
row = 1
zone = "hub"
ref = "internal/hub.Server.apiInbox"

[[diagram.node]]
id = "hubstream"
label = "hub: event stream"
col = 1
row = 2
zone = "hub"
ref = "internal/hub.Server.apiStream"

[[diagram.node]]
id = "hubcmd"
label = "hub: commands"
col = 1
row = 3
zone = "hub"
ref = "internal/hub.Server.apiCommand"

[[diagram.node]]
id = "api"
label = "api.js: requests"
col = 2
row = 1
ref = "file:web/app/api.js~async function request"

[[diagram.node]]
id = "stream"
label = "stream.js: EventSource"
col = 2
row = 2
ref = "file:web/app/stream.js~function open()"

[[diagram.node]]
id = "command"
label = "api.js: command, retries"
col = 2
row = 3
ref = "file:web/app/api.js~export async function command"

[[diagram.node]]
id = "storage"
label = "local storage"
col = 3
row = 1
ref = "file:web/app/store.js~function saveSoon"

[[diagram.node]]
id = "state"
label = "store.js: state"
col = 3
row = 2
ref = "file:web/app/store.js~export const state"

[[diagram.node]]
id = "pending"
label = "pending commands"
col = 3
row = 3
ref = "file:web/app/store.js~function addPending"

[[diagram.node]]
id = "timeline"
label = "timeline.js: window"
col = 4
row = 1
ref = "file:web/app/timeline.js~export function append"

[[diagram.node]]
id = "screens"
label = "screens (Preact, htm)"
col = 4
row = 2
ref = "file:web/app/main.js~function App()"

[[diagram.edge]]
from = "hubread"
to = "api"
label = "snapshot, details"

[[diagram.edge]]
from = "api"
to = "state"
label = "loadSnapshot, loadDetail"

[[diagram.edge]]
from = "hubstream"
to = "stream"
label = "changes (SSE)"

[[diagram.edge]]
from = "stream"
to = "state"
label = "applyChange"

[[diagram.edge]]
from = "stream"
to = "api"
label = "reset: read again"

[[diagram.edge]]
from = "state"
to = "storage"
label = "saved; read at start"

[[diagram.edge]]
from = "state"
to = "timeline"
label = "open sessions' events"

[[diagram.edge]]
from = "state"
to = "screens"
label = "useStore"

[[diagram.edge]]
from = "screens"
to = "pending"
label = "run: send, answer…"

[[diagram.edge]]
from = "pending"
to = "command"
label = "send, ask, retry"

[[diagram.edge]]
from = "command"
to = "hubcmd"
label = "POST /api/commands"
+++
## What it is for

The owner follows recent sessions of every machine from a phone or a browser: the inbox, a session's timeline, questions and requests, prompts, the terminal and the changes. The app must stay right over a flaky mobile connection (no change lost or applied twice, no prompt typed twice because an answer was lost), stay small however long a session runs, and show text that an agent wrote without letting that text act in the page. The hub serves it and holds the data ([[concern:hub-commands-and-terminal]]); this page is the browser's side.

## How it works

**Served by the hub.** `web/` is embedded in the binary ([[pkg:web]]) and served by [[internal/hub.Server.serveFile]]: only the embedded files with a known extension, the app's own revalidated by ETag on every load, the vendored libraries (Preact, htm, xterm.js; version in the path) cached for a year. These routes need a device certificate but no login, since the login screen is part of the app; `/` may be opened from another site's link, its parts not ([[concern:hub-access]]). There is no build step: ES modules, and htm parses templates at run time. The service worker (`web/sw.js`) caches only the app's files, for opening offline, and never touches `/api/` or `/machine`.

**Content security policy.** Every response of the hub carries `default-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'` ([[internal/hub.contentSecurityPolicy]]): scripts, styles, images and connections from the hub's origin only, no inline script or style, no eval, never in a frame. xterm.js sets inline styles; `web/app/cspshim.js` turns its `<style>` elements into constructable stylesheets and its style attributes into CSSOM assignments instead of loosening the policy.

**Start** ([[flow:web-app-start]]). `main.js` shows what local storage kept, asks `/api/me` who it is, and starts the stream module, which reads a snapshot (inbox, machines, requests) and opens the event stream from the snapshot's position.

**Data flow.** [[diagram:data-flow]] One store ([[file:web/app/store.js~export const state|state]] in `store.js`) holds what the hub said; every screen re-renders through `useStore` after any change, batched once per microtask. Three things feed it: snapshot reads, stream messages ([[file:web/app/store.js~export function applyChange|applyChange]]) and a session's detail reads ([[file:web/app/store.js~export async function loadDetail|loadDetail]]). Each session summary remembers the stream position it is current at, and a stream message or detail read older than that does not replace it ([[finding:D1-12]]); a snapshot replaces them all without that check.

**The event stream** ([[file:web/app/stream.js~function open()|stream.js]]). One `EventSource` on `/api/stream`, naming up to 32 open sessions and the last change ID seen ([[internal/hub.Server.apiStream]]). Summaries, machines and requests arrive for everyone, events only for the named sessions. After an error the browser reconnects it by itself; when it closes for good, the app checks the login and reopens it with backoff from one second to a minute (when it gives up: [[flow:web-app-start]]). It reopens when the set of open sessions changes, and closes 30 seconds after the page is hidden.

**Resynchronising.** At three levels:

- The hub sends `reset` when its change log no longer reaches the position, or the position is beyond its newest (another hub's, a new database; another hub's within this range passes). The app closes the stream, reads the snapshot again (of overlapping reads the newest applies), reloads every open session and reopens from the snapshot's position ([[finding:C2-10]]).
- A session's event whose number is not the next one, or a `session.reset`, makes the app read that session's latest events again.
- While such a read runs, the stream's events for the session are held back (up to 1,000; beyond that it reads again) and merged by number afterwards ([[finding:E1-14]]).

**What is bounded.**

| What | Bound | Where |
|---|---|---|
| events held per open session | 1,000; the oldest go | `timeline.js` `MAX_EVENTS` |
| calls in one folded tool run | 200, then a new run | `TOOL_RUN_MAX` |
| rows drawn | 300, more on request | `session.js` `RENDER_STEP` |
| events a read fetches | latest 300; older pages of 200 | `loadDetail` |
| sessions one stream names | 32 | `stream.js` |
| terminal scrollback | none | `terminal.js` |

Paging back detaches the window from the live end: new events are counted ("N newer") until **Back to the latest** ([[finding:D1-13]], [[finding:E1-13]]). Machines, sessions and requests are as many as the hub keeps.

**Commands with an uncertain outcome** ([[flow:web-send]]). Each command gets a UUID in the browser. [[file:web/app/api.js~export async function command|command]] retries a failure whose outcome is uncertain (no answer, an error that says `uncertain`, an answer that cannot be read or is not a receipt) with the same ID, four attempts in all; once one attempt was uncertain, a later definite refusal counts as uncertain too ([[finding:E1-11]]). [[file:web/app/store.js~export async function run(|run]] then keeps the command as pending, also in local storage, shown as unknown. Every ten seconds, when its machine comes online and after a snapshot, the app asks for its receipt: a final one settles it; if the machine has none, it "did not arrive", and **Send again** (same ID) or **Dismiss** is offered. A prompt waiting in the outbox also settles when a prompt event arrives whose text (first 200 characters) the waiting text begins with. A daemon applies an ID once while it keeps the receipt, until seven days after it is final ([[concern:input-and-receipts]]); after that a 404 reads as "did not arrive" too.

**The terminal** ([[flow:hub-terminal]]). xterm.js is loaded when the tab opens, and a WebSocket to `/api/terminal/<machine>/<session>?mode=observe|control` carries the stream; the page sends `input` only in control mode. Control needs the hub's `[terminal] control` (the app learns it from `/api/me`), the same switch in the machine's config.toml, and a confirmation.

**Development mode.** `devc hub serve --dev-no-auth` ([[internal/hub.newDev]]) serves the app without certificates or login, only with `DEVC_DEV=1`, only on a loopback address, over HTTP, Host check kept, with a temporary database removed at stop. Three invented machines speak the daemon's side of the machine link over pipes to the hub's real link code, rather than filling the database ([[internal/hub.Server.runDevMachines]], [[chunk:C12]]). `web/check.sh` drives it in headless Chrome and fails on any console error or policy violation.

## What can go wrong, and what prevents it

- **Text from a box.** Messages, prompts, questions and options, tool names and summaries, request texts, errors, session fields, file names, diffs and terminal output originate in boxes. The hub decodes events strictly with bounded UTF-8 text ([[internal/hub.cleanEvent]]) and cleans projects and changes, making their texts printable ([[internal/hub.cleanProjects]], [[internal/hub.cleanChanges]]). The app passes them to htm and Preact as values, which become text nodes and attribute values and are never parsed as HTML; links are built from `#/` and names. The policy would block an injected script anyway.
- **Terminal output** goes to xterm.js as terminal data. In control mode xterm's replies to a program's queries go back as input, as in any terminal; in observe mode the app does not attach input.
- **Prompt events** come from the box's hooks; one whose text the waiting prompt's begins with (a short one will do, even one of spaces only) settles it, pending entry included ([[doc:docs/dev/LEFTOVER.md#7]]).
- **The hub serves the app's code**, so a compromised hub can make the page do whatever the login allows; this is why commands are not signed in the browser ([[concern:hub-commands-and-terminal]]).
- **Data on the device.** Local storage keeps machines, session summaries (with the last message and question), requests and pending commands; nothing in the app clears it.

## Decisions that are not obvious

- **No build step and vendored libraries** with recorded checksums (`web/vendor/README.md`); Preact's hooks module has its one import edited, since an import map would be an inline script.
- **A shim instead of `'unsafe-inline'`** for xterm.js, at the price of constructable stylesheets ([[doc:docs/dev/LEFTOVER.md#5]]).
- **The stream closes when hidden**, so a phone in a pocket does not hold back notifications ([[chunk:C12]]).

## Where to start reading

1. [[file:web/app/main.js~function begin()|main.js]] and [[file:web/app/stream.js~async function reconnect()|stream.js]]
2. [[file:web/app/store.js~export function applyChange|store.js]]
3. [[file:web/app/timeline.js~export function trim|timeline.js]]
4. [[file:web/app/api.js~export async function command|api.js]] and [[file:web/app/store.js~export async function resolvePending|resolvePending]]
5. [[internal/hub.Server.serveFile]] and [[internal/hub.newDev]]
