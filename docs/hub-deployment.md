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
`~/.local/share/devc-hub`), never mixed with a machine's state. Its view of
the sessions can be rebuilt from the machines; its CA, its registry of
certificates, the password, and the archive of the machines' durable state
(`archive/`, section 10) and their encrypted secrets (`secrets/`) cannot:
back up `ca/`, `hub.db`, `hub.toml`, `archive/` and `secrets/`, or run a
second hub (section 11).

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
- not buffer responses (`/api/stream` is a stream of server-sent events);
- **not log the enrolment path**: `/enroll/<token>` carries a token that is
  the credential until it is used or expires (up to 15 minutes), and a token
  in an access log, an error log or a log of upstream requests could be used
  by whoever reads that log. The configurations below switch logging off for
  that path. (The hub's own access log leaves the token out, and the token of
  a notification button travels in a request header, which proxies do not
  log by default; do not add request headers to a log format.)

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
	# The enrolment path needs no client certificate, and its token must
	# not reach a log.
	@enrol path_regexp ^/enroll/[A-Za-z0-9_-]{43}$
	handle @enrol {
		log_skip
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
must be a DNS name, not an IP address. Caddy writes no access log unless a
`log` directive asks for one; `log_skip` keeps enrolment requests out of it
when one is configured. Caddy versions before 2.8 spell the trust pool
`trusted_ca_cert_file /etc/devc-hub/ca.crt` and `log_skip` as `skip_log`.

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

    # Without a certificate only the enrolment path reaches the hub. Its
    # token must not reach a log: no access log, and the error log (which
    # quotes the request line when the hub cannot be reached) goes nowhere.
    location ~ "^/enroll/[A-Za-z0-9_-]{43}$" {
        access_log off;
        error_log /dev/null;
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
certificate; without one nginx does not send the header at all. An error
for the enrolment path is lost with its error log; `devc hub serve` logs
every enrolment it refuses or completes (without the token). nginx
inherits `proxy_set_header` lines only into blocks that have none of their
own, so every location lists all of them: a location that sets only
`Upgrade` would pass a client's own `X-Devc-Client-Cert` through.

Both configurations were checked (nginx 1.28.0, Caddy 2.10.0) with a hub
behind them: without a certificate the API gets 404, also with a forged
certificate header; a device without login gets 401 and with login 200; the
enrolment path without a certificate answers only while a token is
outstanding, and only for the exact path; a forged `X-Real-IP` is replaced.
With the logging rules, an enrolment request that nginx could not pass on
(502) left no line in its access or error log, while other requests were
logged; Caddy accepted its configuration (`caddy validate`).

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
  `<public_url>/api/act` with a single-use token, valid for one hour and
  bound to that one answer, in the `X-Devc-Action` header (never in the URL,
  which proxies log). It needs the device certificate but no login.
- When the session leaves that state (answered elsewhere, marked seen), the
  message is cleared.

ntfy calls have a timeout and are retried a few times; a slow or unreachable
ntfy server never holds up the hub. The hub keeps a durable record per
notifying state: *pending* until it is published, *sent*, then *cleared*.
A publish or a clear that fails is retried with a growing pause (up to 15
minutes), also after the hub restarts; a state that ends before its message
went out is dropped. A hub that is not (yet) a machine's notifier records
the states as pending, and when the machine names it notifier (because the
first hub went away) it sends those that are still current. So a question
that reaches the second hub during a hand-over is not lost; at worst both
hubs notify it.

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
  certificate's logins for 15 minutes; other devices can still log in. A
  certificate has one login attempt at a time (a second one gets 429), and
  at most two logins hash a password at once with eight more waiting (429
  beyond that), so parallel attempts cannot get past the lock. Every attempt
  is logged, never the password.
- Failed and unauthenticated requests are rate-limited per certificate and per
  address (429 with `Retry-After`). At most 64 requests without a login
  session (logins, buttons, the app's files, enrolment) are served at once,
  and a request body must arrive within 10 seconds.
- Open connections (machine links, event streams, terminals) end when their
  certificate is revoked and when it expires.
- The access log has the time, the certificate's name, the address, the
  method, the path and the status; never bodies, cookies or tokens (the
  token in enrolment paths is left out; button tokens are in a header).
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

(The machine endpoints of the archive are in section 10.)

JSON over HTTPS for device certificates (`machine` certificates may only use
`/machine`). Every endpoint but `POST /api/login` and `POST /api/act`
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
| `POST /api/act` | a notification button: needs the device certificate, `X-Devc-Request: 1` and the button's token in `X-Devc-Action`, no session; performs the one answer the token is bound to and answers with a short text |
| `GET /enroll/<token>`, `POST /enroll/<token>` | enrolment (no certificate needed): a device's bundle, or a machine's certificate for `{"csr"}` |

A session summary is the session's view with `machine`, `stale` (its machine
was offline when the summary was made; the machine list is current),
`requests` (pending requests) and `section` (its inbox section, empty when
it is not in the inbox). A command for an offline machine fails at once with
status 503; the hub does not queue. When the machine may have received a
command but no answer came (it went offline during the call, or did not
answer in time), the error body says `"uncertain": true`: keep the command's
ID, ask for its receipt (`GET /api/commands/<machine>/<id>`; 404 means it
never arrived) or send it again with the same ID, which the machine applies
once. A decision the machine cannot take now (its request executor is full)
gets 429 and the request stays pending. The hub names the session's project
(`ref`) on every call to the machine about a session; a client may give
`ref` in a command's `params`, else the hub fills it in.

`?before=<seq>` pages always end right below `before` and join the previous
page without a gap, also when the machine returns its events in several
reads.

The stream sends `session`, `session.removed`, `machine`, `request` and
`request.removed` messages to everyone, and `event` and `session.reset` messages
for the sessions named in the query (up to 32). Every message has an `id`; a
client that reconnects with `Last-Event-ID` (browsers do this by themselves)
gets exactly the messages after it. A stream without an ID starts with a
`ready` message at the present; one whose ID the hub no longer has gets
`reset` and should read its lists again, also when the hub's change log
moves past a stream that is open but slow to read. A client should then stop
the stream, read the snapshot and resume from the snapshot's `stream_id`. Snapshots (`/api/inbox`,
`/api/sessions`, …) carry `stream_id`, the ID to resume the stream from.
While a stream names a session, the hub sends no notifications for it.

The terminal WebSocket sends the pane's output as binary messages and JSON
text messages `{"type":"open","cols","rows","mode"}`, `{"type":"size",…}`
when the window changes size, `{"type":"ping"}` every 25 seconds and
`{"type":"end","reason"}` before it closes. The browser sends
`{"type":"input","data"}` (control mode only; in observe mode it ends the
stream) and `{"type":"visible","visible"}`, which it should send as soon as
the socket opens: until it says visible, the hub counts the page as hidden.
The machine runs the box's tmux
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

## 10. The archive of durable state

Every machine uploads its realms' durable state to every hub it joined
(realms with `[sync] hub = false` excepted): journals and logs as they grow,
relays and context repositories as git bundles with a log of their refs, and
the shared harness state without credential files. Secrets never; they come
only as encrypted bundles (below). Machines use plain HTTPS requests with
their certificate for this, beside the machine link:

| Request | |
|---|---|
| `PUT /machine/archive/<realm>/<path>?offset=<n>[&final=1][&class=…][&mode=…]` | append to the machine's own archive |
| `GET /machine/archive-index` | every machine's archives (machine, realm, files, bytes) |
| `GET /machine/archive-index/<machine>/<realm>[?after=<path>]` | one archive's files, bundles and ref logs with lengths, in pages of at most 2000 |
| `GET /machine/archive-file/<machine>/<realm>/<path>[?sha256=<n>]` | a stored file (ranges supported), or the SHA-256 of its first n bytes |
| `PUT /machine/secrets/<realm>` | store an encrypted secrets bundle |
| `GET /machine/secrets/<realm>[?machine=<m>]` | the newest bundle (of one machine) |

Uploads need `X-Devc-Request: 1`. The archive is a ratchet:

- A machine writes only below `archive/<its registry name>/<realm>/`; the name
  comes from its certificate, never from the request. Paths are validated
  (`files/<path>` for the kinds of durable state the archive carries,
  `bundles/relay/<project>/…` and `bundles/context/…`), nothing outside
  that tree can be named.
- An upload appends at `offset`, which must equal the stored length, or the
  hub answers 409 with the stored length and the machine resumes from
  there. Nothing is ever overwritten, truncated or deleted by a machine.
- Bundles are stored in sequence: bundle n is accepted only after bundle
  n-1 is complete, is kept as `<name>.part` until the machine says it is
  final, and then becomes visible without replacing anything.
- A file is stored in generations (`files/<path>`, `files/<path>~1`, …),
  each with a record beside it (`<file>~meta`): its class and permission
  bits, given by the machine when it created the generation, and the length
  at which the machine last marked it complete (`final=1`; an empty body
  with `final=1` stores an empty file). A restore uses the newest complete
  generation. Generations only grow: one older than a stored one is refused.
- The class comes from the harness manifests on the machine. Append-only
  files (transcripts, and the host-written logs) keep every generation.
  For a mutable file (anything else in shared harness state, such as a
  database in a shared home) the machine uploads a new generation for any
  change, at most one per `[sync] mutable_interval` (default 6 hours, in the
  machine's `config.toml`), and the hub drops the generations older than
  its newest three complete ones. That is the one thing the hub removes on
  its own; a generation of an append-only file is never removed, whatever a
  machine (or a box rewriting a transcript) does.

Budgets (`[archive]` in `hub.toml`; defaults shown):

```toml
[archive]
machine_quota = "50g"     # one machine's whole archive
realm_quota = "20g"       # one machine's archive of one realm
max_files = 200000        # files and bundles per machine
max_request = "8m"        # one upload request (64k to 64m)
max_realms = 100          # realms per machine (archive and secrets together)
max_uploads = 8           # uploads (archive and secrets) in progress on the hub
machine_uploads = 2       # uploads in progress per machine
max_downloads = 8         # reads (indexes, summaries, files, secrets) in progress on the hub
machine_downloads = 2     # reads in progress per machine
min_free = "2g"           # free space always left on the hub's file system
```

Secrets bundles count against the machine's quotas, file limit and realms.
Every archive and secrets request takes a place among the hub's and the
calling machine's admission limits before the hub reads its body or lists
anything, so one machine cannot take every place; a download that makes no
progress for 30 seconds is ended and frees its place. A reached quota is
answered with 507 and a message saying which quota and what to do; the
machine shows it in `devc sync status` and `devc hub status` and pauses its
uploads to that hub for ten minutes at a time. A full admission gets 429
with `Retry-After`. On the hub:

```sh
devc hub archive ls                         # machines and realms, files, size, last write
devc hub archive du                         # per machine against its quota
devc hub archive prune --machine old-laptop --before 2026-01-01 [--dry-run] [--yes]
```

`prune` is the only way anything else leaves the archive: it deletes a
machine's files last written before the date, and the bundles and ref log of
a repository whose newest bundle and ref log are older (a bundle is useless
without those before it). A running hub notices and counts the usage again.

**Exposure.** Every enrolled machine can read every machine's archive (a new
machine restores a lost one's state with `devc state pull <realm> --from
<hub> --machine <name>`). The hub is therefore a place where every
enrolled machine, and whoever controls the hub, can read all archived realm
state: transcripts, journals, code in the relays, notes and memory. Secrets
are the exception: `devc secret push` encrypts them on the machine with a
key derived from a passphrase (argon2id, XChaCha20-Poly1305) and the hub
keeps only that ciphertext, the last ten versions per machine and realm.

## 11. A second hub

A machine may list several hubs; each builds its own view, keeps its own
archive (uploads go to each hub independently), and the first connected hub
in the machine's list sends notifications. To run a second hub for the same
fleet:

```sh
# on the second server, with a copy of the first hub's ca/ directory in /tmp/hub1
devc hub init --public-name hub2.example.org --ca-from /tmp/hub1   # the same CA, its own database
devc hub user set-password
devc hub ca import phone.crt          # register certificates issued by the first hub,
devc hub ca import laptop.crt         # or enrol machines and devices here anew
devc hub serve --listen …             # as in section 3
```

- **Same CA files.** With the first hub's `ca.crt` and `ca.key`, both hubs
  trust the same certificates and machines use the same `--ca` file for
  both. Each hub has its own registry, though: register a certificate with
  `devc hub ca import <cert>` (it must have been issued by this CA), or enrol
  it on the second hub with a token. Revoke on both.
- **Its own database** (`hub.db`) and its own password; nothing is
  replicated between hubs.
- **Optionally its own ntfy** (`[ntfy]` in its `hub.toml`): only the hub that
  is a machine's notifier publishes for it. When the first hub is
  unreachable, the machine makes the next connected hub its notifier and
  hands the role back when the first returns.

On each machine, join the second hub as well; with the same CA the machine's
existing certificate works once it is registered there:

```sh
devc join https://hub2.example.org --cert <state>/durable/node/hubs/<id>/cert.pem \
    --key <state>/durable/node/hubs/<id>/key.pem --ca ca.crt
devc hub status        # both hubs: connected, which one notifies, the archive on each
devc sync status       # what each hub's archive still lacks
```

