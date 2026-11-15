# C12 — Web app

Read `docs/dev/README.md` and the `STATUS.md` notes of C09, C11a and C11b. Product spec: `specs-v2.md` section 13 (web app). Background: `../v2-research/10-unified-interface.md` section 2 (information architecture that other tools converged on; that section only).

The hub gets its user interface: a small web app, used mostly from a phone, that shows what every agent on every machine is doing and lets the user answer, steer, stop and start sessions. It is served by the hub from files embedded in the binary.

## 1. Constraints

- **No build step.** Plain ES modules, plain CSS. Vendored libraries, each pinned and committed under `web/vendor/` with a note of version, source URL, checksum and licence: Preact with htm, and xterm.js with its stylesheet. Nothing is fetched at run time; the content security policy of C11b (self only, no inline script or style) must hold.
- **Phone first.** Designed for a 390-pixel-wide viewport and touch; usable on a desktop. System fonts. Light and dark through `prefers-color-scheme`. Tap targets at least 44 px. State is always shown as text or icon plus text, never by colour alone.
- **Installable** as a PWA (manifest, icons, a service worker that caches the app shell only). No push through the browser; notifications come from ntfy.
- **Resilient.** The app keeps the last data it had, shows when it is offline or a machine is stale, reconnects the event stream by itself, and resubscribes when the page becomes visible again. Every command carries a client-generated ID and is safe to retry.

## 2. Screens

| Screen | Content |
|---|---|
| Login | password, TOTP field when required; a clear message when the device certificate is missing |
| Inbox (home) | sections in the order of C11a: needs you, failed, finished, working, idle. A row: project, worktree, harness, machine, state, one line of the question or last message, relative time. Filters for machine, realm, project. A counter of rows needing attention. |
| Session | Header with project, worktree, harness, machine, state, elapsed time. A pending question as a card above the composer, with a button per option and a text field. A timeline: the user's prompts, assistant messages, tool calls folded into one line per run ("12 tool calls", expandable), notes, requests. A composer with Send, Steer and Stop. Opening the screen marks the session seen. Tabs: Timeline, Terminal, Changes. |
| Terminal tab | xterm.js showing the session's tmux window as an observer: read-only, and it never resizes the pane. A switch to take control exists only when the hub is configured to allow it. A key bar with Esc, Tab, Ctrl, arrows when in control. |
| Changes tab | files changed with a diffstat, and the unified diff, for: the latest snapshot against the branch tip, and the branch against the upstream default branch. |
| Project | sessions, worktrees with ahead/behind, pending requests with approve and deny for low-risk ones (high-risk ones show the command to run on a machine). |
| New session | machine, project, worktree (existing or a new name), harness, first prompt. |
| Machines | name, online or last seen, number of live sessions. |

## 3. Hub and daemon additions

The API of C11a lacks three things the screens need. Add them, each on the daemon's allowlist for the hub and each read-only or box-scoped:

| Hub endpoint | Daemon method | Notes |
|---|---|---|
| `GET /api/projects?machine=` | `projects.list` | realms, projects, box state, worktrees with ahead/behind and dirty flags, harnesses installed |
| `GET /api/changes/<machine>/<session>?scope=` | `changes.get` | computed on the relay with host git (`--no-ext-diff --no-textconv`), size-capped; never touches the project directory |
| `GET /api/terminal/<machine>/<session>` (WebSocket) | `terminal.open {session, mode}` | the daemon runs the box's tmux client in a pseudo-terminal with the client flags `read-only` and `ignore-size` for observers, and relays bytes. `mode = control` is refused unless `[terminal] control = true` in the hub configuration **and** in the machine's configuration. |

A terminal stream ends when the session ends, when either side disconnects, and after 30 minutes without the page being visible.

## 4. Serving

- Files live in `web/` and are embedded with `go:embed`. The hub serves them with long cache lifetimes for vendored files and revalidation for the app's own.
- The service worker must not cache API responses.

## 5. Checking the result

There is no browser automation in the test suite, so do this:

- Go tests: every file is served with the right content type and the security headers; `index.html` references only files that exist; no file contains an inline script or style attribute handler (a simple scan).
- A hub test mode for UI work: `devc hub serve --dev-no-auth`, which only binds to loopback and only starts when `DEVC_DEV=1`, serves the app and the API without certificates or login, backed by a seeded in-memory set of machines and sessions in every state.
- If a Chrome or Chromium binary exists on this host (look under `~/.local/opt/` and on the `PATH`), use it headless against the test mode: dump the rendered DOM of each screen to confirm the scripts run without errors, and take screenshots at 390×844 and 1280×800 in both colour schemes. Look at the screenshots and fix what is broken or cramped. Keep the small script that does this in `web/check.sh`. If no browser is available, say so in `LEFTOVER.md`.

## 6. Done when

- The Go tests pass, and the hub integration tests of C11a and C11b still pass.
- With a real hub, machine and fake harness on loopback (certificates through the dev mode or a test CA), the flow works end to end: a session appears in the inbox, a question can be answered from the session screen, a prompt can be sent, the terminal tab shows the pane, the changes tab shows a diff.
- `docs/guide.md` gains a section on the web app; `STATUS.md` and `LEFTOVER.md` are updated, noting that nothing was tried on a real phone.
