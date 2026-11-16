+++
id = "ssh-gate"
title = "The SSH gate"
summary = "How a box uses an SSH host the user logged in to without ever holding a credential for it: the master connection on the host, a fresh key per box start, `sshd -i` per connection with a forced command, `devc gate-exec` and its checks, the read-only profile and the log."
order = 53
packages = ["internal/gate"]
files = [
  "internal/boxside/gate.go",
  "internal/daemon/gate.go",
  "internal/cli/sshcmds.go",
]
key = [
  "internal/gate.Client.LoginCmd",
  "internal/gate.Keys.NewBoxKey",
  "internal/gate.ClientConfig",
  "internal/boxside.GateConnect",
  "internal/daemon.boxWorker.serveGate",
  "internal/gate.Server.Prepare",
  "internal/gate.ServerConfig.Render",
  "internal/gate.ExecTarget.Command",
  "internal/gate.Exec.Run",
  "internal/box.Capabilities.SSHGrant",
  "internal/gate.CheckReadOnly",
  "internal/gate.Client.ExecArgs",
]
spec = ["doc:specs-v2.md#10.2"]
tests = [
  "internal/gate.TestSocketPath",
  "internal/gate.TestLoginAndControlCommands",
  "internal/gate.TestExecArgs",
  "internal/gate.TestModeOf",
  "internal/gate.TestCheckReadOnly",
  "internal/gate.TestServerConfig",
  "internal/gate.TestExecTargetCommand",
  "internal/gate.TestServerPrepare",
  "internal/gate.TestExecFullGrant",
  "internal/gate.TestExecRefusals",
  "internal/gate.TestExecStartedWith",
  "internal/gate.TestBoxHooks",
  "internal/gate.TestAppendLog",
  "internal/daemon.TestGateStreams",
  "internal/boxside.TestGateConnectWithoutHost",
  "internal/boxside.TestEnsureSSHInclude",
  "internal/cli.TestSSHCommands",
  "test/integration.TestSSHGate",
]
related = ["secrets", "box-channel", "box-lifecycle", "daemon"]

[[invariant]]
text = "No box mounts a master connection's control socket: the sockets live in `<runtime>/gate`, which only host processes reach."
refs = ["internal/paths.Paths.GateRuntimeDir", "internal/gate.SocketPath", "internal/gate.Exec.Run"]
tests = ["test/integration.TestSSHGate"]

[[invariant]]
text = "The gate's sshd accepts only the key of the box whose channel carried the stream, allows no forwarding of any kind and runs the forced command for everything."
refs = ["internal/gate.ServerConfig.Render", "internal/gate.AuthorizedKeyLine", "internal/gate.Server.Prepare", "internal/daemon.boxWorker.serveGate"]
tests = ["internal/gate.TestServerConfig", "internal/daemon.TestGateStreams", "test/integration.TestSSHGate"]

[[invariant]]
text = "A session runs only for a host granted to the project now and when its box started, read-only if either grant is."
refs = ["internal/daemon.boxWorker.serveGate", "internal/gate.Exec.Run", "internal/box.Capabilities.SSHGrant"]
tests = ["internal/gate.TestExecStartedWith", "internal/daemon.TestGateStreams", "internal/box.TestCapabilities"]

[[invariant]]
text = "gate-exec's ssh never makes a connection of its own: without a master that answers, the session fails."
refs = ["internal/gate.Client.ExecArgs", "internal/gate.Client.Check"]
tests = ["internal/gate.TestExecArgs", "test/integration.TestSSHGate"]

[[invariant]]
text = "Every start of a box with a grant gets a new key pair, made on memory-backed storage; afterwards the private key exists only in the box's secrets directory."
refs = ["internal/gate.Keys.NewBoxKey", "internal/gate.BoxHooks.BoxSecrets", "internal/secrets.CheckMemoryFS"]
tests = ["internal/gate.TestServerPrepare", "internal/gate.TestBoxHooks"]

[[invariant]]
text = "Under the read-only profile only a command whose first word is listed and that holds no shell metacharacter or control character runs; no shell, no sftp."
refs = ["internal/gate.CheckReadOnly", "internal/gate.Exec.Run"]
tests = ["internal/gate.TestCheckReadOnly", "internal/gate.TestExecRefusals"]

[[invariant]]
text = "Every session gate-exec decides for a box of a configured project, run or refused, leaves a line in that realm's gate log."
refs = ["internal/gate.Exec.Run", "internal/gate.AppendLog"]
tests = ["internal/gate.TestAppendLog", "internal/gate.TestExecRefusals"]

[[diagram]]
id = "keys"
title = "Where each key lives, and what the box holds instead"
kind = "graph"

[[diagram.node]]
id = "remote"
label = "SSH host"
col = 1
row = 2
zone = "external"

[[diagram.node]]
id = "exec"
label = "devc gate-exec"
col = 2
row = 1
zone = "host"
ref = "internal/gate.Exec.Run"

[[diagram.node]]
id = "master"
label = "master connection socket"
col = 2
row = 2
zone = "host"
ref = "internal/gate.SocketPath"

[[diagram.node]]
id = "yours"
label = "your keys, agent, 2FA"
col = 2
row = 3
zone = "host"
ref = "internal/gate.Client.LoginCmd"

[[diagram.node]]
id = "sshd"
label = "sshd -i, one per connection"
col = 3
row = 1
zone = "host"
ref = "internal/gate.Server.Command"

[[diagram.node]]
id = "pub"
label = "box key, public half"
col = 3
row = 2
zone = "host"
ref = "internal/paths.Paths.GateBoxKey"

[[diagram.node]]
id = "hostkey"
label = "gate host key, private"
col = 3
row = 3
zone = "host"
ref = "internal/gate.Keys.EnsureHostKey"

[[diagram.node]]
id = "config"
label = "ssh_config of the box"
col = 4
row = 1
zone = "box"
ref = "internal/gate.ClientConfig"

[[diagram.node]]
id = "boxkey"
label = "box key, private half"
col = 4
row = 2
zone = "box"
ref = "internal/gate.BoxHooks.BoxSecrets"

[[diagram.node]]
id = "known"
label = "gate host key, public"
col = 4
row = 3
zone = "box"
ref = "internal/gate.KnownHosts"

[[diagram.edge]]
from = "config"
to = "sshd"
label = "gate-connect, host.gate"
ref = "internal/boxside.GateConnect"

[[diagram.edge]]
from = "boxkey"
to = "pub"
label = "one pair per start"
ref = "internal/gate.Keys.NewBoxKey"

[[diagram.edge]]
from = "known"
to = "hostkey"
label = "the only host key"
ref = "internal/gate.KnownHosts"

[[diagram.edge]]
from = "sshd"
to = "pub"
label = "AuthorizedKeysFile"
ref = "internal/gate.ServerConfig.Render"

[[diagram.edge]]
from = "sshd"
to = "exec"
label = "ForceCommand"
ref = "internal/gate.ExecTarget.Command"

[[diagram.edge]]
from = "exec"
to = "master"
label = "ssh -S, ControlMaster=no"
ref = "internal/gate.Client.ExecArgs"

[[diagram.edge]]
from = "master"
to = "remote"
label = "one connection"

[[diagram.edge]]
from = "yours"
to = "master"
label = "devc ssh login"
ref = "internal/gate.Client.LoginCmd"
+++
## What it is for

An agent often needs a remote machine: a compute server, a cluster's login node, a git server over SSH. Handing it the owner's key would let it keep and reuse the key; asking for 2FA in a box does not work. The gate lets a box run a plain OpenSSH client against a host the owner logged in to on the host, while the box holds no credential for that host, can open no forwarding into the host's or the remote's network, and loses access the moment the owner closes the connection or removes the grant. A grant is full use of the remote account ([[config:project.grants.ssh]]).

## How it works

**The master** ([[term:master-connection]]). `devc ssh login <host>` runs `ssh -M -S <runtime>/gate/m-<hash>.sock -o ControlPersist=<ttl> -o ServerAliveInterval=30 <host> true` attached to the terminal, so passwords and second factors work, with the user's own configuration, keys and agent ([[internal/gate.Client.LoginCmd]]). OpenSSH's connection sharing keeps that authenticated connection in the background; another ssh given the control socket with `-S` runs a new session over it without authenticating. The master ends once no session has used it for `--ttl` (12 hours by default, one minute to 30 days) or with `devc ssh close`. The socket is named by a hash of the alias, because socket paths have 107 bytes; the gate directory is never mounted into a box.

**What a box with a grant gets** ([[internal/gate.BoxHooks]]). Before every start: the gate's host key, made once per machine; `/etc/devc/ssh_config` with one block per granted host (`User` = the host user, `ProxyCommand /opt/devc/bin/devc box gate-connect <host>`, `IdentityFile /run/devc-secrets/gate_key`, `IdentitiesOnly`, `HostKeyAlias devc-gate`, `UserKnownHostsFile /etc/devc/gate_known_hosts`, `StrictHostKeyChecking yes`, no connection sharing, no agent forwarding); and a new ed25519 key pair ([[internal/gate.Keys.NewBoxKey]]). The pair is made by ssh-keygen in a temporary directory below `<runtime>/gate`, which must be on tmpfs or ramfs unless `DEVC_ALLOW_PERSISTENT_SECRETS=1` ([[finding:C1-12]]). The public half becomes `local/gate/keys/<box-id>.pub` with `restrict,pty`; the private half is delivered as the secret `gate_key` in either delivery mode ([[concern:secrets]]). The configuration is also mounted at `/etc/ssh/ssh_config.d/50-devc.conf`, and box init puts `Include /etc/devc/ssh_config` at the top of `~/.ssh/config`. Stop, removal and a failed start remove the public key. [[diagram:keys]]

**One connection** ([[flow:ssh-through-gate]]). The box's ssh starts gate-connect, which asks the host for `host.gate {host}` through the agent and relays bytes ([[internal/boxside.GateConnect]]). [[internal/daemon.boxWorker.serveGate]] takes the project from the channel, requires a valid alias, a configuration without problems and the host in `grants.ssh` now and in the capability record of the running container. It writes a sshd configuration for this box and host into `<runtime>/gate` (0600, [[internal/gate.Server.Prepare]]) and runs `sshd -i -e -f <conf>` with the stream as standard input and output. In inetd mode (`-i`) sshd serves the one connection on its standard input and output; run as the user, it can log in only that user. The configuration ([[internal/gate.ServerConfig.Render]]) allows public-key authentication with the box's key file only, no PAM, every forwarding off (`DisableForwarding`, `AllowTcpForwarding no`, `PermitOpen none`, `PermitListen none`, `PermitTunnel no`, no agent or X11), ignores the user's `~/.ssh/rc` and environment files, allows a terminal, and makes sftp a marker. Its `ForceCommand` is `devc … gate-exec --box <box-id> --host <host> --runtime-dir <dir>`, written by the daemon; sshd hands it to the user's login shell, so every word is single-quoted and a path with a quote, backslash, `!` or `%` is refused ([[internal/gate.ExecTarget.Command]]).

**gate-exec** ([[internal/gate.Exec.Run]]) reads the box's request from `SSH_ORIGINAL_COMMAND`: empty is a shell, the sftp marker is sftp, anything else a command. It loads the configuration itself and refuses (exit 1) on problems, a host no longer granted, a missing capability record, or a host granted only after the start. Under `<host>:ro` ([[term:read-only-profile]]) it refuses a shell and sftp, and requires [[internal/gate.CheckReadOnly]]: the first word must be in `[grants.ssh_allow]` (intersected with the start's list; [[internal/box.Capabilities.SSHGrant]]), with none of `` ;|&$`<>() ``, no newline and no control character but tab. Then `ssh -O check` must find the master (else exit 255 and the advice to run `devc ssh login`). The session runs as `ssh -S <sock> -o ControlMaster=no -o ProxyCommand=false -o BatchMode=yes -o ClearAllForwardings=yes …  -e none -tt|-T <host> -- <command>`: the box's text reaches the remote shell as one argument, neither split nor quoted on the host ([[internal/gate.Client.ExecArgs]]). `ProxyCommand=false` makes a master that died meanwhile fail the session instead of being replaced by a fresh connection with the user's keys.

**The log.** Every session gate-exec decides for a configured project's box becomes a line of `durable/realms/<realm>/gate-log.jsonl` (0600, appended under a lock): box, project, host, mode, terminal, the command up to 4 KiB, exit status, duration, refusal ([[internal/gate.AppendLog]]). A signal from sshd ends the remote command through a context, so the session is still logged.

## What can go wrong, and what prevents it

The box controls its ssh client and configuration, the alias it asks for and every byte of the SSH connection.

- **Taking a credential.** It never sees one: the box key is worthless after the box's next start and only unlocks the gate's sshd for that box.
- **Forwarding into the host's network.** A control socket in reach would let a client ask the master for forwards in the host's namespaces; the socket stays on the host, and sshd, the key's `restrict` and gate-exec's ssh all switch forwarding off.
- **Another project's host.** The project is the channel's; gate-exec finds it by the box ID the daemon wrote.
- **A hostile alias or command.** Aliases are validated before they reach ssh; the command is one argument after `--`.
- **A grant removed.** The daemon and gate-exec check the current configuration; adding a grant needs `devc up --recreate`, because the mount is part of the box's definition.

Limits: the read-only profile is a convenience: an allowed command that runs programs can do anything the account can ([[internal/gate.ReadOnlyNote]]). gate-exec runs through the user's login shell, so a `nologin` shell disables the gate and start-up output corrupts sftp and scp; the log is never rotated ([[doc:docs/dev/LEFTOVER.md#4]], [[doc:docs/dev/LEFTOVER.md#5]]).

## Decisions that are not obvious

- **`sshd -i` per stream**, not a listening sshd: nothing listens on the host, and each connection gets a configuration naming one box's key.
- **A new key at every start.** Nothing durable on the host holds a box's private key ([[chunk:C10b]]).
- **A terminal only when the box asked.** `ssh host < script` must not turn input into echoed CRLF lines.

## Where to start reading

1. [[internal/gate.Client.LoginCmd]] and [[internal/gate.SocketPath]]
2. [[internal/gate.BoxHooks.Prepare]] and [[internal/gate.Keys.NewBoxKey]]
3. [[internal/daemon.boxWorker.serveGate]] and [[internal/gate.ServerConfig.Render]]
4. [[internal/gate.Exec.Run]], [[internal/gate.CheckReadOnly]], [[internal/gate.Client.ExecArgs]]
