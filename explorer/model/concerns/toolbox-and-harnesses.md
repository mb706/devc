+++
id = "toolbox-and-harnesses"
title = "Toolbox and harnesses"
summary = "The read-only tree of devc's own programs and the installed harnesses that every box mounts at /opt/devc; harness manifests; installing in a throwaway container; the settings, launch descriptions and credentials a box gets; logins; versions and their collection."
order = 12
packages = ["internal/toolbox", "internal/harness", "assets"]
files = ["internal/cli/harnesscmds.go", "internal/cli/toolcmds.go"]
key = [
  "internal/toolbox.Toolbox.Sync",
  "internal/toolbox.Installer.Install",
  "internal/toolbox.Toolbox.publish",
  "internal/toolbox.Toolbox.GC",
  "internal/config.Harness",
  "internal/harness.BoxHooks",
  "internal/harness.BoxHooks.Contribute",
  "internal/harness.Describe",
  "internal/harness.Settings",
  "internal/harness.Merge",
  "internal/harness.LoginRunner",
]
spec = ["doc:specs-v2.md#7", "doc:specs-v2.md#8.1", "doc:specs-v2.md#8.2"]
tests = [
  "internal/toolbox.TestSync",
  "internal/toolbox.TestInstallScript",
  "internal/toolbox.TestInstallFailureKeepsCurrent",
  "internal/toolbox.TestInstallPublication",
  "internal/toolbox.TestGCKeepsVersionsInUse",
  "internal/harness.TestContribute",
  "internal/harness.TestPrepareAndCleanup",
  "internal/harness.TestClaudeSettingsMerge",
  "internal/harness.TestRefreshLaunches",
  "test/integration.TestHarnessInstall",
  "test/integration.TestHarnessLaunchAndSecrets",
  "test/integration.TestHarnessState",
  "test/integration.TestHarnessSeedsAndBaseline",
]
related = ["configuration", "host-foundations"]

[[invariant]]
text = "No box can write the toolbox: it lives in the state directory and every box mounts it read-only at /opt/devc."
refs = ["internal/paths.Paths.ToolboxDir", "internal/paths.InBoxTools"]
tests = ["test/integration.TestEscape"]

[[invariant]]
text = "Of the host, a vendor installer sees only the new version directory it fills (read-write) and the toolbox components its manifest needs (read-only); `current` switches only after a check without network succeeds."
refs = ["internal/toolbox.Installer.Install", "internal/toolbox.Toolbox.NeedsFor"]
tests = ["internal/toolbox.TestInstallScript", "internal/toolbox.TestInstallFailureKeepsCurrent"]

[[invariant]]
text = "`installed.json` and the `current` link always agree for a reader: both change under one lock, and a failed switch restores the old metadata."
refs = ["internal/toolbox.Toolbox.publish", "internal/toolbox.Toolbox.lockPublish"]
tests = ["internal/toolbox.TestInstallPublication"]

[[invariant]]
text = "`harness gc` never removes the current version, the two newest, a version a launch description names, or one current at any time since the oldest running box started."
refs = ["internal/toolbox.Toolbox.GC", "internal/cli.gcKeep"]
tests = ["internal/toolbox.TestGCKeepsVersionsInUse", "internal/toolbox.TestGC"]

[[invariant]]
text = "Installing a harness never changes a box's definition: the mounts a harness contributes depend only on the configuration."
refs = ["internal/harness.BoxHooks.Contribute"]
tests = ["internal/harness.TestContribute", "test/integration.TestHarnessInstall"]

[[invariant]]
text = "Rendered settings and instructions reach a box only through a read-only mount of a directory the host writes, and in them devc's required keys win over the user's baseline."
refs = ["internal/harness.BoxHooks.renderBaseline", "internal/harness.Merge"]
tests = ["internal/harness.TestClaudeSettingsMerge", "test/integration.TestHarnessSeedsAndBaseline"]

[[invariant]]
text = "A token or key credential reaches a box only as a 0400 file in its secrets directory (memory-backed unless `DEVC_ALLOW_PERSISTENT_SECRETS=1`) or from the daemon, never through podman's arguments or environment."
refs = ["internal/harness.BoxHooks.deliverSecrets", "internal/secrets.Deliver"]
tests = ["internal/harness.TestPrepareAndCleanup", "test/integration.TestHarnessLaunchAndSecrets"]

[[invariant]]
text = "No shim can take one of devc's own names (`devc`, `devc-*`, `git-remote-devc`), so a manifest cannot hijack the binary's dispatch."
refs = ["internal/toolbox.Toolbox.EnsureShim", "internal/config.validateHarness"]
tests = ["internal/toolbox.TestSubstituteArgsAndShims"]

[[diagram]]
id = "toolbox-parts"
title = "What the toolbox holds and who sees it"
kind = "graph"

[[diagram.node]]
id = "tools"
label = "tmux, node, restic, skills"
col = 1
row = 1
ref = "internal/toolbox.Toolbox.List"

[[diagram.node]]
id = "bin"
label = "bin/devc, shims/<bin>"
col = 1
row = 2
ref = "internal/toolbox.Toolbox.EnsureShim"

[[diagram.node]]
id = "versions"
label = "harness/<id>/v/<stamp>/"
col = 1
row = 3
ref = "internal/toolbox.Toolbox.HarnessDir"

[[diagram.node]]
id = "helper-rootfs"
label = "helper-rootfs/bin/devc"
col = 1
row = 4
ref = "internal/toolbox.Toolbox.HelperRootfs"

[[diagram.node]]
id = "seccomp"
label = "seccomp.json"
col = 1
row = 5
ref = "internal/toolbox.SeccompProfile"

[[diagram.node]]
id = "box"
label = "every box: /opt/devc"
col = 3
row = 2
zone = "box"
ref = "internal/paths.InBoxTools"

[[diagram.node]]
id = "installer"
label = "installer container"
col = 3
row = 3
zone = "aux"
ref = "internal/toolbox.Installer.Install"

[[diagram.node]]
id = "helper"
label = "helper container: /"
col = 3
row = 4
zone = "aux"
ref = "internal/engine.RunOptions"

[[diagram.node]]
id = "containers"
label = "boxes and aux containers"
col = 3
row = 5

[[diagram.edge]]
from = "tools"
to = "box"
label = "/opt/devc/…"

[[diagram.edge]]
from = "bin"
to = "box"
label = "the whole tree, read-only"

[[diagram.edge]]
from = "versions"
to = "box"
label = "the harness a session runs"

[[diagram.edge]]
from = "versions"
to = "installer"
label = "one, read-write"

[[diagram.edge]]
from = "helper-rootfs"
to = "helper"
label = "--rootfs …:O"

[[diagram.edge]]
from = "seccomp"
to = "containers"
label = "seccomp=…"

[[diagram]]
id = "harness-version"
title = "The life of a harness version"
kind = "graph"

[[diagram.node]]
id = "installing"
label = "installing"
col = 1
row = 1
ref = "internal/toolbox.Installer.Install"

[[diagram.node]]
id = "checking"
label = "checking"
col = 2
row = 1
ref = "internal/toolbox.checkScript"

[[diagram.node]]
id = "current"
label = "current"
col = 3
row = 1
ref = "internal/toolbox.Toolbox.publish"

[[diagram.node]]
id = "deleted"
label = "deleted"
col = 1
row = 2
ref = "internal/toolbox.removeTree"

[[diagram.node]]
id = "removed"
label = "removed"
col = 2
row = 2
ref = "internal/toolbox.Toolbox.GC"

[[diagram.node]]
id = "superseded"
label = "superseded"
col = 3
row = 2
ref = "internal/toolbox.GCKeep"

[[diagram.edge]]
from = "installing"
to = "checking"
label = "installer exits 0"

[[diagram.edge]]
from = "installing"
to = "deleted"
label = "installer fails"

[[diagram.edge]]
from = "checking"
to = "current"
label = "the exe runs"

[[diagram.edge]]
from = "checking"
to = "deleted"
label = "check fails"

[[diagram.edge]]
from = "current"
to = "superseded"
label = "a newer install"

[[diagram.edge]]
from = "superseded"
to = "removed"
label = "gc, nothing keeps it"
+++
## What it is for

A box runs the project's own image, which knows nothing of devc. Yet every box needs devc's in-box helper, a tmux that works with the harnesses, and the harnesses themselves (Claude Code, Codex, Cursor's CLI, OpenCode, pi), each with settings devc requires and a credential you provided once. The box must not be able to change any of it, and a session keeps the version it started with while a newer one is installed. The toolbox holds the programs; harness manifests describe everything harness-specific, so a new harness is a file, not new code.

## How it works

**The toolbox.** [[term:toolbox]] is `<state>/toolbox`, mounted read-only at `/opt/devc` in every box. [[internal/toolbox.Toolbox.Sync]] makes it match the running devc: the binary under its build ID (`devc/<version>-<sha12>/devc`), the links `devc/current`, `bin/devc` and `bin/git-remote-devc`, a hard link (or copy) of the binary as the only file of `helper-rootfs/`, the box seccomp profile and the built-in skills. Sync runs whenever devc is about to rely on the toolbox (a box's start, a helper, context or installer container, `devc toolbox sync`, `devc doctor --fix`). Other parts are installed on request: a static tmux built from a pinned release in a throwaway Alpine container ([[internal/toolbox.Toolbox.BuildTmux]]), and pinned releases of restic and Node.js, downloaded once and verified against SHA-256 sums compiled into devc. Everything is world-readable, because in-box root without capabilities maps to a subordinate UID and must still run it. [[diagram:toolbox-parts|The picture below]] shows which part each container sees; the login container mounts the whole toolbox read-only, like a box.

**Manifests.** A [[term:harness]] is described by a [[internal/config.Harness]] ([[surface:config-harness]]): how it is installed (`install`), where its state lives and whether a realm shares it (`home`), how it logs in (`auth`), what settings and instructions it must get (`baseline`), how it is started (`launch`, `env`), which hooks report its events, how input reaches it. Five manifests are built in ([[pkg:assets]]); the configuration chapter explains how a user file replaces one ([[concern:configuration]]).

**Installing.** [[flow:harness-install]] shows it step by step. Each installation is a new directory `harness/<id>/v/<stamp>/`, which the vendor's installer fills in a container that sees it and, read-only, the components the manifest needs; a second container without network checks that `install.exe` runs. Only then does [[internal/toolbox.Toolbox.publish]] write `installed.json`, switch `current` and append the switch to `history.jsonl`; a failure before deletes the new directory. [[diagram:harness-version|The life of a version]] is drawn below.

**What a box gets.** [[internal/harness.BoxHooks]] plugs into the box's life cycle. When the box's arguments are built, [[internal/harness.BoxHooks.Contribute]] adds the mounts: the rendered baseline, read-only, at the directory of the manifest's baseline paths; the harness state of the home strategy (with `private`, the listed subdirectories come from realm state; with `shared`, the whole harness home does, and `per_box` subdirectories stay per box); and the secrets directory at `/run/devc-secrets`, always, even when empty. When the box is started, [[internal/harness.BoxHooks.Prepare]] renders the baselines, writes a launch description and seed files for every installed harness, delivers the credentials, makes sure the shims exist and removes what belongs to harnesses the project no longer lists.

**The baseline.** [[internal/harness.Settings]] takes the user's `baseline/<id>/settings.json` or `.toml` and merges over it the keys the manifest's template requires ([[internal/harness.Merge]]): devc's value wins for every key it sets, objects merge, lists are joined with the user's entries first and without repeating one of devc's. For Claude Code that file is `/etc/claude-code/managed-settings.json`: permission prompts off, updates off, the memory directory, and the hooks that report sessions. [[internal/harness.Instructions]] puts the generated description of the sandbox (template `sandbox.md.tmpl`) first, then `baseline/instructions.md`, `baseline/<id>/instructions.md` and the realm's `instructions.md`.

**Launch descriptions.** [[internal/harness.Describe]] writes what the in-box launcher needs into `/etc/devc/harness/<id>.json`: the executable in the concrete version directory, arguments, environment, where the credential file is and which variable receives it, the home strategy, seeds, granted secrets, hooks and input settings. The description is validated before it is written. A shim is a link `/opt/devc/shims/<bin>` to devc; started under that name, devc looks the name up among the descriptions and runs the launcher ([[flow:binary-dispatch]]).

**Logins.** [[flow:harness-login]] shows `devc login <realm> <harness>`. The method comes from the manifest or the realm's override ([[internal/harness.EffectiveAuth]]): `env-token` runs the vendor's login in a throwaway container with a temporary home and stores the token you paste as a realm secret; `api-key` only asks for the key; `shared-home` lets the vendor write its own credential file into the realm's shared harness home. Token and key reach a box at its next start ([[concern:secrets]]), and the launcher exports them under the manifest's `auth.env`.

**Versions.** `devc harness install` and `devc update` rewrite the launch description of every box that has one, under the box's lock ([[internal/harness.BoxHooks.RefreshLaunches]]), so the next start in a running box gets the new version while running sessions keep theirs. [[internal/toolbox.Toolbox.GC]] removes versions nothing may use any more; `devc harness gc` refuses to remove anything when it cannot list the running boxes.

## What can go wrong, and what prevents it

- **A box changes its tools or settings.** The toolbox, `/etc/devc` (descriptions, seeds) and the baseline directory are read-only mounts of host directories. Validation refuses baseline directories devc or the system needs, such as `/etc/devc`, `/opt/devc`, `/home` or `/usr` ([[internal/config.reservedBaselineDir]]).
- **A vendor installer misbehaves.** It runs with no new privileges, the box seccomp profile, `--http-proxy=false` and only its version directory (read-write) and the components it needs (read-only) mounted; its result is checked in a container without network. It does run with network and as the container's root, which rootless Podman maps to the host user: the user trusts the vendor's script as far as the container lets it act.
- **Shared harness state.** With strategy `shared`, every box of a realm mounts the same home, so one box can change what the others read, the vendor's credential file included; the realm is the sharing boundary (spec rule R5). Homes that nest are refused ([[internal/harness.BoxHooks.Contribute]]), because a writable mount's source inside another box-writable tree could be redirected by a planted symlink.
- **Credentials leak.** Token and key credentials are written only to the box's secrets directory, which must be memory-backed unless `DEVC_ALLOW_PERSISTENT_SECRETS=1`, as 0400 files by rename, and removed at `down` and `rm` ([[internal/harness.BoxHooks.Cleanup]]). A `shared-home` credential is the vendor's file in the shared harness home, durable state the realm's boxes can write (above). The agent can read its own model credential; the spec accepts that.
- **gc removes a version a session runs.** See the invariants; the keep rules use the descriptions and the switch history.

## Decisions that are not obvious

- **Concrete version directories.** Descriptions name `v/<stamp>`, not `current`, so an installation never changes the files a running session has open ([[finding:E1]]).
- **Mounts from configuration only.** Whether a harness is installed changes the launch descriptions, never the box's mounts, so installing does not make every box drift.
- **tmpfs at /home, /opt and /tmp in installer containers.** The directories above devc's bind mounts are then not image-layer directories, which also avoids a fuse-overlayfs bug in which `statx` hangs ([[internal/toolbox.InstallerTmpfs]]).
- **Logins never run on the host.** The vendor's login command runs in the installer image with a temporary home, so it can leave nothing in the user's own home.

## Where to start reading

1. [[internal/toolbox.Toolbox.Sync]]
2. [[internal/toolbox.Installer.Install]] and [[internal/toolbox.Toolbox.publish]]
3. [[internal/harness.BoxHooks.Contribute]] and [[internal/harness.BoxHooks.Prepare]]
4. [[internal/harness.Settings]] and [[internal/harness.Describe]]
5. [[internal/harness.LoginRunner.options]]
6. [[internal/toolbox.Toolbox.GC]]
