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
text = "devc never acts on a container that has a box's name but lacks devc's labels for that project."
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
text = "What a box gets on demand never exceeds what its last successful start was given, nor what the configuration grants now; without such a start it gets nothing."
refs = ["internal/box.Manager.recordCapabilities", "internal/box.Manager.cleanup", "internal/box.Capabilities.AllowsSecret", "internal/box.Capabilities.SSHGrant"]
tests = ["internal/box.TestCapabilities", "internal/box.TestUpCreatesAndIsIdempotent"]

[[invariant]]
text = "`down`, `rm` and a recreation leave a running box running when its work could not be banked, unless `--no-bank` was given."
refs = ["internal/box.Manager.beforeStop", "internal/box.ErrKeepRunning"]
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
label = "a check fails: removed"
ref = "internal/box.Manager.rollback"

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

A box is a project's container, `devc-<realm>-<project>` ([[term:box]]). The user wants one command that brings it up with the definition the configuration describes, and the same definition every time, so that a change in the configuration, the image or devc's hardening is noticed instead of being silently applied or silently ignored. Removing the container must not lose the project files, the home or any state. A start that fails half-way must leave nothing behind, and two devc commands must never work on one box at once. This concern is that life cycle and the engine wrapper under it; [[concern:isolation]] explains what the definition restricts.

## How it works

**The engine.** All container work goes through [[internal/engine.Engine]]. Its production implementation, [[internal/engine.Podman]], runs the `podman` binary through [[pkg:internal/execx]] with an argument vector, from `/`, and parses its JSON. Container names, image references, labels and environment names are checked so that none can be read as an option. Queries have 30 seconds, lifecycle verbs two minutes, builds two hours and pulls 30 minutes. [[internal/engine.Fake]] stands in for Podman in unit tests. Throwaway containers ([[internal/engine.RunOptions]]) serve the helper, context and installer containers.

**The definition.** [[internal/box.BuildSpec]] renders a [[internal/box.Spec]] from [[internal/box.Inputs]]: the resolved configuration, the image the engine reports, the host's facts (whether SELinux is present, which cgroup controllers are delegated) and the seccomp profile. Nothing in it comes from a place a box can write. Other packages add mounts, environment and labels through [[internal/box.Contributor]]s (harness, gate, shares, data); `AddMount`, `AddEnv` and `AddLabel` validate what they add. [[internal/box.Spec.Args]] is the full argument vector of `podman create`, and [[internal/box.Spec.Hash]] the SHA-256 of the arguments, the image reference, the image ID and the profile; it becomes the label `devc.confighash`. The image comes from [[internal/box.SourceFor]]: a project's `image`, an env with a Containerfile (built as `localhost/devc-env/<env>:latest`), or an env's base image, pulled when missing.

**up.** [[flow:box-up]] shows it step by step. [[internal/box.Manager.Up]] refuses while the configuration has problems, takes the box lock and computes the plan (toolbox sync, image, definition). Then it decides from the existing container ([[internal/box.Manager.up]]):

- no container: create and start it;
- `--recreate`: a running box goes through the stop hooks first, then the container is removed and created again;
- another hash: the box has drifted; `up` says so and exits 1;
- running and ready: nothing to do (at level internet only if the host can verify the box's firewall rules; otherwise it is stopped and started again);
- running but never reported ready: the rest of its start runs, without a restart;
- otherwise: start it.

Only a path that creates or starts the box writes anything ([[internal/box.Manager.publish]]): `/etc/devc`, the preparers' files (baselines, launch descriptions, secrets), the mount sources and nested mount points, a `restorecon` of the project on SELinux hosts in labels mode; the capability record, the network binding and the readiness marker of the previous start are removed. Then come `podman create`, [[internal/box.Manager.startBox]] (at level internet `podman init` and the rules before `podman start`) and [[internal/box.Manager.afterStart]]: the identity fix as the box's root, the wait for box init's marker (100 ms polls, 20 seconds), the outside network check at level internet, the capability record, and the start hooks. A failure is rolled back ([[internal/box.Manager.rollback]]): the container this command created is removed, one it started is stopped, and the cleaners remove the delivered secrets. [[diagram:box-states]] shows the states this moves a box through; a box drifts in any state, and the picture draws it only from stopped.

**The box lock.** [[internal/box.LockBox]] takes an exclusive `flock` on `local/boxes/<realm>/<project>/lock` and waits up to ten minutes, because an `up` may be building an image. `up`, `down`, `rm`, `rebuild`, [[internal/box.Manager.EnsureRunning]] (behind `sh`, `run`, `root` and sessions), [[internal/box.Manager.WithRunningBox]], git operations and changes of network exceptions hold it; `ls` does not.

**Running something in a box.** `sh`, `run` and `root` start the box through the same path as `up` when it is not running and ready, then release the lock and run `podman exec`: a login shell or a command as `dev`, in the box's equivalent of the current directory. A running, ready box is used even when it has drifted. [[internal/box.Manager.Root]] runs as root with full capabilities (`podman exec --user 0 --privileged`), with a fixed system `PATH`, `HOME=/root` and `/` as working directory, and first appends the command to the env's `root-log.jsonl`.

**down and rm.** [[flow:box-down]] shows `down`. The stop hooks ingest the box's events and bank its work; a bank that fails keeps the box running ([[internal/box.ErrKeepRunning]]). The desktop sidecar goes, then `podman stop` with ten seconds' grace, then the hooks end the open sessions and commit the realm's context, and last the capability record, the delivered secrets and the gate key are removed. Home, project directory, durable state and network exceptions stay. `down` works even while the configuration has problems. [[internal/box.Manager.Remove]] does the same with `podman rm --force`, and also drops the network exceptions; `--home` empties the home from a helper container ([[internal/box.Manager.clearInHelper]]: no network, only the home mounted, only the capabilities `DAC_OVERRIDE` and `FOWNER`) and then removes the empty directory. `rebuild` is `up --recreate --build`.

**The capability record.** At the end of every successful start [[internal/box.CapabilitiesOf]] writes `capabilities.json` beside the home: the container ID and the secrets, SSH hosts, network level and allowlist, GitHub grant and services the start was given. The daemon and `gate-exec` hand things out only for the intersection of that record and the current configuration ([[internal/box.Capabilities.AllowsSecret]], [[internal/box.Capabilities.SSHGrant]]), so removing a grant takes effect at once while adding one needs a new start ([[finding:C1-02]]).

## What can go wrong, and what prevents it

- **A foreign container with a box's name.** [[internal/box.Manager.Find]] refuses a container without the labels `devc.realm`, `devc.project` and `devc.role=box` for the project; `ls` shows it as `conflict`.
- **A changed definition.** The hash makes it drift; `up` refuses and writes nothing ([[finding:A3]]). A drifted box that is running keeps running with its old definition until the user recreates it.
- **A start that fails half-way.** Rolled back, as above; a running box that `up` did not start itself is left running with a message ([[chunk:C03r]]).
- **A plain `podman start` outside devc.** No capability record exists for it, so the box gets no grants on demand; at level internet box init finds no valid binding and ends the box. The identity fix and the mount-point check run only on starts through devc.
- **A planted symlink at a nested mount point.** [[internal/box.prepareMountPoints]] creates and checks each component beneath the parent's source without following links, just before the start.
- **The readiness marker is box-writable.** Every start through devc removes it first, so for that start only box init writes it; for an already running box at level internet the host checks the binding and the rules itself ([[internal/box.Manager.verifyRunning]], [[finding:B1]]).
- **Deleting a box-written tree.** `rm --home` never walks it on the host, and refuses a home path that is not below the state directory.

## Decisions that are not obvious

- **Drift is refused, not applied.** Recreating ends the sessions and banks the work first, so the user chooses when.
- **The seccomp profile is in the hash.** The engine reads the profile only at creation; a devc upgrade that changes it shows as drift ([[chunk:C02]]).
- **`up` on a running box writes nothing**, so a new secret or baseline reaches the box only with its next start ([[finding:A3]]).
- **The record is intersected, not trusted.** Grants added to the configuration while a box runs would otherwise reach a box whose definition no longer matches.
- **Podman through its command line.** No Go bindings; every call is a process with an argument vector, which a fake replaces in tests.

## Where to start reading

1. [[internal/box.Manager.up]]
2. [[internal/box.BuildSpec]] and [[internal/box.Spec.Hash]]
3. [[internal/box.Manager.publish]], [[internal/box.Manager.startBox]] and [[internal/box.Manager.afterStart]]
4. [[internal/box.Manager.Down]] and [[internal/box.Manager.Remove]]
5. [[internal/box.CapabilitiesOf]]
6. [[internal/engine.Engine]]
