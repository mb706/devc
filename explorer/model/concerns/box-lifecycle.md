+++
id = "box-lifecycle"
title = "Box life cycle"
summary = "From a project's configuration to a running container and back: the box definition and its hash, the image, what the host writes before a start, `up` with its checks and rollback, `down` and `rm`, drift, the per-box lock, and the record of what a start was given."
order = 20
packages = ["internal/box", "internal/engine"]
files = ["internal/cli/boxcmds.go", "internal/boxside/helper.go"]
key = [
  "internal/box.Manager",
  "internal/box.Manager.Up",
  "internal/box.BuildSpec",
  "internal/box.Spec.Hash",
  "internal/box.Manager.publish",
  "internal/box.Manager.startBox",
  "internal/box.Manager.afterStart",
  "internal/box.Manager.Down",
  "internal/box.Manager.Remove",
  "internal/box.LockBox",
  "internal/box.Capabilities",
  "internal/engine.Engine",
]
spec = ["doc:specs-v2.md#6.4", "doc:specs-v2.md#6.2", "doc:specs-v2.md#6.5"]
tests = [
  "internal/box.TestUpCreatesAndIsIdempotent",
  "internal/box.TestUpDrift",
  "internal/box.TestDriftWritesNothing",
  "internal/box.TestUpRollsBack",
  "internal/box.TestRunningButNotReady",
  "internal/daemon.TestStartAndStop",
  "internal/box.TestBoxLock",
  "internal/box.TestStopRefused",
  "internal/box.TestRemoveWithHome",
  "internal/box.TestCapabilities",
  "internal/engine.TestLifecycleCalls",
  "test/integration.TestBoxLifecycle",
  "test/integration.TestBoxDrift",
  "test/integration.TestRecreateKeepsState",
]
related = ["configuration", "toolbox-and-harnesses", "host-foundations", "sessions-and-events"]

[[invariant]]
text = "No box is created or started while its project's configuration has problems."
refs = ["internal/box.Manager.checkProblems", "internal/box.Manager.Up", "internal/box.Manager.EnsureRunning"]
tests = ["internal/box.TestProblemsBlockLifecycle", "internal/box.TestBrokenOverrideBlocksUp"]

[[invariant]]
text = "The box manager never acts on a container that has a box's name but lacks devc's labels for that project."
refs = ["internal/box.Manager.Find"]
tests = ["internal/box.TestForeignContainerIsRefused"]

[[invariant]]
text = "Plain `up` never starts or changes a box whose definition hash differs, and writes nothing for it."
refs = ["internal/box.Manager.up", "internal/box.Spec.Hash"]
tests = ["internal/box.TestUpDrift", "internal/box.TestDriftWritesNothing", "test/integration.TestBoxDrift"]

[[invariant]]
text = "A failed creation or start leaves no container that command created, and no delivered secrets."
refs = ["internal/box.Manager.rollback", "internal/box.Manager.createAndStart"]
tests = ["internal/box.TestUpRollsBack"]

[[invariant]]
text = "Two lifecycle operations on one box never run at the same time."
refs = ["internal/box.LockBox"]
tests = ["internal/box.TestBoxLock"]

[[invariant]]
text = "A grant a box gets on demand never exceeds what the configuration grants now, nor its capability record, which only the start that ran that container writes; without a record for its container ID it gets no grant."
refs = ["internal/box.Manager.recordCapabilities", "internal/box.Manager.cleanup", "internal/box.Capabilities.AllowsSecret", "internal/box.Capabilities.SSHGrant", "internal/daemon.Daemon.startedWith"]
tests = ["internal/box.TestCapabilities", "internal/box.TestUpCreatesAndIsIdempotent"]

[[invariant]]
text = "No command that finds a box running writes its capability record, whatever the box did to its readiness marker; a running box without the marker, or without a record for its container, is refused with a request to restart it, by `up`, `sh`, `run`, `root` and every session start, and nothing runs in it."
refs = ["internal/box.CheckStarted", "internal/box.Manager.up", "internal/box.Manager.ensureRunning", "internal/daemon.commander.startRefusal"]
tests = ["internal/box.TestRunningButNotReady", "internal/daemon.TestStartAndStop"]

[[invariant]]
text = "`down`, `rm` and a recreation leave a running box running when banking its work fails, unless `--no-bank` was given; with configuration problems `down` and `rm` stop it unbanked, after a warning."
refs = ["internal/box.Manager.beforeStop", "internal/box.ErrKeepRunning", "internal/cli.bankHook.BeforeStop"]
tests = ["internal/box.TestStopRefused"]

[[invariant]]
text = "Host code never walks a box home it deletes: `rm --home` empties it from the helper container."
refs = ["internal/box.Manager.clearInHelper", "internal/boxside.ClearDir", "internal/boxside.RequireHelper"]
tests = ["internal/box.TestRemoveWithHome", "internal/boxside.TestClearDir"]

[[diagram]]
id = "box-states"
title = "The states of a box"
kind = "graph"

[[diagram.node]]
id = "absent"
label = "absent"
col = 1
row = 2

[[diagram.node]]
id = "created"
label = "created"
col = 2
row = 1
ref = "internal/engine.Podman.Create"

[[diagram.node]]
id = "initialized"
label = "initialized, rules in place"
col = 3
row = 1
ref = "internal/engine.Podman.Init"

[[diagram.node]]
id = "starting"
label = "running, not ready"
col = 3
row = 2
zone = "box"
ref = "internal/boxside.Init"

[[diagram.node]]
id = "ready"
label = "ready"
col = 4
row = 2
zone = "box"

[[diagram.node]]
id = "stopped"
label = "stopped"
col = 3
row = 3

[[diagram.node]]
id = "drifted"
label = "drifted: up refuses"
col = 2
row = 3
ref = "internal/box.ErrDrift"

[[diagram.edge]]
from = "absent"
to = "created"
label = "up: create"
ref = "internal/box.Manager.createAndStart"

[[diagram.edge]]
from = "created"
to = "initialized"
label = "level internet: init"
ref = "internal/box.Manager.startBox"

[[diagram.edge]]
from = "created"
to = "starting"
label = "start"
ref = "internal/engine.Podman.Start"

[[diagram.edge]]
from = "initialized"
to = "starting"
label = "start"
ref = "internal/engine.Podman.Start"

[[diagram.edge]]
from = "starting"
to = "ready"
label = "marker seen, checks pass"
ref = "internal/box.Manager.afterStart"

[[diagram.edge]]
from = "starting"
to = "absent"
label = "fails; created now: rm"
ref = "internal/box.Manager.rollback"

[[diagram.edge]]
from = "starting"
to = "stopped"
label = "fails; existed: stopped"
ref = "internal/box.Manager.startStopped"

[[diagram.edge]]
from = "starting"
to = "starting"
label = "found running: refused, kept"
ref = "internal/box.CheckStarted"

[[diagram.edge]]
from = "ready"
to = "stopped"
label = "down, banked first"
ref = "internal/box.Manager.Down"

[[diagram.edge]]
from = "stopped"
to = "starting"
label = "up: start"
ref = "internal/box.Manager.startStopped"

[[diagram.edge]]
from = "stopped"
to = "absent"
label = "rm"
ref = "internal/box.Manager.Remove"

[[diagram.edge]]
from = "stopped"
to = "drifted"
label = "definition changed"
ref = "internal/box.Spec.Hash"

[[diagram.edge]]
from = "drifted"
to = "absent"
label = "up --recreate, rm"
ref = "internal/box.Manager.up"
+++
## What it is for

A box is a project's container, `devc-<realm>-<project>` ([[term:box]]). One command brings it up with the definition the configuration describes, the same every time, so that a change of configuration, image or hardening is noticed, neither silently applied nor ignored. Removing the container loses no project files, home or state; a start that fails half-way leaves nothing behind; two devc commands never work on one box at once. This concern is that life cycle and the engine wrapper under it; [[concern:isolation]] explains what the definition restricts.

## How it works

**The engine.** All container work goes through [[internal/engine.Engine]]. Its production implementation, [[internal/engine.Podman]], runs the `podman` binary through [[pkg:internal/execx]] with an argument vector, from `/`, and parses its JSON. Container names, image references, labels and environment names are checked so that none can be read as an option. [[internal/engine.Fake]] stands in for Podman in unit tests ([[concern:testing-and-tools]]).

**The definition.** [[internal/box.BuildSpec]] renders a [[internal/box.Spec]] from [[internal/box.Inputs]]: the resolved configuration, the image the engine reports, the host's facts (whether SELinux is present, which cgroup controllers are delegated) and the seccomp profile. Nothing in it comes from a place a box can write. Other packages add mounts, environment and labels through [[internal/box.Contributor]]s (harness, gate, shares, data); `AddMount`, `AddEnv` and `AddLabel` validate what they add. [[internal/box.Spec.Args]] is the full argument vector of `podman create`, and [[internal/box.Spec.Hash]] the SHA-256 of the arguments, the image reference, the image ID and the profile; it becomes the label `devc.confighash`. The image comes from [[internal/box.SourceFor]]: the project's `image`, or its env's, built from a Containerfile (`localhost/devc-env/<env>:latest`) or pulled.

**up.** [[flow:box-up]] shows it step by step. [[internal/box.Manager.Up]] refuses while the configuration has problems, takes the box lock and computes the plan (toolbox sync, image, definition). Then it decides from the existing container ([[internal/box.Manager.up]]): it creates a missing one and starts a stopped one; a drifted one (another hash) it refuses with exit status 1 unless `--recreate`, which stops and removes the old container first; a running one it leaves alone (at level internet only after verifying its firewall rules, else it restarts it) and writes nothing for, but refuses it without its readiness marker or a capability record of its container ID ([[internal/box.CheckStarted]]).

Only a path that creates or starts the box writes anything ([[internal/box.Manager.publish]]): what the box will see, the mount sources and nested mount points; the capability record, the network binding and the readiness marker of the previous start are removed. Then come `podman create`, [[internal/box.Manager.startBox]] (at level internet `podman init` and the rules before `podman start`) and [[internal/box.Manager.afterStart]]: the identity fix, the wait for the readiness marker, the canary at level internet, the capability record and the start hooks. A failure is rolled back ([[internal/box.Manager.rollback]]): a container this command created is removed, one it started is stopped, the delivered secrets go. [[diagram:box-states]] shows the states; a box drifts in any state, drawn only from stopped.

**The box lock** ([[term:box-lock]]). [[internal/box.LockBox]] serialises everything that starts, stops or changes a box, git operations and network exceptions included, waiting up to ten minutes, because an `up` may be building an image; `ls` takes no lock.

**Running something in a box.** `sh`, `run` and `root` start the box through the same path as `up` when it is not running ([[internal/box.Manager.ensureRunning]]), then release the lock and run `podman exec`: a login shell or a command as `dev`, in the box's equivalent of the current directory. A running box is used as started, even when drifted, once [[internal/box.CheckStarted]] passes. [[internal/box.Manager.Root]] runs as root with full capabilities (`podman exec --user 0 --privileged`, in the environment [[concern:isolation]] describes) and logs the command in the env's directory ([[concern:configuration]]).

**down and rm.** [[flow:box-down]] shows `down`, which works even while the configuration has problems: the stop hooks ingest the box's events and bank its work, and a bank that fails keeps the box running ([[internal/box.ErrKeepRunning]]), except with configuration problems, when nothing is banked ([[finding:B2]]). After the stop the open sessions end, and the capability record, the delivered secrets and the gate key go; home, project directory, durable state and network exceptions stay. [[internal/box.Manager.Remove]] does the same with `podman rm --force` and drops the network exceptions; for a container already gone it skips the after-stop hooks, so open sessions stay open ([[doc:docs/dev/LEFTOVER.md#boxes-and-the-box-side-x5]]). `--home` empties the home from a helper container; `rebuild` is `up --recreate --build`.

**The capability record.** When a start through devc that created or started the container succeeds, [[internal/box.Manager.afterStart]] has [[internal/box.Manager.recordCapabilities]] write `capabilities.json` beside the home ([[internal/box.CapabilitiesOf]]): the container ID and the secrets, SSH hosts, network level and allowlist, GitHub grant and services the configuration grants at that moment. The daemon and `gate-exec` hand grants out only for the intersection of that record and the current configuration ([[internal/box.Capabilities.AllowsSecret]], [[internal/box.Capabilities.SSHGrant]]), so removing a grant refuses the next hand-out while adding one waits for the next start ([[finding:C1-02]]). devc removes the record before every start and when it stops or removes the box. Intersecting keeps grants added while a box runs from reaching a box whose definition no longer matches.

## What can go wrong, and what prevents it

- **A foreign container with a box's name.** [[internal/box.Manager.Find]] refuses a container without the labels `devc.realm`, `devc.project` and `devc.role=box` for the project; `ls` shows it as `conflict`.
- **A changed definition.** The hash makes it drift; `up` refuses and writes nothing ([[finding:A3]]). A drifted box that is running keeps running with its old definition until the user recreates it.
- **A start that fails half-way.** Rolled back, as above; a box left running without its marker or record (devc killed during a start) is refused until it is restarted.
- **A plain `podman start` outside devc.** A box stopped outside devc keeps its capability record, and a restart keeps the container ID, so grants on demand go on. At level internet box init finds a binding for another start and ends the box. The identity fix and the mount-point check run only on starts through devc.
- **The readiness marker is box-writable.** Every start through devc removes it first, so an old marker never counts, but who writes the new one is not checked; at level internet the host checks the binding and the rules itself ([[internal/box.Manager.verifyRunning]], [[finding:B1]]).
- **A box that removes its marker.** Nothing finishes its start with today's configuration: `up`, `sh`, `run`, `root`, `devc <harness>` and the daemon's session start ([[internal/daemon.commander.startRefusal]]) refuse through [[internal/box.CheckStarted]], asking for a restart, and the record keeps the grants of the start that ran the container ([[doc:docs/dev/LEFTOVER.md#found-by-the-accuracy-review]], item 1). After a host reboot the container is stopped, so `up` starts it properly.
- **Deleting a box-written tree.** `rm --home` never walks the home on the host and refuses one not below the state directory.

## Decisions that are not obvious

- **Drift is refused, not applied.** Recreating ends the sessions and banks the work first, so the user chooses when.
- **The seccomp profile is in the hash.** The engine reads the profile only at creation; a devc upgrade that changes it shows as drift ([[chunk:C02]]).
- **`up` on a running, ready box writes nothing**, so a new secret or baseline reaches the box only with its next start ([[finding:A3]]).

## Where to start reading

1. [[internal/box.Manager.up]]
2. [[internal/box.BuildSpec]] and [[internal/box.Spec.Hash]]
3. [[internal/box.Manager.publish]], [[internal/box.Manager.startBox]] and [[internal/box.Manager.afterStart]]
4. [[internal/box.Manager.Down]] and [[internal/box.Manager.Remove]]
5. [[internal/box.CapabilitiesOf]]
6. [[internal/engine.Engine]]
