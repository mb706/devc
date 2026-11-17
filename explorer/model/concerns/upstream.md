+++
id = "upstream"
title = "Upstream: pull, push, forward"
summary = "How commits leave the host and arrive: the relay's single remote, fetching with the user's credentials, fast-forwarding relay branches, devc pull and fetch-through into the box, devc push and forwards to the upstream, the branch policy, and project setup."
order = 41
files = [
  "internal/gitx/upstream.go",
  "internal/gitx/project.go",
  "internal/gitx/github.go",
  "internal/cli/gitcmds.go",
]
key = [
  "internal/gitx.Relay.SetUpstream",
  "internal/gitx.Service.fetchUpstream",
  "internal/gitx.Service.syncHeads",
  "internal/gitx.Service.Pull",
  "internal/gitx.Service.FetchThrough",
  "internal/gitx.Service.Push",
  "internal/gitx.Service.pushUpstream",
  "internal/gitx.Service.forward",
  "internal/gitx.Protected",
  "internal/gitx.Service.InitRelay",
  "internal/gitx.Service.InitBox",
  "internal/gitx.Service.Adopt",
]
spec = ["doc:specs-v2.md#9.3", "doc:specs-v2.md#9.4"]
tests = [
  "internal/gitx.TestEndToEnd",
  "internal/gitx.TestCheckNameAndMatches",
  "internal/gitx.TestPlanBankBoxTips",
  "internal/gitx.TestInitBoxRefusesForeignRepository",
  "internal/gitx.TestRecordPush",
  "internal/daemon.TestRateLimit",
  "internal/cli.TestProjectAddRefusals",
  "test/integration.TestGit",
  "test/integration.TestGitAdopt",
  "test/integration.TestRelayService",
]
related = ["git-ratchet", "worktrees-and-context", "configuration", "requests-and-approval"]

[[invariant]]
text = "The host pushes to the upstream only for devc push, for devc pr create, and to forward a branch that git.forward names."
refs = ["internal/gitx.Service.Push", "internal/gitx.Service.forward", "internal/gitx.Service.pushUpstream"]

[[invariant]]
text = "A forward never rewrites an upstream branch: it pushes without force or lease, so git refuses a non-fast-forward."
refs = ["internal/gitx.Service.forward", "internal/gitx.Service.pushUpstream"]

[[invariant]]
text = "devc push rewrites an upstream branch only with --force, and only while the upstream still has the tip devc last fetched."
refs = ["internal/gitx.Service.Push"]
tests = ["internal/gitx.TestEndToEnd"]

[[invariant]]
text = "devc push sends exactly the commit whose log and diffstat it showed."
refs = ["internal/gitx.Service.Push", "internal/gitx.Service.preview"]

[[invariant]]
text = "A fetch from the upstream moves a relay branch only forward, to a tip it records with a bank ref; a diverged branch keeps its tip."
refs = ["internal/gitx.Service.syncHeads"]
tests = ["internal/gitx.TestEndToEnd", "test/integration.TestGit"]

[[invariant]]
text = "A pull changes no branch, worktree or differing tag in the box; it updates only origin/*, github/* and new tags."
refs = ["internal/gitx.Service.reflect"]
tests = ["test/integration.TestGit"]

[[invariant]]
text = "Upstream commands in the daemon and the relay hooks never prompt: no terminal, no askpass program, no interactive credential manager."
refs = ["internal/gitx.Host.Cmd", "internal/gitx.backgroundUnset"]
tests = ["internal/gitx.TestHostCmd"]

[[invariant]]
text = "No git mutation runs while the project's configuration has problems."
refs = ["internal/cli.app.refuseProblems", "internal/cli.app.withGit", "internal/daemon.boxWorker.serveGit", "internal/cli.app.relayHook"]
+++
## What it is for

Work leaves through the host only when the user lets it: commits leave the relay only with the user's own git credentials, which devc gives no box, by `devc push`, `devc pr create` or a forward the project configured. Whether the box reaches the upstream itself is a matter of its network level and of what it was given ([[concern:network-levels]]): at `internet` or `open`, or at `allowlist` with the upstream's host listed, it can fetch a public upstream and push with any credential it holds, such as a GitHub token from `grants.github`; an SSH grant for the upstream's host does the same through the gate. The other way, the upstream's changes reach the relay on `devc pull`, at project setup and, when a box fetches, by fetch-through; the box sees them as remote-tracking branches, and nothing moves its own branches.

## How it works

**The remote.** The relay has one remote, always named `github` ([[internal/gitx.UpstreamRemote]]), set from project.toml's `upstream` before every operation that uses it ([[internal/gitx.Relay.SetUpstream]]): fetch refspec `+refs/heads/*:refs/remotes/github/*` and `tagOpt = --tags`. It refuses a URL that starts with `-` or contains `::`; configuration loading has refused `<transport>::` URLs already ([[concern:configuration]]).

**Upstream mode.** Commands that talk to the upstream run in [[internal/gitx.Upstream]] mode: only variables that point git elsewhere ([[internal/gitx.repoEnv]]) are removed, and the user's global and system configuration stays, because it carries credentials. On a terminal they are attached to it so prompts reach the user; otherwise `GIT_TERMINAL_PROMPT=0`. In the daemon and the relay hooks ([[internal/gitx.Host]] with `Background`) they also lose askpass programs and the display, run without a controlling terminal, so ssh fails rather than asks, and end with their whole process group. Each gets at most an hour.

**Fetching** ([[internal/gitx.Service.fetchUpstream]]): `git fetch --prune --force --no-write-fetch-head --no-recurse-submodules --no-auto-gc github`. Branches land in `refs/remotes/github/*`, pruned when deleted upstream; tags in `refs/tags/*`, forced but never pruned; the relay's own configuration has every object checked. [[internal/gitx.Service.syncHeads]] then walks the upstream's branches: one the relay lacks is created, one whose relay tip is an ancestor of the upstream's is fast-forwarded, each recorded with a bank ref; a relay branch ahead is left alone silently, a diverged one is left alone and reported.

**Who fetches.** `devc pull` ([[flow:devc-pull]]) fetches and syncs under the box lock, then hands everything to a running box ([[internal/gitx.Service.reflect]]): relay branches as `origin/*`, the upstream's as `github/*`, tags without force, so a box tag with another value is kept and reported. A stopped box gets the refs later. Fetch-through ([[internal/gitx.Service.FetchThrough]]) fetches and syncs before the relay service serves a box's fetch, if `git.fetch_through` is on (the default), at most once a minute per project whether it succeeds or not, waiting five seconds for the box lock and twenty for the upstream; a failure is logged and the box gets what the relay has. `devc project add` and `init` fill a new relay the same way.

**Pushing** ([[flow:devc-push]], [[internal/gitx.Service.Push]]). Under the box lock a running box is banked first. Without arguments the branch checked out in `main` is pushed, or the relay's HEAD branch when the box is down or devc cannot tell (a detached HEAD, a box that does not answer). For each branch devc shows up to 200 commits the upstream lacks and, if the upstream has the branch, a diffstat; a non-fast-forward needs `--force`; then it asks, or needs `--yes` without a terminal. It pushes the commit it showed, `git push --porcelain --no-verify github <oid>:refs/heads/<b>`, with `--force-with-lease=refs/heads/<b>:<last fetched tip>` under `--force`, and sets `refs/remotes/github/<b>`. `--tag <t>` pushes the newest `refs/devc/tags/<t>` record; moving an upstream tag needs `--force`. `devc pr create` pushes this way first; an approved `pr` request runs it with `--yes` ([[concern:requests-and-approval]]).

**Forwarding** ([[internal/gitx.Service.forward]]): a branch matching `git.forward` whose relay tip differs from `github/<b>` is pushed, without force or lease; after a bank every such relay branch, in post-receive after a relay-service push only the branches that push updated and recorded ([[internal/gitx.Service.ReportPush]]). Failures are reported, never fatal.

**The branch policy** (`[git]` in project.toml): `forward` and `protected` are globs in which `*` does not cross `/` ([[internal/gitx.Matches]], [[internal/gitx.Protected]]); `branch_prefix` starts the branch names of task worktrees.

**Setup.** [[internal/gitx.Service.InitRelay]] creates the relay in a temporary directory renamed into place, so a relay that exists is configured, and points HEAD at `default_branch`, else the upstream's HEAD, else the relay's, else `main`. [[internal/gitx.Service.InitBox]] creates the box repository in the box (`git init --separate-git-dir .repo main`) or refuses one whose origin is not `devc::<realm>/<project>`; it configures origin, reflects everything and checks out the default branch only if nothing is checked out. [[internal/gitx.Service.Adopt]] fills a relay from a local clone instead: branches with bank refs, `origin/*` as `refs/remotes/github/*`, tags. A clone inside the projects root or the state directory is refused, since boxes write there.

## What can go wrong, and what prevents it

- **A box publishes.** A push through the relay ends in the relay. A forward pushes only branches the configuration names, and never rewrites one.
- **The user pushes what they did not see.** devc pushes the previewed object, not a ref that could have moved since, and the lease refuses a force when the upstream changed after the last fetch.
- **A prompt in the background.** Upstream commands of the daemon and the relay hooks run without a terminal, without askpass programs and with `GCM_INTERACTIVE=never`, so asking for credentials fails instead of hanging.
- **A hostile upstream** can move `refs/remotes/github/*` and `refs/tags/*`, which are its own. A relay branch moves only forward, to a commit it records; a tag a box pushed stays in `refs/devc/tags/` when the forced fetch overwrites `refs/tags/<t>`.
- **Box-written text on the user's terminal.** Previews, `devc log` and `devc diff` run in display mode with `--no-ext-diff --no-textconv`, and every control character except colour sequences is replaced ([[internal/gitx.Sanitize]]); `--ext-diff` and `--textconv` are refused as arguments.
- **A configuration with problems** stops every git mutation: bank, pull, push, worktree changes, the relay service and its hooks ([[finding:B2]]). A malformed `protected` pattern protects every branch; a malformed `forward` pattern matches none.

## Decisions that are not obvious

- **No merges on the host.** A relay branch follows the upstream only by fast-forward, and a push that is not one needs `--force`; divergence is resolved in the box (`git merge github/<b>`).
- **A pull leaves the box's tips alone,** so the next bank keeps a fast-forwarded relay branch unless the box moved its own ([[finding:B8]]).
- **Fetch-through counts failures.** An attempt counts against the once-a-minute limit whether or not it succeeds, so a failing upstream is not asked again at every fetch of the box ([[internal/daemon.rateLimit]]).

## Where to start reading

1. [[internal/gitx.Service.fetchUpstream]] and [[internal/gitx.Service.syncHeads]]
2. [[internal/gitx.Service.Push]] and [[internal/gitx.Service.pushUpstream]]
3. [[internal/gitx.Service.forward]]
4. [[internal/gitx.Service.InitRelay]] and [[internal/gitx.Service.InitBox]]
