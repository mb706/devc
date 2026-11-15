# C08 — Daemon core

Read `docs/dev/README.md`, `docs/dev/plan.md` (sections 3.3, 3.6, 3.11) and the `STATUS.md` notes of the earlier chunks first. Product spec: `specs-v2.md` sections 12 and 13.

Phase 1 works without any long-running process. This chunk adds the machine daemon: one user service per machine that keeps a live channel to every running box, ingests events continuously, and serves a local API with event streams. Later chunks hang the relay service, the SSH gate, requests and the hub connection on it. Everything from phase 1 must keep working when the daemon is not running.

## 1. Protocol (`internal/proto`)

- Move the event types from C04 here and add the types below. One JSON object per line; a reader refuses lines above 1 MiB.
- JSON-RPC 2.0: requests, responses, notifications. A small codec with a server side (method table, per-connection context) and a client side (calls with timeouts, notification handlers). The same codec is used on the daemon's Unix socket, on box channel streams, and later inside WebSocket frames.
- Types: `SessionView` (the derived fields of C04 plus machine, realm, project and box state), `BoxView`, `Cursor` (session ID to last seen `seq`), `Command` and `Receipt` (defined here, used from C09), and error codes.

## 2. The daemon

`devc daemon` (foreground; `--log-level`). One instance per machine, enforced with a lock file in the runtime directory. It listens on `<runtime>/daemon.sock` (directory 0700, socket 0600).

Jobs in this chunk:

1. **Box watch.** Every few seconds, list containers with `devc.role=box`. For each running box keep a channel (section 3) and a spool tailer. Drop both when the box stops.
2. **Continuous ingestion.** Use C04's ingestion, driven by the daemon instead of on demand: poll each spool directory (250 ms while events are arriving, 2 s when idle), and immediately when the box's helper sends a hint. Ingestion from the CLI and from the daemon must exclude each other: take a per-box ingestion lock in both.
3. **Reconciliation** of lost sessions (C04) every 30 s.
4. **Housekeeping** every five minutes: `bank` for every running box and `context commit` for every realm, as the timer from C05 did. `devc service install` now installs a `devc.service` unit for the daemon and removes the timer; keep the timer available as `--timer-only` for machines that should not run the daemon.
5. **Serving the API** (section 4).

The daemon holds no state that cannot be rebuilt from the journals, the engine and the configuration. It reloads configuration when the files change (check modification times before each box-watch pass).

## 3. Box channel

The direction is fixed by SELinux and by trust: **the box listens, the host connects.**

**In the box.** `devc box init` gains an agent loop (in the same process, started after the ready file is written; a failure in it is logged to `/run/devc/agent.log` and never takes the box down):

- It listens on `/run/devc/agent.sock`, which the host can reach through the runtime directory, and accepts one host connection at a time; a new connection replaces the old.
- It listens on the abstract socket `@devc-agent` for clients inside the box (hooks, launcher, and later the git remote helper and the SSH shim). Abstract sockets are scoped to the network namespace, so the host and other boxes cannot reach it.
- It runs yamux over the host connection. Each request from an in-box client is forwarded on a new stream; each stream opened by the host is served locally.
- When no host is connected, in-box clients get an immediate "host unavailable" error, and everything that has a fallback keeps using it (hooks still write the spool).

**On the host.** The daemon connects to each running box:

- The socket file is box-controlled. Open the path beneath the runtime directory without following symlinks (`O_PATH | O_NOFOLLOW`), check with `fstat` that it is a socket, and connect through `/proc/self/fd/<n>` so the daemon reaches exactly that inode.
- After connecting, read the peer's credentials (`SO_PEERCRED`) and verify that the peer process belongs to that box's container: its cgroup path must contain the container ID. Otherwise close the connection and retry later.
- Everything received on the channel is hostile input: bounded sizes, strict decoding, no value used in a path or command without validation.

**Methods in this chunk.**

| Direction | Method | Purpose |
|---|---|---|
| host → box | `box.hello` | versions and box identity; the daemon refuses a helper whose protocol version it does not know |
| host → box | `box.sessions` | the live-session listing C04 obtains with `box session ps` |
| box → host | `host.hint` (notification) | "the spool has new data" |
| box → host | `host.ping` | returns the daemon version; used by tests and by `devc box agent-status` |

Hooks send the hint after writing the spool. The spool file stays the source of truth: a lost hint only costs latency.

## 4. Local API

JSON-RPC on `daemon.sock`. Any process of the same user may connect; the socket permissions are the access control.

| Method | Result |
|---|---|
| `daemon.info` | version, start time, an instance ID that changes on every start, boxes with channel state |
| `boxes.list` | `BoxView` list |
| `sessions.list` | `SessionView` list; filters for realm, project, live only |
| `sessions.get` | one session with its recent events |
| `events.read` | events of one session from a `seq`, bounded |
| `events.subscribe` | starts a stream on this connection (below) |

**Subscription.** The client sends its cursor: a map from session ID to the last `seq` it has, and optional filters. The daemon first sends, per session, the events after the cursor (all events for sessions the client does not know, limited to sessions active within a configurable window, default 30 days), then live events as they are ingested. Every event notification carries the session ID and `seq`, so a client that reconnects with its updated cursor gets no gaps and no duplicates. A `heartbeat` notification goes out every 15 s. If the daemon cannot serve from a cursor, it sends `missed` for that session and the client re-reads it.

## 5. CLI

- `devc ps` and `devc ls` ask the daemon when it is running (try the socket; fall back to the phase 1 path silently when it is not). `devc ps --watch` uses the subscription and redraws.
- `devc events [session] [--follow] [--json]` prints events; useful for debugging and for tests.
- `devc daemon status` and `devc daemon stop`.
- `devc box agent-status` (in the box) says whether a host is connected.

## 6. Tests

Unit: the codec (framing, size limit, unknown methods, malformed input); subscription replay from cursors, including a session the client has never seen and a cursor beyond the journal's end; the socket-opening helper refusing a symlink, a regular file and a directory.

Integration (start the daemon with temporary directories; fake harness):

1. With a box up, `daemon.info` shows the channel connected, and `devc box agent-status` in the box agrees.
2. A fake-harness event reaches a subscriber in under a second.
3. Stop and restart the daemon while events are produced: a client resubscribing with its cursor ends with every event exactly once.
4. `devc ps` gives the same sessions with and without the daemon, and running it while the daemon ingests neither duplicates nor loses events.
5. Hostile channel endpoints, each handled without a crash and without the daemon talking to the wrong peer:
   - `agent.sock` replaced by a symlink to another Unix socket on the host that a test process listens on;
   - replaced by a regular file, by a directory;
   - a listener at that path run by a host process outside the box (the credential check must reject it);
   - a helper that sends oversized or malformed frames.
6. The box is stopped and started again: the channel reconnects.
7. With the daemon stopped, hooks still record events to the spool and `devc ps` shows them.
8. Housekeeping runs `bank` and `context commit` (shorten the interval through a test-only setting).

## 7. Done when

- The tests pass through `./dev test` and `./dev itest`, and the phase 1 suites still pass with and without a daemon running.
- `STATUS.md` and `LEFTOVER.md` are updated. `LEFTOVER.md` must say that the box-listens/host-connects direction has not been tried under SELinux.
