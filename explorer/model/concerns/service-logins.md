+++
id = "service-logins"
title = "Service logins"
summary = "How a box uses a web service that needs a credential without holding it: definitions in the configuration directory, a loopback port per service, the daemon's checks on each request's raw head and rules, a reverse proxy that adds the credential and filters both directions, and cookie logins in a throwaway container."
order = 56
packages = ["internal/svclogin"]
files = [
  "internal/daemon/services.go",
  "internal/config/services.go",
  "internal/cli/svccmds.go",
]
key = [
  "internal/config.Service",
  "internal/config.Service.Fingerprint",
  "internal/box.Capabilities.Service",
  "internal/daemon.boxWorker.serveService",
  "internal/svclogin.CheckHead",
  "internal/svclogin.NormalizeTarget",
  "internal/svclogin.Allowed",
  "internal/svclogin.Proxy.ServeHTTP",
  "internal/svclogin.rewrite",
  "internal/svclogin.modifyResponse",
  "internal/svclogin.HeaderCredential",
  "internal/daemon.serviceManager.credential",
  "internal/svclogin.Login.Run",
]
spec = ["doc:specs-v2.md#10.4", "doc:docs/guide.md#service-logins"]
tests = [
  "internal/svclogin.TestCheckHead",
  "internal/svclogin.TestReadHead",
  "internal/svclogin.TestNormalizeTarget",
  "internal/svclogin.TestPathMatchesAndAllowed",
  "internal/svclogin.TestProxyHeaders",
  "internal/svclogin.TestProxyLimits",
  "internal/svclogin.TestHeaderCredential",
  "internal/svclogin.TestProxyShortCredential",
  "internal/svclogin.TestProxyCookieLogin",
  "internal/svclogin.TestParseLoginOutput",
  "internal/svclogin.TestJar",
  "internal/svclogin.TestLogin",
  "internal/config.TestBrokenServices",
  "internal/box.TestServiceCapabilities",
  "internal/daemon.TestServiceStreams",
  "internal/daemon.TestServiceHostileStreams",
  "internal/daemon.TestServiceStreamDeadlines",
  "test/integration.TestServiceHeader",
  "test/integration.TestServiceCookie",
]
related = ["security-model", "secrets", "allowlist-proxy", "box-channel", "box-lifecycle", "configuration"]

[[invariant]]
text = "No project of the realm receives a service's credential secret (value_secret, auth.secrets) or any cookie jar (_jar.<name>), whether it is granted the service or not: one guard refuses such a name in grants.secrets, as a harness's credential and in grants.github = \"token:<secret>\". A service's definition may not use the realm's GitHub credential, its App's key or a cookie jar."
refs = ["internal/config.Config.checkBoxSecret", "internal/config.Config.reservedSecret", "internal/config.Config.checkServiceSecret", "internal/config.Service.CredentialSecrets", "internal/daemon.serviceManager.credential"]
tests = ["internal/config.TestReservedSecrets", "internal/config.TestBrokenServices", "test/integration.TestServiceHeader"]

[[invariant]]
text = "A request reaches the upstream only if the service is in force for the box's container, its head passes CheckHead, and a rule allows its method on its normalised path."
refs = ["internal/daemon.Daemon.serviceInForce", "internal/box.Capabilities.Service", "internal/svclogin.CheckHead", "internal/svclogin.Allowed"]
tests = ["internal/daemon.TestServiceStreams", "internal/svclogin.TestCheckHead"]

[[invariant]]
text = "The upstream receives exactly the path the rules were checked against, and only the first request of a connection."
refs = ["internal/svclogin.rewrite", "internal/svclogin.NormalizeTarget", "internal/daemon.newServiceManager"]
tests = ["internal/svclogin.TestProxyHeaders", "internal/daemon.TestServiceHostileStreams"]

[[invariant]]
text = "The box's own credential, cookie, proxy and forwarding headers never reach the upstream; Set-Cookie, the upstream's 1xx responses and trailers never reach the box."
refs = ["internal/svclogin.rewrite", "internal/svclogin.modifyResponse", "internal/svclogin.noTrace"]
tests = ["internal/svclogin.TestProxyHeaders"]

[[invariant]]
text = "No request body larger than max_body reaches the upstream, nor any part of one: a body without a declared length is read whole through the limit before the credential is fetched."
refs = ["internal/svclogin.Proxy.ServeHTTP", "internal/svclogin.readBody"]
tests = ["internal/svclogin.TestProxyLimits"]

[[invariant]]
text = "Every credential part that goes out has at least eight characters and is among the values scrubbed from response headers."
refs = ["internal/svclogin.HeaderCredential", "internal/svclogin.Credential.check", "internal/svclogin.checkCookies", "internal/svclogin.scrubber.header"]
tests = ["internal/svclogin.TestHeaderCredential", "internal/svclogin.TestProxyShortCredential", "internal/svclogin.TestProxyPaddedCredential"]

[[invariant]]
text = "Once its head passed the checks, a service stream ends at limits.timeout plus five seconds (closed a second later), whatever the box or the upstream does; the head has 30 seconds."
refs = ["internal/daemon.boxWorker.serveService", "internal/daemon.relayConn.clamp"]
tests = ["internal/daemon.TestServiceStreamDeadlines"]

[[invariant]]
text = "A login runs without capabilities and with nothing mounted but its scratch directory; its output becomes a jar only if every cookie is valid and for the service's host."
refs = ["internal/svclogin.Login.run", "internal/svclogin.ParseLoginOutput", "internal/svclogin.checkCookies"]
tests = ["internal/svclogin.TestLogin", "internal/svclogin.TestParseLoginOutput"]

[[diagram]]
id = "service-credentials"
title = "Where a service's credential lives, and what the box sees"
kind = "graph"

[[diagram.node]]
id = "def"
label = "service.toml"
col = 1
row = 1
zone = "host"
ref = "internal/config.Service"

[[diagram.node]]
id = "valsecret"
label = "secret value_secret"
col = 1
row = 2
zone = "host"
ref = "internal/config.ServiceAuth.ValueSecret"

[[diagram.node]]
id = "loginsec"
label = "login secrets (auth.secrets)"
col = 1
row = 3
zone = "host"
ref = "internal/config.ServiceAuth.Secrets"

[[diagram.node]]
id = "proxy"
label = "daemon: service proxy"
col = 2
row = 2
zone = "host"
ref = "internal/svclogin.Proxy.ServeHTTP"

[[diagram.node]]
id = "jar"
label = "secret _jar.<name>"
col = 2
row = 3
zone = "host"
ref = "internal/config.JarSecret"

[[diagram.node]]
id = "login"
label = "login container"
col = 2
row = 4
zone = "aux"
ref = "internal/svclogin.Login.Run"

[[diagram.node]]
id = "box"
label = "box: 127.0.0.1:<port>"
col = 3
row = 1
zone = "box"
ref = "internal/boxside.Agent.setServices"

[[diagram.node]]
id = "upstream"
label = "upstream"
col = 3
row = 3
zone = "external"

[[diagram.edge]]
from = "def"
to = "proxy"
label = "rules, port, upstream"

[[diagram.edge]]
from = "valsecret"
to = "proxy"
label = "read per request"
ref = "internal/svclogin.HeaderCredential"

[[diagram.edge]]
from = "jar"
to = "proxy"
label = "Cookie for the path"
ref = "internal/svclogin.Jar.Credential"

[[diagram.edge]]
from = "loginsec"
to = "login"
label = "0600 files in /login"

[[diagram.edge]]
from = "login"
to = "upstream"
label = "logs in"

[[diagram.edge]]
from = "login"
to = "jar"
label = "cookies, checked"
ref = "internal/svclogin.ParseLoginOutput"

[[diagram.edge]]
from = "box"
to = "proxy"
label = "request, no credential"
ref = "internal/svclogin.CheckHead"

[[diagram.edge]]
from = "proxy"
to = "box"
label = "headers scrubbed"
ref = "internal/svclogin.modifyResponse"

[[diagram.edge]]
from = "proxy"
to = "upstream"
label = "credential added"
ref = "internal/svclogin.rewrite"
+++
## What it is for

Some services an agent needs offer no token a box could hold safely: only a password login, or a token that can do much more than the task. A service login lets a box use such a service, an intranet wiki or an internal API, without ever holding its credential. You define the service in the realm's configuration and grant it to a project; the box sees an ordinary HTTP server on a loopback port; the daemon holds the credential, checks every request against the service's rules and adds the credential on the way out. None of this has run against a real service ([[doc:docs/dev/LEFTOVER.md#1]], "Services").

## How it works

**Definitions** ([[internal/config.Service]]) live in `realms/<realm>/services/<name>/service.toml`: the `upstream` (https, or http with `allow_insecure`; scheme, host and port only), the loopback `port` in boxes (above 1024, not 3128, unique in the realm), `[auth]`, one to 64 `[[allow]]` rules of methods and a path, and `[limits]` (`max_body`, default 10m; `timeout`, default 60s). A header service names the header and the realm secret that holds its value; a cookie service names a login command and its secrets, `refresh` (default 12h), and optionally `login_path` and `auto`. A rule's path must already be in the normal form requests are compared in ([[internal/config.CheckRulePath]]). A project grants services with `grants.services`; a granted service whose definition has problems blocks the project. [[diagram:service-credentials]]

**Ports.** Every start records each granted service with its port and a fingerprint of upstream, port, kind, credential header and secret, and rules ([[internal/config.Service.Fingerprint]]). For every connected box the daemon sends `box.services` with the services in force, at connect and whenever they change (compared every five seconds, [[internal/daemon.boxWorker.serviceLoop]]); the agent listens on exactly those ports of 127.0.0.1 and relays each connection unparsed on a `host.service` stream that names the service. A service is in force while it is granted now, was granted at the start, and its fingerprint and port are unchanged ([[internal/box.Capabilities.Service]]): a change of those needs a restart, while the login command, `refresh`, `auto`, `login_path` and the limits apply at once.

**A request** ([[flow:service-request]]) meets these checks in order. Each refusal from step 2 on is answered with a status and a message for the agent, and written to the service's log (in step 2 only for a service that is defined); a head that never completes only closes the stream:

| # | Check | Refusal |
|---|---|---|
| 1 | a relay place among the box's 64, shared with the proxy and host exceptions | 503 |
| 2 | the service is in force for this container ([[internal/daemon.Daemon.serviceInForce]]) | 403 with the reason |
| 3 | the head arrives within 30 s, at most 64 KiB, every line ending in CRLF ([[internal/svclogin.ReadHead]]) | 431, 400 |
| 4 | [[internal/svclogin.CheckHead]]: `METHOD /path HTTP/1.x` with single spaces, origin form only, token header names, no line folding or control characters, at most 100 fields, one `Content-Length` of digits or one `Transfer-Encoding: chunked` (never both, never with HTTP/1.0), exactly one `Host` that is `127.0.0.1:<port>` or `localhost:<port>`, no upgrade, `Expect` only `100-continue` | 400, 431, 501, 505, 403, 417 |
| 5 | the path in normal form ([[internal/svclogin.NormalizeTarget]]): unreserved characters decoded; no encoded `/`, `\`, `%`, reserved or control character; no `.` or `..` segment, also before `;`; no empty segment but the last | 400 |
| 6 | a rule allows the method on that path, matched at a segment boundary ([[internal/svclogin.Allowed]]) | 403 with the `[[allow]]` block that would be needed |
| 7 | the body fits `max_body`: a declared `Content-Length` is checked, a body without one is read whole, in memory up to 1 MiB and beyond that in an unlinked temporary file, and sent on with its length ([[internal/svclogin.readBody]]) | 413 |
| 8 | the credential is on the host and valid | 503 with what to tell the user |

Only after step 6 does net/http's server parse the same bytes, with keep-alives off, so nothing after the first request is served. [[internal/svclogin.rewrite]] sends the normalised path to the upstream's `Host`, removes the box's credential, cookie, proxy and forwarding headers and adds the credential; the transport uses the host's trust store, no proxy and no redirects. [[internal/svclogin.modifyResponse]] drops `Set-Cookie` and its kin, maps redirects on the upstream to the local address, scrubs every header and drops trailers ([[flow:service-request]] lists them all); the upstream's 1xx responses never pass ([[internal/svclogin.noTrace]]), though net/http's server answers `Expect: 100-continue` itself; a response over `max_body` gets 502 or, streamed, is aborted. Each request is logged to `durable/realms/<realm>/service-log/<name>.jsonl`: time, box, method, normalised path, status and devc's reason.

**Credentials.** A header service's secret is read for each request and brought into one normal form ([[internal/svclogin.HeaderCredential]]): printable ASCII, a leading scheme such as `Bearer` that counts as public, and every other word, Basic's password included, of at least eight characters. The value and each secret part are scrubbed from response headers, as written and URL-encoded. A cookie service's credential is the jar's `Cookie` header for the request's host and path ([[internal/svclogin.Jar.Credential]]); every stored cookie value is scrubbed.

**Cookie logins** ([[flow:service-login]]). [[internal/svclogin.Login.Run]] takes the service's lock, copies the service directory's other files and the login secrets (0600) into a scratch directory, and runs the login command in a container from `auth.image` or the installer image, with network but no capabilities and only the scratch directory mounted, for five minutes at most. The command prints the cookies as a JSON array, which [[internal/svclogin.ParseLoginOutput]] checks strictly: domains that match the upstream's host, values of at least eight characters, at least one live. The jar becomes the realm secret `_jar.<name>` with the time of the login, and the login state is reset. A missing jar, one older than `refresh` or without a live cookie, or an upstream answering 401 or redirecting to `login_path` means a login is needed: the box gets 503, `local/services/<realm>/<name>.json` records it, and the box's live sessions get a note. With `auto` the daemon runs `devc service login --quiet` itself, one at a time per service and at most once a minute ([[internal/daemon.serviceManager.autoLogin]]): a request that finds the jar unusable waits for it, then goes on if it helped; one answered 401 still gets 503.

## What can go wrong, and what prevents it

A box controls every byte of its requests and when it opens connections, and can open a `host.service` stream for any name.

- **Another service or path.** The service must be in force for the box's container. The rules see the normalised path and the upstream receives that same path, so nothing the upstream decodes later can change what was checked ([[finding:D2-16]]). Methods other than GET, HEAD, POST, PUT, PATCH, DELETE and OPTIONS can never be allowed.
- **A smuggled second request.** The raw head is checked before net/http sees it, ambiguous framing is refused, and keep-alives are off.
- **The credential.** The box's own credential headers are removed before the credential is added; it is in no file or environment of the box. Its secrets and every jar are [[term:reserved-secret|reserved]]: no project of the realm may name one for its box ([[internal/config.Config.checkBoxSecret]]). Headers are scrubbed, bodies are not: an upstream that reflects request headers in its body shows the credential ([[doc:docs/dev/LEFTOVER.md#5]]). Logs and messages hold no header or body; a failed automatic login's text stays on the host.
- **The host's resources.** A stream needs a relay place, the head must come within 30 seconds, and the exchange ends at `timeout` plus five seconds, also when the box stops reading ([[finding:D2-04]]). A body without a length is held whole (up to `max_body`), past 1 MiB on disk.
- **The account.** Within its rules the box acts with the user's account, and the log records what it did.
- **The login.** The login container sees only its scratch directory; its output is parsed strictly, and messages name cookies by position and never repeat the output ([[finding:D2-08]]). Every login secret is removed from its standard error and from errors before they are shown or stored.

## Decisions that are not obvious

- **No service containers.** The daemon holds the credential and proxies; only a cookie login runs in a container ([[chunk:C18]]).
- **One request per connection**, at the cost of clients that pipeline.
- **Services work at every network level**, `none` included: like the [[concern:ssh-gate|SSH gate]], they are an explicit grant.
- **Ports come from the daemon**, not from the box's definition, so a removed grant closes the port within seconds.
- **Eight characters** is the shortest credential part: a shorter value could not be scrubbed without garbling ordinary headers ([[finding:D2-07]], [[finding:E2-02]]).
- **No private-address check** on upstreams: they are the user's configuration, and internal services are the point.

## Where to start reading

1. [[internal/daemon.boxWorker.serveService]]
2. [[internal/svclogin.CheckHead]] and [[internal/svclogin.NormalizeTarget]]
3. [[internal/svclogin.Proxy.ServeHTTP]], [[internal/svclogin.rewrite]] and [[internal/svclogin.modifyResponse]]
4. [[internal/daemon.serviceManager.credential]] and [[internal/svclogin.HeaderCredential]]
5. [[internal/svclogin.Login.Run]]
