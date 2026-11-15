# C04 — Sessions

Read `docs/dev/README.md`, `docs/dev/plan.md` (sections 3.3, 3.5, 3.6, 3.13) and the `STATUS.md` notes of C01–C03 first. Product spec: `specs-v2.md` sections 8.3 and 12.

After this chunk an agent can be started in a box from the host, re-attached from any terminal, and observed: hooks write events, the host ingests them into a journal the box cannot touch, and `devc ps` shows what every session is doing. The context repository becomes a real git repository.

## 1. tmux in a box

- At `devc up`, write `/etc/devc/tmux.conf` (in the box's generated etc directory). Content: `default-terminal tmux-256color`; `allow-passthrough on`; `focus-events on`; `extended-keys on` with the matching `terminal-features` line; `mouse on`; `history-limit 100000`; `set-clipboard on`; `set-environment -g TERMINFO_DIRS "/opt/devc/terminfo:"`; then `source-file -q ~/.tmux.conf` so the user's own file can override.
- All tmux calls use the toolbox binary, the socket name `devc` (`tmux -L devc -f /etc/devc/tmux.conf …`), and `TERMINFO_DIRS=/opt/devc/terminfo:` in the client's environment. The user's own tmux use inside a box is thereby left alone.
- One tmux session named `devc`. One window per devc session, named `<worktree>/<harness>`, carrying the user option `@devc_session=<id>`.
- A client attaches through its own grouped session (`new-session -t devc`, `destroy-unattached on`), so two clients can look at different windows.
- Put all tmux command construction in one package with unit tests; no tmux command line is assembled anywhere else.

## 2. Starting and attaching

| Command | Behaviour |
|---|---|
| `devc <harness> [ref] [-w worktree] [--detach] [-- args…]` | One subcommand per harness, named after the manifest's `bin` (`devc claude`, `devc codex`, …), registered from the loaded manifests. Brings the box up if needed, generates the session ID on the host, and runs `devc box session start` in the box. Attaches unless `--detach`. Fails with the install hint if the harness is not installed, and with a build hint if tmux is missing from the toolbox. |
| `devc attach [ref] [session]` | Attaches to the box's tmux. With a session ID (or unique prefix) selects that window. With several live sessions and no argument, on a terminal: a numbered list to choose from; otherwise the most recent. |
| `devc ps [ref] [--all] [--json]` | Sessions (section 5). |
| `devc stop <session>` | Kills the session's window. |

- Attaching from the host is `podman exec -it --detach-keys= -e TERM -e COLORTERM -e TERMINFO_DIRS=… <box> tmux … new-session -t devc -s <client-name> ; set destroy-unattached on ; select-window -t <window>`. It must work as `ssh <machine> -t devc attach <ref>`.
- `devc box session start --id <sid> --harness <id> --worktree <name> [--detach] [-- args]` (in the box): working directory is `/w/<realm>/<project>/<worktree>` when it exists, else the project root. It creates the tmux session if needed, creates the window running the harness's shim with `DEVC_SESSION` and `DEVC_WORKTREE` in its environment, tags it, and installs a `pane-exited`/`pane-died` hook that runs `devc box session ended <sid> <exit status>`.
- A harness started by typing its name in a box shell gets a session too: the launcher (C03) already generates an ID; fill in its `registerSession` placeholder so it writes `session.start`.

## 3. Events from the box

Event format and types: plan 3.6. In the box an event has no `seq`.

- `devc box hook <kind> <event>` reads the hook's JSON from stdin (cap 1 MiB), maps it, and appends one line to `/run/devc/spool/<DEVC_SESSION>.jsonl` (`O_APPEND|O_CREATE`, mode 0600, line at most 64 KiB; long text fields are truncated with a marker). Without a valid `DEVC_SESSION` it does nothing. It prints nothing and always exits 0: some harnesses treat hook output as model context, and a failing hook must never break a session.
- Mapping for kind `claude`:

  | Hook | Events written |
  |---|---|
  | `SessionStart` | `harness` (the harness's own session ID, transcript path, working directory) and `state idle` |
  | `UserPromptSubmit` | `prompt` (SHA-256, length, first 2,000 characters) and `state working` |
  | `Stop` | `message` (last assistant message, first 4,000 characters) and `state done` |
  | `Notification` | permission prompt → `state blocked` with kind `permission` and the message as `question`; needs-input and elicitation types → `state blocked` with kind `question`; idle prompt → `state done`; anything else → `note` |
  | `SessionEnd` | `session.end` with the reason |

  Add `harness` to the event types in `proto`. Take field names from the vendor's hook documentation.
- Kind `codex`: the same mapping for Codex's hook names and fields.
- Kind `fake`: a pass-through for the fake harness (`{"type": …, "data": …}`).
- `session.start` (harness, worktree, working directory, PID) is written by the launcher; `session.end` by the tmux hook when the harness did not write one itself.

## 4. Ingestion and journal

`internal/session`, host side. The spool is box-writable and therefore hostile (README security rule 2).

- Open the spool directory through `safefs`. Consider only regular files whose names are `<valid session ID>.jsonl`.
- For each file, read from the stored offset (offsets kept in `local/boxes/<realm>/<project>/spool-offsets.json`), take complete lines only, and validate each: JSON object, known version and type, the `session` field equals the file's ID, timestamp parses, sizes within limits. Invalid lines are counted and skipped.
- Valid events get a `seq` (per session, continuing from the journal) and are appended to `durable/realms/<realm>/journal/<project>/<session>.jsonl` (0600). The original timestamp is kept as `ts`; add `at` with the host's ingestion time.
- If a spool file is shorter than its stored offset, write a `note` event saying the spool was truncated and continue from the file's current end.
- Bound each run (events per file, bytes per file) so a box cannot make the host spin.
- After a session has ended and its spool file is fully ingested, remove the file (add a `safefs` removal that is also resolved beneath the root).
- **Reconciliation.** `devc box session ps` (in the box) reports the sessions it can see alive: tmux windows with `@devc_session`, plus processes of user `dev` whose environment contains `DEVC_SESSION`. Any session the journal shows as started and not ended that is not in that list gets a host-written `session.end` with reason `lost`.
- Ingestion and reconciliation run at the start of `devc ps`, `devc ls` and `devc attach`, and at `devc down` before the box stops.

## 5. Listing

- Derived per session from its journal: harness, worktree, start and end time, current state (last `state` event; `ended` after `session.end`), pending question, last message.
- `devc ps`: session (first 8 characters after the `s`), project, worktree, harness, state, age, and the last message cut to one line. By default live sessions and those ended in the last 24 hours; `--all` for everything; `--json` with full fields.
- `devc ls` gains a sessions column: the number of live sessions and how many are `blocked` or `done` ("2, 1 needs you").

## 6. Context repository

`durable/realms/<realm>/context/` holds `notes/<project>/` and `memory/<group>/` (mounted into boxes since C02) and becomes a git repository.

- The repository's `.git` is never mounted into a box; only the leaf directories are.
- Its working tree is box-written, so the host never runs git there directly. Every git operation on it runs in a throwaway container from the installer image (C03), with no network and only the context directory mounted: `init` on first use, `add -A` and `commit` with a fixed identity (`devc`), `log`, `diff`.
- `devc context commit [realm]` commits if anything changed. It also runs at `devc down`.
- `devc context log [realm]` and `devc context diff [realm]` show history and uncommitted changes.
- Hostile content must not break a commit or leak host files: a nested `.git` directory or file, a symlink pointing outside, device files. Skip or record what cannot be added safely; never follow anything.

## 7. Fake harness additions

Extend `test/fakeharness`: on start it calls `devc box hook fake` with a start event; each input line produces a `prompt` event and `state working`, then after the command a `message` and `state done`; the command `ask <text>` writes a `question` and `state blocked` and waits for the next line as the answer; `exit` writes `session.end`. A flag makes it die without writing an end event.

## 8. Tests

Unit: tmux command construction; hook mappings (table of recorded hook inputs to events, including oversized and malformed input); event validation; journal derivation; offset handling.

Integration (fake harness; tmux built in C03):

1. `devc <fake> --detach` creates a window; `devc ps --json` lists the session as idle.
2. Typing into the window (tests may call `tmux send-keys` in the box directly) produces `prompt`, `state working`, `message`, `state done` in the journal with increasing `seq`.
3. `ask` leads to `blocked` with the question; the next line clears it.
4. `exit` ends the session; `devc ps` shows it ended; its spool file is removed after ingestion.
5. A harness started from a plain shell in the box is registered and ended.
6. Killing the fake harness without an end event: reconciliation marks the session `lost`.
7. Hostile spool: garbage lines, an oversized line, an event naming another session, a symlink in the spool directory whose target exists on the host, a FIFO, a directory named like a session file. Ingestion completes, the journal contains only valid events, and no host file content appears in it.
8. A truncated spool file produces the `note` event and no duplicates.
9. `devc attach` under a pseudo-terminal (use `creack/pty` in the test) shows the fake harness's output and returns after the tmux detach key.
10. Two clients attached at once can select different windows.
11. Context: a file written under `/ctx/notes` in a box is in the repository after `devc context commit`; `/ctx` in the box has no `.git`; a nested `.git` file and an outward symlink under `/ctx/notes` neither break the commit nor bring host content into the repository.
12. `devc down` ingests the remaining events and commits the context.

## 9. Done when

- The tests pass through `./dev test` and `./dev itest`.
- On this host, `devc <fake>` attaches, the session is visible in `devc ps` from a second terminal, and `devc attach` returns to it.
- `STATUS.md` and `LEFTOVER.md` are updated, including the hook field names that could not be confirmed against a live Claude Code or Codex.
