# C15 — GitHub

Read `docs/dev/README.md` and the `STATUS.md` notes of C05, C08 and C10a. Product spec: `specs-v2.md` section 10.1. Background: `../v2-research/09-github-minting.md` (short answer, sections 1 and 4, and the recommendation).

By default a box holds no GitHub credential: git goes through the relay (C05, C10a). This chunk adds the rest: reading from GitHub through the daemon, opening pull requests and commenting as host-side actions, and, as an option per project, short-lived tokens from a GitHub App or a stored token handed into the box.

No real GitHub account is used. Every HTTP call goes to a base URL that tests point at a fake server (`DEVC_GITHUB_API`, default `https://api.github.com`).

## 1. Host credential

The daemon needs a credential of its own for API calls. Sources, in order: the realm secret `github-token`; otherwise the output of `gh auth token` if `gh` is installed on the host. It is never passed to a box.

The project's repository is derived from its `upstream` URL (`owner/repo`); projects whose upstream is not on GitHub get a clear "not a GitHub project" error from everything below.

## 2. Reading from a box

In the box: `devc gh …`, served over the channel (`host.github`). All of it is read-only and limited to the project's own repository plus any listed in `grants.github_read = ["owner/repo", …]`.

| Command | Shows |
|---|---|
| `devc gh pr list`, `devc gh pr view <n> [--comments] [--diff]` | pull requests |
| `devc gh issue list`, `devc gh issue view <n> [--comments]` | issues |
| `devc gh run list [--branch b]`, `devc gh run view <id> [--log] [--failed]` | workflow runs and logs |
| `devc gh api <path>` | a raw GET of a REST path under `/repos/<owner>/<repo>/` |

- The daemon builds the request itself from validated parts; a box never supplies a full URL, a method other than GET, or headers. Paths are checked against the allowed repositories after normalisation (no `..`, no encoded slashes).
- Responses are size-capped (5 MiB; logs are truncated from the front with a note) and passed back as received. `--json` gives the raw JSON; the default output is a short readable form.
- GraphQL is not offered.
- Extend the sandbox description: these commands exist, the real `gh` has no credential unless the project grants a token, and how to ask for a pull request.

## 3. Writing: host-side actions

- `devc pr create [ref] --branch <b> [--title t] [--body b | --body-file f] [--base <b>] [--draft]`: banks, pushes the branch to the upstream (as `devc push` does, with its confirmation unless `--yes`), then creates the pull request through the API and prints its URL.
- `devc pr comment [ref] <n> --body <b>`.
- The request kinds `pr` and (new) `comment` from a box (C10a) now execute on approval through the same code. Both stay high-risk: a box asking for a pull request is asking for its code to be published.

## 4. Tokens in a box (optional, per project)

`grants.github` selects the mode: `relay` (default, nothing in the box), `token:<secret>` (a stored fine-grained token), or `app`.

**`app`** — installation tokens from a GitHub App the user owns:

- Realm configuration: `[github.app]` with `app_id`, `key_secret` (the realm secret holding the private key in PEM form), and optionally `installation_id` (otherwise looked up for the repository's owner).
- Project configuration: `[grants.github_token] permissions = { contents = "read", pull_requests = "write", … }`. `workflows` is refused by validation. Without this table the default is `contents = "read"`, `metadata = "read"`.
- Minting: an RS256 JWT signed with the App key (standard library only), then `POST /app/installations/<id>/access_tokens` restricted to the one repository and the configured permissions. Cache until five minutes before expiry.
- Delivery needs the daemon: the launcher and a git credential helper (`devc box git-credential`, registered for `https://github.com` in the box's git configuration when the mode is `app` or `token:`) ask over the channel each time, so a fresh token is used after the old one expires. Sessions also get it as `GH_TOKEN`; say in the documentation that a long-running process keeps the token it started with for at most an hour.
- In `app` and `token:` modes the box's repository gets a second remote `github` pointing at the upstream's HTTPS URL, so the agent can choose between the relay and a direct push.

**`token:<secret>`** — the stored token is delivered the same way, without minting.

Document the trade-off in `docs/guide.md`: with a token in the box the ratchet no longer covers what that token can do; protect the default branch on GitHub with a ruleset.

## 5. Tests

Unit: repository parsing from upstream URLs (HTTPS, SSH, with and without `.git`); path validation for the read facade (traversal, other repositories, encoded separators); JWT construction verified with the public key; permission validation.

Integration with a fake GitHub API server:

1. `devc gh pr view`, `issue view`, `run view --log` and `gh api` return the fake's data; a path to another repository and a non-GET attempt are refused before any request leaves the daemon; the host credential never appears in the box (search the box's environment and files for it).
2. `devc pr create` pushes the branch to a bare upstream and posts the expected body to the fake; a `pr` request from a box stays pending and executes after `devc requests approve`.
3. `app` mode: the fake receives a correctly signed JWT and the repository and permission restriction; the box's `git push github` uses the minted token through the credential helper (the fake upstream accepts it over a local HTTPS or HTTP test server); after forcing expiry a new token is minted.
4. `token:` mode delivers the stored token.
5. Default mode: no token anywhere in the box.

## 6. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `STATUS.md` and `LEFTOVER.md` are updated: nothing was tried against GitHub itself; setting up the App (creation, installation, rulesets) is described in `docs/guide.md` as steps for the user.
