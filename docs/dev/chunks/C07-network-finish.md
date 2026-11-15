# C07 — Network and finish of phase 1

Read `docs/dev/README.md`, `docs/dev/plan.md` (sections 3.9, 3.13) and the `STATUS.md` notes of the earlier chunks first. Product spec: `specs-v2.md` sections 11, 16 and 17 (phase 1 "done when").

After this chunk phase 1 is complete: boxes get the default network level for real, ports can be forwarded without an IDE, a new machine can be set up from a config repository, the escape suite proves the protections, v1 moves to `legacy/`, and there is user documentation.

## 1. Network levels (`internal/netpolicy`)

Replace the placeholder from C02. Since the fixes after review A, `internet` is refused outright and the example configuration and the tests set `open` explicitly. Once `internet` works, switch the example configuration back to the default (`internet`), keep `open` only in tests that need it, and make sure the generated sandbox description states the level actually in force.

| Level | Implementation in this chunk |
|---|---|
| `none` | `--network none` |
| `open` | the engine's default network, no rules |
| `internet` | the engine's default network plus rules installed by the host, below |
| `allowlist` | not available until C13: `devc up` fails with a clear message |

### 1.1 `internet`

The box must be able to reach the internet and nothing private: no LAN, no host services, no link-local or carrier-grade NAT ranges, no IPv6.

- Creation adds the sysctls that disable IPv6 in the box's network namespace.
- Start sequence, owned by devc: `podman init`; read the network namespace path and the box's resolver addresses from the engine; install the rules with `podman unshare nsenter --net=<path> nft -f -` (rules on stdin); then `podman start`. If any step fails, remove the container and fail.
- Rules, in one `inet` table named `devc`, output hook, default accept:
  1. accept loopback;
  2. drop all IPv6;
  3. accept destinations in the set `allow4` (IPv4 address and port, with timeouts; empty for now, used by C13 for exceptions);
  4. accept UDP and TCP port 53 to the box's resolver addresses;
  5. the sentinel: TCP to `192.0.2.1` port 9 is rejected with a TCP reset;
  6. reject, with an ICMP error so connections fail fast, every destination in `0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10`, `127.0.0.0/8`, `169.254.0.0/16`, `172.16.0.0/12`, `192.168.0.0/16`, `224.0.0.0/4`, `240.0.0.0/4`.
- **Fail closed inside the box.** `/etc/devc/box.json` carries the level. For `internet`, `devc box init` connects to the sentinel before it writes the ready file: an immediate "connection refused" proves the rules are loaded. Anything else (timeout, unreachable, success) makes init exit non-zero, which stops the box. A box started behind devc's back with `podman start` therefore does not come up unfiltered.
- **Check from outside.** After start, `devc up` runs `devc box net-canary` in the box against a listener the host opens for a moment on all its addresses: the box must fail to connect to every private address of the host and to the engine's host alias. If it connects, stop the box and fail.
- **Restart.** The rules live in the namespace and vanish when the box stops. Find out whether `podman init` can be applied to a stopped container; if not, `devc up` on a stopped `internet` box removes and recreates the container (home and state are outside it, so nothing is lost). Never use a plain `podman start` for this level.
- The rules text is produced by one function with unit tests. Keep the table and set names stable: C13 builds on them.

Record in `LEFTOVER.md`: DNS to the resolver remains a channel at this level; pasta on Podman 6 is untested; behaviour when the host's resolver is itself a private address.

## 2. Port forwarding

| Command | Behaviour |
|---|---|
| `devc forward <port> [ref] [--host-port N] [--bind ADDR] [--detach]` | Listens on the host (default `127.0.0.1`, same port number; if taken, pick a free one and print it) and relays each connection to `127.0.0.1:<port>` inside the box. |
| `devc forward ls`, `devc forward stop <id>` | Detached forwards, tracked in the runtime directory. |

- Per connection the host runs `nsenter -t <box PID> -U -n --preserve-credentials <devc> box dial 127.0.0.1:<port>` and copies bytes both ways. `devc box dial <addr>` connects and relays stdin and stdout.
- This reaches services bound to loopback in the box, needs nothing in the image, and works at every network level including `none`.
- `--bind` with anything but a loopback address prints a warning that the port becomes reachable from the network.
- A detached forward exits when its box is gone.

## 3. Setting up a machine

- `devc init <git-url>` clones the user's configuration repository into the config directory. `devc init --example` writes the example configuration (embed `examples/config`). Both refuse a non-empty config directory, and print the next steps: `doctor`, `toolbox build tmux`, `harness install`, `env build`.
- `devc doctor --fix` does what needs no root (create directories, sync the toolbox) and prints the root commands for the rest as one script: lingering, subordinate ID ranges, the SELinux file-context rule and `restorecon`, the AppArmor profile.
- Add a doctor check `overlay-statx`: run the helper container with a worker that calls `statx` asking for the birth time on a file of the root filesystem, with a 10 s limit. A hang means the fuse-overlayfs bug found in C02 and C03 (Debian trixie's `apt-get` and Claude Code's installer hang on it). Report it as a failure with the remedy: use native overlay storage or a fixed fuse-overlayfs.
- `devc up --all` and `devc down --all`.

## 4. Editors

- `devc cursor [ref]` and `devc code [ref]`: open the editor attached to the running box at the `main` checkout, as v1 did (`--folder-uri vscode-remote://attached-container+<hex of the container name>/<path>`). Fail with a clear message when the editor's CLI is missing.
- Document Emacs through TRAMP: `/podman:devc-<realm>-<project>:/w/<realm>/<project>/main/`.
- Both the documentation and the command's help must say that an attached editor is a deliberate hole: code in the box can act on the host through the editor's server.

## 5. Escape suite

`test/integration/escape_test.go`. One realm with a project that has data, a relay and a running session; a second realm with one project. Every attempt below is made from inside the first box as `dev` and must fail; a final group checks the host.

- **Sealed data and vault:** delete, overwrite, append, rename, `chmod` a sealed entry; list or enter `.devc`; rename the data root or a zone directory.
- **Read-only parts:** write under `/opt/devc`, `/etc/devc`, a harness baseline directory, `/run/devc-secrets`, an input zone; remount any of them read-write.
- **Privilege:** every item of `devc box selftest`; `mount`, `unshare`, `setns` fail; no setuid binary helps.
- **Other realm:** nothing of the second realm appears in the box's mount table; its project path does not exist.
- **Network (`internet`):** connecting to the host's LAN addresses, to the engine's host alias and to a second box of the same realm fails; IPv6 is off; the sentinel answers; the rules cannot be listed or changed from inside.
- **Secrets:** only granted secrets are present.
- **Git:** pushing odd refs through `origin` changes nothing in the relay; there is no path to the relay.
- **Host side:** plant, inside the box, git hooks and `core.fsmonitor` in `.repo`, an `.envrc`, a `.vscode/tasks.json`, symlinks in the spool pointing at host files, and a `.git` file under `/ctx/notes`, each rigged to create a marker file if executed. Then run on the host: `devc ls`, `devc ps`, `devc bank`, `devc data ls`, `devc context commit`, `devc down`. No marker may exist on the host, and no host file content may appear in the journal or the context repository.

Where an earlier chunk already has the identical test, call it from the suite instead of duplicating it.

## 6. Repository tidy-up

- Move v1 into `legacy/`: the `devc/` script directory, `podman-dev-images/`, `specs.md`, `podman_cheatsheet.md`. Use `git mv`.
- Port v1's images to harness-free example envs under `examples/config/envs/`: `r-base` and `python-base`, keeping the system libraries and tools, and dropping the Node, Claude Code and Codex installation, the user creation and the dotfile copying. These are examples and are not built by tests.
- Copy v1's `.zshrc`, `.tmux.conf`, `.gitconfig` and `.gitignore_global` into `examples/config/dotfiles/`, removing lines that only made sense in v1 (for instance the GitHub credential helper in `.gitconfig`).

## 7. Documentation

- `README.md`: what devc is, a quick start from nothing to a running agent session, and a map of the other documents.
- `docs/guide.md`: concepts (realm, project, env, box, worktree, session); daily use; the git workflow with `bank`, `pull` and `push`; data zones, sealing and savepoints; network levels and forwarding; logins and secrets; what is protected and what is not.
- `docs/host-setup.md`: host requirements and the root commands for each (lingering, subordinate IDs, SELinux rule and optional per-realm level, AppArmor profile, an XFS volume for the projects root to get reflink savepoints), plus known host problems (the fuse-overlayfs `statx` hang).
- Write for a reader who has not seen the spec. Describe only what exists after this chunk; do not document later phases as if they were available.

## 8. Tests

Unit: rule generation; resolver parsing; forward bookkeeping.

Integration:

1. `internet`: the box starts; the sentinel is refused; the canary passes; a public address is reachable when the host has connectivity (skip that item otherwise).
2. A box of level `internet` started with plain `podman start` after a stop does not become ready and stops.
3. `devc down` then `devc up` on an `internet` box ends with the rules in place again.
4. `none`: no network, and `forward` still works.
5. `forward`: a server bound to `127.0.0.1` in the box answers through the forwarded port; the listener is on loopback only; a detached forward is listed, stops on request, and exits when the box goes down.
6. `allowlist` is refused.
7. `init --example` produces a configuration that passes `config check`.
8. `doctor` reports the `overlay-statx` check (on this host it is expected to fail; assert that it completes within its time limit either way).
9. The escape suite.

## 9. Done when

- The whole suite passes through `./dev test` and `./dev itest`.
- The phase 1 "done when" of the spec holds on this host as far as it can be shown without a real harness login: a box can be recreated without losing the fake harness's stored token or its history, and the escape suite passes.
- `STATUS.md` and `LEFTOVER.md` are updated.
