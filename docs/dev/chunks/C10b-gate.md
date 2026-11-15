# C10b — SSH gate

Read `docs/dev/README.md`, `docs/dev/plan.md` (section 3.12) and the `STATUS.md` notes of C08 and C10a. Product spec: `specs-v2.md` section 10.2. Background on why the design is what it is: `../v2-research/04-credentials.md` section 5 (read that section only).

A box with a grant can use an SSH connection the user has authenticated on the host, including password and 2FA logins, without ever holding a credential for it. The grant is full use of the remote account; the gate adds no per-command approval. What it does guarantee: the credential and the master connection stay on the host, no forwarding of any kind, per-box and per-host grants, a log, and one command to cut access.

A bind-mounted control socket is not an option: through it a box can ask the master for port and socket forwards into the host's namespace. That was demonstrated during design.

## 1. Host side: master connections

All host `ssh` calls take `-F <file>` when `DEVC_SSH_CONFIG` is set (used by tests); otherwise the user's own SSH configuration applies. Host aliases are validated: no leading `-`, letters, digits, `.`, `_`, `-` only.

| Command | Behaviour |
|---|---|
| `devc ssh login <host> [--ttl 12h]` | Runs `ssh -M -S <socket> -o ControlPersist=<ttl> -o ServerAliveInterval=30 <host> true` attached to the terminal, so the user can type a password or a second factor. The master stays in the background. |
| `devc ssh ls` | Hosts with a socket and whether the master answers `-O check`. |
| `devc ssh close <host>` | `-O exit`. |

Sockets live in `<runtime>/gate/`, named by a short hash of the host alias (socket paths are limited to 108 bytes). That directory is never mounted into any box.

## 2. Host side: the gate

- Once per machine, generate a host key for the gate (`local/gate/host_ed25519`).
- At `devc up` for a project with `grants.ssh`, generate a key pair for the box. The public key goes to `local/gate/keys/<box-id>.pub` with the options `restrict,pty`; the private key is delivered with the box's secrets as `gate_key`. Remove both at `rm`.
- New channel stream `host.gate {host}`. The daemon checks that the calling box's project is granted that host, then starts `sshd -i -e` with the stream as stdin and stdout and a generated configuration:
  - `HostKey` as above; `AuthorizedKeysFile` set to that box's key file only; public-key authentication only; `UsePAM no`; `StrictModes no`;
  - `AllowTcpForwarding no`, `AllowStreamLocalForwarding no`, `AllowAgentForwarding no`, `X11Forwarding no`, `PermitTunnel no`, `GatewayPorts no`, `PermitUserEnvironment no`, no `AcceptEnv`;
  - `PermitTTY yes`;
  - `Subsystem sftp devc-sftp-marker`;
  - `ForceCommand <devc> gate-exec --box <box-id> --host <host>`.
  An unprivileged `sshd` can only log in the user it runs as, which is what is wanted here.
- `devc gate-exec` (hidden host command, run by `sshd` as the user):
  - if the master for the host does not answer, print one line telling the agent to ask the user to run `devc ssh login <host>`, and exit 255;
  - `SSH_ORIGINAL_COMMAND` empty: an interactive shell, `ssh -S <socket> -o ControlMaster=no -tt <host>`;
  - equal to the sftp marker: `ssh -S <socket> -o ControlMaster=no -s <host> sftp`;
  - otherwise: `ssh -S <socket> -o ControlMaster=no [-tt when a TTY was requested, else -T] <host> -- "<the original command, as one argument>"`, so the remote shell parses exactly what the box typed;
  - append to `durable/realms/<realm>/gate-log.jsonl`: time, box, host, the command (truncated to 4 KiB), exit status, duration;
  - exit with the remote exit status.
- **Read-only profile.** A grant written `"<host>:ro"` restricts the box to commands whose first word is in `[grants.ssh_allow] <host> = ["squeue", "ls", …]` and that contain no shell metacharacters (`;`, `|`, `&`, `$`, backquote, `<`, `>`, parentheses, newline). No interactive shell and no sftp in this profile. It is a convenience, not a strong boundary; say so in the help text.

## 3. Box side

- `/etc/devc/ssh_config` (generated at `up`), one block per granted host:

  ```
  Host <host>
      User <the host user's login name>
      ProxyCommand /opt/devc/bin/devc box gate-connect <host>
      IdentityFile /run/devc-secrets/gate_key
      IdentitiesOnly yes
      HostKeyAlias devc-gate
      UserKnownHostsFile /etc/devc/gate_known_hosts
      StrictHostKeyChecking yes
      ControlMaster no
      ForwardAgent no
  ```

- Make the box's SSH client read it: mount it read-only at `/etc/ssh/ssh_config.d/50-devc.conf`, and have `devc box init` put `Include /etc/devc/ssh_config` at the top of `~/.ssh/config` if the line is not there (creating the file with mode 0600 if needed).
- `devc box gate-connect <host>` opens the `host.gate` stream through the in-box agent and relays stdin and stdout. Without a connected daemon it prints a one-line explanation and exits non-zero.
- With `secrets.delivery = "daemon"` (C10a) the gate key is still delivered as a file, because the SSH client needs one; note this exception.
- Extend the generated sandbox description: which hosts can be reached with plain `ssh <host>`, that `scp`, `sftp`, `rsync` and git over SSH work, that forwarding does not, and what to tell the user when the connection is not established.

## 4. Doctor

The `tools` check already warns when `sshd` is missing. Add `ssh-keygen` to it.

## 5. Tests

Unit: host alias validation; generated `sshd` and client configuration; `gate-exec` argument construction for each case; the read-only filter (allowed command, disallowed first word, each metacharacter).

Integration. Stand in for the remote machine with a second unprivileged `sshd` on a loopback port of this host, with its own temporary host key and an authorized key, addressed through a temporary SSH configuration in `DEVC_SSH_CONFIG`. If no `sshd` binary is available, skip these tests and record that in `LEFTOVER.md`.

1. `devc ssh login <host>` (key authentication, so no prompt) establishes the master; `devc ssh ls` shows it alive.
2. In a box with the grant: `ssh <host> echo hello` prints `hello` and returns the remote exit status (test a failing command too).
3. A command with quoting and pipes (`ssh <host> 'echo "a b" | wc -w'`) behaves as it would over direct SSH.
4. `scp` and `sftp` transfer a file in each direction; `rsync -e ssh` if rsync is present on both sides; `git clone <host>:<path>` of a bare repository.
5. `ssh -tt <host> tty` reports a terminal.
6. Forwarding is refused: `ssh -L`, `-R`, `-D` and agent forwarding all fail to forward.
7. A host that is not granted is refused; another box's key is not accepted for this box's stream.
8. After `devc ssh close <host>`, the box gets the one-line explanation and exit status 255.
9. The gate log contains the commands run.
10. Read-only profile: an allowed command runs; a disallowed one, one with a metacharacter, an interactive shell and sftp are refused.
11. Nothing under `<runtime>/gate/` is visible in the box.

## 6. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `STATUS.md` and `LEFTOVER.md` are updated, noting that password and 2FA logins were not exercised (key authentication only) and that SELinux was not.
