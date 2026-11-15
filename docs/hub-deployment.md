# Deploying a hub

A hub is one always-on `devc hub serve` process on a server that your
machines can reach on port 443. Machine daemons dial out to it over TLS; the
hub never connects to a machine, so machines need no open port and no VPN.
The hub keeps a view of every session on every machine, derives the inbox,
and offers a JSON API (and, later, the web app) to your devices.

Nothing speaks HTTP to the hub without a client certificate from the hub's
own small CA. Every request is then checked against the hub's registry of
issued certificates, so a revoked certificate stops working at once, also
for connections that are already open.

The hub keeps its state in its own directory, `$DEVC_HUB_DIR` (default
`~/.local/share/devc-hub`), never mixed with a machine's state. It holds
nothing that cannot be rebuilt from the machines, except its CA and its
registry of certificates: back up `ca/` and `hub.db`.

## 1. Create the hub and its certificates

On the server:

```sh
devc hub init --public-name hub.example.org
devc hub ca issue machine laptop --out ~/hub-certs     # one per machine
devc hub ca issue device phone --out ~/hub-certs       # one per browser or phone
devc hub ca ls
```

- `init` creates the CA (`ca/ca.crt`, `ca/ca.key`, ECDSA P-256, ten years)
  and the database (`hub.db`). With `--public-name` it also writes a server
  key (`server/server.key`), a certificate request for a public CA
  (`server/server.csr`) and a server certificate for that name signed by the
  devc CA (`server/server.crt`). `devc hub ca issue server <name>` replaces
  them later.
- Machine and device certificates are valid for one year, name the machine or
  device as common name and the role (`machine` or `device`) as organisational
  unit. The hub takes both from its registry, not from the certificate.
- A device certificate comes with a PKCS#12 bundle (`phone.p12`) holding the
  key, the certificate and the CA, protected by a passphrase that is printed
  once. Import it into the phone's or browser's certificate store. Older
  phones may need `--legacy-p12` (weaker encryption of the bundle file).
- Move the files to their machine or device over a channel you trust and
  delete them from the server; the hub does not need them.
- `devc hub ca revoke <name>` (or a fingerprint prefix, optionally with
  `--role`) revokes certificates. The running hub closes their connections
  within a second.

The public name is the host (and port, when it is not 443) that machines and
browsers use. The hub answers only requests whose `Host` header names it.

## 2a. Direct TLS

The hub terminates TLS itself and requires a client certificate from its CA
on every connection:

```sh
devc hub serve --listen :443 --tls-cert /etc/letsencrypt/live/hub.example.org/fullchain.pem \
               --tls-key /etc/letsencrypt/live/hub.example.org/privkey.pem
```

Without `--tls-cert` and `--tls-key` it uses `server/server.crt` from `init`.
That certificate is signed by the devc CA: machines accept it when they join
with `--ca ca.crt`, browsers only after the devc CA was installed as a trusted
root. A certificate from a public CA (Let's Encrypt, for example) avoids that;
renewing it means restarting the hub. Listening on port 443 needs the
capability to bind low ports (`setcap cap_net_bind_service=+ep` on the binary,
or a systemd socket); a reverse proxy avoids both.

## 2b. Behind a reverse proxy

The proxy terminates TLS, verifies the client certificate against the devc CA,
and passes it to the hub in the `X-Devc-Client-Cert` header. The hub listens on
plain HTTP on a loopback address only (it refuses to start otherwise) and
accepts the header only from loopback peers:

```sh
devc hub serve --behind-proxy --listen 127.0.0.1:8443 --public-name hub.example.org
```

The proxy must:

- require a client certificate and verify it against `ca/ca.crt`;
- **set** `X-Devc-Client-Cert`, replacing anything the client sent, to the
  verified certificate: URL-encoded PEM or base64 DER (the hub verifies it
  against the CA again);
- pass the original `Host` header;
- pass WebSocket upgrades (machines connect to `/machine`);
- not buffer responses (`/api/stream` is a stream of server-sent events).

### Caddy

```caddy
hub.example.org {
	tls {
		client_auth {
			mode require_and_verify
			trust_pool file /etc/devc-hub/ca.crt
		}
	}
	request_body {
		max_size 1MB
	}
	reverse_proxy 127.0.0.1:8443 {
		header_up X-Devc-Client-Cert {http.request.tls.client.certificate_der_base64}
		flush_interval -1
	}
}
```

Caddy obtains the server certificate for `hub.example.org` itself, keeps the
original `Host` header and proxies WebSockets without further settings.
`header_up` replaces a header the client sent. With client authentication
Caddy requires the TLS server name to match the `Host` header, so the public
name must be a DNS name, not an IP address. Caddy versions before 2.8 spell
the trust pool `trusted_ca_cert_file /etc/devc-hub/ca.crt`.

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

    # Only clients with a certificate from the devc CA get any HTTP.
    ssl_client_certificate /etc/devc-hub/ca.crt;
    ssl_verify_client on;
    ssl_verify_depth 1;

    client_max_body_size 1m;

    location / {
        proxy_pass http://127.0.0.1:8443;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Devc-Client-Cert $ssl_client_escaped_cert;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_buffering off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }
}
```

`$ssl_client_escaped_cert` is the URL-encoded PEM of the verified
certificate. With `ssl_verify_client on`, nginx refuses a connection without
a valid certificate before any request reaches the hub.

Both configurations were checked (nginx 1.28.0, Caddy 2.10.0) with a hub
behind them: a request without a certificate fails in the TLS handshake, a
forged header is replaced, a revoked certificate gets 403, and a machine
daemon connects through the proxy.

## 3. Run the hub as a service

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

Run `devc hub init` and `devc hub ca …` as the same user with the same
`DEVC_HUB_DIR`.

## 4. Join machines

On each machine, with the machine's certificate and key:

```sh
devc join https://hub.example.org --cert laptop.crt --key laptop.key   # add --ca ca.crt for a devc server certificate
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

## 5. What a hub can and cannot do

Over its connection the hub is the caller and the machine daemon decides what
it may call: reading boxes, sessions and events, the session commands
(`send`, `answer`, `interrupt`, `keys`, `seen`, `start`, `stop`), receipts, and
listing and deciding requests. The daemon refuses everything else (stopping
the daemon, host commands, policy changes) and decides low-risk requests only
(seal, savepoint, note); high-risk requests are decided with the CLI on the
machine. A compromised hub can therefore read the transcripts it mirrors and
type into online sessions, but cannot leave the boxes.

## 6. The API

JSON over HTTPS for device certificates (`machine` certificates may only use
`/machine`). State-changing requests must be `application/json` and, when the
browser sends an `Origin`, come from the hub's own origin.

| Endpoint | Purpose |
|---|---|
| `GET /api/machines` | machines with `online`, `last_seen`, `notifier` and their boxes |
| `GET /api/inbox` | sections `needs-you`, `failed`, `finished`, `working`, `idle`, each with session summaries, most recent change first |
| `GET /api/sessions?machine=&realm=&project=&live=&limit=` | session summaries, most recent change first |
| `GET /api/sessions/<machine>/<session>?before=&limit=` | a summary and recent events; `before=<seq>` pages backwards, asking the machine for events the hub does not hold |
| `POST /api/commands` | `{"machine", "type", "id", "params", "wait"}`; `type` is `send`, `answer`, `interrupt`, `keys`, `seen`, `start` or `stop`, `params` those of the daemon's method without the ID, `id` a UUID chosen by the client, `wait` up to 60 seconds for a final receipt. Returns the machine's receipt. |
| `GET /api/commands/<machine>/<id>?wait=` | the receipt |
| `GET /api/requests?machine=&realm=&project=` | pending requests |
| `POST /api/requests/<machine>/<id>/decide` | `{"approve", "reason"}`; the machine refuses high-risk requests |
| `GET /api/stream?session=<machine>/<session>` | server-sent events (below) |

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
