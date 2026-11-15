# C18 — Service logins

Read `docs/dev/README.md` and the `STATUS.md` notes of C08, C10a and C13. Product spec: `specs-v2.md` section 17 (service logins). Background: `../v2-research/04-credentials.md` sections 2 and 4 (those sections only).

Some services have no access tokens, only a login with a password, or a token the user does not want an agent to hold. A *service* gives boxes an authenticated, filtered way to use such a thing without the credential ever entering a box: the daemon holds the credential and proxies requests, adding it on the way out.

## 1. Definition

`realms/<realm>/services/<name>/service.toml`:

```toml
upstream = "https://internal.example.org"     # scheme and host the proxy talks to
port = 8601                                   # loopback port inside granted boxes

[auth]
kind = "header"                               # header | cookie
header = "Authorization"                      # for kind = header
value_secret = "example-token"                # realm secret; may contain a full header value
login = []                                    # for kind = cookie: command that performs the login (section 3)
refresh = "12h"                               # for kind = cookie: how long a cookie jar is trusted

[[allow]]                                     # at least one rule; first match wins; default deny
methods = ["GET"]
path = "/api/"                                # prefix

[limits]
max_body = "10m"
timeout = "60s"
```

A project gets a service with `grants.services = ["name"]`. Validate strictly: `https` upstreams only (plain `http` needs `allow_insecure = true`), a port above 1024 that is unique among the realm's services, methods from the standard set, paths starting with `/`.

## 2. Proxy

- In a granted box the in-box agent listens on `127.0.0.1:<port>` and relays connections on a `host.service` stream (the pattern of C13's proxy and host exceptions). Box processes use `http://127.0.0.1:<port>/…`; the sandbox description lists each granted service with its base URL and what is allowed.
- The daemon parses each request and applies the rules: method, path prefix after normalisation (no `..`, no encoded slashes, no absolute-form targets). Anything else gets 403 with a body saying which rule would be needed.
- It forwards to the upstream over TLS with the host's trust store, sets `Host` to the upstream's, removes any `Authorization`, `Cookie` and proxy headers the box sent, and adds the credential.
- Response handling: strip `Set-Cookie`; rewrite `Location` headers that point at the upstream to the local base URL; pass bodies through with the size limit. WebSocket upgrades are refused in this version.
- A response that echoes the credential would leak it. Scan response headers for the secret value and replace it; note in the documentation that an upstream endpoint that reflects request headers in its body can still disclose it, so rules should not allow such endpoints.
- Log each request (time, box, method, path, status) to `durable/realms/<realm>/service-log/<name>.jsonl`. Never log headers or bodies.

## 3. Cookie logins

For `kind = "cookie"`, a login produces a cookie jar that the proxy then attaches.

- `devc service login <realm> <name>` runs the service's `login` command in a throwaway container (the image named by `auth.image`, default the installer image; network on; nothing mounted but a scratch directory). The command receives the secrets named in `auth.secrets` as files in that scratch directory, and must write the cookies as JSON (name, value, domain, path, expiry) to its standard output. devc stores the jar as a realm secret and records the time.
- When the jar is older than `refresh`, or the upstream answers 401 or redirects to its login path (`auth.login_path`), the daemon marks the service as needing login, answers 503 to boxes with a clear message, and raises a `note` for the user. It does not log in by itself unless `auth.auto = true`, in which case it runs the login command once, serialised.
- Ship one worked example under `examples/services/`: a login script using plain HTTP form posting with curl, and notes on what a browser-driven login would need. Do not add a browser automation dependency.

## 4. Commands

`devc service ls [realm]`, `devc service login <realm> <name>`, `devc service status <realm> <name>`, `devc service log <realm> <name>`.

## 5. Tests

Unit: rule matching and path normalisation; header stripping in both directions; credential scrubbing; cookie jar parsing and expiry.

Integration with a local test upstream (TLS with a test CA added to the daemon's trust for the test):

1. Header service: an allowed GET from a box reaches the upstream with the credential; the box never sees it (search the response and the box for the value); a disallowed method and a disallowed path get 403; traversal and encoded-slash tricks are refused.
2. The upstream's `Set-Cookie` does not reach the box; a redirect to the upstream is rewritten.
3. Cookie service: `service login` with a scripted form login stores a jar; requests carry the cookie; an expired jar gives 503 and a note; `auto = true` logs in again once under concurrent requests.
4. A box without the grant cannot reach the service; a box of another realm cannot either.
5. With the daemon stopped the port does not answer.

## 6. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `docs/guide.md` gains a section on services, including the limits stated above and a reminder to check the proxied service's terms.
- `STATUS.md` and `LEFTOVER.md` are updated.
