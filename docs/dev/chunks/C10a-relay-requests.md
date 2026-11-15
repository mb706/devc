# C10a — Relay service, requests, on-demand secrets

Read `docs/dev/README.md`, `docs/dev/plan.md` (sections 3.7, 3.10, 3.11) and the `STATUS.md` notes of C05, C06 and C08. Product spec: `specs-v2.md` sections 9, 10 and 13 (requests).

With the daemon and the box channel in place, a box can now reach host services directly. This chunk adds three: git push and fetch against the relay, requests (the agent's way to ask for something outside its box), and secrets fetched on demand. All of them identify the caller by the channel the call arrives on, never by anything the box says about itself.

## 1. Relay service

Replaces the stub remote helper of C05 whenever the daemon is connected; the stub remains the fallback when it is not.

**In the box.** `git-remote-devc` implements the remote-helper `connect` capability: for `connect git-upload-pack` or `connect git-receive-pack` it opens a stream to the in-box agent, which forwards it to the daemon as a `host.git` stream with the service name, then relays bytes both ways.

**In the daemon.**

- The project is the one bound to the channel. The URL the box supplies (`devc::<realm>/<project>`) is ignored for authorisation.
- Spawn host `git upload-pack <relay>` or `git receive-pack <relay>` with the stream as its stdin and stdout, a neutral working directory, and the hardened environment of C05's wrapper.
- Hide devc's bookkeeping from the box: `uploadpack.hideRefs` and `receive.hideRefs` for `refs/devc`.
- The box's `origin` gets a second fetch refspec so it also receives `refs/remotes/github/*`, and tags.
- **Fetch-through.** Before serving an `upload-pack`, fetch the upstream into the relay, non-interactively (`GIT_TERMINAL_PROMPT=0`, SSH batch mode), with a 20 s limit and at most once a minute per project; then fast-forward relay branches as `devc pull` does. On any failure serve what the relay has. `git.fetch_through = false` in the project definition turns this off.

**Policy on pushes**, enforced by hooks in a host-controlled hooks directory that call `devc relay-hook update|post-receive` (the daemon passes the project in the environment):

| Ref | Rule |
|---|---|
| `refs/heads/<b>` | create and update allowed, including non-fast-forward; delete refused; refused when `<b>` is protected; name limits as in C05 |
| `refs/tags/<t>` | create allowed; update and delete refused |
| anything else | refused |

`post-receive` records a bank ref for every accepted branch update, forwards branches matching `git.forward` to the upstream (non-interactive; the result is printed back to the pusher), and tells the daemon so it can emit a `note` event.

**Banking after turns.** The daemon watches its own event stream: when a session goes from `working` to `done` or `idle`, it schedules a `bank` for that box, at most once every 30 s per box. This is what captures snapshots and commits the agent did not push.

## 2. Requests

A request is a small structured message from a box asking the user for something. It never executes anything by itself.

**In the box.** `devc request …` (the toolbox binary is on the `PATH` as `devc`):

| Command | Kind | Risk |
|---|---|---|
| `devc request seal <path>` | `seal` | low |
| `devc request savepoint [name]` | `savepoint` | low |
| `devc request note <text>` | `note` | low |
| `devc request package <apt\|dnf\|apk> <name>…` | `package` | high |
| `devc request net <host:port \| lan> [--for 1h]` | `net` | high |
| `devc request grant <ssh\|secret> <name>` | `grant` | high |
| `devc request pr --title <t> [--body <b>] [--branch <b>]` | `pr` | high |
| `devc request status <id> [--wait]` | | |

Each sends `host.request` over the channel and prints the request ID. Without a connected daemon it fails with a message telling the agent to ask the user directly.

**In the daemon.**

- Validate every field strictly: package names against `^[a-z0-9][a-z0-9+.-]{0,63}$`, hosts and ports, durations, paths as in C06, text lengths. Reject anything else.
- Store requests in `durable/realms/<realm>/requests/<project>.jsonl`: ID, time, session (from the caller's environment, validated), kind, parameters, risk, status (`pending`, `approved`, `denied`, `done`, `failed`, `expired`), result text. Write a `request` event into the session's journal so listings and, later, the inbox show it. Add the event type.
- Pending requests expire after seven days.
- **Decisions.** `requests.list`, `requests.decide {id, approve, origin}`. A decision arriving on the local socket has origin `local` and may decide anything. Other origins (the hub, from C11) may decide low-risk requests only; a high-risk decision from them is refused. `[requests] auto = ["seal", "savepoint", "note"]` in the project definition (that list is the default) approves those kinds automatically; only low-risk kinds may be listed.
- **Execution** on approval, by the daemon, using the existing code paths: `seal` and `savepoint` from C06; `package` through the root exec of C02 with the manager's install command, recorded in the root log; `net`, `grant` and `pr` are accepted and stored but answer "not available yet" until C13 and C15 implement them.

**CLI on the host.** `devc requests [--all] [--json]`, `devc requests approve <id>`, `devc requests deny <id> [--reason …]`.

Extend the generated sandbox description: how to make a request, and that sealing a finished run can now be requested.

## 3. Secrets on demand

- New global setting `[secrets] delivery = "files"` (default, as in C03) or `"daemon"`.
- With `daemon`: nothing is written to `/run/devc-secrets`. The launcher asks `host.secret {name}` over the channel for the harness token and each granted secret; the daemon returns only what the project is granted. Values travel in the response body and are never logged.
- If the daemon is not reachable in `daemon` mode, the launcher starts the harness without the credential and says how to fix it.

## 4. Tests

Unit: the push policy table; request validation (every kind, hostile values); risk and origin rules; fetch-through rate limit.

Integration (daemon running; fake harness; bare repositories as upstream):

1. In the box, `git push origin feature` lands in the relay, a bank ref exists, and `git status` shows the branch in sync. `git fetch origin` brings a commit that was added to the relay on the host.
2. Policy: deleting a branch, pushing a protected branch, moving a tag and pushing to `refs/devc/x` are refused with a message; creating a tag works.
3. A branch matching `git.forward` reaches the upstream on push, and the pusher sees the result.
4. Fetch-through: a commit pushed to the upstream by a third clone appears in the box after `git fetch`, without `devc pull`.
5. Two boxes of different projects: each reaches only its own relay, whatever URL it configures for `origin`.
6. A box that writes garbage into a git stream does not disturb the daemon or the other box.
7. With the daemon stopped, the stub behaviour of C05 is back and nothing hangs.
8. A fake-harness turn that edits a file without committing: after the turn ends, a snapshot ref appears in the relay.
9. `devc request seal` is approved automatically and the directory is sealed; `devc request status --wait` returns `done`.
10. `devc request package` stays pending; `devc requests approve` runs it (use a harmless package or a stubbed manager) and the root log records it; `deny` leaves the box unchanged.
11. A high-risk decision with a non-local origin is refused; a low-risk one is accepted.
12. Hostile requests (shell metacharacters in package names, `..` in paths, oversized text) are rejected and stored nowhere.
13. `secrets.delivery = "daemon"`: the fake harness receives its token, `/run/devc-secrets` is empty, and an ungranted secret is refused.

## 5. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `STATUS.md` and `LEFTOVER.md` are updated.
