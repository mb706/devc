+++
id = "network-levels"
title = "Network levels"
summary = "What each of the four levels lets a box reach and how the host enforces it: the engine arguments, the nftables rules of level internet installed between `podman init` and `podman start`, the binding of readiness to that start, trusted resolvers, the sentinel and the canary."
order = 50
files = [
  "internal/netpolicy/netpolicy.go",
  "internal/netpolicy/rules.go",
  "internal/netpolicy/install.go",
  "internal/netpolicy/binding.go",
  "internal/netpolicy/resolvers.go",
  "internal/netpolicy/canary.go",
  "internal/boxside/net.go",
]
key = [
  "internal/netpolicy.BoxArgs",
  "internal/netpolicy.Rules",
  "internal/netpolicy.Gather",
  "internal/netpolicy.TrustedResolvers",
  "internal/netpolicy.Installer.Install",
  "internal/netpolicy.Installer.Bind",
  "internal/netpolicy.CheckBinding",
  "internal/netpolicy.CheckSentinel",
  "internal/box.Manager.startBox",
  "internal/box.Manager.verifyRunning",
  "internal/box.Manager.checkNetwork",
  "internal/boxside.NetworkCheck",
]
spec = ["doc:specs-v2.md#11"]
tests = [
  "internal/netpolicy.TestBoxArgs",
  "internal/netpolicy.TestRules",
  "internal/netpolicy.TestInstall",
  "internal/netpolicy.TestGather",
  "internal/netpolicy.TestTrustedResolvers",
  "internal/netpolicy.TestBinding",
  "internal/netpolicy.TestBind",
  "internal/netpolicy.TestSentinel",
  "internal/netpolicy.TestCanary",
  "internal/box.TestInternetLevel",
  "internal/boxside.TestInitNetworkCheck",
  "internal/boxside.TestNetworkCheckSelection",
  "test/integration.TestNetworkInternet",
  "test/integration.TestNetworkPlainStart",
  "test/integration.TestNetworkResolvers",
  "test/integration.TestNetworkLevels",
  "test/integration.TestEscape",
]
related = ["allowlist-proxy", "forwards-and-local-urls", "box-lifecycle", "isolation", "box-init-and-agent"]

[[invariant]]
text = "No process of a box of level internet runs before devc's table is in the box's network namespace and has been verified from the host."
refs = ["internal/box.Manager.startBox", "internal/netpolicy.Installer.Install", "internal/netpolicy.Installer.Verify"]
tests = ["internal/box.TestInternetLevel", "test/integration.TestNetworkInternet"]

[[invariant]]
text = "A box of level internet reports ready only in the start whose network namespace, PID 1 start time and boot the host recorded after installing the rules."
refs = ["internal/boxside.NetworkCheck", "internal/netpolicy.CheckBinding", "internal/netpolicy.Installer.Bind"]
tests = ["internal/boxside.TestInitNetworkCheck", "internal/netpolicy.TestBinding", "test/integration.TestNetworkPlainStart"]

[[invariant]]
text = "devc uses a running box of level internet only after checking from the host that its binding matches and its table is there; otherwise it stops and starts it."
refs = ["internal/box.Manager.verifyRunning", "internal/box.Manager.up", "internal/box.Manager.ensureRunning"]
tests = ["internal/box.TestInternetLevel", "test/integration.TestNetworkPlainStart"]

[[invariant]]
text = "A start of level internet fails when a connection from the box to one of the host's addresses gets through, by the box's report or by the host listener's count."
refs = ["internal/box.Manager.checkNetwork", "internal/netpolicy.EvaluateCanary"]
tests = ["internal/netpolicy.TestCanary", "internal/box.TestInternetLevel"]

[[invariant]]
text = "Port 53 is open only towards resolvers the host computed from its own files and the engine's inspect data, never towards one only the box's resolv.conf names."
refs = ["internal/netpolicy.TrustedResolvers", "internal/netpolicy.Gather"]
tests = ["internal/netpolicy.TestTrustedResolvers", "internal/netpolicy.TestGather", "test/integration.TestNetworkResolvers"]

[[invariant]]
text = "A box of level allowlist or none has no network interface but loopback, and an unknown level creates no box."
refs = ["internal/netpolicy.BoxArgs", "internal/box.BuildSpec"]
tests = ["internal/netpolicy.TestBoxArgs", "test/integration.TestNetworkLevels"]

[[diagram]]
id = "enforcement"
title = "Who puts the rules of level internet in place, and who checks them"
kind = "graph"

[[diagram.node]]
id = "up"
label = "devc up (host)"
col = 1
row = 2
zone = "host"
ref = "internal/box.Manager.startBox"

[[diagram.node]]
id = "nft"
label = "podman unshare nsenter nft"
col = 2
row = 1
zone = "host"
ref = "internal/netpolicy.Installer.Install"

[[diagram.node]]
id = "table"
label = "table inet devc in the box's namespace"
col = 4
row = 1
zone = "box"
ref = "internal/netpolicy.Rules"

[[diagram.node]]
id = "binding"
label = "etc/netns.json (read-only in the box)"
col = 2
row = 3
zone = "host"
ref = "internal/netpolicy.Binding"

[[diagram.node]]
id = "init"
label = "box init"
col = 4
row = 3
zone = "box"
ref = "internal/boxside.NetworkCheck"

[[diagram.node]]
id = "listener"
label = "canary listener on the host's addresses"
col = 1
row = 4
zone = "host"
ref = "internal/netpolicy.OpenCanary"

[[diagram.node]]
id = "canary"
label = "devc box net-canary"
col = 4
row = 4
zone = "box"
ref = "internal/boxside.NetCanary"

[[diagram.edge]]
from = "up"
to = "nft"
label = "after podman init"
ref = "internal/box.Manager.startBox"

[[diagram.edge]]
from = "nft"
to = "table"
label = "rules on stdin, then list"
ref = "internal/netpolicy.Installer.Verify"

[[diagram.edge]]
from = "up"
to = "binding"
label = "Bind, then podman start"
ref = "internal/netpolicy.WriteBinding"

[[diagram.edge]]
from = "init"
to = "binding"
label = "must match /proc"
ref = "internal/netpolicy.CheckBinding"

[[diagram.edge]]
from = "init"
to = "table"
label = "192.0.2.1:9 must reset"
ref = "internal/netpolicy.CheckSentinel"

[[diagram.edge]]
from = "up"
to = "canary"
label = "podman exec, once ready"
ref = "internal/box.Manager.checkNetwork"

[[diagram.edge]]
from = "canary"
to = "listener"
label = "must not connect"
ref = "internal/netpolicy.EvaluateCanary"
+++
## What it is for

The owner wants an agent to download packages and call APIs without reaching the machines of the home or office network, the host's services or a cloud metadata address. A box's level is its project's `network.level`, else the realm's `network.default`, `internet` unless set ([[config:project.network.level]], [[config:realm.network.default]]); [[internal/netpolicy.BoxArgs]] maps it to engine arguments. The rules must be in place before anything in the box runs, depend on nothing the box can write, and not vanish silently when something other than devc restarts the container. [[concern:allowlist-proxy]] covers the proxy and the exceptions.

## How it works

**The four levels.**

| Level | What `podman create` gets | What the box reaches |
|---|---|---|
| `none` | `--network none` | nothing; only host exceptions (`host:<port>`) through the agent |
| `allowlist` | `--network none` and the proxy variables pointing at `127.0.0.1:3128` | listed names, through the daemon's proxy |
| `internet` (default) | the engine's network, two sysctls switching IPv6 off, rules installed by the host | public IPv4 addresses; no IPv6, LAN or host |
| `open` | the engine's network | whatever the engine allows; devc adds nothing |

At `none` and `allowlist` a box has no interface but loopback: without the host it has no network at all.

**The rules.** [[internal/netpolicy.Rules]] renders one script, applied with `nft -f -` as one transaction; its structure:

```
table inet devc
delete table inet devc
table inet devc {
  set allow4 { type ipv4_addr . inet_service; flags timeout }
  set lan4   { type ipv4_addr; flags interval, timeout }
  chain output {
    type filter hook output priority filter; policy accept;
    oifname "lo" accept
    meta nfproto ipv6 drop
    meta l4proto { tcp, udp } ip daddr . th dport @allow4 accept
    ip daddr { <trusted resolvers> } meta l4proto { tcp, udp } th dport 53 accept
    ip daddr 192.0.2.1 tcp dport 9 reject with tcp reset
    ip daddr { <host's addresses and aliases> } reject with icmp type admin-prohibited
    ip daddr @lan4 accept
    ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16,
               172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 240.0.0.0/4 } reject with icmp type admin-prohibited
  }
}
```

The first two lines make a second application replace the first. An nftables set is a kernel table that one rule matches against; its elements may carry a timeout after which the kernel drops them, which is how exceptions expire without a process. There is no rule for established connections, so every outgoing packet passes the chain. Only outgoing traffic is filtered.

**The start sequence** ([[internal/box.Manager.startBox]], [[flow:network-install]]). `podman init` creates the container's namespaces and runs nothing of the box; a network namespace is the kernel's private copy of interfaces, routes and firewall tables. The host reads the namespace's path and the box's resolv.conf and hosts file, which the box could rewrite once it runs ([[internal/netpolicy.Gather]]). [[internal/netpolicy.Installer.Install]] runs `podman unshare nsenter --net=<namespace> nft -f -`: `podman unshare` enters the engine's user namespace, in which the user is root and below which the box's namespaces were made, and `nsenter` enters the box's network namespace. Verify lists the table and requires the set `allow4`, the chain `output` and the sentinel address. Then the binding is written, and only then comes `podman start`. A failure removes a new container or stops an existing one.

**Trusted resolvers.** [[internal/netpolicy.TrustedResolvers]] takes the host's IPv4 nameservers, systemd-resolved's upstream servers when `/etc/resolv.conf` names a loopback stub, and the network backend's built-in resolver from the inspect data. A nameserver only the box's resolv.conf names gets no exception, with a warning. [[internal/netpolicy.BackendHostAddrs]] adds the backend's ways to the host (slirp4netns's gateway, pasta's mapped address, named networks' gateways) to the rejected host addresses.

**The binding** ([[term:network-binding]]). [[internal/netpolicy.Installer.Bind]] records the namespace's inode (read with `podman unshare stat -L`), the start time of the box's main process from `/proc/<pid>/stat` and the kernel's boot ID in `etc/netns.json`, which the box sees read-only as `/etc/devc/netns.json`. Where the host can read `/proc/<pid>/ns/net`, it must be that namespace. Every start through devc removes the old record first.

**Box init's check** ([[internal/boxside.NetworkCheck]], [[concern:box-init-and-agent]]). At level internet, or when `box.json` cannot be read, box init requires the record to equal its own namespace inode, PID 1's start time and the boot ID ([[internal/netpolicy.CheckBinding]]), then requires TCP to the sentinel `192.0.2.1:9` to be refused within five seconds ([[internal/netpolicy.CheckSentinel]]). Without the rules a connection to that TEST-NET-1 address times out or is unreachable, never refused. A failure ends init and the box.

**The canary** ([[term:canary]]). Once the box is ready, [[internal/box.Manager.checkNetwork]] opens a listener on `0.0.0.0` and runs `devc box net-canary` in the box against each of the host's IPv4 addresses and host aliases. The start fails if the box reports a connection, if the listener accepted one, or if the report is malformed or leaves an address out ([[internal/netpolicy.EvaluateCanary]]). Then the capability record is written and the exceptions still valid are applied.

**A running box.** `up`, `sh`, `run`, `root`, sessions and forwards first check a running box of level internet from the host: the record must equal what Bind computes now and the table must verify ([[internal/box.Manager.verifyRunning]]); otherwise the box is stopped and started again.

[[diagram:enforcement]]

## What can go wrong, and what prevents it

A box controls its processes, its resolv.conf and hosts file once running, and everything in its writable mounts; it has no capability, so it cannot list or change the table ([[concern:isolation]]).

- **A start without rules.** A plain `podman start` (by the user, or a tool) gets a fresh namespace and main process; box init finds the binding wrong and the box stops. Namespace inodes are reused across restarts on the development host, which is why the record holds the start time and boot ID too ([[finding:B1]]).
- **A planted resolver.** Only resolvers the host computed itself get port 53 ([[finding:B13]]).
- **The host behind another address.** Its own addresses, the hosts file's alias read before start and the backend's gateways are rejected before the LAN exception set, so even `lan` does not open the host ([[finding:D1-21]]).
- **Rules that do not work.** The canary tests from outside; hiding a connection in the report does not help, since the host counts connections too.

What the level does not cover: DNS to the trusted resolvers is a channel out (queries can carry data); everything public, TCP, UDP and ICMP alike, is allowed; the canary tries one TCP port per address; Verify looks for the table's set, chain and sentinel, not for every rule; a trusted resolver on the LAN (a router named in the host's resolv.conf) is reachable on port 53 ([[doc:docs/dev/LEFTOVER.md#4]], [[doc:docs/dev/LEFTOVER.md#5]]).

## Decisions that are not obvious

- **Rules from outside, before start.** Nothing in the box holds a capability that could change them, and no box process exists yet.
- **Readiness bound to the start, not to a file the box writes.** Any process in the box can write the readiness marker, so it proves nothing by itself; for level internet the host verifies the binding and the table itself.
- **IPv6 off twice, and reject rather than drop.** The sysctls leave no IPv6 address and the rule drops IPv6 anyway; forbidden IPv4 connections fail at once instead of hanging.

## Where to start reading

1. [[internal/netpolicy.BoxArgs]] and [[internal/netpolicy.Rules]]
2. [[internal/box.Manager.startBox]]
3. [[internal/netpolicy.Installer.Bind]] and [[internal/netpolicy.CheckBinding]]
4. [[internal/box.Manager.checkNetwork]]
5. [[internal/box.Manager.verifyRunning]]
