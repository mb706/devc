# C09 — Status and input

Read `docs/dev/README.md`, `docs/dev/plan.md` (sections 3.5, 3.6, 3.11) and the `STATUS.md` notes of the earlier chunks, especially C04 and C08. Product spec: `specs-v2.md` section 12. Background: `../v2-research/10-unified-interface.md` sections 0, 1 and 6 (what other tools learned about status and input; read these sections only).

After this chunk the daemon knows each session's state from the harness's own status reports, and it can deliver input to a live session reliably: prompts, answers, interrupts. This is the machinery the hub and the web app will drive.

Only the fake harness can be exercised here. Everything specific to Claude Code or Codex is written from their documentation, kept behind one interface, switched off by default where it cannot be tested, and listed in `LEFTOVER.md`.

## 1. Status wrapper: `devc box pty`

Harnesses that support the Program Status Protocol report `idle`, `working`, `done`, `blocked` or `error` through the terminal escape sequence OSC 7501, but only after the terminal answers their query. tmux does not answer it, so a thin wrapper does. Read the protocol specification on the web (search for "Program Status Protocol OSC 7501") before writing the parser.

- When a launch description has `status.osc7501 = true`, the launcher (C03) runs the harness as `devc box pty -- <exe> <args…>` instead of executing it directly. `DEVC_NO_PTY=1` forces the direct path.
- The wrapper allocates a pseudo-terminal, runs the harness on it, puts its own terminal into raw mode, and copies bytes in both directions. It propagates window-size changes, forwards termination signals, restores the terminal on exit, and exits with the harness's status.
- It is transparent: every byte passes unchanged in both directions, with two exceptions. OSC 7501 sequences from the harness are removed from the output stream, and the answer to the harness's query is injected into its input.
- The scanner handles sequences split across reads, both terminators (BEL and ST), and caps a sequence at 4 KiB (longer ones are passed through untouched). Scan for the escape byte with a fast search; do not process output byte by byte.
- Each report becomes a `state` event with source `osc`, written the same way hook events are. The protocol's extra fields (kind, message) map onto the event's `kind` and `message`.
- When the wrapper is active, the hook receiver (C04) no longer writes `state` events for that session; hooks continue to supply `prompt`, `message`, `question` and `harness` events. Signal this through an environment variable the wrapper sets.

## 2. Turns and "seen"

- A session's turn counter increases with every `prompt` event. `SessionView` gains `turn`, `done_turn` (the last turn that reached `done`) and `seen_turn`.
- `session.seen {session, turn}` records a host-written `seen` event in the journal. It is shared by all clients: what you have looked at on one device is read everywhere.

## 3. Commands and receipts

New daemon methods. Every command has a client-chosen `id` (validated like a session ID or a UUID).

| Method | Meaning |
|---|---|
| `session.send {id, session, text, mode}` | `mode` is `queue` (deliver when the session is `idle` or `done`) or `steer` (deliver now) |
| `session.answer {id, session, question, choice?, text?}` | answer the pending question |
| `session.interrupt {id, session}` | stop the current turn |
| `session.keys {id, session, keys}` | named keys from a fixed list (`Enter`, `Escape`, `Tab`, arrows, `C-c`, digits, `y`, `n`) |
| `session.start {id, ref, harness, worktree?, prompt?}` | start a detached session; an initial prompt is queued |
| `session.stop {id, session}` | end the session |
| `command.get {id}` | the stored receipt |

- Before acting, the daemon writes a receipt for the ID (`local/receipts/`), state `accepted`. A repeated ID returns the stored receipt and does nothing else. Final states: `delivered`, `typed-not-submitted`, `unknown`, `refused` (with a reason). Receipts are kept for seven days.
- One command at a time per session; later ones wait in order. Queued commands survive a daemon restart.
- **Input gate.** Before injecting anything, ask tmux for the clients attached to the session's window and their last activity. If a client was active there in the last 3 s, wait (up to 60 s, then deliver anyway). The point is not to interleave with a person typing in that terminal.

## 4. Adapters

One interface, chosen by the manifest's `input.adapter`: `Send`, `Interrupt`, `Answer`, and a capability report.

**`paste`** — works with any terminal program, and is the only adapter exercised by tests.

- The daemon calls the box helper over the channel. In the box: load the text into a uniquely named tmux buffer from stdin, paste it into the session's pane as a bracketed paste (`paste-buffer -p -r -d`), wait 150 ms, then send the manifest's `submit_key` as a separate `send-keys`.
- Verification: the `prompt` event carries the SHA-256 of the submitted text. If a matching event arrives within 5 s, the receipt is `delivered`. If not, send the submit key once more and wait 3 s; then `typed-not-submitted`. A harness without prompt events yields `unknown`. Never retype the text.
- Interrupt: the manifest's `input.interrupt_keys` (default `Escape`), confirmed by a state change within 5 s.
- Answer: for questions raised through a hook, see section 5. For a dialog already on screen, send the manifest's key mapping for the chosen option (`input.answer_keys`, default: the option's number followed by `Enter`).

**`mod`** (Claude Code) and **`app-server`** (Codex) — structured paths through the vendors' own extension points. Write them from the documentation:

- Claude Code: a plugin shipped in the toolbox and loaded through the launch description. It connects to the in-box agent, submits a user message through the plugin API when asked, aborts a turn when asked, and reports turn boundaries.
- Codex: the session is started with the TUI attached to an app-server the launcher starts on a Unix socket in the box; the adapter connects as a second client to start or steer a turn and to interrupt.
- If the documentation is not specific enough to implement one of them with confidence, implement the adapter interface as far as it goes, return "not available" at run time, and describe precisely in `LEFTOVER.md` what is missing.
- Both stay opt-in: the built-in manifests keep `adapter = "paste"`. `LEFTOVER.md` gets the steps to verify each with a real login and switch it on.

## 5. Questions

When a harness asks the user something through a hook that can carry an answer back (Claude Code's `AskUserQuestion` reaches a `PreToolUse` hook, which may return the answers):

- The hook receiver writes the `question` event (ID, text, options) and `state blocked`.
- If no client has been active on the session's window in the last 60 s, it waits for a remote answer through the box channel, up to the manifest's `input.question_wait` (default 10 minutes, and below the hook's own timeout). An answer that arrives is returned in the form the harness expects; the event `answer` is recorded.
- If someone is at the terminal, or the wait runs out, or no daemon is connected, the hook returns nothing and the harness shows its normal dialog. A later `session.answer` then uses the adapter's on-screen path.
- The hook must still never break a session: any internal error means "return nothing".

## 6. CLI

`devc send <session> [text | -]` (`--steer`; `-` reads stdin), `devc answer <session> <choice or text>`, `devc interrupt <session>`, `devc keys <session> <key>…`, `devc seen <session>`. `devc <harness> --detach --prompt "<text>"` starts a session with a queued first prompt. All print the receipt; `--wait` blocks until it is final. They need the daemon and say so when it is not running.

## 7. Fake harness additions

- At start it sends the OSC 7501 query and, if answered, reports its state changes through OSC 7501.
- It enables bracketed paste and treats a paste followed by the submit key as one prompt, reporting the prompt's hash through its hook.
- Modes for tests: ignore the first submit key; never submit; stay busy for N seconds; a `cat` mode that echoes input bytes unchanged; print the window size when it changes; ask a question through a hook that waits for an answer.
- It reacts to the interrupt key by ending the current turn.

## 8. Tests

Unit: the OSC scanner (split sequences, both terminators, oversized sequences, sequences mixed with arbitrary bytes); receipt state machine and idempotence; queue ordering; gate decision.

Integration:

1. Wrapper: states from OSC reports appear as events with source `osc`, and hook `state` events for that session stop.
2. Wrapper transparency: random bytes sent through the `cat` mode come back identical; a resize reaches the harness; the exit status is passed on; the terminal is restored after the harness is killed.
3. `devc send`: the fake harness receives the prompt exactly once; the receipt is `delivered`. Sending the same command ID again returns the same receipt and types nothing.
4. A 2,000-character, five-line prompt arrives intact.
5. `queue` waits while the session is working and delivers when it is done; `steer` delivers at once.
6. Gate: with a client attached and recently active on that window, delivery is delayed.
7. A harness that ignores the first submit key ends as `delivered` after the second key; one that never submits ends as `typed-not-submitted`; the text was typed once.
8. `interrupt` ends a long turn.
9. Question with nobody attached: `devc answer` is returned through the hook and the turn continues. With the wait expired: the harness shows its dialog and `devc answer` uses the on-screen path.
10. Commands to an ended session are `refused`.
11. Queued commands and receipts survive a daemon restart.
12. `--detach --prompt` delivers the first prompt after the session becomes idle.

## 9. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `STATUS.md` and `LEFTOVER.md` are updated, with a verification recipe for each harness-specific part.
