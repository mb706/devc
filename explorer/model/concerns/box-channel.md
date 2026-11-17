+++
id = "box-channel"
title = "The box channel"
summary = "How the daemon and a box's agent talk: the box listens and the daemon connects, how the daemon makes sure whom it reached, the hello, yamux streams in both directions, the frame inspector and the other budgets, raw streams, and reconnection."
order = 31
files = [
  "internal/daemon/channel.go",
  "internal/daemon/socket.go",
  "internal/daemon/muxguard.go",
  "internal/proto/rpc.go",
]
key = [
  "internal/daemon.boxWorker.channelLoop",
  "internal/daemon.boxWorker.connect",
  "internal/daemon.DialBoxSocket",
  "internal/daemon.CheckPeer",
  "internal/daemon.muxGuard",
  "internal/proto.HelloReply.Validate",
  "internal/daemon.boxWorker.call",
  "internal/daemon.boxWorker.acceptStreams",
  "internal/daemon.boxWorker.serveStream",
  "internal/proto.Conn",
  "internal/boxside.HostPeer",
]
spec = ["doc:specs-v2.md#12.5", "doc:docs/dev/plan.md#3.11"]
tests = [
  "internal/daemon.TestDialBoxSocket",
  "internal/daemon.TestCheckPeer",
  "internal/daemon.TestHostileChannel",
  "internal/daemon.TestMuxGuard",
  "internal/daemon.TestHostileTransport",
  "internal/proto.TestReadLine",
  "internal/proto.TestParseMessage",
  "internal/proto.TestMalformedInput",
  "internal/proto.TestExposeErrors",
  "internal/proto.TestDecodeResult",
  "test/integration.TestDaemon",
  "test/integration.TestDaemonHostileEndpoints",
]
related = ["security-model", "daemon", "box-init-and-agent", "sessions-and-events", "input-and-receipts", "requests-and-approval"]

[[invariant]]
text = "The daemon connects only to the socket inode it found beneath the box's runtime directory, never through a symlink."
refs = ["internal/daemon.DialBoxSocket"]
tests = ["internal/daemon.TestDialBoxSocket"]

[[invariant]]
text = "A channel is used only after its listener proved to be a process of the box's current container and its hello named that box's project and the daemon's protocol."
refs = ["internal/daemon.boxWorker.connect", "internal/daemon.CheckPeer", "internal/proto.HelloReply.Validate"]
tests = ["internal/daemon.TestCheckPeer", "internal/daemon.TestHostileChannel", "test/integration.TestDaemonHostileEndpoints"]

[[invariant]]
text = "Every request a box makes is served for the project of the channel it arrived on; nothing in the request names the project."
refs = ["internal/daemon.boxWorker.acceptStreams", "internal/daemon.boxWorker.serveStream"]

[[invariant]]
text = "A box that exceeds the frame inspector's budgets loses its channel instead of growing what the daemon holds; a stream beyond the application's own limits is closed or refused alone."
refs = ["internal/daemon.muxGuard.frame", "internal/daemon.boxWorker.acceptStreams", "internal/daemon.boxWorker.serveStream"]
tests = ["internal/daemon.TestMuxGuard", "internal/daemon.TestHostileTransport"]

[[invariant]]
text = "A handler error that is not a JSON-RPC error reaches a box only as \"internal error\"; a JSON-RPC error reaches it with the message its handler chose."
refs = ["internal/proto.Conn.serve"]
tests = ["internal/proto.TestExposeErrors"]

[[invariant]]
text = "A line longer than 1 MiB ends the connection; everything else is decoded strictly, with no unknown field and nothing after the object."
refs = ["internal/proto.ReadLine", "internal/proto.ParseMessage", "internal/proto.DecodeParams", "internal/proto.DecodeResult"]
tests = ["internal/proto.TestReadLine", "internal/proto.TestMalformedInput", "internal/proto.TestDecodeResult"]

[[diagram]]
id = "channel-states"
title = "Channel states, as `devc daemon status` shows them"
kind = "graph"

[[diagram.node]]
id = "none"
label = "none (box not running)"
col = 1
row = 1
ref = "internal/proto.ChannelNone"

[[diagram.node]]
id = "connecting"
label = "connecting"
col = 2
row = 1
ref = "internal/proto.ChannelConnecting"

[[diagram.node]]
id = "connected"
label = "connected"
col = 4
row = 1
ref = "internal/proto.ChannelConnected"

[[diagram.node]]
id = "noagent"
label = "no-agent"
col = 2
row = 3
ref = "internal/proto.ChannelNoAgent"

[[diagram.node]]
id = "refused"
label = "refused"
col = 3
row = 3
ref = "internal/proto.ChannelRefused"

[[diagram.node]]
id = "error"
label = "error"
col = 4
row = 3
ref = "internal/proto.ChannelError"

[[diagram.edge]]
from = "none"
to = "connecting"
label = "a worker starts"
ref = "internal/daemon.Daemon.startWorker"

[[diagram.edge]]
from = "connecting"
to = "connected"
label = "checks and hello pass"
ref = "internal/daemon.boxWorker.connect"

[[diagram.edge]]
from = "connected"
to = "connecting"
label = "the channel closed"

[[diagram.edge]]
from = "connecting"
to = "noagent"
label = "no socket"
ref = "internal/daemon.ErrNoSocket"

[[diagram.edge]]
from = "connecting"
to = "refused"
label = "a check or budget failed"
ref = "internal/daemon.boxWorker.channelLoop"

[[diagram.edge]]
from = "connecting"
to = "error"
label = "anything else"

[[diagram.edge]]
from = "noagent"
to = "connected"
label = "a later attempt passes"

[[diagram.edge]]
from = "refused"
to = "connected"
label = "a later attempt passes"

[[diagram.edge]]
from = "error"
to = "connected"
label = "a later attempt passes"
+++
## What it is for

The daemon must reach into each running box (live sessions, typing into panes, listeners for [[concern:allowlist-proxy|network exceptions]] and [[concern:service-logins|services]]), and programs in a box must reach the host (questions, requests, secrets, git, SSH, the network). Both directions share one connection per box, the [[term:box-channel]]. The box controls the socket's directory, every process in it and every byte on the connection. The channel must make sure the daemon talks to that box and no one else, attribute everything that arrives to that box, and bound what the box can make the daemon hold.

## How it works

**Who listens.** The [[term:box-agent]] listens on `/run/devc/agent.sock`, which the host sees as `<runtime>/<box-id>/agent.sock`, and the daemon connects: the box never gets a socket that can control the daemon. The agent takes a connection as the host's only if `SO_PEERCRED` reports PID 0, a process outside the box's PID namespace ([[internal/boxside.HostPeer]], [[concern:box-init-and-agent]]).

**Whom the daemon reached.** Each [[term:box-worker]] runs [[internal/daemon.boxWorker.channelLoop]], which calls [[internal/daemon.boxWorker.connect]]. It checks, in order:

1. [[internal/daemon.DialBoxSocket]] opens the box's runtime directory with `O_PATH|O_NOFOLLOW`, then `agent.sock` beneath it the same way, requires a socket by `fstat`, and connects to `/proc/self/fd/<n>`: the connection reaches the inode it checked, so a symlink is refused and a rename cannot redirect it.
2. The engine must still report the worker's container running with the same ID; it gives the main PID ([[internal/box.Manager.Find]]).
3. [[internal/daemon.CheckPeer]] takes the listener's PID from `SO_PEERCRED` (for a connecting client, the process that called `listen`). Its `/proc/<pid>/cgroup` must name the container (`libpod-<id>` or `libpod-<id>.scope`; conmon's `libpod-conmon-<id>.scope` does not match) and, when both can be read, its PID namespace must be the container's main process's.
4. yamux, which multiplexes streams over one connection, runs on top, with the daemon as client. On the first stream, the control stream, it calls [[rpc:box.hello]] with ten seconds to answer; the reply is decoded strictly, validated ([[internal/proto.HelloReply.Validate]]) and must carry the daemon's protocol version and this box's realm and project.

Only then is the channel `connected` ([[diagram:channel-states]]). [[flow:channel-connect]] goes through it.

**Streams.** The control stream carries `box.hello` and [[rpc:box.sessions]], which reconciliation asks with 30 seconds to answer ([[internal/daemon.boxWorker.live]]). Every other request of the daemon gets a stream of its own and at most a minute ([[internal/daemon.boxWorker.call]]), so a slow answer holds up nothing else; results are decoded strictly, and a `box.input.clients` report is also range-checked. The agent opens a stream for every client in the box and relays its bytes unparsed. [[internal/daemon.boxWorker.acceptStreams]] accepts them; [[internal/daemon.boxWorker.serveStream]] reads the first line within 30 seconds and dispatches:

- `host.proxy`, `host.dial`, `host.service`: raw relays of one TCP connection each, for the network levels and services;
- `host.git`, `host.gate`, `host.github`: the stream is taken over by the [[concern:git-ratchet|relay service]], the [[concern:ssh-gate|SSH gate]], the [[concern:github|GitHub facade]];
- anything else: JSON-RPC served from the table of [[surface:host-api]] ([[rpc:host.ping]], [[rpc:host.hint]], [[rpc:host.question]], [[rpc:host.request]], [[rpc:host.secret]], …), closed after 31 minutes without input, longer than the longest question wait.

Every handler gets the worker's project; nothing a box sends selects one.

**The frame inspector.** yamux keeps a stream it closed until the peer closes it too (or a minute passes) and answers every ping in a goroutine of its own; handler and message limits reach neither. [[internal/daemon.muxGuard]] sits between the socket and yamux, reads every frame header in both directions, counts the streams the box opens and the pings and go-aways it sends per ten seconds, tracks every stream that is open or half-closed, and closes the whole connection when a budget is exceeded ([[finding:C1-03]], [[finding:C1-04]]). Data is not budgeted: a push or a terminal moves a lot of it over few streams.

**The codec.** [[internal/proto.Conn]] is the JSON-RPC 2.0 of every devc connection: one object per line, at most 1 MiB, decoded strictly ([[internal/proto.ParseMessage]]); unknown methods get an error and responses that match no call are dropped. A handler error that is not a JSON-RPC error becomes "internal error" unless the connection exposes errors, which only the daemon's own socket does; a handler panic is answered the same way. A JSON-RPC error passes unchanged, with the message its handler wrote. A connection serves requests one at a time unless it is given more workers, and runs notification handlers on its reader in arrival order.

**Reconnection.** After a failed attempt the loop records the state with a printable reason and waits, from one second doubling to 30, at most four seconds while there is no socket (a box still starting). A channel that ended normally is tried again after one second; one the inspector closed goes back to `connecting` as it ends and is then recorded as refused. A failure state stays until the next attempt ends.

## What can go wrong, and what prevents it

- **A planted symlink, file or FIFO at `agent.sock`.** Refused: only a socket reached without following a symlink is connected.
- **Another box's agent, or something else at the socket.** The cgroup and PID namespace checks establish that the listener is a process of this box's container, and the hello must name this project. They do not tell devc's agent from another program in the same box that listens in its place and answers the hello: the daemon then talks to that program, which is still this box, and trusts it no more than the box.
- **A box process posing as the host towards the agent.** The PID 0 rule; it assumes a box never shares the host's PID namespace (`docs/dev/LEFTOVER.md` part 5).
- **A recreated box.** The engine must report the worker's own container ID.
- **Floods.** At most 96 streams waiting for their first line, then 32 for requests and the taken-over streams and 64 relays; a stream beyond is closed or refused at once, and the channel stays. The inspector allows 1,024 new streams and 32 pings or go-aways per ten seconds and 192 streams open or half-closed, each holding at most a 256 KiB window (48 MiB in all).
- **Malformed or endless messages.** The 1 MiB line limit, strict decoding, the first-line and idle timeouts.
- **Host details in answers.** Ordinary errors are hidden from the box, but a JSON-RPC error's message passes, and some carry host details: a seal request for a path outside the data root is told the data root's path on the host ([[internal/data.Normalize]], `docs/dev/LEFTOVER.md` part 7).
- **A box that removes its socket.** No new channel can be made until the box restarts (an accepted limit); removing the file does not close a channel already open.

## Decisions that are not obvious

- **The box listens, the host connects.** No socket in a box can reach the daemon, and the direction is expected to work under SELinux without a policy module (spec section 12.5).
- **Budget frames, not bytes.** The application's limits cannot see what yamux keeps; only counting frames bounds it, and only closing the channel ends a hostile peer.
- **A stream per request.** A slow answer from the box never holds up the control stream or another request.
- **Events do not travel on the channel.** The box writes them to its spool, which works without a daemon; a hint only shortens the wait ([[concern:sessions-and-events]]).

## Where to start reading

1. [[internal/daemon.boxWorker.connect]]
2. [[internal/daemon.DialBoxSocket]] and [[internal/daemon.CheckPeer]]
3. [[internal/daemon.muxGuard.frame]]
4. [[internal/daemon.boxWorker.serveStream]]
5. [[internal/proto.Conn]] and [[internal/proto.ParseMessage]]
