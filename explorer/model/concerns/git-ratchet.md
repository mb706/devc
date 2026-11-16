+++
id = "git-ratchet"
title = "The git ratchet"
summary = "Why commits that reached the host stay there whatever a box does: git lives in the box, the relay on the host, banks and the relay service write the relay only through checks that record every tip, and nothing a box sends moves or deletes a record."
order = 40
files = [
  "internal/gitx/host.go",
  "internal/gitx/relay.go",
  "internal/gitx/policy.go",
  "internal/gitx/bank.go",
  "internal/gitx/box.go",
  "internal/gitx/relayservice.go",
  "internal/gitx/archive.go",
  "internal/daemon/relay.go",
  "internal/cli/relaycmds.go",
  "internal/boxside/remotehelper.go",
]
key = [
  "internal/gitx.Host.Cmd",
  "internal/gitx.Service.Bank",
  "internal/gitx.PlanBank",
  "internal/gitx.Relay.Update",
  "internal/boxside.RemoteHelperMain",
  "internal/daemon.boxWorker.serveGit",
  "internal/gitx.ServeCall",
  "internal/cli.app.relayHook",
  "internal/gitx.CheckUpdate",
  "internal/gitx.Service.RecordUpdate",
  "internal/daemon.Daemon.runPush",
]
spec = ["doc:specs-v2.md#9.2", "doc:specs-v2.md#9.3"]
tests = [
  "internal/gitx.TestCheckUpdate",
  "internal/gitx.TestServeCall",
  "internal/gitx.TestHostCmd",
  "internal/gitx.TestPlanBank",
  "internal/gitx.TestRecordPush",
  "internal/gitx.TestEndToEnd",
  "internal/cli.TestRelayHooksRecordFirst",
  "internal/daemon.TestRelayStreams",
  "internal/daemon.TestPushWatchdog",
  "internal/daemon.TestPushEndedDuringIndexPack",
  "internal/daemon.TestPushGroupLeft",
  "test/integration.TestRelayService",
  "test/integration.TestEscape",
]
related = ["box-channel", "daemon", "box-lifecycle", "host-foundations"]

[[invariant]]
text = "Bank refs, snapshot refs and tag records are only ever created: every write of one requires that it does not exist yet, and no code deletes or moves them."
refs = ["internal/gitx.Service.Bank", "internal/gitx.Service.RecordUpdate", "internal/gitx.Service.syncHeads", "internal/gitx.Relay.RestoreRefs", "internal/gitx.Relay.Update"]
tests = ["internal/cli.TestRelayHooksRecordFirst", "test/integration.TestEscape"]

[[invariant]]
text = "Every tip a bank, a push, a pull or an adoption gives a relay branch is the target of one of that branch's bank refs."
refs = ["internal/gitx.PlanBank", "internal/gitx.Service.RecordUpdate", "internal/gitx.Service.syncHeads"]
tests = ["internal/gitx.TestPlanBank", "internal/gitx.TestRecordPush", "internal/cli.TestRelayHooksRecordFirst"]

[[invariant]]
text = "Nothing a box pushes or offers a bank deletes a relay ref, moves a protected branch or moves a tag."
refs = ["internal/gitx.CheckUpdate", "internal/gitx.ServeCall", "internal/gitx.PlanBank"]
tests = ["internal/gitx.TestCheckUpdate", "test/integration.TestEscape"]

[[invariant]]
text = "git applies a pushed ref only after the update hook has recorded it; a record that cannot be written refuses the ref."
refs = ["internal/cli.app.relayHook", "internal/gitx.Service.RecordUpdate"]
tests = ["internal/cli.TestRelayHooksRecordFirst"]

[[invariant]]
text = "No git command devc runs on a relay collects garbage or runs maintenance, so no object that reached a relay is removed."
refs = ["internal/gitx.relayConfig", "internal/gitx.ServeCall", "internal/gitx.Service.fetchFromBox"]

[[invariant]]
text = "No hook of a box repository or of the user's configuration runs on the host; only receive-pack for a box's push runs devc's three relay hooks."
refs = ["internal/gitx.Host.Cmd", "internal/gitx.EnsureEmptyDir", "internal/gitx.EnsureRelayHooks"]
tests = ["internal/gitx.TestHostCmd", "internal/gitx.TestEnsureRelayHooks", "test/integration.TestEscape"]

[[invariant]]
text = "A push that would take the relay over git.relay_quota or the state file system below 512 MiB free is ended, and its git processes are gone before the box lock is released."
refs = ["internal/daemon.Daemon.watchPush", "internal/daemon.Daemon.runPush", "internal/daemon.Daemon.holdRelay"]
tests = ["internal/daemon.TestPushWatchdog", "internal/daemon.TestPushEndedDuringIndexPack", "internal/daemon.TestPushGroupLeft"]

[[invariant]]
text = "The relay a push or fetch is served on is that of the channel's project; nothing the box sends selects it."
refs = ["internal/daemon.boxWorker.serveGit", "internal/daemon.hookEnv"]
tests = ["internal/daemon.TestRelayStreams"]

[[diagram]]
id = "repositories"
title = "The repositories, and which refs pass between them"
kind = "graph"

[[diagram.node]]
id = "box"
label = "box repository (.repo)"
col = 1
row = 2
zone = "box"
ref = "internal/paths.InBoxRepoDir"

[[diagram.node]]
id = "incoming"
label = "incoming/* (scratch)"
col = 2
row = 3
zone = "host"
ref = "internal/gitx.IncomingPrefix"

[[diagram.node]]
id = "heads"
label = "refs/heads/* (relay branches)"
col = 3
row = 2
zone = "host"
ref = "internal/gitx.HeadsPrefix"

[[diagram.node]]
id = "records"
label = "records: bank, snap, tags, boxtip"
col = 3
row = 3
zone = "host"
ref = "internal/gitx.BankPrefix"

[[diagram.node]]
id = "github"
label = "remotes/github/*, tags/*"
col = 3
row = 1
zone = "host"
ref = "internal/gitx.GithubPrefix"

[[diagram.node]]
id = "upstream"
label = "upstream"
col = 4
row = 1
zone = "external"

[[diagram.edge]]
from = "box"
to = "heads"
label = "git push (relay service)"
ref = "internal/daemon.boxWorker.serveGit"

[[diagram.edge]]
from = "heads"
to = "box"
label = "origin/*"
ref = "internal/gitx.Service.reflect"

[[diagram.edge]]
from = "github"
to = "box"
label = "github/*, tags"
ref = "internal/gitx.Service.reflect"

[[diagram.edge]]
from = "box"
to = "incoming"
label = "bank: fetch"
ref = "internal/gitx.Service.fetchFromBox"

[[diagram.edge]]
from = "incoming"
to = "heads"
label = "bank policy"
ref = "internal/gitx.PlanBank"

[[diagram.edge]]
from = "incoming"
to = "records"
label = "snapshots, box tags"
ref = "internal/gitx.PlanBank"

[[diagram.edge]]
from = "heads"
to = "records"
label = "every new tip"
ref = "internal/gitx.Service.RecordUpdate"

[[diagram.edge]]
from = "upstream"
to = "github"
label = "fetch"
ref = "internal/gitx.Service.fetchUpstream"

[[diagram.edge]]
from = "github"
to = "heads"
label = "fast-forward"
ref = "internal/gitx.Service.syncHeads"

[[diagram.edge]]
from = "heads"
to = "upstream"
label = "devc push, forward"
ref = "internal/gitx.Service.pushUpstream"
+++
## What it is for

An agent in a box can do anything to the box's repository: rewrite history, delete branches, wipe a worktree, plant hooks. The owner still wants to leave it alone for hours. The ratchet makes that safe: every tip a relay branch takes, every snapshot of uncommitted work and every tag created in the box that the host records stays reachable from a ref the box can neither move nor delete, and the relay never collects garbage. The box still moves branch heads in the relay, since that is how its work arrives; it cannot make the host forget a value a branch once had.

## How it works

**Three repositories.** The box repository is `/w/<realm>/<project>/.repo` with its worktrees ([[concern:worktrees-and-context]]); host git never opens it. The relay ([[term:relay]]) is a bare repository at `durable/realms/<realm>/relay/<project>.git`, which no box mounts; the box knows it as `origin`, `devc::<realm>/<project>`. The upstream is the URL in project.toml ([[concern:upstream]]). [[diagram:repositories]]

**Host git.** Every git command on a relay goes through [[internal/gitx.Host.Cmd]]: `--git-dir=<relay>`, working directory `/`, `core.hooksPath` pointing at the empty `local/git/no-hooks` (checked empty by [[internal/gitx.EnsureEmptyDir]]), `core.fsmonitor=false`, `--no-replace-objects` and `GIT_NO_REPLACE_OBJECTS=1`. Commands that do not talk to the upstream also lose every `GIT_*` variable and read no global or system configuration (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`), so only the relay's configuration applies. [[internal/gitx.Relay.Configure]] sets it: `gc.auto=0`, `maintenance.auto=false`, `core.logAllRefUpdates=always`, object checks (fsck) on every fetch and receive, and `unpackLimit=1`, so an incoming pack is kept as it came.

**Banking** ([[term:banking]], [[internal/gitx.Service.Bank]]) runs under the box lock. `devc box snapshot --all` commits each worktree's uncommitted work in the box. Host git then fetches over git's `ext::` transport, which runs a command and speaks git's protocol over its standard input and output: here `podman exec -i -u 1000:1000 <box> git-upload-pack <repo>` ([[internal/gitx.BoxURL]]), so whatever the box repository holds runs in the box. The fetch writes only below `refs/devc/incoming/`, emptied before and after. [[internal/gitx.PlanBank]] decides; each branch change is one `update-ref --stdin` transaction of head, bank ref and box tip, with old values checked ([[internal/gitx.Relay.Update]]). Banks run for `devc bank`, after turns, every five minutes, before a box stops, and before `push`, `pr create` and `wt rm` ([[flow:turn-snapshot]]).

**The relay service** serves `git push` and `git fetch` in the box while a daemon is connected ([[flow:box-git-push]], [[flow:box-git-fetch]]). git starts the remote helper [[internal/boxside.RemoteHelperMain]] for `origin`; it asks the host for `host.git` through the agent, then relays bytes. [[internal/daemon.boxWorker.serveGit]] runs upload-pack or receive-pack on the relay of the channel's project with the settings of [[internal/gitx.ServeCall]] (upload-pack hides `refs/devc`); receive-pack runs devc's relay hooks ([[internal/cli.app.relayHook]]). With no host connected the helper is a stub whose push transfers nothing and says that a bank will.

**What git does at a push.** receive-pack writes the pack into a quarantine directory (`objects/tmp_objdir-incoming-*`) and checks every object. It runs `pre-receive` once with every `<old> <new> <ref>` line; a failure refuses the whole push and deletes the quarantine. Otherwise git moves the objects into the relay and runs `update <ref> <old> <new>` per ref, a failure refusing that ref only, before updating it. `post-receive` gets the applied lines; its exit status changes nothing. Hook output reaches the pusher as `remote:` lines.

**Ref namespaces** ([[internal/gitx.HeadsPrefix]] and its neighbours):

| Namespace | Holds | A box can |
|---|---|---|
| `refs/heads/<b>` | relay branches | create, move (also non-fast-forward) unless protected |
| `refs/tags/<t>` | the upstream's tags, tags a box pushed | create, by a push |
| `refs/remotes/github/<b>` | the upstream's branches | nothing |
| `refs/devc/bank/<b>/<time>` | every tip a branch was given | nothing |
| `refs/devc/snap/<w>/<time>` | snapshots of uncommitted work | nothing |
| `refs/devc/tags/<t>/<time>` | tags created in the box | nothing |
| `refs/devc/boxtip/<b>` | the box's tip at the last bank or push | nothing |
| `refs/devc/incoming/`, `refs/devc/restore/` | scratch of a bank, of a restore | nothing |

In a relay, host code deletes only the two scratch namespaces, `refs/remotes/github/HEAD` after an adoption, and upstream branches the upstream deleted (fetch `--prune`).

## What can go wrong, and what prevents it

A box controls its repository, every ref and object it offers, every byte of its `host.git` stream, its snapshot report, and when it stops reading.

- **Rewriting recorded history by push.** git itself refuses updates to the hidden `refs/devc` and `refs/remotes` (`receive.hideRefs`) and branch deletions (`receive.denyDeletes`). `pre-receive` refuses more than 1,000 refs, malformed input, a configuration with problems and a relay over budget. `update` applies [[internal/gitx.CheckUpdate]], then [[internal/gitx.Service.RecordUpdate]] writes the bank ref or tag record with `core.fsync=reference`, or the ref is refused ([[finding:C1-05]]).
- **Rewriting it by bank.** The fetch's refspecs write only `refs/devc/incoming/`, with `--no-tags`. [[internal/gitx.PlanBank]] skips names that fail [[internal/names.CheckRef]] or `git check-ref-format --branch`, keeps protected branches, deletes nothing, records box tags only as `refs/devc/tags/`, and applies at most 1,000 branches and 1,000 tags per run.
- **Changing how history reads.** A `refs/replace/` ref could make a banked commit show other content; host git never interprets one, and a restore creates refs only in devc's namespaces ([[internal/gitx.ArchiveRef]], [[finding:D1-07]]).
- **Code on the host.** `local/git/relay-hooks` is made to hold exactly three links to the daemon's devc binary before every push ([[internal/gitx.EnsureRelayHooks]]); every other host git command runs no hook.
- **The cost of a push.** The daemon waits at most two minutes for the box lock, and refuses when the state file system has less than 1 GiB free or the relay exceeds `git.relay_quota` (default 20 GiB). git refuses a pack over `git.max_push_size` (default 2 GiB, `receive.maxInputSize`); the daemon stops feeding it 64 MiB later. Every 200 ms a watchdog measures the quarantine and ends the push when the relay would exceed its budget or less than 512 MiB stays free ([[internal/daemon.Daemon.watchPush]], [[finding:D1-05]]). receive-pack gets 30 minutes, each hook 15; a box that stops reading for two minutes loses the stream. Sizes must be positive, so no limit can be switched off.
- **A push that is ended.** receive-pack runs in a process group of its own; ending it kills index-pack and the hooks too and waits until none is left ([[internal/daemon.Daemon.runPush]], [[finding:E1-16]]). Only then is the quarantine removed and the lock released. A group still alive after 30 seconds leaves the relay held, refusing pushes until its last process ends ([[internal/daemon.Daemon.holdRelay]], [[finding:F1-01]]).

The budget and the free-space reserve guard the relay service only: a bank's fetch has neither, nor a size limit ([[doc:docs/dev/LEFTOVER.md#7]]).

## Decisions that are not obvious

- **Record before accept.** git runs the update hook after the objects left quarantine, so a bank ref can point at them, and before it updates the ref.
- **Box tips.** A pull can advance a relay branch the box never moved; a bank leaves a branch alone while the box's tip equals `refs/devc/boxtip/<b>` ([[finding:B8]]). A push moves the box tip only when the box's own branch has the pushed tip, so `git push origin wip:feature` says nothing about the box's `feature`.
- **Packs stay packs.** A small pack of deltas cannot expand into loose objects many times its size, so what the watchdog measures while a push arrives is what the relay keeps ([[finding:D1-05]]).

## Where to start reading

1. [[internal/gitx.Host.Cmd]] and [[internal/gitx.ServeCall]]
2. [[internal/gitx.CheckUpdate]] and [[internal/gitx.Service.RecordUpdate]]
3. [[internal/cli.app.relayHook]]
4. [[internal/daemon.boxWorker.serveGit]] and [[internal/daemon.Daemon.runPush]]
5. [[internal/gitx.Service.Bank]] and [[internal/gitx.PlanBank]]
