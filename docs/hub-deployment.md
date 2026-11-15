# Deploying a hub

A hub is one always-on `devc hub serve` process on a server that your
machines can reach on port 443. Machine daemons dial out to it over TLS; the
hub never connects to a machine, so machines need no open port and no VPN.
The hub keeps a view of every session on every machine, derives the inbox,
sends notifications to your phone through ntfy, and offers a JSON API (and,
later, the web app) to your devices.

A hub that can type into agent sessions is effectively a remote shell, so it
has two locks:

- **A client certificate** from the hub's own small CA on every connection.
  Every request is checked against the hub's registry of issued certificates,
  so a revoked certificate stops working at once, also for connections that
  are already open. The one exception is the enrolment path, which needs no
  certificate and exists only while an enrolment token is outstanding.
- **A login**: the API also needs a session created with the hub's password
  (and a TOTP code, when enabled). A session is bound to the certificate it
  was created with; the same cookie with another certificate is refused.

The hub keeps its state in its own directory, `$DEVC_HUB_DIR` (default
`~/.local/share/devc-hub`), never mixed with a machine's state. It holds
nothing that cannot be rebuilt from the machines, except its CA, its registry
of certificates and the password: back up `ca/`, `hub.db` and `hub.toml`.

## 1. Create the hub and set the password

On the server:

```sh
devc hub init --public-name hub.example.org
devc hub user set-password          # hidden prompt, twice; or one line on stdin
devc hub user totp enable           # optional: prints the secret and an otpauth:// URI once
devc hub doctor                     # permissions, password, public URL, ntfy
```

- `init` creates the CA (`ca/ca.crt`, `ca/ca.key`, ECDSA P-256, ten years)
  and the database (`hub.db`). With `--public-name` it also writes a server
  key (`server/server.key`), a certificate request for a public CA
  (`server/server.csr`) and a server certificate for that name signed by the
  devc CA (`server/server.crt`). `devc hub ca issue server <name>` replaces
  them later.
- The hub has one user. The password (at least 12 characters) is stored as an
  argon2id hash. With TOTP enabled, a login also needs the six-digit code of
  an authenticator app (scan the URI, or type the secret); a code works once.
  `devc hub user totp disable` turns it off. Passkeys are not implemented yet.
- The public name is the host (and port, when it is not 443) that machines and
  browsers use. The hub answers only requests whose `Host` header names it.

## 2. Enrol machines and devices

Enrolment hands out certificates without copying files around. On the
server:

```sh
devc hub enroll machine laptop      # prints: devc join https://hub.example.org --token <token>
devc hub enroll device phone        # prints a URL and the bundle's passphrase
```

Each token works once and for 15 minutes (`--ttl` shortens that). While at
least one token is outstanding, the hub serves `/enroll/<token>` to clients
without a certificate; otherwise that path does not exist (404, and in
direct TLS mode a connection without a certificate fails in the handshake).

- **A machine** runs the printed command. It makes its key itself, sends a
  certificate request, and stores the certificate the hub signs; the key never
  leaves the machine. Add `--ca ca.crt` (a copy of the hub's `ca/ca.crt`) when
  the hub serves the server certificate of `init` rather than one from a
  public CA. `--token -` reads the token from stdin instead of the command
  line.
- **A device** (a phone, a browser) opens the URL. It downloads a PKCS#12
  bundle (`phone.p12`) with its key, certificate and the CA, protected by the
  passphrase printed in the terminal that ran `enroll`, and nowhere else.
  Import it into the phone's or browser's certificate store. Older phones may
  need `--legacy-p12` (weaker encryption of the bundle file).

The manual route still works: `devc hub ca issue machine|device <name> --out
dir` writes the certificate, the key (and for devices the bundle) to files;
move them to the machine or device over a channel you trust and delete them
from the server. Machine and device certificates are valid for one year, name
the machine or device as common name and the role (`machine` or `device`) as
organisational unit; the hub takes both from its registry.

## 3a. Direct TLS

The hub terminates TLS itself:

```sh
devc hub serve --listen :443 --tls-cert /etc/letsencrypt/live/hub.example.org/fullchain.pem \
               --tls-key /etc/letsencrypt/live/hub.example.org/privkey.pem
```

A client certificate, when given, must be from the hub's CA and valid in the
registry, or the handshake fails. A connection without a certificate is
accepted only while an enrolment token is outstanding, and then one gate in
the hub refuses everything but `/enroll/<token>`.

Without `--tls-cert` and `--tls-key` the hub uses `server/server.crt` from
`init`. That certificate is signed by the devc CA: machines accept it when
they join with `--ca ca.crt`, browsers only after the devc CA was installed as
a trusted root. A certificate from a public CA (Let's Encrypt, for example)
avoids that; renewing it means restarting the hub. Listening on port 443 needs
the capability to bind low ports (`setcap cap_net_bind_service=+ep` on the
binary, or a systemd socket); a reverse proxy avoids both. In direct TLS mode
the hub sends `Strict-Transport-Security`.

## 3b. Behind a reverse proxy

The proxy terminates TLS, verifies the client certificate against the devc CA,
and passes it to the hub in the `X-Devc-Client-Cert` header. The hub listens on
plain HTTP on a loopback address only (it refuses to start otherwise) and
accepts the header only from loopback peers:

```sh
devc hub serve --behind-proxy --listen 127.0.0.1:8443 --public-name hub.example.org
```

The proxy must:

- verify a client certificate against `ca/ca.crt` when one is given, and
  without one let **only** `/enroll/<token>` through (43 characters of
  `A-Z a-z 0-9 _ -`); everything else without a certificate gets no answer
  from the hub (the hub's own gate refuses it too);
- **set** `X-Devc-Client-Cert`, replacing anything the client sent, to the
  verified certificate: URL-encoded PEM or base64 DER (the hub verifies it
  against the CA again); on the enrolment path it may remove it;
- **set** `X-Real-IP` to the client's address (the hub's rate limits count
  per address; without the header every client counts as one);
- pass the original `Host` header;
- pass WebSocket upgrades (machines connect to `/machine`);
- not buffer responses (`/api/stream` is a stream of server-sent events).

### Caddy

```caddy
hub.example.org {
	tls {
		client_auth {
			mode verify_if_given
			trust_pool file /etc/devc-hub/ca.crt
		}
	}
	request_body {
		max_size 1MB
	}
	# The enrolment path needs no client certificate.
	@enrol path_regexp ^/enroll/[A-Za-z0-9_-]{43}$
	handle @enrol {
		reverse_proxy 127.0.0.1:8443 {
			header_up -X-Devc-Client-Cert
			header_up X-Real-IP {remote_host}
		}
	}
	# Everything else needs a verified client certificate.
	handle {
		@nocert vars {tls_client_fingerprint} ""
		respond @nocert 404 {
			close
		}
		reverse_proxy 127.0.0.1:8443 {
			header_up X-Devc-Client-Cert {tls_client_certificate_der_base64}
			header_up X-Real-IP {remote_host}
			flush_interval -1
		}
	}
}
```

Caddy obtains the server certificate for `hub.example.org` itself, keeps the
original `Host` header and proxies WebSockets without further settings.
`header_up` replaces a header the client sent. Without a client certificate
Caddy leaves the certificate placeholder unreplaced, which is why the
enrolment path removes the header instead. With client authentication Caddy
requires the TLS server name to match the `Host` header, so the public name
must be a DNS name, not an IP address. Caddy versions before 2.8 spell the
trust pool `trusted_ca_cert_file /etc/devc-hub/ca.crt`.

### nginx

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    server_name hub.example.org;

    ssl_certificate     /etc/letsencrypt/live/hub.example.org/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/hub.example.org/privkey.pem;

    # A certificate, when given, must be from the devc CA (else 400).
    ssl_client_certificate /etc/devc-hub/ca.crt;
    ssl_verify_client optional;
    ssl_verify_depth 1;

    client_max_body_size 1m;

    # Without a certificate only the enrolment path reaches the hub.
    location ~ "^/enroll/[A-Za-z0-9_-]{43}$" {
        proxy_pass http://127.0.0.1:8443;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Devc-Client-Cert $ssl_client_escaped_cert;
        proxy_set_header X-Real-IP $remote_addr;
    }

    location / {
        if ($ssl_client_verify != SUCCESS) {
            return 404;
        }
        proxy_pass http://127.0.0.1:8443;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Devc-Client-Cert $ssl_client_escaped_cert;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_buffering off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }
}
```

`$ssl_client_escaped_cert` is the URL-encoded PEM of the verified
certificate; without one nginx does not send the header at all. nginx
inherits `proxy_set_header` lines only into blocks that have none of their
own, so every location lists all of them: a location that sets only
`Upgrade` would pass a client's own `X-Devc-Client-Cert` through.

Both configurations were checked (nginx 1.28.0, Caddy 2.10.0) with a hub
behind them: without a certificate the API gets 404, also with a forged
certificate header; a device without login gets 401 and with login 200; the
enrolment path without a certificate answers only while a token is
outstanding, and only for the exact path; a forged `X-Real-IP` is replaced.

## 4. Notifications through ntfy

The hub publishes to an [ntfy](https://ntfy.sh) server, ideally your own.
Configure it in `hub.toml` in the hub directory (mode 0600: it holds the
access token):

```toml
public_url = "https://hub.example.org"   # optional; default https://<public name>

[ntfy]
url = "https://ntfy.example.org"
topic = "devc-3k9x7q2m"                  # a hard-to-guess topic name
token = "tk_..."                         # access token, optional
```

The hub reads `hub.toml` at start. It sends one message when a session enters
**needs you**, **failed** or **finished**, but only for machines that named
this hub as their notifier (the first connected hub in their list), and not
while a logged-in client follows that session's events:

- title `<project> · <harness>`, the question (or the last message, cut to
  300 characters) as body, high priority for needs-you and failed;
- a click opens the session in the web app (`<public_url>/#/s/<machine>/<session>`);
- for a question with one or two options, one button per option, plus
  **Open**. An option button answers the question directly: it posts to
  `<public_url>/api/act/<token>` with a single-use token, valid for one hour
  and bound to that one answer. It needs the device certificate but no login.
- When the session leaves that state (answered elsewhere, marked seen), the
  message is cleared.

ntfy calls have a timeout and are retried a few times; a slow or unreachable
ntfy server never holds up the hub.

**On a phone without Google services** (GrapheneOS, LineageOS without
microG):

1. Install ntfy from F-Droid. Its F-Droid build has no Firebase; it keeps its
   own connection to your server in a foreground service.
2. On the ntfy server, deny anonymous access (`auth-default-access:
   deny-all`) and give a user read access to the topic and the hub a token
   with write access (`ntfy token add`, `ntfy access`).
3. In the app, add your server as the default server and subscribe to the
   topic with that user.
4. The option buttons call the hub, which requires the device certificate:
   in the app's settings, under client certificates, add the device's
   `.p12` bundle for `https://hub.example.org` (and for the ntfy server, if
   it requires one too). Without it the buttons fail and **Open** still
   works.

`devc hub doctor` checks that the ntfy server answers (`/v1/health`).

`hub.toml` also decides whether the web app may take control of a session's
terminal (off by default; the machine must allow it in its own `config.toml`
too):

```toml
[terminal]
control = true
```

## 5. Run the hub as a service

A systemd unit for a dedicated user `devc-hub` (adapt the paths):

```ini
[Unit]
Description=devc hub
After=network-online.target
Wants=network-online.target

[Service]
User=devc-hub
Environment=DEVC_HUB_DIR=/var/lib/devc-hub
ExecStart=/usr/local/bin/devc hub serve --behind-proxy --listen 127.0.0.1:8443 --public-name hub.example.org
Restart=on-failure
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/devc-hub
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

Run `devc hub init`, `devc hub user …`, `devc hub enroll …` and `devc hub ca …`
as the same user with the same `DEVC_HUB_DIR`.

## 6. Join machines

On each machine, with a token from `devc hub enroll machine <name>`, or with
the certificate and key from `devc hub ca issue machine <name>`:

```sh
devc join https://hub.example.org --token <token>                       # add --ca ca.crt for a devc server certificate
devc join https://hub.example.org --cert laptop.crt --key laptop.key   # the manual route
devc hub status            # each hub and whether the daemon is connected to it
devc leave https://hub.example.org
```

`join` stores the credentials under `durable/node/hubs/` in the state
directory (the key 0600) and adds a `[[hubs]]` entry to `config.toml`; `leave`
removes both. The machine daemon (`devc daemon` or `devc service install`)
keeps one WebSocket connection to every listed hub and reconnects with a
backoff of at most two minutes. A machine without a hub works fully; the hub
catches up when the machine reconnects, without gaps or duplicates.

A machine may list several hubs and connects to all of them. Each hub builds
its own view from the machine's journals. The first connected hub in the list
is the one asked to send notifications; when it goes away, the next one takes
over. Commands carry the client's ID and receipts live on the machine, so a
command sent through two hubs is applied once.

## 7. Managing access

```sh
devc hub devices ls                 # device certificates, last use, login sessions
devc hub devices revoke phone       # its connections and login sessions end at once
devc hub machines ls                # machine certificates, online, last seen
devc hub machines revoke laptop
devc hub sessions ls                # login sessions: device, created, last used, expiry
devc hub sessions revoke <id|device> | --all
devc hub ca ls                      # the whole registry
devc hub doctor
```

- A login session is created by `POST /api/login` and returned as a cookie
  (`HttpOnly`, `Secure`, `SameSite=Strict`, `Path=/`). It lasts 30 days
  without use. The hub stores only a hash of it.
- Five failed logins in a row with one device certificate lock that
  certificate's logins for 15 minutes; other devices can still log in. Every
  attempt is logged, never the password.
- Failed and unauthenticated requests are rate-limited per certificate and per
  address (429 with `Retry-After`).
- The access log has the time, the certificate's name, the address, the
  method, the path and the status; never bodies, cookies or tokens (the
  tokens in enrolment and button paths are left out).
- `devc hub doctor` checks that nothing in the hub directory is readable or
  writable by others (the CA key, the database and `hub.toml` not even by the
  group), that a password is set, that the public URL is `https`, and that
  ntfy answers. It exits with 1 when a check fails.

## 8. What a hub can and cannot do

Over its connection the hub is the caller and the machine daemon decides what
it may call: reading boxes, sessions and events, the session commands
(`send`, `answer`, `interrupt`, `keys`, `seen`, `start`, `stop`), receipts,
listing and deciding requests, and for the web app listing projects, reading a
session's changes from the relay and showing a session's terminal (typing into
it only when `[terminal] control = true` is set on the machine as well as in
`hub.toml`). The daemon refuses everything else (stopping
the daemon, host commands, policy changes) and decides low-risk requests only
(seal, savepoint, note); high-risk requests are decided with the CLI on the
machine. A compromised hub can therefore read the transcripts it mirrors and
type into online sessions, but cannot leave the boxes.

## 9. The API

JSON over HTTPS for device certificates (`machine` certificates may only use
`/machine`). Every endpoint but `POST /api/login` and `POST /api/act/<token>`
needs a login session bound to the request's certificate (401 with
`"login": true` otherwise). State-changing requests must carry
`X-Devc-Request: 1`, an `Origin` equal to the hub's public origin, and an
`application/json` body. Reads, including the stream, are refused when they
send an `Origin` or `Sec-Fetch-Site` from elsewhere. No CORS headers are ever
sent; every response carries a content security policy for the web app
(`default-src 'self'`, no inline script, never framed).

| Endpoint | Purpose |
|---|---|
| `POST /api/login` | `{"password", "totp"}`; sets the session cookie |
| `POST /api/logout` | ends the session |
| `GET /api/me` | the device, the session's expiry and `terminal_control` (hub.toml allows taking control of terminals) |
| `GET /api/machines` | machines with `online`, `last_seen`, `notifier` and their boxes |
| `GET /api/inbox` | sections `needs-you`, `failed`, `finished`, `working`, `idle`, each with session summaries, most recent change first |
| `GET /api/sessions?machine=&realm=&project=&live=&limit=` | session summaries, most recent change first |
| `GET /api/sessions/<machine>/<session>?before=&limit=` | a summary and recent events; `before=<seq>` pages backwards, asking the machine for events the hub does not hold |
| `POST /api/commands` | `{"machine", "type", "id", "params", "wait"}`; `type` is `send`, `answer`, `interrupt`, `keys`, `seen`, `start` or `stop`, `params` those of the daemon's method without the ID, `id` a UUID chosen by the client, `wait` up to 60 seconds for a final receipt. Returns the machine's receipt. |
| `GET /api/commands/<machine>/<id>?wait=` | the receipt |
| `GET /api/requests?machine=&realm=&project=` | pending requests |
| `POST /api/requests/<machine>/<id>/decide` | `{"approve", "reason"}`; the machine refuses high-risk requests |
| `GET /api/stream?session=<machine>/<session>` | server-sent events (below) |
| `GET /api/projects?machine=` | per machine (all, or the one named): `online` and its projects with box state, default branch, harnesses (`ready`: installed in the box), worktrees (branch, `dirty`, ahead/behind the banked branch and the upstream's default branch, live sessions), live and needs-you counts |
| `GET /api/changes/<machine>/<session>?scope=snapshot\|branch` | files changed with a diffstat and the unified diff (at most 384 KiB, `truncated`), computed on the machine's relay: `snapshot` the uncommitted work of the worktree's last snapshot against the commit it was made on (`stale` when the branch has moved since), `branch` the worktree's branch against the upstream's default branch from their merge base |
| `GET /api/terminal/<machine>/<session>?mode=observe\|control` | a WebSocket to the session's tmux window (below) |
| `POST /api/act/<token>` | a notification button: needs the device certificate and `X-Devc-Request: 1`, no session; performs the one answer the token is bound to and answers with a short text |
| `GET /enroll/<token>`, `POST /enroll/<token>` | enrolment (no certificate needed): a device's bundle, or a machine's certificate for `{"csr"}` |

A session summary is the session's view with `machine`, `stale` (its machine
is offline), `requests` (pending requests) and `section` (its inbox section,
empty when it is not in the inbox). A command for an offline machine fails at
once with status 503; the hub does not queue.

The stream sends `session`, `session.removed`, `machine`, `request` and
`request.removed` messages to everyone, and `event` and `session.reset` messages
for the sessions named in the query (up to 32). Every message has an `id`; a
client that reconnects with `Last-Event-ID` (browsers do this by themselves)
gets exactly the messages after it. A stream without an ID starts with a
`ready` message at the present; one whose ID the hub no longer has gets
`reset` and should read its lists again. Snapshots (`/api/inbox`,
`/api/sessions`, …) carry `stream_id`, the ID to resume the stream from.
While a stream names a session, the hub sends no notifications for it.

The terminal WebSocket sends the pane's output as binary messages and JSON
text messages `{"type":"open","cols","rows","mode"}`, `{"type":"size",…}`
when the window changes size, `{"type":"ping"}` every 25 seconds and
`{"type":"end","reason"}` before it closes. The browser sends
`{"type":"input","data"}` (control mode only; in observe mode it ends the
stream) and `{"type":"visible","visible"}`. The machine runs the box's tmux
client with the flags `read-only` and `ignore-size` (in control mode only
`ignore-size`): a terminal never resizes the pane. `mode=control` is refused
with 403 unless `hub.toml` has `[terminal] control = true`, and ends at once
unless the machine's `config.toml` has it too. A stream ends with the session,
when either side goes, after 30 minutes without the page visible, and when the
login session ends.

The web app's own files (`/`, `/app/`, `/vendor/`, `/icons/`, `/sw.js`,
`/manifest.webmanifest`) need the device certificate but no login; the page
may be opened from a link on another site, its parts only from the hub's own
pages.

With curl (the cookie jar keeps the session):

```sh
C="--cacert ca.crt --cert phone.crt --key phone.key -c jar -b jar"
curl $C -H 'Content-Type: application/json' -H 'X-Devc-Request: 1' -H 'Origin: https://hub.example.org' \
     -d '{"password":"…"}' https://hub.example.org/api/login
curl $C https://hub.example.org/api/inbox
```
