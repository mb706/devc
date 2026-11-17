# C20 — Defects found while writing the explorer

The authors of the explorer's content read the code area by area and recorded what looked wrong (`docs/dev/LEFTOVER.md` part 7). A second model then checked each item against the code (`docs/dev/reviews/G1-review.md` to `G5-review.md`, Part B): 50 are confirmed, five of them high. These chunks fix the confirmed defects that break the project's own rules: the threat model (a box must not be able to exhaust the host or destroy savepoints, banked commits, journals or archives; it must not obtain a credential it was not granted) and the six security rules of `docs/dev/README.md`. The remaining confirmed items stay in `LEFTOVER.md` for the owner to decide.

Read `docs/dev/README.md` (binding), `docs/dev/LEFTOVER.md` part 7, and the Part B of the review that covers each item. The explorer's concern pages for each area (`explorer/model/concerns/`) describe how the code works today and name the functions; use them and `./dev explore query` to find your way.

Fix each item with a regression test that fails without the fix. A limit must fail closed and be tested by exceeding it, with a positive control. Record the outcome per item in `docs/dev/reviews/C20-response.md`.

## C20a: the five high-severity items

| Item | Decision |
|---|---|
| X6 3, journals without bound | Two budgets, checked by the ingester before it appends: per session (`[sessions] session_journal_max`, default 128 MiB) and per project (`journal_quota`, default 2 GiB), plus the free-space reserve that pushes already use. Over a budget, ingestion of that session (or project) stops: the spool keeps what it holds within its own limits, the host writes one `note` event saying which budget was reached and how to continue, and `devc ps` and `devc doctor` show it. Nothing already in a journal is dropped. `devc sessions prune <ref> --before <date or duration>` removes the journals of ended sessions older than that (never a live session, never without the flag), so the owner can make room; the hub's archive keeps what it has. |
| X7 1, banking without a storage budget | A bank's fetch from the box is bounded like a push: the free-space reserve is checked before it starts, the fetch runs under the push watchdog against `git.relay_quota` in its own process group, and a fetch that is ended leaves no temporary packs behind. A failed bank reports why and leaves earlier bank refs untouched. |
| X7 2, banking fetches every ref a box offers | List the box's refs first, with a bound on their number (the policy's limits) and on name length, and fetch only the listed refs by name. No wildcard refspec reaches git. Over the limit the bank is refused with a message that names the limit; nothing is fetched. |
| X8 4, forwards without an admission limit | A forward serves at most a fixed number of connections at a time (64 per forward, 256 across the daemon's router); a connection beyond that is closed at once and logged once per interval. No dial process is started for a refused connection. |
| X9 1, a box can push out the owner's savepoints | A savepoint made on a box's request is a class of its own, recorded with the savepoint. Retention of requested savepoints counts and drops only requested ones (keep the newest ten per project, whatever name the request gave; a name is a label, not a promise to keep). Retention of the owner's and devc's own savepoints never counts requested ones. At most one requested savepoint per project in ten minutes; a request inside that window is refused with the reason. Both drivers. |

## C20b: the rule violations of medium severity

| Item | Decision |
|---|---|
| G1-01 (accuracy review), the capability record is rewritten for a running box | The record of what a box was started with is written only by the operation that started the container. No path that finds the container already running (`up`, `sh`, `run`, `root`, a session start) replaces it, whatever the box did to its readiness marker. If the record is missing or the box never reported ready, the command says so and asks for a restart instead of finishing the start with today's configuration. |
| X4 2, `devc login status` prints a container's output raw | Everything a login or status container prints goes through the same terminal escaping as other text from places a box can write. |
| X6 2, a panic in a stream handler ends the daemon | Every goroutine the daemon starts for work that handles a box's input (raw streams for git, the gate, the proxy and services; project workers; the web handlers) recovers a panic, logs it with the box and the method, and closes that stream or fails that request only. One common function. |
| X7 3, the update hook leaves records for updates git then refuses | A record written by the `update` hook counts only once the push it belongs to has succeeded: the receive wrapper confirms or removes the records of its push when `receive-pack` ends, and a record that was never confirmed is ignored and cleaned up at the next bank. |
| X8 3, the proxy's host-address check fails open | While the list of the host's addresses is unknown, the proxy refuses every destination that is not provably outside it (it refuses the connection and retries the listing), instead of treating the list as empty. |
| X10 1, a project may be granted the GitHub token or the App key as an ordinary secret | Configuration validation refuses a `grants.secrets` entry that names the realm's GitHub credential or an App's key secret, with the same message form as the guard for service credentials. |
| X10 3, a pull request is pushed before it is checked | `pr create` and an approved `pr` request validate everything that can be checked locally before anything is pushed: the branch is not the base, is not a protected or default branch of the upstream, and exists in the bank. Only then is the branch published. |
| X12 1, one realm's full quota stops every later realm's archive | A realm's quota or other refusal ends that realm's pass, is reported in `sync status`, and backs that realm off; the uploader goes on with the realm's other kinds where they are independent and with the other realms. |
| X12 6, the web app settles a waiting prompt from a box-written event | Only a receipt settles a command. A prompt event that matches a waiting prompt may be shown, but it never removes the pending command or its outbox entry. |

## For both chunks

- The explorer's model must stay true. For every item, update the concern pages, invariants, flows and glossary terms that describe the changed behaviour (`grep` the model for the functions you change), add an invariant with its enforcing code and test where a new protection exists, and run `./dev explore --check`. In `LEFTOVER.md` part 7, mark each fixed item `Fixed in C20a` or `Fixed in C20b` with one line on how, keeping its text.
- Document new configuration keys and commands in `docs/guide.md`.
- Security rules and conventions of `docs/dev/README.md` apply as in every chunk: argument vectors, validated names, no secrets in arguments or logs, fail closed.
- Tests: targeted while working; at the end the full unit suite and the integration tests that cover what you changed (name them in your report). Add an integration test where a limit can only be shown against a real box (the journal budget, the bank budget, requested savepoints).
- Never `rm` with an unguarded variable. No commits. Clean up what your tests create.

At the end add a section to `docs/dev/STATUS.md` and report in at most 300 words: each item fixed and how; anything decided differently from the tables and why; tests run and results; what you noticed nearby that is also wrong.
