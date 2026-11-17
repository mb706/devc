+++
id = "hub-access"
title = "Who may reach the hub"
summary = "Client certificates from the hub's own CA for machines and devices, judged by a registry on every request; enrolment without a certificate while a token is outstanding; a password login bound to the device's certificate; the access class and cross-site rule of every route; and the limits on unauthenticated work."
order = 80
files = [
  "internal/hub/hub.go",
  "internal/hub/ca.go",
  "internal/hub/server.go",
  "internal/hub/front.go",
  "internal/hub/auth.go",
  "internal/hub/login.go",
  "internal/hub/enrol.go",
  "internal/hub/store_access.go",
  "internal/hub/doctor.go",
  "internal/cli/hubcmds.go",
  "internal/cli/hubaccess.go",
]
key = [
  "internal/hub.Server.ServeHTTP",
  "internal/hub.Server.identify",
  "internal/hub.Server.listen",
  "internal/hub.Server.gate",
  "internal/hub.routeTable",
  "internal/hub.Server.guard",
  "internal/hub.Server.checkSite",
  "internal/hub.Server.checkSession",
  "internal/hub.Server.apiLogin",
  "internal/hub.Store.UseEnrolment",
  "internal/hub.CA.signClient",
  "internal/hub.Server.addLive",
]
spec = ["doc:specs-v2.md#13"]
tests = [
  "internal/hub.TestEveryRouteNeedsACertificate",
  "internal/hub.TestProxyFront",
  "internal/hub.TestDirectTLS",
  "internal/hub.TestRegistry",
  "internal/hub.TestLogin",
  "internal/hub.TestLoginLimits",
  "internal/hub.TestRateLimit",
  "internal/hub.TestSlowBodies",
  "internal/hub.TestEnrolment",
  "internal/hub.TestRevokeEndsSessions",
  "internal/hub.TestCertificateExpiry",
  "internal/hub.TestInitAndIssue",
  "internal/hub.TestDoctor",
  "internal/cli.TestHubAccessCommands",
  "test/integration.TestHubAccess",
  "test/integration.TestHubBehindProxy",
]
related = ["hub-machine-link", "hub-commands-and-terminal", "hub-inbox-and-notifications", "secrets"]

[[invariant]]
text = "No handler runs for a request without a client certificate except the two enrolment routes, and those only while an enrolment token is outstanding."
refs = ["internal/hub.Server.gate", "internal/hub.isEnrolPath", "internal/hub.Server.buildMuxes", "internal/hub.Server.listen"]
tests = ["internal/hub.TestEveryRouteNeedsACertificate", "internal/hub.TestDirectTLS"]

[[invariant]]
text = "A certificate counts only while its registry record is not revoked or expired and names the role machine or device; name and role come from the record, not the certificate."
refs = ["internal/hub.Server.identify", "internal/hub.CertRecord.Valid"]
tests = ["internal/hub.TestRegistry", "internal/hub.TestProxyFront"]

[[invariant]]
text = "Machine routes serve only machine certificates, and every other route that needs a certificate serves only device certificates."
refs = ["internal/hub.Server.guard", "internal/hub.routeTable"]
tests = ["internal/hub.TestEveryRouteNeedsACertificate", "internal/hub.TestProxyFront"]

[[invariant]]
text = "A route of class user runs only with a login session made with the request's own certificate and used within the last 30 days."
refs = ["internal/hub.Server.checkSession"]
tests = ["internal/hub.TestLogin"]

[[invariant]]
text = "Every POST and PUT needs `X-Devc-Request: 1`, and those of the web app also an Origin equal to the hub's and a JSON body."
refs = ["internal/hub.Server.checkSite", "internal/hub.routeTable"]
tests = ["internal/hub.TestProxyFront", "internal/hub.TestEveryRouteNeedsACertificate"]

[[invariant]]
text = "An enrolment token works once, within at most 15 minutes, and is used up in the same transaction that registers its certificate."
refs = ["internal/hub.CreateEnrolment", "internal/hub.Store.UseEnrolment"]
tests = ["internal/hub.TestEnrolment"]

[[invariant]]
text = "Five failed logins in a row lock a certificate's logins for 15 minutes, however many attempts are sent at once."
refs = ["internal/hub.Store.LoginFailed", "internal/hub.Server.loginSlot", "internal/hub.Server.apiLogin"]
tests = ["internal/hub.TestLogin", "internal/hub.TestLoginLimits"]

[[invariant]]
text = "A machine link, event stream or terminal ends when its certificate is revoked or expires, and a revoked certificate's login sessions are deleted."
refs = ["internal/hub.Server.addLive", "internal/hub.Server.watchRegistry", "internal/hub.Store.Revoke"]
tests = ["internal/hub.TestCertificateExpiry", "internal/hub.TestRevokeEndsSessions"]

[[diagram]]
id = "access"
title = "Who reaches which routes"
kind = "graph"

[[diagram.node]]
id = "none"
label = "no client certificate"
col = 1
row = 1
zone = "external"

[[diagram.node]]
id = "machine"
label = "machine certificate"
col = 1
row = 3
zone = "host"
ref = "internal/hub.RoleMachine"

[[diagram.node]]
id = "device"
label = "device certificate"
col = 1
row = 4
zone = "device"
ref = "internal/hub.RoleDevice"

[[diagram.node]]
id = "user"
label = "device certificate and login session"
col = 1
row = 5
zone = "device"
ref = "internal/hub.SessionCookie"

[[diagram.node]]
id = "enrol"
label = "GET, POST /enroll/{token}"
col = 3
row = 1
ref = "internal/hub.accessEnrol"

[[diagram.node]]
id = "refused"
label = "404, connection closed"
col = 3
row = 2
ref = "internal/hub.Server.gate"

[[diagram.node]]
id = "mroutes"
label = "/machine and the archive"
col = 3
row = 3
ref = "internal/hub.accessMachine"

[[diagram.node]]
id = "droutes"
label = "login, buttons, the app's files"
col = 3
row = 4
ref = "internal/hub.accessDevice"

[[diagram.node]]
id = "uroutes"
label = "every other /api/ route"
col = 3
row = 5
ref = "internal/hub.accessUser"

[[diagram.edge]]
from = "none"
to = "enrol"
label = "a token outstanding"
ref = "internal/hub.Server.enrolOpen"

[[diagram.edge]]
from = "none"
to = "refused"
label = "anything else"
ref = "internal/hub.Server.gate"

[[diagram.edge]]
from = "machine"
to = "mroutes"
label = "registry: role machine"
ref = "internal/hub.Server.identify"

[[diagram.edge]]
from = "device"
to = "droutes"
label = "registry: role device"
ref = "internal/hub.Server.identify"

[[diagram.edge]]
from = "user"
to = "uroutes"
label = "session of this certificate"
ref = "internal/hub.Server.checkSession"
+++
## What it is for

The hub is the one part of devc on the internet, and through it a browser can type into agent sessions on your machines. So nothing may reach it without a credential: every client presents a certificate from the hub's own CA, and the hub's registry of issued certificates, not the certificate itself, says who the client is and whether it still counts. A browser also needs a login with the hub's password (and a TOTP code when that is on), bound to its certificate. A new machine or phone has no certificate yet; enrolment covers that moment with a short-lived, single-use token.

## How it works

**Keys.** Each kind of key lives in one place:

| Key | Kept in (0600) | Read by |
|---|---|---|
| CA | `<hub>/ca/ca.key` | the hub's commands; the server only at a machine's enrolment |
| Server | `<hub>/server/server.key`, or the `--tls-key` file | the server, in direct TLS |
| Machine | `durable/node/hubs/<id>/key.pem` on the machine | the daemon |
| Device | a PKCS#12 bundle: a file, or the enrolment's row until downloaded | the browser that imports it |

`devc hub init` makes the CA ([[internal/hub.createCA]]) and, with `--public-name`, the server key and certificate ([[internal/hub.CA.IssueServer]]). A machine makes its own key at `devc join --token` ([[internal/hub.NewMachineCSR]]); `devc hub ca issue machine` makes one on the hub for you to copy instead. A device's key is made by the hub's commands ([[internal/hub.CA.IssueClient]]), its bundle's passphrase printed once. Behind a proxy, the proxy holds its own server key.

Client certificates last a year, carry the name as CN and the role as OU, and serve client authentication only ([[internal/hub.CA.signClient]]). Each is recorded in the `certs` table by the SHA-256 of its DER encoding; [[internal/hub.CertRecord.Valid]] (not revoked, not expired, role machine or device) is the whole test. The user's argon2id hash and the TOTP secret, which must be kept as it is to compute codes, are in the same database (0600).

**Who a request comes from.** In direct TLS the listener ([[internal/hub.Server.listen]]) asks for a client certificate without requiring one. A certificate that is given must chain to the CA for client authentication, and `VerifyConnection` asks the registry, so an unknown or revoked one gets no HTTP at all; a handshake without a certificate passes only while an enrolment token is outstanding. Behind a reverse proxy the hub listens only on a loopback address, refuses peers that are not loopback, and takes the certificate from the `X-Devc-Client-Cert` header (one value, URL-encoded PEM or base64 DER), which it verifies against the CA again; an empty header counts as none. Either way [[internal/hub.Server.identify]] then asks the registry.

**The front.** [[internal/hub.Server.ServeHTTP]] runs before any handler: security headers (a CSP of `default-src 'self'` with `frame-ancestors 'none'`, nosniff, no-store, HSTS in direct TLS), a Host header naming the public name (else 421), then identification. A refused certificate gets its error at once. A request without a certificate goes to [[internal/hub.Server.gate]], the only code that handles such requests: it serves exactly `/enroll/<token>` while a token is outstanding, from a mux holding the enrolment routes alone, and answers everything else with 404 and a closed connection. A request with a certificate gets a body limit (1 MiB; archive uploads their own) and ten seconds to send its body (uploads two minutes: [[internal/hub.Server.bodyDeadline]]), then goes to the route table. [[diagram:access]]

**Routes.** [[internal/hub.routeTable]] gives every route an access class and a cross-site rule; [[internal/hub.Server.guard]] enforces both before the handler. The Interfaces view lists every route with both ([[surface:hub-routes]]).

| Class | Needs | Routes |
|---|---|---|
| `accessMachine` | a machine certificate | `/machine` and the archive below `/machine/` |
| `accessDevice` | a device certificate | login, notification buttons, the web app's page and files |
| `accessUser` | a device certificate and a login session bound to it | every other `/api/` route |
| `accessEnrol` | nothing; the token in the path is the credential | `GET` and `POST /enroll/{token}` |

| Rule | What [[internal/hub.Server.checkSite]] checks | Used by |
|---|---|---|
| `xsiteRead` | `Origin`, if sent, is the hub's; `Sec-Fetch-Site`, if sent, is `same-origin` or `none` | every GET but two, the terminal's WebSocket included |
| `xsiteStrict` | `X-Devc-Request: 1`, an `Origin` equal to the hub's, `Sec-Fetch-Site` as above, a JSON body | the web app's POSTs |
| `xsiteToken` | `X-Devc-Request: 1`; `Origin` and `Sec-Fetch-Site` only if sent | buttons, `devc join`'s enrolment, archive uploads |
| `xsiteOpen` | nothing | the page `/` and the enrolment download |

A page on another site cannot make the browser add `X-Devc-Request` without a CORS preflight, and the hub sends no CORS headers.

**Login.** The hub has one user ([[internal/hub.Server.apiLogin]], [[flow:hub-login]]). `devc hub user set-password` stores an argon2id hash (t=3, 64 MiB, p=4; at least 12 characters); `devc hub user totp enable` a TOTP secret (SHA-1, six digits, 30-second steps, one step of skew). A login stores a session under the SHA-256 of a 256-bit token and sets the token as the cookie `__Host-devc-session` (Secure, HttpOnly, SameSite=Strict). [[internal/hub.Server.checkSession]] accepts it only with the certificate it was made with, for 30 days after its last use.

**Enrolment.** `devc hub enroll machine|device <name>` stores a token's hash, valid for at most 15 minutes ([[internal/hub.CreateEnrolment]]). For a device the key, certificate and bundle are made at once and wait in the row; `GET /enroll/<token>` serves the bundle and registers the certificate ([[flow:device-enrol]]). For a machine, `POST /enroll/<token>` carries a certificate request; the hub checks the token before reading the body, accepts an ECDSA P-256 key with a valid signature and signs a machine certificate for the token's name ([[flow:machine-join]]). [[internal/hub.Store.UseEnrolment]] uses up the token in the transaction that registers the certificate.

**Revocation and expiry.** `devc hub ca revoke`, `devices revoke` and `machines revoke` mark records revoked and delete their login sessions ([[internal/hub.Store.Revoke]]). Machine links, event streams and terminals are registered with [[internal/hub.Server.addLive]], which sets a timer for the certificate's end; [[internal/hub.Server.watchRegistry]] reads the registry's generation every second and ends the connections whose certificate no longer counts.

## What can go wrong, and what prevents it

- **A client without a certificate.** Only the gate handles it, and [[internal/hub.isEnrolPath]] accepts no other segment, so a path that cleaning would change (`/enroll/x/../api/…`) routes nowhere.
- **A copied cookie.** It works only with the certificate it was made with; presented with another, the session is deleted.
- **Guessing the password.** Five failures in a row lock that certificate's logins for 15 minutes, kept in the database ([[internal/hub.Store.LoginFailed]]). One attempt per certificate runs at a time ([[internal/hub.Server.loginSlot]]) and the lock is read again right before the hash ([[finding:C2-03]]). A TOTP code works once ([[internal/hub.Store.UseTOTPStep]]).
- **Load without credentials.** At most 64 requests of class device or enrolment run at once (429); two password hashes at a time, eight waiting ([[internal/hub.Server.hashSlot]]). Failures drain a token bucket per address and per certificate (30, then one per two seconds: [[internal/hub.limiter]]); a body must arrive within ten seconds ([[finding:C2-04]]).
- **Requests forged by another site.** The cross-site rules (above).
- **A browser posing as a machine, or the reverse.** The class is checked against the registry's role.
- **Tokens in logs.** The access log shows `/enroll/-` and no query; button tokens travel in a header ([[finding:C2-06]]).
- **Local processes behind a proxy.** The hub believes the header from any loopback peer. A certificate is not secret, so a process on the hub's server that can reach the loopback port and holds a copy of a registered certificate could pose as its owner without the key; the setup relies on the proxy being the only local client.

## Decisions that are not obvious

- **The registry decides, not the certificate.** Revocation needs no revocation list, and a second hub with the same CA trusts only what it registered (`devc hub ca import`).
- **The server reads the CA key at enrolment** so that `devc hub enroll machine` can print a token and return ([[chunk:C11b]], deviation 5).
- **A device's key is made on the hub** and handed over in a PKCS#12 bundle; its passphrase exists only in the terminal that made it.
- **Buttons need a device certificate but no login**: the token is bound to one answer.
- **Rate limits count failures only**, and user routes consult them only when the login check fails, so a valid login is never limited there. Device routes ignore a login: login and buttons consult the certificate's bucket first, and every device request takes one of 64 places.
- **A new password keeps login sessions**; `devc hub sessions revoke --all` ends them.

## Where to start reading

1. [[internal/hub.Server.ServeHTTP]] and [[internal/hub.Server.identify]]
2. [[internal/hub.Server.gate]]
3. [[internal/hub.routeTable]], [[internal/hub.Server.guard]] and [[internal/hub.Server.checkSite]]
4. [[internal/hub.Server.apiLogin]] and [[internal/hub.Server.checkSession]]
5. [[internal/hub.Server.enrolMachine]] and [[internal/hub.Store.UseEnrolment]]
