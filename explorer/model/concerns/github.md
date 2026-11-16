+++
id = "github"
title = "GitHub: reads, requests, tokens"
summary = "The three ways a box deals with GitHub: reads through the daemon with the host's credential, pull requests and comments the user approves, and in the token modes a token of its own; where each credential lives and what the box can do with what it gets."
order = 70
packages = ["internal/github"]
files = [
  "internal/daemon/github.go",
  "internal/boxside/github.go",
  "internal/gitx/github.go",
  "internal/cli/ghcmds.go",
]
key = [
  "internal/github.ParseRepo",
  "internal/github.PlanRead",
  "internal/github.ParseAPIPath",
  "internal/daemon.boxWorker.serveGitHub",
  "internal/github.HostToken",
  "internal/daemon.Daemon.githubToken",
  "internal/box.Capabilities.GitHubToken",
  "internal/github.Minter.Token",
  "internal/github.Minter.verify",
  "internal/boxside.GitCredential",
  "internal/cli.app.prCreate",
  "internal/gitx.Service.ConfigureGitHub",
]
spec = ["doc:specs-v2.md#10.1", "doc:docs/guide.md#github"]
tests = [
  "internal/github.TestPlanRead",
  "internal/github.TestParseAPIPath",
  "internal/github.TestRead",
  "internal/github.TestMinter",
  "internal/github.TestAppJWT",
  "internal/github.TestPermissions",
  "internal/github.TestHostToken",
  "internal/daemon.TestGitHubFacade",
  "internal/daemon.TestGitHubToken",
  "internal/daemon.TestGitHubRequests",
  "internal/box.TestGitHubCapabilities",
  "internal/boxside.TestGitCredential",
  "internal/gitx.TestConfigureGitHub",
  "test/integration.TestGitHub",
  "test/integration.TestGitHubTokens",
]
related = ["upstream", "requests-and-approval", "secrets", "box-lifecycle", "git-ratchet", "box-channel"]

[[invariant]]
text = "The host's own GitHub credential is read only by the daemon's read facade and by devc pr on the host; answers and errors that reach a box are scrubbed of it."
refs = ["internal/github.HostToken", "internal/daemon.boxWorker.serveGitHub", "internal/cli.app.githubClient", "internal/github.Client.Scrub"]
tests = ["internal/daemon.TestGitHubFacade", "test/integration.TestGitHub"]

[[invariant]]
text = "A facade read is a GET of the project's repository, or of one that grants.github_read lists both at the box's start and now."
refs = ["internal/github.PlanRead", "internal/box.Capabilities.GitHubReadRepos", "internal/github.Client.Read"]
tests = ["internal/github.TestPlanRead", "internal/daemon.TestGitHubFacade"]

[[invariant]]
text = "Nothing a box sends becomes a URL, a method or a header as sent: paths are built from known segments and validated names, and an API path has no percent-encoding and no '.' or '..' segment."
refs = ["internal/github.ParseAPIPath", "internal/github.Client.endpoint", "internal/github.Client.do"]
tests = ["internal/github.TestParseAPIPath", "internal/github.TestRead"]

[[invariant]]
text = "A box gets a token only in a token mode, with a start record of its running container, and only while the mode, the repository and the App are those of its start."
refs = ["internal/daemon.Daemon.githubToken", "internal/daemon.Daemon.startedWith", "internal/box.Capabilities.GitHubToken"]
tests = ["internal/daemon.TestGitHubToken", "internal/box.TestGitHubCapabilities"]

[[invariant]]
text = "A minted token is restricted to the one verified repository ID and to the permissions granted at the start and now, never workflows; an answer listing other repositories is revoked and refused."
refs = ["internal/github.Minter.Token", "internal/github.Minter.verify", "internal/github.Minter.mint", "internal/github.IntersectPermissions", "internal/github.CheckPermission"]
tests = ["internal/github.TestMinter", "internal/github.TestPermissions"]

[[invariant]]
text = "The App's private key never leaves the daemon; it is parsed before any cache lookup, and every cache is per project and per key."
refs = ["internal/github.Minter.Token", "internal/github.KeyFingerprint"]
tests = ["internal/github.TestMinter"]

[[invariant]]
text = "A box opens a pull request or comments only through a request that the user approved on the host's CLI; devc pr then acts with the host's credential."
refs = ["internal/requests.MayDecide", "internal/daemon.requestCommands", "internal/cli.app.prCreate", "internal/cli.app.prComment"]
tests = ["internal/daemon.TestGitHubRequests", "test/integration.TestGitHub"]

[[diagram]]
id = "github-credentials"
title = "Where each GitHub credential lives, and what reaches the box"
kind = "graph"

[[diagram.node]]
id = "hosttoken"
label = "secret github-token, or gh auth token"
col = 1
row = 1
zone = "host"
ref = "internal/github.HostToken"

[[diagram.node]]
id = "appkey"
label = "App key (key_secret)"
col = 1
row = 3
zone = "host"
ref = "internal/github.ParsePrivateKey"

[[diagram.node]]
id = "stored"
label = "secret of token:<name>"
col = 1
row = 4
zone = "host"
ref = "internal/config.Grants.GitHubTokenSecret"

[[diagram.node]]
id = "facade"
label = "daemon: read facade"
col = 2
row = 1
zone = "host"
ref = "internal/daemon.boxWorker.serveGitHub"

[[diagram.node]]
id = "pr"
label = "devc pr create, comment"
col = 2
row = 2
zone = "host"
ref = "internal/cli.app.prCreate"

[[diagram.node]]
id = "minter"
label = "daemon: minter and caches"
col = 2
row = 3
zone = "host"
ref = "internal/github.Minter.Token"

[[diagram.node]]
id = "tokensrv"
label = "daemon: token requests"
col = 2
row = 4
zone = "host"
ref = "internal/daemon.Daemon.githubToken"

[[diagram.node]]
id = "gh"
label = "devc gh"
col = 3
row = 1
zone = "box"
ref = "internal/cli.app.ghRead"

[[diagram.node]]
id = "req"
label = "devc request pr, comment"
col = 3
row = 2
zone = "box"
ref = "internal/cli.newRequestCmd"

[[diagram.node]]
id = "helper"
label = "credential helper, GH_TOKEN"
col = 3
row = 4
zone = "box"
ref = "internal/boxside.GitCredential"

[[diagram.node]]
id = "github"
label = "GitHub"
col = 4
row = 2
zone = "external"

[[diagram.edge]]
from = "hosttoken"
to = "facade"
label = "read per read"

[[diagram.edge]]
from = "hosttoken"
to = "pr"
label = "read per command"

[[diagram.edge]]
from = "facade"
to = "gh"
label = "status, body (scrubbed)"
ref = "internal/github.Client.Scrub"

[[diagram.edge]]
from = "req"
to = "pr"
label = "after local approval"
ref = "internal/daemon.requestCommands"

[[diagram.edge]]
from = "appkey"
to = "minter"
label = "signs the JWT"
ref = "internal/github.AppJWT"

[[diagram.edge]]
from = "minter"
to = "tokensrv"
label = "token for one repo"

[[diagram.edge]]
from = "stored"
to = "tokensrv"
label = "read per request"

[[diagram.edge]]
from = "tokensrv"
to = "helper"
label = "the token itself"

[[diagram.edge]]
from = "facade"
to = "github"
label = "GET"
ref = "internal/github.Client.Read"

[[diagram.edge]]
from = "pr"
to = "github"
label = "push, POST"

[[diagram.edge]]
from = "minter"
to = "github"
label = "verify, probe, mint"
ref = "internal/github.Minter.mint"

[[diagram.edge]]
from = "helper"
to = "github"
label = "git, gh with the token"
+++
## What it is for

An agent on a GitHub project wants to read pull requests, issues and failed CI runs, open a pull request and answer a review. The user's own credential can do far more, on every repository the user reaches, so by default no GitHub credential enters the box ([[doc:specs-v2.md#10.1]]). Reads go through the daemon, which holds the user's credential and serves reads of the project's repositories only; pull requests and comments are requests the user approves; and a project that needs `git push` to GitHub from the box can be given a token of its own, at best an App installation token for the one repository. None of this has run against the real GitHub; what the code assumes about GitHub's answers is listed in [[doc:docs/dev/LEFTOVER.md#1]] under "GitHub (real account)".

## How it works

**Which repository.** [[internal/github.ParseRepo]] reads `owner/name` from the project's `upstream` (https, ssh, git or `host:owner/repo`), which must be on the web host of `DEVC_GITHUB_URL` (default github.com); any other upstream makes the project "not a GitHub project", and all three ways refuse. API calls go to `DEVC_GITHUB_API` (default api.github.com) through one client ([[internal/github.Client]]), which caps answers, follows at most four redirects and only to https, and replaces its own credential in every body and error with `[redacted]`.

**The host's credential.** [[internal/github.HostToken]] reads the realm secret `github-token`, else runs `gh auth token --hostname <host>` from `/` (15 seconds, no terminal); the value must be one word of printable ASCII. It is read for each facade read by the daemon and for each `devc pr` command on the host, and nowhere else. [[diagram:github-credentials]]

| | The box sends | The host does | The box gets |
|---|---|---|---|
| Read facade (every mode) | an operation and its parameters | validates; reads with the host's credential | GitHub's status and body |
| Requests | `devc request pr` or `comment` | after local approval, `devc pr create --yes` or `devc pr comment` | the request's outcome, with the URL |
| Token modes (`app`, `token:<secret>`) | `host.github.token` | checks the start record; mints or reads the token | the token itself |

**Reading** ([[flow:gh-read]]). `devc gh pr|issue|run list|view` and `devc gh api <path>` validate locally, then open a `host.github` stream through the agent. [[internal/daemon.boxWorker.serveGitHub]] works for the project of the channel: its configuration must be free of problems; repositories of `grants.github_read` count only if the container's start record lists them too ([[internal/box.Capabilities.GitHubReadRepos]]); [[internal/github.PlanRead]] accepts only the fields of a known operation and a repository that is the project's or listed. Then come the budget, the credential and [[internal/github.Client.Read]], with 150 seconds for all its requests. The answer is one JSON line (status, length, truncated, note) followed by exactly that many bytes. `run view --log` keeps the last 5 MiB of the jobs' logs.

**Asking.** `devc request pr --title …` (the branch checked out, unless `--branch`) and `devc request comment <n> --body …` are high-risk requests ([[concern:requests-and-approval]]): only `devc requests approve` on the host decides them ([[flow:request-pr]]). The daemon then runs `devc pr create --yes …` or `devc pr comment …` as a child process; [[internal/cli.app.prCreate]] reads the host's credential first, banks and pushes the branch as `devc push` does ([[concern:upstream]]), and opens the pull request, whose URL the box can read in the outcome.

**Token modes** ([[flow:app-token]]). With `grants.github = "app"` or `"token:<secret>"`, the start record holds the mode, the upstream, the App's ID and installation, and the permissions ([[internal/box.CapabilitiesOf]]). A start hook gives the box repository a second remote `github` at the upstream's HTTPS URL and registers `/opt/devc/bin/devc box git-credential` as credential helper for the GitHub host, after an empty entry that drops the helpers of the box's global git configuration ([[internal/gitx.Service.ConfigureGitHub]]); in relay mode it removes exactly those entries. The launcher asks for the token at every harness start and exports `GH_TOKEN` ([[internal/boxside.addGitHubToken]]); the helper asks whenever git needs a credential. [[internal/daemon.Daemon.githubToken]] checks the budget, the mode, the start record of this container and [[internal/box.Capabilities.GitHubToken]]: the same mode, repository and, for `app`, App and installation as at the start, with the permissions granted then and now, each at the lower level. `token:` answers the stored secret; `app` asks [[internal/github.Minter.Token]].

**Minting.** The App's key is parsed before any cache lookup, and every cache key holds the project and a fingerprint of the key's public half. At most once a day per project and repository, and again after a failed mint, [[internal/github.Minter.verify]] signs a nine-minute JWT ([[internal/github.AppJWT]]), checks that the installation GitHub names for the repository belongs to its owner (and is the configured one), and learns the repository's ID through a metadata-only probe token, which it then revokes. The token is minted for that ID and the permissions; an answer listing other repositories is revoked and refused. A token is reused until five minutes before its `expires_at`.

## What can go wrong, and what prevents it

A box controls every field of a read and of a request, when and how often it asks for a token, and everything it does with a token it holds.

- **Reading other repositories.** A repository is named only as `owner/name` or in an API path, checked against GitHub's name rules, and must be the project's or listed in `grants.github_read` at the start and now. The project's own is its upstream as configured now, without a start record.
- **Shaping the request.** Unknown parameter fields are refused when the parameters are decoded; [[internal/github.ParseAPIPath]] wants `repos/<owner>/<repo>[/…]` in printable ASCII without `%`, `\`, `#`, spaces, or empty, `.` and `..` segments. Every request is a GET built on the API base. What stays open is whatever the host's credential may GET below the allowed repositories.
- **Getting the credential.** It is in no file, variable or argument of the box; answers, errors and the daemon's log never carry it.
- **Spending the user's rate limit.** 120 reads and 120 token requests a minute per project, 5 MiB per answer.
- **Publishing.** Only the local CLI may approve `pr` and `comment` ([[internal/requests.MayDecide]]). Branch, title, body and base are the box's: approving pushes that branch to the upstream under the same name, a fast-forward only. Box-chosen text goes only into `--flag=value` arguments before the `--` that ends the options, so none of it becomes an option or names the project ([[internal/daemon.requestCommands]]).
- **A token in the box.** In the token modes the box holds a credential: GitHub, not the relay's branch policy, decides what it may do ([[doc:docs/dev/LEFTOVER.md#5]]). A minted token covers one repository by ID; `workflows` is never granted ([[internal/github.CheckPermission]]). A stored `token:` secret is whatever the user made it. After a change of the mode, the upstream or the App a box gets no token until it is brought up again.
- **The App's key.** It stays in the daemon; no project receives a token cached for another ([[finding:D2-09]]); an installation of another account, or a probe that sees another repository, is refused ([[finding:D2-03]]).

Nothing in the configuration stops a project from listing `github-token` or the App's key secret in `grants.secrets`, which would hand that credential to the box ([[doc:docs/dev/LEFTOVER.md#7]]).

## Decisions that are not obvious

- **A raw stream for reads**, not a JSON-RPC result: an answer may hold 5 MiB, a channel line at most 1 MiB.
- **No percent-encoding at all** in API paths: the simplest way to rule out encoded separators; a query with encoded values cannot be expressed.
- **The token always comes from the daemon**, whatever `secrets.delivery` says, so the next ask after expiry gets a fresh one; a process keeps the `GH_TOKEN` it started with.
- **The probe token** exists because no call authenticated with the App's JWT gives a repository's ID.
- **`grants.github = "none"`** means the same as `relay`.

## Where to start reading

1. [[internal/github.PlanRead]] and [[internal/github.ParseAPIPath]]
2. [[internal/daemon.boxWorker.serveGitHub]] and [[internal/github.Client.Read]]
3. [[internal/daemon.Daemon.githubToken]] and [[internal/box.Capabilities.GitHubToken]]
4. [[internal/github.Minter.Token]] and [[internal/github.Minter.verify]]
5. [[internal/boxside.GitCredential]] and [[internal/gitx.Service.ConfigureGitHub]]
6. [[internal/cli.app.prCreate]]
