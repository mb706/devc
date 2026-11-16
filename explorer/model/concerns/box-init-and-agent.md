+++
id = "box-init-and-agent"
title = "Box init and the agent"
summary = "The first process in a box: what `devc box init` does before the box counts as ready, the agent it then runs on two sockets, what the agent serves the host, and how programs in the box reach the host through it."
order = 22
files = [
  "internal/boxside/boxside.go",
  "internal/boxside/init.go",
  "internal/boxside/agent.go",
  "internal/boxside/agentclient.go",
  "internal/boxside/extension.go",
]
key = [
  "internal/boxside.Init",
  "internal/boxside.PrepareHome",
  "internal/boxside.writeReady",
  "internal/boxside.RunAgent",
  "internal/boxside.HostPeer",
  "internal/boxside.Agent.acceptHost",
  "internal/boxside.Agent.serveHost",
  "internal/boxside.Agent.relay",
  "internal/boxside.AgentCall",
  "internal/boxside.SendHint",
  "internal/boxside.Agent.serveExtension",
]
spec = ["doc:specs-v2.md#12.5", "doc:specs-v2.md#6.1"]
tests = [
  "internal/boxside.TestInitWritesReadyAndWaits",
  "internal/boxside.TestInitStartsAgentAfterReady",
  "internal/boxside.TestInitNetworkCheck",
  "internal/boxside.TestPrepareHome",
  "internal/boxside.TestHostPeer",
  "internal/boxside.TestAgentWithoutHost",
  "internal/boxside.TestAgentListenReplacesStaleEntries",
  "internal/boxside.TestCappedLog",
  "internal/boxside.TestExtensionRegistration",
  "test/integration.TestNetworkPlainStart",
  "test/integration.TestDaemon",
]
related = ["box-lifecycle", "isolation", "sessions-and-events", "launching-a-harness"]

[[invariant]]
text = "A box at level internet never reports ready unless the binding record names its own network namespace and start."
refs = ["internal/boxside.Init", "internal/boxside.NetworkCheck", "internal/netpolicy.CheckBinding"]
tests = ["internal/boxside.TestInitNetworkCheck", "internal/boxside.TestNetworkCheckSelection", "test/integration.TestNetworkPlainStart"]

[[invariant]]
text = "The readiness marker a start through devc waits for was written by the box init of that start."
refs = ["internal/box.Manager.clearReady", "internal/boxside.Init", "internal/boxside.writeReady"]
tests = ["internal/boxside.TestInitWritesReadyAndWaits"]

[[invariant]]
text = "No process inside a box is accepted as the host on the agent's socket."
refs = ["internal/boxside.HostPeer", "internal/boxside.Agent.acceptHost"]
tests = ["internal/boxside.TestHostPeer"]

[[invariant]]
text = "Nothing the agent does can end box init: panics are logged and the log never fails."
refs = ["internal/boxside.Agent.goSafe", "internal/boxside.Init", "internal/boxside.cappedLog.Write"]
tests = ["internal/boxside.TestInitStartsAgentAfterReady", "internal/boxside.TestCappedLog"]

[[invariant]]
text = "Without a connected host, a request from inside the box fails at once instead of waiting."
refs = ["internal/boxside.Agent.relay"]
tests = ["internal/boxside.TestAgentWithoutHost"]

[[invariant]]
text = "A harness extension receives a session's messages only if its process was started for that session."
refs = ["internal/boxside.Agent.sessionOf", "internal/boxside.Agent.serveExtension"]
tests = ["internal/boxside.TestExtensionRegistration"]

[[diagram]]
id = "processes"
title = "Processes in a running box, and who starts them"
kind = "graph"

[[diagram.node]]
id = "pid1"
label = "Podman's init (PID 1)"
col = 1
row = 1
zone = "box"

[[diagram.node]]
id = "init"
label = "devc box init and the agent"
col = 2
row = 1
zone = "box"
ref = "internal/boxside.Init"

[[diagram.node]]
id = "exec"
label = "podman exec from the host"
col = 1
row = 2
zone = "host"

[[diagram.node]]
id = "start"
label = "devc box session start"
col = 2
row = 2
zone = "box"
ref = "internal/boxside.StartSession"

[[diagram.node]]
id = "tmux"
label = "tmux server"
col = 3
row = 2
zone = "box"
ref = "internal/tmux.NewSession"

[[diagram.node]]
id = "launch"
label = "devc box launch"
col = 4
row = 2
zone = "box"
ref = "internal/boxside.RunLaunch"

[[diagram.node]]
id = "pty"
label = "devc box pty"
col = 4
row = 3
zone = "box"
ref = "internal/boxside.RunPTY"

[[diagram.node]]
id = "harness"
label = "harness"
col = 3
row = 3
zone = "box"

[[diagram.node]]
id = "hook"
label = "devc box hook"
col = 2
row = 3
zone = "box"
ref = "internal/boxside.RunHook"

[[diagram.edge]]
from = "pid1"
to = "init"
label = "runs the entrypoint"

[[diagram.edge]]
from = "exec"
to = "start"
label = "runs"
ref = "internal/cli.app.startSession"

[[diagram.edge]]
from = "start"
to = "tmux"
label = "new window"
ref = "internal/tmux.NewWindow"

[[diagram.edge]]
from = "tmux"
to = "launch"
label = "window command"

[[diagram.edge]]
from = "launch"
to = "pty"
label = "execs (OSC 7501)"
ref = "internal/boxside.PrepareLaunch"

[[diagram.edge]]
from = "pty"
to = "harness"
label = "starts on a new pty"
ref = "internal/ptywrap.Run"

[[diagram.edge]]
from = "harness"
to = "hook"
label = "runs per hook"

[[diagram.edge]]
from = "init"
to = "tmux"
label = "paste, keys"
ref = "internal/boxside.Input.Paste"
+++
## What it is for

Every box needs a main process that is devc's, whatever the image declares. It turns a fresh home into a usable one, refuses to come up at level internet unless the host's rules are bound to this start, tells the host when the box is ready, and lives as long as the box. The same process hosts the box's end of the channel to the daemon, the [[term:box-agent]]: the daemon calls into the box through it (live sessions, input to panes), and programs in the box (hooks, the launcher, git's remote helper, `devc request`) reach the host through it. Since the box listens and the host connects, no box ever holds a socket that could control the daemon.

## How it works

**Box init.** `devc box init` is the container's entrypoint ([[concern:isolation]]) and runs as `dev`. The command ([[internal/cli.newBoxCmd]]) refuses to run outside a box ([[internal/cli.requireBox]]: `DEVC_BOX=1` and `/etc/devc/box.json`), cancels its context on SIGTERM or SIGINT and ignores SIGHUP. It reads `box.json` ([[internal/boxside.ReadInfo]]) for the home's mount points and the network level; when the file cannot be read, the level counts as unknown and the network check runs as for level internet. Then [[internal/boxside.Init]]:

1. removes a readiness marker left from an earlier run (devc removes it before its starts; a plain `podman start` does not);
2. at level internet runs [[internal/boxside.NetworkCheck]]: the binding record in the read-only `/etc/devc` must name this box's network namespace, the start of its PID 1 and the boot, and a connection to the sentinel must be refused, as devc's rules refuse it; a failure ends init, and with it the box;
3. prepares the home ([[internal/boxside.PrepareHome]]): each top-level entry of `/etc/devc/dotfiles` becomes a symlink in the home unless a real file of that name exists, links to dotfiles that are gone are removed, a home that holds nothing but mount points gets a copy of `/etc/skel`, and `~/.local/bin` is created;
4. writes the harness seeds of every launch description ([[internal/boxside.ApplySeeds]]), links the shares that are skills into `~/.agents/skills` and `~/.claude/skills` ([[internal/boxside.LinkSkills]]), and adds an `Include` of the gate's SSH configuration to `~/.ssh/config`;
5. creates `/run/devc/spool` and writes `/run/devc/ready` by rename ([[internal/boxside.writeReady]]), which the host polls for;
6. starts the agent in a goroutine of its own and waits.

Problems in steps 3 and 4 go to init's standard error, which `podman logs` shows, and do not stop the box: a box with an odd home is still usable. [[flow:box-init]] goes through these steps; [[diagram:processes]] shows the processes of a running box and who starts them.

**The agent.** [[internal/boxside.RunAgent]] runs a listener for the host, one for programs in the box and, at level allowlist, a proxy listener on 127.0.0.1:3128 that relays to the host's allowlist proxy. Each is set up again five seconds after a failure. The agent logs to `/run/devc/agent.log`, opened without following symlinks and started over at 1 MiB; every goroutine recovers from panics ([[internal/boxside.Agent.goSafe]]), so nothing the agent does can end box init.

*The host's side.* The agent removes whatever lies at `/run/devc/agent.sock` and listens there ([[internal/boxside.listenSocket]]). A connection counts as the host's only if the kernel reports its peer with PID 0 ([[internal/boxside.HostPeer]]): `SO_PEERCRED` gives the peer's PID as the receiver's PID namespace sees it, and a process outside that namespace, which for a box means the host, appears as 0. The connection then carries yamux, which multiplexes many streams over one connection; a newer host connection replaces an older one. The host opens a stream per request, and [[internal/boxside.Agent.serveHost]] answers with its table of handlers ([[surface:box-agent]]): [[rpc:box.hello]] (the description from `box.json`), [[rpc:box.sessions]] (the live sessions), [[rpc:box.input.clients]], [[rpc:box.input.paste]] and [[rpc:box.input.keys]] (input to panes, [[concern:launching-a-harness]]), [[rpc:box.listen]] and [[rpc:box.services]] (listeners for network exceptions and services), [[rpc:box.input.submit]] and [[rpc:box.input.extension]] (harness extensions). The daemon's side of the connection, with its own checks, is the tour [[flow:box-channel]].

*The box's side.* The agent listens on the abstract socket `@devc-agent`. An abstract Unix socket has no file and belongs to a network namespace, so only processes of this box reach it. At most 32 clients are served at once ([[internal/boxside.Agent.localLoop]]). [[internal/boxside.Agent.relay]] reads a client's first line within ten seconds; a harness extension's registration stays with the agent, anything else goes unparsed on a new stream to the host, followed by the rest of the connection in both directions. Without a host, a request is answered at once with "host unavailable". Clients use [[internal/boxside.AgentCall]] for one request (secrets at launch, questions, `devc box agent-status`) and [[internal/boxside.SendHint]] for the notification [[rpc:host.hint]] after a spool write, which gives up after 200 ms.

*Extensions.* A harness that can load code (pi) gets an extension that connects to `@devc-agent` and registers for its session with [[rpc:box.input.register]]. The agent accepts it only from a process whose initial environment (`/proc/<pid>/environ`) names that session in `DEVC_SESSION` ([[internal/boxside.Agent.sessionOf]]); the newest registration wins. The daemon's messages for the session then reach the harness's own API as [[rpc:input.submit]] ([[internal/boxside.Agent.handleSubmit]]); without an extension the agent answers "not found" and the daemon types the message instead.

## What can go wrong, and what prevents it

The box controls its runtime directory and every process in it, devc's own included.

- **A box process posing as the host.** A process in the box always has a PID in the box's namespace, so the PID-0 rule refuses it. The rule assumes that a box never shares the host's PID namespace (`docs/dev/LEFTOVER.md` part 5).
- **A box process posing as the agent.** The daemon checks the listener from its side: the socket is opened beneath the runtime directory without following symlinks, the listening process must belong to the box's container, and `box.hello` must name this project ([[internal/daemon.CheckPeer]]).
- **Something planted at `agent.sock`.** It is removed before the agent listens. A box that deletes the socket later only cuts itself off until it restarts.
- **A plain `podman start` at level internet.** Its binding record names another start, so init ends with an error and the box stops.
- **A dotfiles repository with symlinks.** The host copies only regular files and directories into `/etc/devc/dotfiles` ([[internal/box.copyDotfiles]]), so no link can bring a host file into a box. In the box, a real file the user made wins over a dotfile link.
- **Many clients.** 32 at once in the box; the daemon's budgets bound the streams and frames a box can make it hold.

## Decisions that are not obvious

- **The box listens, the host connects.** The spec expects this direction also to work under SELinux without a policy module (spec section 12.5).
- **Ready only after the network check.** At level internet the marker then means that the host's rules are bound to this start, which is what `up` waits for.
- **Hints over the channel, events in files.** The spool is the source of truth and works without a daemon; a hint only shortens the host's wait ([[concern:sessions-and-events]]).
- **The agent inside init, not beside it.** One process to start and supervise; its failures are logged and retried, never fatal.

## Where to start reading

1. [[internal/cli.newBoxCmd]] (the `init` command)
2. [[internal/boxside.Init]]
3. [[internal/boxside.RunAgent]] and [[internal/boxside.Agent.acceptHost]]
4. [[internal/boxside.Agent.serveHost]]
5. [[internal/boxside.Agent.relay]] and [[internal/boxside.AgentCall]]
