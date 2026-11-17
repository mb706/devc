+++
id = "allowlist-proxy"
title = "Allowlist proxy and exceptions"
summary = "How a box of level allowlist reaches only listed names through the daemon's proxy, in which order a request is checked and how its connection stays watched; and how network exceptions are granted on the host, applied at each level, logged and ended."
order = 51
files = [
  "internal/netpolicy/proxy.go",
  "internal/netpolicy/allowlist.go",
  "internal/netpolicy/presets.go",
  "internal/netpolicy/netlog.go",
  "internal/netpolicy/exceptions.go",
  "internal/daemon/netproxy.go",
  "internal/boxside/netrelay.go",
  "internal/box/netexceptions.go",
  "internal/cli/netallow.go",
]
key = [
  "internal/boxside.Agent.relayTCP",
  "internal/daemon.boxWorker.serveProxy",
  "internal/daemon.Daemon.proxyPolicy",
  "internal/box.Capabilities.ProxyAllowlist",
  "internal/netpolicy.Proxy.check",
  "internal/netpolicy.Proxy.CheckAddr",
  "internal/netpolicy.Proxy.watch",
  "internal/netpolicy.NormalizeHost",
  "internal/cli.app.netAllow",
  "internal/netpolicy.ExceptionScript",
  "internal/daemon.boxWorker.syncNet",
  "internal/daemon.boxWorker.serveDial",
]
spec = ["doc:specs-v2.md#11"]
tests = [
  "internal/netpolicy.TestNormalizeHost",
  "internal/netpolicy.TestParseEntry",
  "internal/netpolicy.TestEntryMatches",
  "internal/netpolicy.TestExpandAndIntersect",
  "internal/netpolicy.TestProxyPlain",
  "internal/netpolicy.TestProxyConnect",
  "internal/netpolicy.TestProxyRefusals",
  "internal/netpolicy.TestProxyDialChecksAddress",
  "internal/netpolicy.TestProxyResponseHead",
  "internal/netpolicy.TestProxyHeadBudget",
  "internal/netpolicy.TestProxyTunnelsEnd",
  "internal/netpolicy.TestProxyTunnelStart",
  "internal/netpolicy.TestNetLog",
  "internal/netpolicy.TestParseTarget",
  "internal/netpolicy.TestExceptionStore",
  "internal/netpolicy.TestExceptionScript",
  "internal/daemon.TestNetProxy",
  "internal/daemon.TestNetHostDial",
  "internal/boxside.TestAgentRelays",
  "internal/boxside.TestRelayLimiter",
  "internal/cli.TestNetCommands",
  "test/integration.TestNetworkAllowlist",
  "test/integration.TestNetworkExceptions",
]
related = ["network-levels", "box-channel", "requests-and-approval", "daemon"]

[[invariant]]
text = "The proxy connects only to a public IPv4 address, checked again on the socket it actually connects, and not to one of the host's as last listed; before a first listing succeeds, it refuses none of them."
refs = ["internal/netpolicy.Proxy.CheckAddr", "internal/netpolicy.Proxy.dial", "internal/daemon.hostAddrCache.contains"]
tests = ["internal/netpolicy.TestProxyDialChecksAddress", "internal/netpolicy.TestProxyRefusals"]

[[invariant]]
text = "The proxy serves only a name and port that the allowlist allows both in the box's start record ([[term:capability-record]]) and now, or that an exception in force allows."
refs = ["internal/daemon.Daemon.proxyPolicy", "internal/box.Capabilities.ProxyAllowlist", "internal/netpolicy.ProxyPolicy.Allows"]
tests = ["internal/daemon.TestNetProxy", "internal/netpolicy.TestExpandAndIntersect"]

[[invariant]]
text = "An IP literal in any form, a numeric last label included, is refused as a destination and never resolved."
refs = ["internal/netpolicy.NormalizeHost", "internal/netpolicy.Proxy.check"]
tests = ["internal/netpolicy.TestNormalizeHost", "internal/netpolicy.TestProxyRefusals"]

[[invariant]]
text = "A tunnel or upstream connection is closed once what allowed it is no longer in force: at once on revoke, at an exception's end, else within five seconds."
refs = ["internal/netpolicy.Proxy.watch", "internal/netpolicy.Tunnels.Recheck", "internal/daemon.boxWorker.syncNet"]
tests = ["internal/netpolicy.TestProxyTunnelsEnd", "internal/netpolicy.TestProxyTunnelStart", "internal/daemon.TestNetProxy"]

[[invariant]]
text = "Only `devc net allow` adds an exception: run by the user, or by the daemon for a net request, which only the local socket can approve."
refs = ["internal/cli.app.netAllow", "internal/requests.MayDecide", "internal/daemon.requestCommands"]
tests = ["internal/cli.TestNetCommands", "internal/daemon.TestRequests"]

[[invariant]]
text = "An exception that cannot be applied to the running box does not stay in the record."
refs = ["internal/cli.app.netAllow"]
tests = ["internal/cli.TestNetCommands"]

[[invariant]]
text = "The daemon connects a host.dial stream only to the host's loopback, and only for a port a host exception allows at that moment."
refs = ["internal/daemon.boxWorker.serveDial", "internal/netpolicy.HostAllowed"]
tests = ["internal/daemon.TestNetHostDial"]

[[diagram]]
id = "exception-life"
title = "The life of an exception"
kind = "graph"

[[diagram.node]]
id = "asked"
label = "asked for (devc request net)"
col = 1
row = 1
zone = "box"
ref = "internal/requests.Validate"

[[diagram.node]]
id = "user"
label = "devc net allow"
col = 1
row = 2
zone = "host"
ref = "internal/cli.app.netAllow"

[[diagram.node]]
id = "record"
label = "in the record"
col = 2
row = 2
ref = "internal/netpolicy.WriteExceptions"

[[diagram.node]]
id = "force"
label = "in force in the box"
col = 3
row = 2
ref = "internal/cli.app.applyExceptions"

[[diagram.node]]
id = "down"
label = "box stopped: record only"
col = 3
row = 3
ref = "internal/box.NetExceptionHook"

[[diagram.node]]
id = "ended"
label = "ended (expired or revoked)"
col = 4
row = 2
ref = "internal/netpolicy.Exception.Active"

[[diagram.node]]
id = "gone"
label = "gone from the record"
col = 2
row = 3
ref = "internal/netpolicy.WriteExceptions"

[[diagram.edge]]
from = "asked"
to = "user"
label = "approved locally"
ref = "internal/daemon.requestCommands"

[[diagram.edge]]
from = "user"
to = "record"
label = "under the box lock"
ref = "internal/netpolicy.WriteExceptions"

[[diagram.edge]]
from = "record"
to = "force"
label = "applied"
ref = "internal/cli.app.applyExceptions"

[[diagram.edge]]
from = "record"
to = "gone"
label = "cannot be applied"
ref = "internal/cli.app.netAllow"

[[diagram.edge]]
from = "force"
to = "down"
label = "box stops"

[[diagram.edge]]
from = "down"
to = "force"
label = "next start"
ref = "internal/box.NetExceptionHook.AfterStart"

[[diagram.edge]]
from = "force"
to = "ended"
label = "until passes, net revoke"
ref = "internal/cli.app.netRevoke"

[[diagram.edge]]
from = "ended"
to = "gone"
label = "next write of the record"
ref = "internal/netpolicy.WriteExceptions"

[[diagram.edge]]
from = "down"
to = "gone"
label = "devc rm"
ref = "internal/box.Manager.Remove"
+++
## What it is for

At level allowlist a box may reach a short list of names (its package index, its model's API) and nothing else, enforced by the host, not in the box. Separately, at any level the user sometimes wants to open one more destination for a while: a database on the LAN, a service on the host, one more name. Those exceptions must come only from the host, end by themselves, and be visible. [[concern:network-levels]] explains the levels themselves.

## How it works

**The path of a connection** ([[flow:allowlist-connect]]). A box of level allowlist has only loopback and the proxy variables `HTTP_PROXY`/`HTTPS_PROXY` = `http://127.0.0.1:3128` (plus `NO_PROXY` for loopback and `NODE_USE_ENV_PROXY=1`). Box init has the [[term:box-agent]] listen there; [[internal/boxside.Agent.relayTCP]] relays each connection unparsed on a new `host.proxy` stream of the box channel ([[concern:box-channel]]). The agent's own limits are in the flow. The daemon gives the stream one of 64 relay places per box (else `503`) and serves it with a [[internal/netpolicy.Proxy]] built for that box ([[internal/daemon.boxWorker.serveProxy]]).

**What a request is checked against.** For every request [[internal/daemon.Daemon.proxyPolicy]] computes the policy anew: the project's configuration must be free of problems; the capability record must belong to the running container; both the record and the configuration must say level allowlist; the configured entries are the realm's and project's `domains` and `presets` that were also recorded at the start ([[internal/box.Capabilities.ProxyAllowlist]]); and the names of exceptions in force are added. An entry is a name or `*.suffix` (below the suffix, not the suffix itself; at least two labels after `*.`) with an optional port; without one it allows 80 and 443 ([[internal/netpolicy.ParseEntry]]). The presets are short explicit lists in [[internal/netpolicy.Presets]].

**The checks, in order** ([[internal/netpolicy.Proxy.Serve]], [[internal/netpolicy.Proxy.check]]):

1. A request head within 30 seconds and 64 KiB (`431`), well-formed (`400`).
2. `CONNECT host:port`, or a plain request with an absolute `http://` URL; anything else `400`.
3. The host must be a name: [[internal/netpolicy.NormalizeHost]] refuses IP literals in any form, also a numeric last label that inet_aton would read as an address (`403`).
4. The policy must be readable (`403`) and allow the name and port (`403`, with the hint `devc request net <name>:<port>`).
5. The daemon resolves the name itself, IPv4 only, within ten seconds (`502`).
6. [[internal/netpolicy.Proxy.CheckAddr]] drops every address that is not IPv4, lies in the private ranges or is one of the host's own; none left is `403` (`502` when the name has no IPv4 address).
7. Up to eight addresses are dialled in turn, ten seconds each; the dialer's control function checks the socket's address again, so an answer that changed between check and connect does not count ([[internal/netpolicy.Proxy.dial]]).
8. [[internal/netpolicy.Proxy.watch]] checks the policy once more, now that the connection exists, before the tunnel is acknowledged or a byte is forwarded ([[finding:E1-12]]).

A tunnel is then relayed unchanged; it is closed after ten idle minutes, and once one direction has ended, after one idle minute of the other. A plain connection may carry several requests: each is checked again, an upstream connection is reused only for the same host and port, hop-by-hop headers and `Proxy-Authorization` are dropped both ways, the proxy answers `Expect: 100-continue` itself, and a response head must arrive within five minutes and 64 KiB ([[finding:D1-02]], [[finding:E1-15]]).

**Watching open connections.** Every tunnel and every upstream connection serving a request stays registered in the worker's [[internal/netpolicy.Tunnels]]. Its watch checks the policy every five seconds, when the earliest exception in force ends, and whenever [[internal/daemon.boxWorker.syncNet]] asks (every five seconds and on `net.sync`); a destination no longer allowed, or a policy that cannot be read, closes it ([[finding:D1-06]]).

**The log.** Every decision of the proxy, refusals and closings included, goes to `durable/realms/<realm>/netlog/<project>.jsonl` (0600), with box-supplied text reduced to printable characters and 255 bytes; past 4 MiB the file moves to `.1` ([[internal/netpolicy.NetLog.Append]]). `devc net log` reads both. A stream refused for want of a relay place is not logged.

**Exceptions** ([[term:network-exception]], [[flow:net-allow]]). [[internal/cli.app.netAllow]] parses `<address-or-name>:<port>`, `lan` or `host:<port>` and a duration of one second to 30 days (default two hours) or `forever`, which it warns about. It checks the kind against the running box's level from its capability record, else the configured one: `lan` only at internet; an address or name at internet, the name resolved now on the host to at most 16 IPv4 addresses; only names at allowlist; neither at none or open; `host:<port>` at every level, with a running daemon. Under the box lock it writes `local/boxes/<realm>/<project>/net-exceptions.json` (0600, by rename, at most 256, expired ones dropped) and applies the record:

- **at internet**, [[internal/box.Manager.SyncExceptions]] verifies the binding and the table, then [[internal/netpolicy.ExceptionScript]] flushes `allow4` and `lan4` and adds every element in force with its remaining seconds, in one transaction. An `allow4` element opens TCP and UDP to that address and port; `lan` adds 10/8, 172.16/12, 192.168/16 and 100.64/10, never link-local;
- **at allowlist**, nothing is installed: the proxy reads the record for every request; `net.sync` only has open tunnels checked now;
- **`host:<port>`**: the daemon sends the box agent the list of host ports (`box.listen`); a connection to `127.0.0.1:<port>` in the box becomes a `host.dial` stream, which [[internal/daemon.boxWorker.serveDial]] connects to the host's loopback only while an exception allows that port; relays of a port no longer allowed are ended within five seconds.

An exception that cannot be applied is removed from the record again. Kernel elements vanish with the namespace when the box stops; the start hook puts back those still valid after the canary ([[internal/box.NetExceptionHook]]). `devc rm` drops the record. [[diagram:exception-life]]

## What can go wrong, and what prevents it

The box controls its agent, every byte of a proxy request and the names it asks for.

- **Reaching the LAN or the host through a name.** Addresses are checked after resolution and on the connected socket. The host's own addresses are listed again every ten seconds, a failed listing keeping the last; while none has succeeded, the host's public addresses are not refused ([[internal/daemon.hostAddrCache.contains]]; [[doc:docs/dev/LEFTOVER.md#7]]).
- **Granting itself access.** The box channel has no method that writes an exception; `devc request net` creates a high-risk request, which only the CLI can approve; the daemon then runs `devc net allow --request <id>` ([[concern:requests-and-approval]]).
- **Keeping a connection beyond its permission.** The watch closes it.
- **Flooding.** Relay places, the agent's own limits and the channel's frame inspector; the log rotates.

Limits: the allowlist limits names, not content; CONNECT relays TLS unchanged, so domain fronting cannot be detected; IPv6-only upstreams are unreachable; tools that ignore the proxy variables have no network. At level internet an address exception comes before the rule rejecting the host's addresses, so `devc net allow <host's LAN address>:<port>` opens that host port.

## Decisions that are not obvious

- **The allowlist is a start capability, exceptions are not.** Growing the configured list waits until the capability record is written again (the box's next start, or devc finishing the start of a box that removed its readiness marker); removing an entry applies at once ([[finding:C1-02]]).
- **Names only at allowlist.** The proxy never connects to a private address, so a LAN host is reached at internet with an address exception, or through `host:<port>`.

## Where to start reading

1. [[internal/netpolicy.Proxy.check]] and [[internal/netpolicy.Proxy.watch]]
2. [[internal/daemon.Daemon.proxyPolicy]]
3. [[internal/boxside.Agent.relayTCP]] and [[internal/daemon.boxWorker.serveProxy]]
4. [[internal/cli.app.netAllow]] and [[internal/netpolicy.ExceptionScript]]
5. [[internal/daemon.boxWorker.syncNet]] and [[internal/daemon.boxWorker.serveDial]]
