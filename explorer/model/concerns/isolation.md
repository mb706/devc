+++
id = "isolation"
title = "Isolation"
summary = "What a box cannot do, and the exact argument, file or call that puts each restriction in place: the user namespace and its IDs, no capabilities, no new privileges, the seccomp profile, the mounts and their modes, SELinux labels, resource limits, the identity fix and the self-test."
order = 21
files = [
  "internal/box/spec.go",
  "internal/toolbox/seccomp.go",
  "internal/boxside/identity.go",
  "internal/boxside/selftest.go",
  "internal/boxside/clone_linux.go",
  "internal/boxside/clone_other.go",
  "internal/boxside/probe_amd64.go",
]
key = [
  "internal/box.BuildSpec",
  "internal/box.Spec.argsWithoutHash",
  "internal/toolbox.DeriveProfile",
  "internal/box.Spec.AddMount",
  "internal/box.prepareMountPoints",
  "internal/netpolicy.BoxArgs",
  "internal/box.Manager.relabel",
  "internal/box.rootExec",
  "internal/boxside.FixIdentity",
  "internal/boxside.SelfTest",
]
spec = ["doc:specs-v2.md#6.2", "doc:specs-v2.md#6.1", "doc:specs-v2.md#6.3", "doc:specs-v2.md#6.6", "doc:specs-v2.md#2"]
tests = [
  "internal/box.TestSpecGolden",
  "internal/box.TestSpecProperties",
  "internal/box.TestSpecRejects",
  "internal/box.TestSpecLimits",
  "internal/box.TestNestedMountPointSymlinkRefused",
  "internal/box.TestSELinuxRelabel",
  "internal/box.TestRunAndRoot",
  "internal/toolbox.TestSeccompNamespaces",
  "internal/toolbox.TestSeccompSockets",
  "internal/toolbox.TestSeccompSocketcall",
  "internal/toolbox.TestSeccompRejectsUnexpectedBase",
  "internal/boxside.TestFixIdentityFiles",
  "test/integration.TestEscape",
  "test/integration.TestIdentityAndNetwork",
  "test/integration.TestMountsAndRealms",
]
related = ["security-model", "box-lifecycle", "toolbox-and-harnesses", "host-foundations", "configuration"]

[[invariant]]
text = "No process in a box holds a capability, and none can gain one or another user's rights by executing a program; the one exception is the command of `devc root`, which the user runs from the host."
refs = ["internal/box.Spec.argsWithoutHash", "internal/box.BuildSpec", "internal/box.Manager.Root"]
tests = ["internal/box.TestSpecProperties", "test/integration.TestEscape"]

[[invariant]]
text = "No process in a box can create a user namespace or an AF_ALG socket."
refs = ["internal/toolbox.DeriveProfile", "internal/toolbox.restrictNamespaces", "internal/toolbox.restrictSockets", "internal/toolbox.refuseSocketcall"]
tests = ["internal/toolbox.TestSeccompNamespaces", "internal/toolbox.TestSeccompSockets", "internal/toolbox.TestSeccompSocketcall", "test/integration.TestEscape"]

[[invariant]]
text = "A base profile of another shape than the derivation expects stops every `up` instead of producing a weaker profile."
refs = ["internal/toolbox.DeriveProfile", "internal/toolbox.ParseProfile"]
tests = ["internal/toolbox.TestSeccompRejectsUnexpectedBase"]

[[invariant]]
text = "A box cannot write the toolbox, its /etc/devc, its harness baselines, its secrets or its shares."
refs = ["internal/box.BuildSpec", "internal/harness.BoxHooks.Contribute", "internal/shares.BoxHooks.Contribute"]
tests = ["test/integration.TestEscape"]

[[invariant]]
text = "Just before a start, devc refuses a symlink at any component of a mount source below its own directories, at the last component of any other source, and at any component of a nested mount point."
refs = ["internal/box.prepareDirs", "internal/box.prepareMountPoints", "internal/box.checkRealDir", "internal/paths.EnsureBeneath"]
tests = ["internal/box.TestNestedMountPointSymlinkRefused"]

[[invariant]]
text = "A configured PID or memory limit that the host cannot apply fails the box; only `none` and -1 switch a limit off."
refs = ["internal/box.BuildSpec"]
tests = ["internal/box.TestSpecLimits"]

[[invariant]]
text = "Every command devc runs as root in a box runs from `/` with the image's system PATH and root's own HOME."
refs = ["internal/box.rootExec", "internal/box.RootPATH"]
tests = ["internal/box.TestRunAndRoot"]

[[invariant]]
text = "In every box UID and GID 1000 are `dev` with home /home/dev, whatever the image's /etc/passwd said."
refs = ["internal/boxside.FixIdentity", "internal/boxside.RewritePasswd", "internal/box.Manager.afterStart"]
tests = ["internal/boxside.TestFixIdentityFiles", "test/integration.TestIdentityAndNetwork"]

[[diagram]]
id = "ids"
title = "Who the box's users are on the host"
kind = "graph"

[[diagram.node]]
id = "dev"
label = "dev, UID and GID 1000"
col = 1
row = 1
zone = "box"

[[diagram.node]]
id = "others"
label = "root (0), vault (2000), every other ID"
col = 1
row = 2
zone = "box"

[[diagram.node]]
id = "you"
label = "your own UID and GID"
col = 3
row = 1
zone = "host"

[[diagram.node]]
id = "sub"
label = "your subordinate IDs (/etc/subuid)"
col = 3
row = 2
zone = "host"

[[diagram.edge]]
from = "dev"
to = "you"
label = "--userns=keep-id"
ref = "internal/box.Spec.argsWithoutHash"

[[diagram.edge]]
from = "others"
to = "sub"
label = "the rest of the mapping"
+++
## What it is for

The harness in a box runs without asking: the baseline sets Claude Code's permission mode to `bypassPermissions` and Codex's sandbox to `danger-full-access` with approval `never` ([[concern:toolbox-and-harnesses]]). devc's claim is that whatever runs there changes only what the box was given (its project directory, its home, the realm's shared state and caches), and can neither gain privileges, reach host files beyond its mounts, change devc's tools and policy, nor create the kernel objects that expose the most kernel code. For each restriction this page names the argument or step that applies it and what happens when it cannot be applied; the network has pages of its own ([[concern:network-levels]]).

## How it works

[[internal/box.BuildSpec]] fills a [[internal/box.Spec]] and [[internal/box.Spec.argsWithoutHash]] renders it in a fixed order; `internal/box/testdata/basic.golden` is the whole vector for a plain project. Every value comes from the configuration directory, the image's own metadata (its ID and `PATH`) or devc. devc relies on Podman to refuse a container it cannot set up as asked, and `up` then removes what it made ([[concern:box-lifecycle]]); nothing checks a running box's hardening from the host, only the self-test from inside, on request.

**The user namespace.** `--userns=keep-id:uid=1000,gid=1000 --user 1000:1000`. A user namespace is a kernel mapping between user IDs inside and outside: rootless Podman maps your own UID and GID to 1000 in the box and every other in-box ID, root included, to your subordinate IDs, which own nothing of yours ([[diagram:ids]]). Files `dev` writes in the project are yours on the host; in-box root is nobody there. `--passwd-entry` names the user until the identity fix runs.

**The identity fix.** After every start through devc, `up` runs `devc box fix-identity` as the box's root: [[internal/boxside.FixIdentity]] replaces the entries named `dev` or with ID 1000 in /etc/passwd and /etc/group by one `dev` entry with home /home/dev, and those named `vault` or with ID 2000 by one `vault` entry (an owner the data workers use), keeping every other line byte for byte; it needs no capability, only ownership of the two files ([[flow:box-up]]). Like every root command devc runs in a box ([[internal/box.rootExec]]) it has the image's system `PATH`, `HOME=/root` and `/` as working directory, so nothing the box planted is found ([[finding:A1]]).

The other arguments, with what each does and what happens when it cannot be applied (Podman refuses a container it cannot set up as asked):

| Argument | What it does | When it cannot be applied |
|---|---|---|
| `--cap-drop=ALL` | No process in the box, root included, holds a capability, so none can mount, take over others' files, use raw sockets or touch the firewall rules in its network namespace. Only the command of `devc root` gets them, at your command ([[internal/box.Manager.Root]]). | |
| `--security-opt no-new-privileges` | The kernel ignores set-ID bits and file capabilities on exec, so `su`, `sudo` or a planted set-ID program gains nothing. | |
| `--pids-limit`, `--memory` | 4096 processes and `16g` by default; no CPU limit. | Rootless Podman applies a limit only where the host delegates its cgroup controller, so `BuildSpec` fails when one is missing, unless the limit is switched off (`memory = "none"`, `pids = -1`) ([[finding:A7]]). |
| `--env` | `HOME`, `USER`, `LOGNAME`, `DEVC_BOX=1`, `DEVC_REALM`, `DEVC_PROJECT`, `PATH` (devc's shims and tools first, then `~/.local/bin`, then the image's) and `TERMINFO_DIRS`, plus the proxy variables at level allowlist. | |
| `--http-proxy=false` | The host's proxy variables, which may carry credentials, stay out of the box. | |
| `--entrypoint /opt/devc/bin/devc`, `--init` | devc's `box init` is the main process whatever the image declares, behind Podman's minimal init. | |
| the network level's arguments | [[internal/netpolicy.BoxArgs]] ([[concern:network-levels]]). | An unknown level fails the definition. |
| `--device` | `capabilities.gpu` adds `nvidia.com/gpu=all`, `capabilities.kvm` adds `/dev/kvm` with `--group-add keep-groups`; both are off unless the project asks. | |

**The seccomp profile.** `--security-opt seccomp=<state>/toolbox/seccomp.json`. Seccomp is a kernel filter on system calls and their register arguments. The profile is Podman's default (containers/common, embedded in devc) with two changes by [[internal/toolbox.DeriveProfile]]. First, `clone` and `unshare` are allowed only with `CLONE_NEWUSER` clear, and `clone3`, whose flags lie in memory where seccomp cannot see them, fails with ENOSYS: no process can make a user namespace, in which it would be root and reach kernel code meant for privileged users. Second, `socket` with domain 38 (AF_ALG, the kernel's crypto interface) fails with EAFNOSUPPORT, and `socketcall`, the 32-bit multiplexer that hides its arguments, with EPERM. Every socket allow rule also requires the domain's upper 32 bits to be zero, because the kernel truncates the domain while seccomp compares 64 bits. The derivation checks that the base has the shape it relies on and otherwise fails, which stops `up` before anything exists. The profile is part of the box's hash, so a changed one shows as drift ([[concern:box-lifecycle]]); `capabilities.nested = true` is refused rather than opening this.

**The mounts.** Only bind mounts devc lists exist; the State view shows each with its source and mode. Writable: the project directory, the home, the realm's caches, notes and memory, the runtime directory `/run/devc`, harness state, and other projects of the realm where `[[mounts]]` says `mode = "rw"`. Read-only: the toolbox, everything devc renders, the secrets, shares and data input zones. The configuration directory is never mounted. [[internal/box.Spec.AddMount]] accepts only clean absolute paths without commas, quotes or control characters, one mount per target; sources below devc's roots (which may themselves be symlinks) are created without following symlinks and must be real directories ([[internal/box.prepareDirs]]); a mount inside another needs a real directory at its place ([[internal/box.prepareMountPoints]]).

**SELinux.** Only where `/sys/fs/selinux/enforce` exists: in mode `labels` (the default) a realm with a level gets `label=level:<level>` and the project directory is relabelled before a start ([[internal/box.Manager.relabel]]); mode `disable` passes `label=disable` and warns at every `up`. Both are tested with fakes only ([[doc:docs/dev/LEFTOVER.md#selinux-enforcing-fedora]]); devc passes no AppArmor option.

**The self-test.** `devc box selftest` ([[internal/boxside.SelfTest]], [[term:self-test]]) tries from inside what must fail (privileges, user namespaces, AF_ALG, `socketcall`, nftables, writing read-only mounts) and what must still work. devc never runs it by itself; `TestEscape` does, and so can you with `devc run <project> -- /opt/devc/bin/devc box selftest`.

## What can go wrong, and what prevents it

The box controls everything inside it: any program, as `dev`, with any file in its writable mounts.

- **Becoming root on the host.** In-box root is a subordinate ID without capabilities, and set-ID programs gain nothing; `TestEscape` tries `su`, `mount`, `unshare` and `nsenter`.
- **Kernel attack surface.** No user namespaces, no AF_ALG; a base profile the derivation does not understand stops every `up`.
- **Redirecting a mount.** Symlinks at mount sources and nested mount points are refused when devc checks them, just before the start; for the same reason the secrets are mounted at `/run/devc-secrets`, outside the box-writable `/run/devc`. Another running box of the realm can still swap a component in a realm-shared parent between this check and Podman's mount (an accepted limit).
- **Changing tools, descriptions, baselines or secrets.** They are read-only mounts; `TestEscape` tries to write, remount and unmount them.
- **A planted program run as root.** Root execs never search a box-writable directory.
- **Exhausting the host, or reaching another realm.** The limits apply or the box does not start; nothing of another realm is mounted, which `TestEscape` checks in the box's mount table.

What remains: a realm is one trust domain ([[doc:docs/dev/LEFTOVER.md#isolation-and-realms]]); at level `open` the box has the engine's network; and what the box prints reaches the terminal of whoever is attached, with devc's tmux configuration passing escape sequences and clipboard writes through ([[internal/tmux.Config]], [[doc:docs/dev/LEFTOVER.md#boxes-and-the-box-side-x5]]).

## Decisions that are not obvious

- **ENOSYS for `clone3`.** It makes libc fall back to `clone`, whose flags the filter can see.
- **EPERM for `socketcall`.** ENOSYS would equal the default action, which runc drops, and without an explicit rule libseccomp turns the socket rules into colliding socketcall rules on x86 ([[chunk:C03r]]).
- **Masked comparisons on all 64 bits.** `socket(0x100000026, …)` created an AF_ALG socket under the unmodified base profile ([[chunk:C02]]).
- **An identity fix instead of an image contract.** Images may lack UID 1000 or give it another name and home; `TestIdentityAndNetwork` uses both kinds.

## Where to start reading

1. [[internal/box.Spec.argsWithoutHash]] and [[internal/box.BuildSpec]]
2. [[internal/toolbox.DeriveProfile]], then [[internal/toolbox.restrictNamespaces]] and [[internal/toolbox.restrictSockets]]
3. [[internal/box.prepareMountPoints]]
4. [[internal/boxside.FixIdentity]] and [[internal/box.rootExec]]
5. [[internal/boxside.SelfTest]]
