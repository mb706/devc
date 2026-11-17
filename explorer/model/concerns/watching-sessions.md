+++
id = "watching-sessions"
title = "Watching sessions"
summary = "How the user sees what sessions do: `devc ps` with and without the daemon, the live list of `ps --watch`, `devc events`, what \"needs you\" means and the seen marks, `devc attach`, and the terminal, projects and changes the daemon serves to the web app."
order = 34
files = [
  "internal/cli/sessioncmds.go",
  "internal/cli/daemoncmds.go",
  "internal/daemon/terminal.go",
  "internal/daemon/web.go",
  "internal/proto/web.go",
]
key = [
  "internal/cli.app.collectAny",
  "internal/cli.app.sessionsFromDaemon",
  "internal/cli.app.psWatch",
  "internal/cli.newEventsCmd",
  "internal/proto.SessionView.NeedsYou",
  "internal/daemon.Daemon.apiSeen",
  "internal/cli.app.attach",
  "internal/tmux.Attach",
  "internal/daemon.termManager.open",
  "internal/tmux.Observe",
  "internal/daemon.Daemon.apiProjects",
]
spec = ["doc:specs-v2.md#12.2", "doc:specs-v2.md#15"]
tests = [
  "internal/cli.TestCommandsThroughDaemon",
  "internal/cli.TestEventLine",
  "internal/cli.TestWriteJSONEscapesControls",
  "internal/proto.TestTurnsAndSeen",
  "internal/daemon.TestDaemon",
  "internal/daemon.TestWebMethods",
  "test/integration.TestSessions",
  "test/integration.TestDaemon",
  "test/integration.TestTmux",
  "test/integration.TestWebApp",
]
related = ["sessions-and-events", "daemon", "input-and-receipts", "launching-a-harness"]

[[invariant]]
text = "With and without a daemon, `devc ps` ingests and reconciles first and derives the views from the journals; only a last journal event without its newline is read by the CLI and not yet by the daemon."
refs = ["internal/cli.app.collectAny", "internal/daemon.Daemon.apiSessions", "internal/cli.app.collect"]
tests = ["test/integration.TestDaemon"]

[[invariant]]
text = "Box-written text reaches the user's terminal only made printable, and JSON output escapes control characters."
refs = ["internal/cli.oneLine", "internal/proto.Printable", "internal/cli.writeJSONLine"]
tests = ["internal/cli.TestWriteJSONEscapesControls", "internal/cli.TestEventLine"]

[[invariant]]
text = "A terminal stream observes unless it was opened in control mode on a machine whose configuration allows control, and only its own connection can act on it."
refs = ["internal/daemon.termManager.open", "internal/daemon.termManager.input", "internal/daemon.termManager.get"]
tests = ["test/integration.TestWebApp", "internal/daemon.TestWebMethods"]

[[invariant]]
text = "A seen mark only ever moves forward; after a session's end it is the one field of its view that changes, besides the event count, last seq and last-event time."
refs = ["internal/proto.SessionView.Apply", "internal/daemon.Daemon.apiSeen"]
tests = ["internal/proto.TestTurnsAndSeen"]

[[invariant]]
text = "Nothing in this concern writes into a session except a control-mode terminal; everything else reads."
refs = ["internal/daemon.termManager.input"]
+++
## What it is for

The user wants to see what sessions do: a list with what each waits for, the events of one, a list that stays current, and the harness's own screen, at the terminal or in the web app. All of it reads, except a terminal the user opened to type in. What a box wrote is shown only after it is made safe for a terminal.

## How it works

**`devc ps`.** When a daemon serves the same directories, [[internal/cli.app.collectAny]] asks [[rpc:sessions.list]] with `sync`: the daemon ingests and reconciles each matching project first (two minutes in all) and answers from its views in pages of at most 1,000 sessions or 512 KiB, with the warnings it collected (invalid spool lines, refused spool files). Without a daemon, or with `DEVC_NO_DAEMON=1`, the CLI does the same itself ([[internal/cli.app.collect]]): it refreshes each box and derives the views from the journals, skipping old ended journals by modification time and last line. It lists live sessions and those that ended in the last 24 hours (`--all`: every one) with project, worktree, harness, state, age and the last message, or `? <question>` when blocked. An ID is shown by its shortest prefix of at least twelve characters that tells it from the other listed sessions, so it always includes random characters ([[finding:B12]]). `devc ls` counts live sessions and those that need the user the same way.

**`devc ps --watch`.** [[internal/cli.app.psWatch]] needs the daemon. It takes the list once, then subscribes with a [[term:cursor]] holding every listed session's last seq, folds the events it receives into its own views ([[internal/proto.SessionView.Apply]]), reads a session again with [[rpc:sessions.get]] when told it was `missed`, and redraws 100 ms after a change or a heartbeat. When the daemon goes away it tries again every two seconds and resubscribes with its cursor.

**`devc events`.** One session by key, ID or unique prefix, live or ended ([[internal/cli.newEventsCmd]]). The history comes from the daemon with [[rpc:events.read]] or, without one, from the journal. `--follow` needs the daemon: it subscribes from the last seq printed and ends with the session's `session.end`; without a session it follows every session from now. Each event is one line, or with `--json` one object with control characters escaped ([[flow:event-ingestion]]).

**Needs you, and seen.** [[internal/proto.SessionView.NeedsYou]]: a live session that is blocked, or done with a turn nobody marked seen. `devc seen <session> [--turn n]` sends [[rpc:session.seen]]; the daemon checks the turn against the view and appends a host-only `seen` event ([[internal/daemon.Daemon.apiSeen]]). Since every view follows the journals, the mark reaches the daemon's subscribers, and with them the hubs.

**`devc attach`.** [[internal/cli.app.attach]] needs a terminal and no daemon. It refreshes the box, which also asks it for its live sessions, picks the session's window (asking when several are live, the most recent by default), and runs a tmux client in the box with `podman exec -it`. The client attaches through a grouped session of its own that tmux destroys when it detaches ([[internal/tmux.Attach]]), so two clients can watch different windows. [[flow:attach]] goes through it.

**The web app's terminal.** [[rpc:terminal.open]] (on the local socket, or from a hub for its [[concern:web-app|web app]]) needs a live session in a running box with a tmux window, found by asking the agent or, without a channel, from the last reconciliation. [[internal/daemon.termManager.open]] allocates a pseudo-terminal of the daemon's and runs the box's tmux client on it through `podman exec -t`, attached through its own grouped session with `ignore-size`, so it never resizes the window ([[internal/tmux.Observe]]). Output goes out as `terminal.data` notifications on the opening connection; the window's size is read again every five seconds. In observe mode the daemon, the only writer of that pseudo-terminal, refuses `terminal.input`; control mode needs `[terminal] control = true` in `config.toml`. A stream ends with its session, its client, its connection, after twelve hours, with `terminal.close`, or when its opener has not confirmed it (`terminal.confirm`) within 30 seconds ([[internal/daemon.termStream.watch]]), unless the opener is a hub whose hello did not announce that it confirms; then its grouped session is killed.

**Projects and changes.** [[rpc:projects.list]] describes each configured project for the web app: box state, harnesses with a launch description, live and needs-you counts, and worktrees listed in the box (`devc box worktree list`). A list read successfully is kept for 20 seconds; after a failure the next request asks again. One call asks at most four boxes at once; concurrent calls each have their own four, and never ask the same box at the same time ([[internal/daemon.worktreeCache]]). A stopped box shows the last list with its time. [[rpc:changes.get]] computes a session's changes on the relay with host git, never in the project directory ([[internal/daemon.Daemon.apiChanges]]).

## What can go wrong, and what prevents it

- **Box-written text on a terminal.** Messages, questions, reasons and box error output go through [[internal/proto.Printable]] or [[internal/proto.PrintableText]]; JSON output escapes DEL and C1 controls, which `encoding/json` leaves raw.
- **A box that lies about its live sessions.** [[internal/session.ParseLive]] rejects the whole report when an entry or field is not as expected, or when anything follows the report ([[internal/proto.StrictJSON]]).
- **An attached terminal.** The tmux client runs in the box and draws what the box sends; attaching puts the terminal under the box's influence.
- **Terminal input in the wrong place.** A stream is bound to the connection that opened it, reaches one session's window through its own client, and takes input only in control mode.
- **Too many terminals.** Four per connection and sixteen for the daemon, counted before the window's size is read and not again before the stream is added: opens that run at the same time (on several connections, or on a hub link, which serves eight requests at once) can all pass the count (`docs/dev/LEFTOVER.md` part 7).
- **A slow subscriber.** Disconnected at its budgets; `--watch` resubscribes ([[concern:daemon]]).

## Decisions that are not obvious

- **`ps` asks the daemon to sync.** The daemon's answer is as fresh as the CLI's own, so both paths give the same sessions, but for a last journal event cut off before its newline ([[chunk:C08]], [[concern:sessions-and-events]]).
- **Observing is enforced by the daemon, not by tmux.** tmux refuses `send-keys` while a read-only client is the most recently active one, which would stop the daemon's own typing ([[internal/tmux.Observe]]).
- **Attach without the daemon.** It works in a phase-1 setup and over plain ssh.
- **Times are the host's.** Age and the 24-hour window use the ingestion times, which a box cannot choose.

## Where to start reading

1. [[internal/cli.app.collectAny]] and [[internal/daemon.Daemon.apiSessions]]
2. [[internal/cli.app.psWatch]]
3. [[internal/cli.newEventsCmd]]
4. [[internal/cli.app.attach]]
5. [[internal/daemon.termManager.open]]
