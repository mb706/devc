+++
id = "security-model"
title = "Security model"
summary = "What devc defends against and where: the box as the adversary, the six rules every part of the code follows with the code that carries each, what each zone is trusted with, the limits accepted on purpose, the gaps the code has today, and an index to every page's defences."
order = 1
key = [
  "internal/config.Load",
  "internal/safefs.Root.Open",
  "internal/proto.ParseSpoolLine",
  "internal/execx.Cmd",
  "internal/names.Check",
  "internal/secrets.Deliver",
  "internal/box.Capabilities",
  "internal/daemon.boxWorker.serveStream",
  "internal/requests.MayDecide",
  "internal/daemon.hubMethods",
]
spec = ["doc:specs-v2.md#2", "doc:docs/dev/README.md#security-rules-for-code", "doc:docs/dev/LEFTOVER.md#5", "doc:docs/dev/LEFTOVER.md#7"]
tests = [
  "test/integration.TestEscape",
  "test/integration.TestDaemonHostileEndpoints",
  "internal/session.TestIngestHostile",
  "internal/daemon.TestHostileTransport",
  "internal/daemon.TestHubAllowlist",
]
related = ["isolation", "host-foundations", "box-channel", "git-ratchet", "network-levels", "secrets", "hub-access", "hub-commands-and-terminal", "testing-and-tools"]

[[invariant]]
text = "Rule 1. Policy comes only from the configuration directory, which no box mounts; a grant handed out on demand must be in the box's capability record, which only the start that ran its container writes, and the configuration of now (reading its own GitHub repository needs no record)."
refs = ["internal/config.Load", "internal/paths.Resolve", "internal/box.Capabilities.AllowsSecret", "internal/box.Capabilities.SSHGrant", "internal/box.CheckStarted", "internal/daemon.boxWorker.serveGitHub", "internal/daemon.Daemon.reload"]
tests = ["internal/box.TestCapabilities", "internal/box.TestRunningButNotReady", "test/integration.TestEscape"]

[[invariant]]
text = "Rule 2. The host reads a box-writable file only beneath a directory descriptor, without following links and with a size cap, and walks such trees only in helper and context containers, but for `restorecon -R` on SELinux hosts."
refs = ["internal/safefs.Root.Open", "internal/safefs.Root.ReadFrom", "internal/data.Service.work", "internal/boxside.ArchiveWorker.Run", "internal/boxside.ScanContext", "internal/gitx.Host.Cmd", "internal/box.Manager.relabel"]
tests = ["internal/safefs.TestRefused", "test/integration.TestEscape"]

[[invariant]]
text = "Rule 2. Every JSON message and report a box sends the host is bounded, decoded with unknown fields refused and validated field by field before it is used or stored."
refs = ["internal/proto.ParseMessage", "internal/proto.ParseSpoolLine", "internal/requests.Validate", "internal/data.DecodeResult", "internal/gitx.decodeStrict"]
tests = ["internal/session.TestIngestHostile", "internal/proto.TestMalformedInput", "internal/data.TestDecodeResult"]

[[invariant]]
text = "Rule 3. External commands are argument vectors, run by the host from `/`; where a shell or tmux does parse a line, every value in it is quoted and checked again."
refs = ["internal/execx.Cmd", "internal/execx.Real.command", "internal/boxside.ExecGit", "internal/gate.ExecTarget.Command", "internal/boxside.SessionEnded"]
tests = ["internal/execx.TestWorkDir", "internal/gate.TestExecTargetCommand", "internal/boxside.TestSessionEnded"]

[[invariant]]
text = "Rule 4. No name reaches a path, a container name, a ref or a label without passing the name rules; a layout getter panics on one that did not."
refs = ["internal/names.Check", "internal/names.CheckRef", "internal/paths.must", "internal/config.subdirs"]
tests = ["internal/names.TestRealmNames", "internal/paths.TestUnvalidatedNamesPanic"]

[[invariant]]
text = "Rule 5. Secret values move on standard input, in files of mode 0400 or 0600 (memory-backed when delivered to a box, unless the user allows otherwise) or in an answer body; the runner masks the values a caller names."
refs = ["internal/secrets.Deliver", "internal/secrets.CheckMemoryFS", "internal/execx.Cmd.Redact", "internal/daemon.Daemon.secret", "internal/gui.Secrets.Archive", "internal/cli.app.readPassphrase"]
tests = ["internal/secrets.TestDeliver", "internal/secrets.TestFileStore"]

[[invariant]]
text = "Rule 6. A protection that cannot be applied stops the operation: an undelegated limit, an unexpected seccomp base, unverified rules, a secrets directory on disk (unless the user allows it), a configuration with problems, a proxy that cannot list the host's addresses."
refs = ["internal/box.BuildSpec", "internal/toolbox.DeriveProfile", "internal/netpolicy.Installer.Verify", "internal/secrets.CheckMemoryFS", "internal/box.Manager.checkProblems", "internal/daemon.hostAddrCache.forbidden"]
tests = ["internal/box.TestSpecLimits", "internal/toolbox.TestSeccompRejectsUnexpectedBase", "internal/box.TestInternetLevel", "internal/box.TestProblemsBlockLifecycle", "internal/daemon.TestHostAddrsFailClosed"]

[[invariant]]
text = "What a box sends makes the host store and run only bounded amounts: pushes and banks within `git.relay_quota`, journals within their budgets, savepoints it requests within `requested_keep`, each keeping 1 GiB free on the state file system where it writes there, and at most 64 connections per forward and 256 through the router."
refs = ["internal/gitx.Watch", "internal/gitx.Service.fetchFromBox", "internal/session.room.fits", "internal/data.RequestedRetention", "internal/netpolicy.Admission.Acquire"]
tests = ["internal/gitx.TestBankFetchBudget", "internal/session.TestJournalProjectBudget", "internal/data.TestRequestedSavepoints", "internal/netpolicy.TestForwarderAdmission", "test/integration.TestBankBudget", "test/integration.TestJournalBudgets", "test/integration.TestRequestedSavepoints"]

[[invariant]]
text = "A box's request is served for the project of the channel it arrived on, and a hub reaches a machine only through the methods its daemon allowlists, deciding low-risk requests only."
refs = ["internal/daemon.boxWorker.acceptStreams", "internal/daemon.CheckPeer", "internal/daemon.hubMethods", "internal/requests.MayDecide"]
tests = ["test/integration.TestDaemonHostileEndpoints", "internal/daemon.TestHubAllowlist", "internal/daemon.TestHubOrigin"]
+++
## What it is for

devc lets an agent run unattended with its permission prompts off. The question is therefore not whether the agent behaves but what it can do when it does not: a prompt injection in an issue it reads, a malicious dependency it installs or a compromised harness all end in arbitrary code running as the box user. devc assumes that, and treats the whole box as the adversary, devc's own helpers in it included ([[zone:box]]). This page gathers what follows: the rules every part of the code follows and where each is enforced, what each zone is trusted with, the limits accepted on purpose, and the gaps the code has today. Every other concern has a section "What can go wrong, and what prevents it"; the table at the end points to each. The tour [[flow:trust-boundaries]] visits every crossing into a more trusted side, most exposed first.

## The threat model

**The box** controls every process in it, everything in its writable mounts (the project directory, its home, the realm's shared harness state and caches, its notes and memory, the runtime directory with its spool and the agent's socket) and every byte it sends the host: on the box channel, in spool files, through git, the proxy, the SSH gate and services. It must not be able to gain privileges or reach host files beyond its mounts ([[concern:isolation]]), change policy or devc's tools, make the host run anything it wrote ([[concern:host-foundations]]), speak for another project ([[concern:box-channel]], [[concern:sessions-and-events]]), make the host forget banked commits or sealed results ([[concern:git-ratchet]], [[concern:data-and-sealing]]), obtain a credential it was not granted ([[concern:secrets]]), reach more network than its level allows ([[concern:network-levels]]), approve its own requests ([[concern:requests-and-approval]]), or exhaust the host with what it sends. For the same reason nothing the in-box devc reports is taken as proof: the readiness marker proves nothing, a list of live sessions can only end the box's own sessions, and a reported prompt hash makes a receipt `delivered` only as far as the box tells the truth.

**Other boxes of the realm** share its harness state and caches, and its memory when their projects name the same memory group: a [[term:realm|realm]] is one trust domain. Nothing writable crosses realms.

**Beyond the machine.** The hub is a server on the internet. A compromised hub can do on a machine what the machine's daemon lets a hub call (read the transcripts it mirrors, type into and start sessions, decide low-risk requests, watch terminals) and what it likes with the archive it holds; it cannot decide high-risk requests, start or stop boxes, change policy or read secret values ([[concern:hub-commands-and-terminal]]). A compromised machine writes only under its own name on a hub, but can fill its part and read every machine's archive ([[concern:hub-machine-link]], [[concern:fleet-archive]]). A web page in your browser can send requests to the hub and to local URLs; cross-site rules refuse them ([[concern:hub-access]], [[concern:forwards-and-local-urls]]). Upstreams, registries and data remotes are your choices: devc checks what it can (fetched objects through fsck, relay branches only fast-forward, published checksums) and trusts the rest.

**Not defended against.** You and your configuration are trusted: `devc root`, `devc data unseal` and the grants exist to override protections, and a few grants are easy mistakes (below). `devc code` and `devc cursor` attach a desktop editor whose server in the box can act on the host through the editor, a hole the command warns about at every use ([[internal/cli.newEditorCmd]]). devc relies on the kernel, Podman and OpenSSH. A vendor's installer runs with network in a container that sees only its version directory, and its code is trusted that far. The agent can read the credentials its box was given, which the spec accepts ([[doc:specs-v2.md#10.3]]). Processes on the host run as you: the daemon's socket is yours alone, but the router and a forward's listener answer any local connection.

## The six rules

The development rules ([[doc:docs/dev/README.md#security-rules-for-code]]) turn the spec's R1 to R6 ([[doc:specs-v2.md#2]]) into six rules for code; a violation is a bug even if everything works. The invariants below state each with the code that carries it.

| Rule | Concentrated in | How it holds |
|---|---|---|
| 1. Policy comes only from the configuration directory | [[pkg:internal/config]], [[pkg:internal/paths]], [[internal/box.Capabilities]] | Loaded from one directory no box mounts; a grant (a secret, an SSH host, the allowlist, a GitHub token or further repository, a service) is used only while the capability record of the box's start and the configuration of now both have it. Reads of the project's own GitHub repository need no record: they follow the upstream configured now ([[internal/daemon.boxWorker.serveGitHub]]). |
| 2. Box-writable data is hostile input | [[pkg:internal/safefs]], [[pkg:internal/proto]], the workers of [[pkg:internal/boxside]] in helper and context containers | Single files beneath a directory descriptor, without links, capped; trees walked only by workers, but for `restorecon -R` of a project on SELinux hosts ([[internal/box.Manager.relabel]]); every JSON message decoded with unknown fields refused and validated, though not every decoder refuses what follows the value (below); no host git, interpreter or build tool in a box's tree. |
| 3. No shell interpolation | [[internal/execx.Cmd]] | Argument vectors, from `/` on the host (in the box, git runs in the repository it works on and the harness in the directory its session started in); the gate's forced command and tmux's pane-died hook quote every value and check it again. |
| 4. Names are validated | [[pkg:internal/names]] | `^[a-z0-9][a-z0-9._-]{0,62}$`, realms stricter, refs by git's rules too; session IDs made by devc and checked when read back. |
| 5. Secrets stay out of arguments and logs | [[pkg:internal/secrets]], [[internal/execx.Cmd]] | Standard input, files of mode 0400 or 0600 (memory-backed when delivered to a box, unless `DEVC_ALLOW_PERSISTENT_SECRETS=1`), answer bodies; in the box, the harness gets its granted secrets in its environment, on purpose. |
| 6. Fail closed | where each protection is applied | Unapplied limits, an unknown seccomp base, unverified firewall rules, a disk-backed secrets directory (unless `DEVC_ALLOW_PERSISTENT_SECRETS=1`) and configuration problems all stop the operation. |

## What each zone is trusted with

| Zone | Trusted with | Not trusted with |
|---|---|---|
| [[zone:device]] | everything: the CLI on the host is you; a browser holds a device certificate and the hub's password | |
| [[zone:host]] | policy, credentials, durable state; every check runs here | |
| [[zone:hub]] | the transcripts it mirrors, low-risk decisions, typing into and starting sessions, watching terminals (typing only where both hub.toml and the machine allow control), an unencrypted copy of the archived state, encrypted secrets bundles | high-risk decisions, boxes' life cycle, configuration, host shells, secret values |
| [[zone:aux]] | one job each, with only its mounts: a worker sees one tree, with no network and at most a few capabilities, which count only in its own user namespace; the installer and login containers have network but see only the directory of their job | anything else: their reports are checked like a box's |
| [[zone:box]] | what its project grants | everything it sends |
| [[zone:external]] | what you chose to use them for | their answers, as far as devc can check them |

## Limits accepted on purpose

[[doc:docs/dev/LEFTOVER.md#5]] lists every limit the design accepts; the main ones:

| Area | Accepted | Details |
|---|---|---|
| Realms | a realm is one trust domain: a box can plant hooks in a shared harness home that run in other boxes | [[doc:docs/dev/LEFTOVER.md#isolation-and-realms]] |
| Network | DNS to trusted resolvers is a channel out; the allowlist limits names, not content | [[doc:docs/dev/LEFTOVER.md#network]] |
| Git | a box can make the host store what it pushes or a bank takes within `git.relay_quota`; relays are never pruned | [[doc:docs/dev/LEFTOVER.md#git-and-relays]] |
| Data | a push chooses files by size and time, which the box controls; sealing protects against the box only | [[doc:docs/dev/LEFTOVER.md#data-and-sync]] |
| Sessions | journals are never rotated, only bounded by their budgets; box text is escaped for terminals but untrusted elsewhere | [[doc:docs/dev/LEFTOVER.md#sessions-harnesses-and-input]] |
| Channel | the agent's peer check assumes no box shares the host's PID namespace | [[doc:docs/dev/LEFTOVER.md#daemon-and-channel]] |
| Hub | a compromised machine fills its own part of the hub; the development mode has no authentication | [[doc:docs/dev/LEFTOVER.md#hub-and-web-app]] |
| Brokered access | a token in a box does what its permissions allow; a service acts with your account within its rules | [[doc:docs/dev/LEFTOVER.md#github]], [[doc:docs/dev/LEFTOVER.md#services]] |
| Shares, sources, desktop | shares put text before every agent that lists them; `/src` is writable by the realm's boxes; the box can read what you type into its desktop | [[doc:docs/dev/LEFTOVER.md#shares-and-sources]], [[doc:docs/dev/LEFTOVER.md#desktop]] |

## Where the code falls short today

The authors of these pages read the code area by area, and a second reader checked the pages; together they found defects that no test catches. The four rated high that let a box exhaust the host or push out the owner's savepoints are fixed: journals have budgets ([[concern:sessions-and-events]]), a bank lists and bounds what it fetches and runs under the push watchdog ([[concern:git-ratchet]]), savepoints a box requests are a class of their own ([[concern:savepoints]]), and forwards admit a bounded number of connections ([[concern:forwards-and-local-urls]]). So are those that broke a rule of the project: only the start that ran a container writes its capability record ([[concern:box-lifecycle]]); a panic ends one stream or request, not the daemon ([[concern:daemon]]); the proxy connects nowhere while it cannot list the host's addresses ([[concern:allowlist-proxy]]); a push's records of updates git refused are removed ([[concern:git-ratchet]]); a pull request is checked before its branch is pushed, and the GitHub credentials cannot be granted as secrets ([[concern:github]]); a refused realm no longer stops the others' archive ([[concern:fleet-archive]]); only a receipt settles a prompt in the web app ([[concern:web-app]]); login output is escaped ([[concern:toolbox-and-harnesses]]). The rest are open: [[doc:docs/dev/LEFTOVER.md#7]] lists each with where to look, and the pages linked here describe the code as it is. The severities are part 7's; its findings from the accuracy review carry none.

| Gap | Severity | Page |
|---|---|---|
| `grants.github = "token:<secret>"` can name any realm secret, the host's GitHub credential and the App's key included | medium | [[concern:github]] |
| A trash restore that fails part-way strands the rest of the entry | medium | [[concern:data-and-sealing]] |
| A regular file's seal can be recorded for a file that was swapped in and out during the seal | not graded | [[concern:data-and-sealing]] |
| The CLI reads only the first JSON value of a live-session report, so `[]` and anything after it ends the box's open sessions; decoders that end with `Decoder.More` pass a stray `]` or `}` | not graded | [[concern:sessions-and-events]], [[concern:data-and-sealing]] |
| `devc wt rm` removes ignored files and what snapshots leave out | not graded | [[concern:worktrees-and-context]] |
| `devc secret pull` takes any machine's newest bundle and shows the hub's labels | medium | [[concern:secret-transfer]] |
| Nothing bounds how often a session notifies; behind a proxy a local process can pose with a copy of a certificate | medium | [[concern:hub-inbox-and-notifications]], [[concern:hub-access]] |
| tmux in a box passes escape sequences and clipboard writes to an attached terminal | medium | [[concern:isolation]] |

## Every page's defences

| Page | What it keeps the other side from doing |
|---|---|
| [[concern:configuration]] | turning a mistake in the configuration into a weaker box |
| [[concern:host-foundations]] | redirecting or stalling host reads and writes in box trees; running a command inside a project |
| [[concern:toolbox-and-harnesses]] | changing devc's tools, settings or credentials; an installer reaching beyond its directory |
| [[concern:command-line]] | a manifest taking over devc's names; box output on your terminal |
| [[concern:box-lifecycle]] | foreign containers, drift, half-finished starts, two commands on one box |
| [[concern:isolation]] | privileges, kernel attack surface, redirected mounts |
| [[concern:box-init-and-agent]] | a box process posing as the host; a start without its rules |
| [[concern:launching-a-harness]] | text becoming commands; one session's identity in another |
| [[concern:daemon]] | other users and configurations; floods of clients and subscribers |
| [[concern:box-channel]] | reaching the wrong box; floods of streams and frames; host details in answers |
| [[concern:sessions-and-events]] | rewriting the record, speaking for another project, floods |
| [[concern:input-and-receipts]] | typing twice or into the wrong session; typing over a person, for about a minute (then it types anyway) |
| [[concern:watching-sessions]] | box text on terminals; terminal input without consent |
| [[concern:requests-and-approval]] | approving its own requests; box values as options |
| [[concern:git-ratchet]] | rewriting recorded history; code on the host; the cost of a push or a bank |
| [[concern:upstream]] | a box publishing; pushing what you did not see |
| [[concern:worktrees-and-context]] | hostile reports; planted git metadata in the context |
| [[concern:network-levels]] | a start without rules; planted resolvers; reaching the host |
| [[concern:allowlist-proxy]] | reaching the LAN through a name; granting itself access |
| [[concern:forwards-and-local-urls]] | executing on the host; one box's page driving another's server; floods of connections |
| [[concern:secrets]] | another project's secret; a grant added while the box runs; a removed grant, from the next request in daemon mode (a delivered file stays until the box stops, and no mode takes back a value already read); copies on disk, unless you allow them |
| [[concern:ssh-gate]] | taking a credential; forwarding into the host's network |
| [[concern:github]] | reading other repositories; shaping requests; getting the credential |
| [[concern:service-logins]] | another service or path; smuggled requests; the credential |
| [[concern:desktop]] | reaching further through the sidecar; impersonating it |
| [[concern:data-and-sealing]] | tricking a worker; a directory swapped during a seal (a regular file can be: see the gaps above) |
| [[concern:savepoints]] | reaching a savepoint; pushing the owner's savepoints out |
| [[concern:data-sync]] | a pull destroying data here, a push destroying data there |
| [[concern:shares]] | hostile trees; changing a share from a box |
| [[concern:dependency-sources]] | tampered archives; registries naming paths |
| [[concern:hub-access]] | clients without certificates; copied cookies; guessed passwords; forged cross-site requests |
| [[concern:hub-machine-link]] | a machine writing another's data; filling or stalling the hub |
| [[concern:hub-inbox-and-notifications]] | box text on the phone; replayed buttons |
| [[concern:hub-commands-and-terminal]] | a compromised hub beyond the allowlist; commands applied twice |
| [[concern:web-app]] | box text acting in the page; lost or repeated commands |
| [[concern:fleet-archive]] | a box or a machine emptying the backup |
| [[concern:state-restore]] | a hostile archive writing elsewhere or replacing state |
| [[concern:secret-transfer]] | the hub reading secrets; a hostile bundle's cost |
| [[concern:host-setup]] | changing the host as root; injecting into unit files |

## Decisions that are not obvious

- **The whole box is untrusted, devc's in-box code included.** The binary in the box is devc's, but anything in the box can replace its process or answer in its place, so the host verifies from outside instead: the binding of the firewall rules and the canary rather than the readiness marker ([[concern:network-levels]]), the peer's cgroup rather than its word ([[concern:box-channel]]).
- **Capabilities rather than credentials** (spec R4). The relay service, the SSH gate, the read facade and service logins give the box the use of something while the credential stays on the host; where a credential must enter the box (a harness's token, a GitHub token mode), it enters only as granted, and an App token is minted for one repository.
- **The hub is optional and outside.** A machine works without one, dials out to it, and lets it call only an allowlist; the limit lives on the machine, so a compromised hub gains no more ([[concern:hub-commands-and-terminal]]).

## Where to start reading

1. [[internal/box.Spec.argsWithoutHash]] and [[internal/toolbox.DeriveProfile]]
2. [[internal/safefs.Root.Open]] and [[internal/proto.ParseSpoolLine]]
3. [[internal/daemon.boxWorker.serveStream]]
4. [[internal/box.CapabilitiesOf]] and [[internal/box.Capabilities.AllowsSecret]]
5. [[internal/requests.MayDecide]] and [[internal/daemon.hubMethods]]
