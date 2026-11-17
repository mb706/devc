+++
id = "forwards-and-local-urls"
title = "Forwards and local URLs"
summary = "How the host reaches a server in a box: `devc forward` and the daemon's router for `http://<port>.<project>.<realm>.localhost:7480`, both dialling inside the box's pinned network namespace with the host's own devc binary, and what the router refuses."
order = 52
files = [
  "internal/netpolicy/forward.go",
  "internal/netpolicy/registry.go",
  "internal/daemon/router.go",
  "internal/cli/netcmds.go",
  "internal/boxside/net.go",
]
key = [
  "internal/netpolicy.Pin",
  "internal/netpolicy.Forwarder",
  "internal/netpolicy.Forwarder.Serve",
  "internal/netpolicy.Forwarder.Dial",
  "internal/boxside.Dial",
  "internal/netpolicy.Registry",
  "internal/netpolicy.ProcessAlive",
  "internal/cli.app.forward",
  "internal/daemon.router.ServeHTTP",
  "internal/daemon.crossOrigin",
  "internal/daemon.ParseLocalHost",
  "internal/daemon.router.pin",
  "internal/netpolicy.Admission",
]
spec = ["doc:specs-v2.md#11"]
tests = [
  "internal/netpolicy.TestPin",
  "internal/netpolicy.TestForwarder",
  "internal/netpolicy.TestForwarderDial",
  "internal/netpolicy.TestForwarderAdmission",
  "internal/boxside.TestForwardClientGone",
  "internal/netpolicy.TestListen",
  "internal/netpolicy.TestRegistry",
  "internal/netpolicy.TestProcessAlive",
  "internal/daemon.TestParseLocalHost",
  "internal/daemon.TestRouterRefusals",
  "test/integration.TestForward",
  "test/integration.TestLocalURLs",
]
related = ["network-levels", "allowlist-proxy", "daemon", "box-lifecycle"]

[[invariant]]
text = "A forward's connections only ever enter the namespaces of the box generation it pinned; when the box stops, is removed or restarts, the forward ends."
refs = ["internal/netpolicy.Pin", "internal/netpolicy.Forwarder.watch", "internal/netpolicy.Pinned.Same"]
tests = ["internal/netpolicy.TestPin", "internal/netpolicy.TestForwarder"]

[[invariant]]
text = "The dial process runs the host's devc binary in the box's user and network namespaces only, never in its mount namespace, so nothing from the box is executed."
refs = ["internal/netpolicy.Forwarder.DialArgs", "internal/boxside.Dial"]
tests = ["internal/netpolicy.TestForwarder", "internal/netpolicy.TestForwarderDial"]

[[invariant]]
text = "A forward runs at most 64 dial processes at once and the router at most 256 across every box; a connection beyond that is closed at once, without a dial process."
refs = ["internal/netpolicy.Admission.Acquire", "internal/netpolicy.Forwarder.Serve", "internal/netpolicy.Forwarder.Dial", "internal/daemon.router.dial"]
tests = ["internal/netpolicy.TestForwarderAdmission"]

[[invariant]]
text = "A forwarded connection's dial process ends at most 10 s after its client's side ended, whatever the box's side does, and gives its place back; a client that stays connected keeps its connection."
refs = ["internal/netpolicy.Forwarder.relay", "internal/netpolicy.Forwarder.endAfterClient", "internal/netpolicy.clientGone"]
tests = ["internal/boxside.TestForwardClientGone"]

[[invariant]]
text = "The router routes only to a port listed in the project's box.ports or forwarded now, of the running box of a configured project."
refs = ["internal/daemon.router.ServeHTTP", "internal/daemon.router.forwarded", "internal/daemon.match"]
tests = ["internal/daemon.TestRouterRefusals", "test/integration.TestLocalURLs"]

[[invariant]]
text = "Through the router, a WebSocket upgrade or a method other than GET and HEAD needs no Origin or its own; a cross-site request passes only as a top-level GET navigation."
refs = ["internal/daemon.crossOrigin"]
tests = ["internal/daemon.TestRouterRefusals", "test/integration.TestLocalURLs"]

[[invariant]]
text = "The configuration accepts `router.listen` only as a loopback address with a port, or `off`."
refs = ["internal/config.validateGlobal", "internal/daemon.Daemon.startRouter"]

[[invariant]]
text = "A forward record counts as live only while its process exists and is that forward: by the ID in its command line, or by its recorded start time."
refs = ["internal/netpolicy.ProcessAlive", "internal/netpolicy.Registry.List"]
tests = ["internal/netpolicy.TestProcessAlive", "internal/netpolicy.TestRegistry"]
+++
## What it is for

An agent starts a web server, a notebook or a database in its box and the owner wants to look at it from the host, whatever the box's network level, including none, and with nothing extra in the image. Servers often listen only on the box's loopback interface. The way in must not let the box run anything on the host, and must not let one box's web page talk to another box's server through the owner's browser.

## How it works

**Entering the box's network.** Both ways in end in the same dial process: the host's own devc binary, started as

```
nsenter --user=/proc/self/fd/3 --net=/proc/self/fd/4 --preserve-credentials <devc> box dial 127.0.0.1:<port>
```

with the connection as standard input and output, the calling process's environment with `DEVC_BOX=1` added (so it loads no host configuration) and `/` as working directory ([[internal/netpolicy.Forwarder.DialArgs]], [[internal/execx.Real]]). `nsenter` enters the namespaces given as open descriptors 3 and 4: the box's user namespace (so the process is the box user and holds no capability once devc runs) and its network namespace, where `127.0.0.1` is the box's loopback. It stays in the host's mount namespace, so it runs the host's binary and reads nothing of the box. [[internal/boxside.Dial]] connects within ten seconds and relays both ways, passing half-closes on. The box's side may never close, so for `devc forward` the host watches the socket it handed over (without reading it): once the client's side has ended, the dial process gets 10 s ([[internal/netpolicy.ClientGoneGrace]]), then is killed and its place freed ([[internal/netpolicy.Forwarder.endAfterClient]]). At level internet the connection passes the box's rules on the loopback interface, which they accept.

**Pinning** ([[internal/netpolicy.Pin]]). The descriptors are opened once, from `/proc/<pid>/ns/user` and `/proc/<pid>/ns/net` of the box's main process. Between two engine reports of the same PID and start time, and two reads of that PID's start time in `/proc`, they are proven to belong to this box, not to a process that reused its PID ([[finding:B5]]). Every connection uses the pinned descriptors, so it reaches this box generation or nothing.

**`devc forward`** ([[internal/cli.app.forward]]). It starts the box if needed (as `sh` does), then listens on `--bind` (default `127.0.0.1`) at the box port's number, or a free one when that is taken and `--host-port` was not given ([[internal/netpolicy.Listen]]); another address than loopback is allowed with a warning. [[internal/netpolicy.Forwarder.Serve]] hands every accepted socket to a dial process, at most 64 at once ([[internal/netpolicy.Admission]]): a connection beyond that is closed at once, and the refusals are logged at most once a minute with their number. It polls the engine every two seconds: a missing container, another PID or start time ends the forward. In the foreground it runs until Ctrl-C; with `--detach` it passes the listener to `devc forward serve --id <id>` as descriptor 3, in a session of its own, logging to `<runtime>/forwards/<id>.log`; that process pins the box again itself. Both kinds leave a record in `<runtime>/forwards/<id>.json` ([[internal/netpolicy.Registry]]), which no box can reach; `devc forward ls` and `stop` work from the records, and [[internal/netpolicy.ProcessAlive]] counts a record as live only while its process is that forward.

**The router** ([[term:local-url]], [[flow:local-url]]). The daemon listens on `router.listen` ([[config:config.router.listen]], default `127.0.0.1:7480`; validation accepts only a loopback address with a port, or `off`), read once at start. For each request [[internal/daemon.router.ServeHTTP]]:

1. parses the Host as `<port>.<project>.<realm>.localhost` with an optional port ([[internal/daemon.ParseLocalHost]]); realm names have no dots, so the realm is the last label (`404` otherwise);
2. refuses a WebSocket upgrade or a method other than GET and HEAD from another origin, and a cross-site request that is not a top-level GET navigation ([[internal/daemon.crossOrigin]], `403`; below);
3. reloads the configuration if it changed and requires a configured project without problems (`404`);
4. requires the port in the project's `box.ports` ([[config:project.box.ports]]) or a live forward of that port (`404` with both ways to allow it);
5. requires the daemon's box table to show the box running (`502`) and one of 128 places (`503`);
6. proxies with `httputil.ReverseProxy`: the box sees the local URL as Host and `X-Forwarded-*`; WebSocket upgrades pass. The transport dials through [[internal/daemon.router.pin]], which pins the box once per container ID and start time, and [[internal/netpolicy.Forwarder.Dial]], which gives the dial process one end of a socket pair. Nothing answering is `502`. The router's dial processes, idle ones of the transport's pool included, share one limit of 256; a dial beyond it starts no process and the request gets `503`.

## What can go wrong, and what prevents it

- **A box executing something on the host.** The dial process is the host's binary in the box's network namespace; it reads nothing from the box and runs nothing it names.
- **A recycled PID.** Pinning proves the namespaces belong to the box; a restart ends a forward, and the router drops its pin when the daemon sees the box stop.
- **A page of one box driving another box's server.** Every local URL is an origin of its own, but a WebSocket is not bound by the same-origin policy and a form POST needs no CORS. So an upgrade or a state-changing method must carry no `Origin` or exactly its own, and a request Fetch Metadata marks as cross-site passes only as a top-level GET navigation ([[finding:D1-01]]). Programs that send neither header pass, and so does a plain GET or HEAD from another origin that Fetch Metadata calls same-site: the router checks no `Origin` on those.
- **DNS rebinding.** Routing only by a Host below `.localhost` means a page that rebinds its own name to `127.0.0.1` matches no route.
- **A planted forward record.** Records live in the runtime directory outside every box's mount, are decoded strictly, must name the project's box, and count only while their process is the forward.

Limits: the router has no authentication, so any process on the host that can connect to its loopback address reaches the routed ports, and so does anyone who can reach a forward's `--bind` address, up to the connection limits. Local URLs share the parent name `localhost`, so one box's page can set cookies for another's. A change of `router.listen` needs a daemon restart ([[doc:docs/dev/LEFTOVER.md#5]]).

## Decisions that are not obvious

- **The host's binary in the box's namespaces**, not a relay inside the box: nothing is needed in the image, it works at every level, and the box can neither replace nor observe the dialer's code.
- **Forwards are CLI processes.** The daemon routes them but does not own them; a detached forward is devc itself in the background, found again by its record.
- **Routes only for listed or forwarded ports.** A box cannot open a route by listening on a port; the owner lists it or forwards it.

## Where to start reading

1. [[internal/netpolicy.Pin]] and [[internal/netpolicy.Forwarder.DialArgs]]
2. [[internal/boxside.Dial]]
3. [[internal/cli.app.forward]] and [[internal/netpolicy.Forwarder.Serve]]
4. [[internal/daemon.router.ServeHTTP]] and [[internal/daemon.crossOrigin]]
