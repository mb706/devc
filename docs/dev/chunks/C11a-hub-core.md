# C11a — Hub core

Read `docs/dev/README.md`, `docs/dev/plan.md` (section 3.11) and the `STATUS.md` notes of C08, C09 and C10a. Product spec: `specs-v2.md` section 13. Background: `../v2-research/10-unified-interface.md` section 2 (how other tools structure events, commands and reconnection; that section only).

The hub is one always-on process on a server. Machines dial out to it; it keeps a view of every session on every machine and offers an API for a user interface. This chunk builds the hub's core: certificates, the link between machine daemons and the hub, projections, the inbox, and the HTTP API. User login, device handling and notifications are C11b; the web app is C12.

The hub is the same `devc` binary. Its state lives in `DEVC_HUB_DIR` (default `~/.local/share/devc-hub`), never mixed with a machine's state. The hub holds nothing that cannot be rebuilt from the machines, except its own registry of certificates and users.

## 1. Certificates

A small private CA is the front door: nothing speaks HTTP to the hub without a certificate from it.

| Command | Does |
|---|---|
| `devc hub init` | creates the hub directory, the CA (ECDSA P-256, 10 years), a server certificate request helper, and the SQLite database |
| `devc hub ca issue machine <name> [--out dir]` | key and certificate (1 year) with the name and the role `machine` in the subject |
| `devc hub ca issue device <name> [--out dir]` | the same with role `device`, plus a PKCS#12 bundle with a generated passphrase, printed once, for import into a phone or browser |
| `devc hub ca ls`, `devc hub ca revoke <name>` | registry of issued certificates by SHA-256 fingerprint; revocation takes effect immediately |

A PKCS#12 encoder is a new dependency (`software.sslmate.com/src/go-pkcs12`); note it in `STATUS.md`.

## 2. Serving

`devc hub serve`, two deployment modes:

- **Direct TLS:** `--listen :443 --tls-cert <file> --tls-key <file>`. The TLS configuration requires and verifies a client certificate against the devc CA for every connection.
- **Behind a reverse proxy:** `--listen 127.0.0.1:<port> --behind-proxy`. The proxy terminates TLS, verifies the client certificate against the devc CA, and passes it on in a header (`X-Devc-Client-Cert`, URL-encoded PEM). The hub accepts that header only from loopback peers and refuses to start in this mode on a non-loopback address.

In both modes the hub then checks the certificate's fingerprint against its registry (known, not revoked) and takes the role and name from the registry, not from the request. Write example proxy configurations for Caddy and nginx into `docs/hub-deployment.md`.

Every request has a size limit; no wildcard CORS; `Host` must match the configured public name.

## 3. Machine link

**Machine side** (extends the daemon of C08):

- `devc join <hub-url> --cert <file> --key <file> [--ca <file>]` stores the hub's credentials under `durable/node/hubs/` and adds the URL to the list of hubs. `devc hub status` (on a machine) shows each hub and whether it is connected. `devc leave <hub-url>`.
- The daemon keeps one WebSocket connection per listed hub (`wss://<hub>/machine`), with reconnection and backoff capped at two minutes. The same JSON-RPC codec as everywhere, one object per text frame.
- Over that connection **the hub is the caller**: it may invoke a fixed allowlist of the daemon's methods, and nothing else: `daemon.info`, `boxes.list`, `sessions.list`, `sessions.get`, `events.read`, `events.subscribe`, the session commands of C09, `command.get`, `requests.list`, `requests.decide`. Calls arriving this way carry origin `hub`, so the daemon applies the rules for that origin: high-risk request decisions are refused (C10a), and no method that touches the host or changes policy is reachable.
- The machine calls `hub.hello` (name, version, instance ID) and `hub.role {notifier}`. A machine connected to several hubs marks the first connected one in its list as notifier and updates the others when that changes.

**Hub side.**

- On connect, subscribe to the machine's event stream with the cursors the hub has stored for that machine (C08's subscription), and refresh the box and session lists.
- Store in SQLite: machines (name, fingerprint, last seen, online, notifier flag), the latest `SessionView` per machine and session, a bounded tail of events per session (the most recent 500 or 1 MiB), cursors, and requests.
- When a machine disconnects, keep its rows and mark them stale; show `last_seen`.
- Receipts stay on the machine. The hub forwards a command with the client's ID and returns what the machine returns; a retry with the same ID is therefore safe.

## 4. Inbox

Derived, never stored as truth. Each live session falls into exactly one section, in this order of precedence:

1. `needs-you`: state `blocked`, or a pending request from that session;
2. `failed`: state `error`, or ended as `lost`;
3. `finished`: `done_turn` is greater than `seen_turn`;
4. `working`;
5. `idle`.

Within a section, most recent change first. Ended sessions leave the inbox once seen.

## 5. HTTP API

JSON over HTTPS. In this chunk every request with a valid `device` certificate is accepted; C11b adds login on top. `machine` certificates are accepted only on `/machine`.

| Endpoint | Purpose |
|---|---|
| `GET /api/machines` | machines with online state |
| `GET /api/inbox` | sections with session summaries |
| `GET /api/sessions?machine=&realm=&project=&live=` | filtered list |
| `GET /api/sessions/<machine>/<session>` | view plus recent events; `?before=<seq>` pages backwards (fetched from the machine when the hub's tail does not reach) |
| `POST /api/commands` | body: machine, command type and parameters, client-chosen ID; forwards to the machine and returns the receipt. Types: `send`, `answer`, `interrupt`, `keys`, `seen`, `start`, `stop` |
| `GET /api/commands/<machine>/<id>` | the receipt |
| `GET /api/requests`, `POST /api/requests/<machine>/<id>/decide` | list; decide (the machine refuses high-risk ones) |
| `GET /api/stream` | server-sent events: inbox changes and, for sessions named in the query, their events. Each message has an ID so a browser's automatic reconnect resumes without gaps |

A command for an offline machine is refused at once with a clear error; the hub does not queue.

## 6. Tests

Unit: certificate issue and registry; the proxy-header path (accepted from loopback only); inbox derivation table; the origin allowlist on the daemon; event-tail bounds.

Integration, all on loopback with a test CA: one hub process, one machine daemon joined to it, a box with the fake harness.

1. Fake-harness events appear in `GET /api/sessions/...` and move the session through the inbox sections (`working`, `finished`, after `seen` gone from `finished`; a question puts it in `needs-you`).
2. `POST /api/commands` with `send` reaches the fake harness; the receipt comes back; repeating the ID does not type again.
3. Restart the hub: after reconnection the event tail has no gaps and no duplicates. Restart the daemon: the same.
4. `GET /api/stream` delivers a new event within a second; reconnecting with the last ID resumes exactly.
5. Two hubs: both receive events; exactly one is marked notifier; stopping it moves the role to the other.
6. A connection without a client certificate fails; one with a revoked certificate fails; a `machine` certificate cannot call `/api/*`; a `device` certificate cannot connect to `/machine`.
7. From the hub, a method outside the allowlist is refused by the daemon; a high-risk `requests.decide` is refused; a low-risk one works.
8. Machine offline: its sessions stay listed as stale; a command is refused.
9. Behind-proxy mode: the header is honoured from loopback and ignored (connection refused) otherwise.

## 7. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `docs/hub-deployment.md` describes both deployment modes with working example proxy configurations.
- `STATUS.md` and `LEFTOVER.md` are updated.
