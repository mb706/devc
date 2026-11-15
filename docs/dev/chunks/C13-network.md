# C13 — Network: allowlist, exceptions, local URLs

Read `docs/dev/README.md`, `docs/dev/plan.md` (section 3.9) and the `STATUS.md` notes of C07, C08 and C10a. Product spec: `specs-v2.md` section 11.

**Design change, decided before this chunk.** The plan foresaw a gateway container per realm. It is not needed: C07's `internet` level already fails closed (the sentinel check), and the box channel gives a simpler and stricter way to build the rest. No gateway container, no extra Podman network, no address management.

| Feature | How |
|---|---|
| `allowlist` level | the box has **no network at all** (`--network none`); an HTTP proxy is reachable on the box's own loopback, relayed over the box channel to the daemon, which enforces a domain allowlist and makes the outbound connection from the host |
| exceptions for LAN hosts | timed elements in the nft sets that C07 installs in the box's namespace |
| exceptions for host services | a loopback listener in the box, relayed over the channel to the same port on the host's loopback |
| local URLs for forwarded ports | an HTTP router in the daemon |

Update `specs-v2.md` section 11 and `docs/dev/plan.md` section 3.9 to describe this instead of the gateway.

## 1. `allowlist` level

- Box creation: `--network none`, plus the environment `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, `https_proxy` set to `http://127.0.0.1:3128`, `NO_PROXY=127.0.0.1,localhost`, and `NODE_USE_ENV_PROXY=1`.
- The in-box agent listens on `127.0.0.1:3128` and relays every connection, as opaque bytes, on a `host.proxy` stream. It does no parsing: the policy runs on the host.
- The daemon speaks the proxy protocol on that stream: `CONNECT host:port`, and plain HTTP requests with absolute URLs. For each request:
  1. the host name must match the project's allowlist (exact names and `*.suffix` patterns; IP literals are refused);
  2. the port must be 443 or 80 unless the entry names another;
  3. the daemon resolves the name itself and refuses any address in the private ranges of C07's rule list, checking the address it actually connects to;
  4. the decision is appended to `durable/realms/<realm>/netlog/<project>.jsonl` (time, session if known, host, port, allowed or denied).
- Configuration: `[network.allowlist]` in the realm and in the project (`domains = […]`, `presets = […]`), merged. Built-in presets, each a short explicit list kept in one source file: `anthropic`, `openai`, `github`, `pypi`, `npm`, `cran`, `debian`. Keep them minimal; no wildcard over a hosting provider.
- Without a connected daemon the proxy does not answer, so the box has no network. That is the intended failure mode; the sandbox description for this level says so, and names the proxy and how to request another domain.
- `devc net log [ref] [--denied]` shows the log.

## 2. Exceptions

| Command | Effect |
|---|---|
| `devc net allow <ip-or-name>:<port> [ref] [--for 2h]` | LAN host and port, for a box at level `internet`: resolved on the host, added to the box's `allow4` set with a timeout |
| `devc net allow lan [ref] [--for 30m]` | all private ranges, through a second, interval set that the rules consult before the reject rule |
| `devc net allow host:<port> [ref] [--for 2h]` | a service on the host's loopback: the in-box agent listens on `127.0.0.1:<port>` and relays over the channel (`host.dial`); the daemon dials the host's loopback only while the exception is active. Works at every level. Needs the daemon. |
| `devc net ls [ref]`, `devc net revoke <id>` | active exceptions with remaining time |

- The default duration is 2 hours; `--for forever` is accepted and shown prominently in `devc net ls` and `devc ls`.
- Set timeouts make LAN exceptions expire in the kernel without any process. They vanish when the box restarts; `devc up` re-applies those still valid from a small record in `local/`.
- For level `allowlist`, an exception for a name adds it to that box's allowlist for the duration instead.
- The request kind `net` (C10a) now executes on approval through the same code.

## 3. Local URLs

When the daemon runs, it listens on `127.0.0.1:7480` (configurable) and routes by `Host`: `<port>.<project>.<realm>.localhost:7480` goes to `127.0.0.1:<port>` inside that project's box, using C07's namespace dialer. WebSocket upgrades must work. Only ports listed in the project's `box.ports` or currently forwarded are routed; anything else gets a 404 that says how to allow it. `devc forward` prints the URL.

## 4. Tests

Unit: allowlist matching (exact, suffix, IP literal, port); the private-address check on resolved addresses (including a name that resolves to a private address, and a redirect-style second request); exception bookkeeping and expiry; the router's host parsing.

Integration (daemon running):

1. `allowlist`: the box has no interface besides loopback; a request through the proxy to an allowed name succeeds (use a local test server reached through a host-side resolver override that the test controls, so no real internet is needed); a request to a name that is not listed is refused and logged; a direct connection without the proxy fails; an allowed name that resolves to a private address is refused.
2. With the daemon stopped, the box has no network; after it starts again, the proxy works without restarting the box.
3. LAN exception: a listener on the host's LAN address is unreachable, reachable after `net allow`, and unreachable again after the timeout (use a short one) and after `revoke`.
4. Host exception: a server on the host's loopback is reachable from the box only while the exception exists.
5. The exception survives `devc down` and `up` while still valid.
6. Router: a server on the box's loopback answers at its local URL, including a WebSocket echo; an unlisted port gets the 404.
7. The escape suite of C07 still passes; add: at level `allowlist` the box cannot reach the host's LAN address or any public address directly.

## 5. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `docs/guide.md` describes the three levels, exceptions and local URLs as they now are.
- `STATUS.md` and `LEFTOVER.md` are updated. `LEFTOVER.md` must list: whether each real harness honours the proxy variables is unverified; tools that ignore them have no network at this level by design.
