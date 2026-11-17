# Response to the accuracy review G5 (the hub, fleet and the archive, the web app)

Chunk X15c. Every finding was checked in the code; all 27 are right and were fixed where they stand, together with the same belief elsewhere in the model and in code comments. Code comments changed, code did not (`./dev explore comments-only`). Gaps in the code are in `docs/dev/LEFTOVER.md` part 7, "Found by the accuracy review" (item 8 and the smaller list of item 9); Part B's verdicts are recorded there under X11 and X12.

**G5-01 (nothing is renamed or deleted) — fixed.** `hub.archiveStore.put` renames a bundle's `.part` (without replacing), `archiveStore.retain` unlinks generations after a completion, `Server.apiSecretsPut` deletes bundles beyond the newest ten. The invariant of `fleet-archive.md` now says what an upload can and cannot do to stored data (with `retain` and `apiSecretsPut` among its references). The package comment of `internal/hub/archive.go` said the same and was corrected; its retention sentence now also names the generations retention keeps.

**G5-02 (credential files never reach a hub) — fixed, and a gap in the code.** `archive.Excluded` runs only on the machine (`boxside.archiveWalk.walk`, `fleet.Uploader.realmPass`, export, restore); `archive.ValidRel` and `hub.Server.apiArchivePut` accept any harness path. The invariant now says an honest machine leaves such paths out, the hub does not check, and a copy under another name is archived; so do the "Left out" sentence, the box and hub items of `fleet-archive.md`, the invariant of `secrets.md` and the package comment of `internal/archive`. LEFTOVER part 7, item 9.

**G5-03 (quotas before the body) — fixed.** `apiArchivePut` takes admission (`Server.admitted`), reads the body, then `put` checks names and quotas. The invariant now says admission comes before the bounded body is read, and the budget checks before a file is created or extended.

**G5-04 (typed at most once) — fixed.** `daemon.commander.prune` drops final receipts after seven days, `submit` then takes the ID as new, and `resolvePending` reads the 404 as "did not arrive". The summary of `flows/web-send.toml`, its submit and "receipts are kept" steps, a detail on its finish step, the commands paragraph of `web-app.md`, two sentences of `hub-commands-and-terminal.md` and the glossary's pending command now bound the guarantee by the receipt's life. Same as X12 item 7 (confirmed, medium).

**G5-05 (four and sixteen terminals) — fixed.** `daemon.termManager.open` counts, reads the window's size, then inserts. The open step of `flows/hub-terminal.toml` and the terminal paragraph of `hub-commands-and-terminal.md` now say concurrent opens can exceed both; `watching-sessions.md` and the comment of `open` already did (G2-04). X6 item 7.

**G5-06 (`terminal.close` in every case) — fixed, and a gap in the code.** `hub.Server.apiTerminal` defers the close only after a valid answer to `terminal.open`. `hub-commands-and-terminal.md` and the close step of `flows/hub-terminal.toml` now say so; a detail on the reply step says that a stream the daemon did open stays until its session ends, the link closes, the client exits or twelve hours pass (`termMaxLife`). LEFTOVER part 7, item 9.

**G5-07 (an older summary never replaces a newer) — fixed, and a gap in the code.** `putSession` checks the position; `loadSnapshot` replaces every summary and keeps the higher position. The invariant and the data-flow paragraph of `web-app.md` and the loadSnapshot step of `flows/web-app-start.toml` (with a detail) now say so. Because the higher position is kept, stream changes below it are then refused; LEFTOVER part 7, item 9.

**G5-08 (events only in order) — fixed, and a gap in the code.** `hub.machineLink.finishResync` applies every returned event above the view's seq without a succession check. The invariant of `hub-machine-link.md` now separates notifications from re-reads, and the re-read paragraph says the machine's events replace what the hub held unchecked. LEFTOVER part 7, item 9.

**G5-09 (ntfy over HTTPS) — fixed.** `hub.Config.validate` accepts `http` and `https`; `ntfyClient.do` sends the token either way. The channel `hub-ntfy` in `system.toml` and the hub.toml paragraph of `hub-inbox-and-notifications.md` now say so. Not recorded as a gap: the configuration allows it on purpose (its message says "http(s)").

**G5-10 (pending until a receipt or dismissal) — fixed.** `run` keeps no uncertain `seen`; `settleOutbox` deletes a send's pending entry. The invariant of `web-app.md` (with `settleOutbox` among its references) and the glossary's pending command now name both.

**G5-11 (restore refuses a realm with durable state) — fixed.** `fleet.ExistingState` does not look at `gate-log.jsonl.1` or service logs. The invariant of `state-restore.md`, the ExistingState step of `flows/state-pull.toml` (with a detail) and the function's comment now say what it checks; an existing `gate-log.jsonl.1` is kept by the restore's exclusive create. X12 item 7.

**G5-12 (`archive prune` deletes from both trees) — fixed.** `hub.PruneArchive` walks `archive/<machine>` only. The actor `hub-archive` in `system.toml`, the retention paragraph of `fleet-archive.md` and the prune step of `flows/retention.toml` now say so; the actor `hub-cli` names pruning.

**G5-13 (retention window) — fixed.** `hub.retentionDrops` keeps untimed complete generations and incomplete ones newer than the newest complete one, and drops nothing while a recorded time lies ahead. The glossary's retention window and the `files/<path>` entry of `layout.toml` now say so, and that retention runs when a generation first completes. The page's retention paragraph already did.

**G5-14 (a failed upload removes what it created) — fixed.** In `archiveStore.putFile` a failing completion record (`writeMeta`) returns without the undo. `fleet-archive.md`, the create step of `flows/retention.toml`, a detail on the writeMeta step of `flows/archive-upload.toml` and the package comment of `archive.go` now name the exception; `TestArchiveDurableCompletion` shows it and the later completion.

**G5-15 (a receipt for this ID) — fixed.** `api.js` `command` rejects a different `command` only when one is present. The receipt step of `flows/web-send.toml` now says what is accepted.

**G5-16 (uncertain after either call) — fixed.** `hub.Server.apiCommand` answers 200 with the receipt so far when `command.get` fails. The last hub step of `flows/phone-answer.toml` now says so; `hub-commands-and-terminal.md` already did.

**G5-17 (the message is cleared) — fixed, and a gap in the code.** `hub.notifier.update` drops pending records without a clear. The invariant and the records list of `hub-inbox-and-notifications.md`, the glossary's notification record and the last step of `flows/phone-answer.toml` now say that only a confirmed message is cleared. LEFTOVER part 7, item 9.

**G5-18 (another hub's position) — fixed, and a gap in the code.** `hub.Server.apiStream` resets only positions beyond its newest change or below its log. `web-app.md` and the stream paragraph of `hub-commands-and-terminal.md` now say that another hub's position within the range passes; the reset step of `flows/web-app-start.toml` was already right. LEFTOVER part 7, item 9.

**G5-19 (backoff from one second to a minute) — fixed, and a gap in the code.** `stream.js` leaves an erroring `EventSource` to the browser and runs `checkAuth` and `schedule` only when it is closed (or a snapshot fails). `web-app.md` and the last step of `flows/web-app-start.toml` (with a detail) now say so. Checking this showed that when `/api/me` cannot be reached at that moment nothing schedules another try, so the app can stay offline until the browser fires `online` or the page is reloaded: LEFTOVER part 7, item 8.

**G5-20 (three positions) — fixed.** `hub.Server.apiMachines` returns no `stream_id`; `loadSnapshot` uses the inbox's. Steps 13 to 15 of `flows/web-app-start.toml` now say so, with a detail on what a concurrent machine change can then miss.

**G5-21 (breaking a bound closes the link) — fixed.** Only refresh failures close it (`machineLink.fail`); a full re-read queue sets `resyncLost`, a held-event overflow sets `again`. `hub-machine-link.md` and the package comment of `internal/hub/link.go` now distinguish them.

**G5-22 (sessions that ended 30 days ago) — fixed.** `machineLink.prune` and `Store.PruneMachine` select sessions that are not live by `LastEvent`. `hub-machine-link.md` now says so (a later `seen` postpones it), as do the comments of `MaxMachineSessions`, `itemPrune`, `requestPrune`, `prune`, `PruneMachine`, `housekeeping` and `pruneSessions`.

**G5-23 (prefix direction) — fixed.** `settleOutbox` tests whether the waiting text begins with the event's text. The settleOutbox step of `flows/web-send.toml` (with a detail), `web-app.md` (commands paragraph and the prompt-events item) and the glossary's outbox now give the right direction. An event whose text is only spaces settles the first waiting prompt whatever it says; added to X12 item 6.

**G5-24 (`max_file_size`) — fixed.** `fleet.hostFiles` gives host-written files a cap of 1 TiB; only harness candidates get `ArchiveMaxFile`. The "Left out" sentence of `fleet-archive.md` and the syncFile step of `flows/archive-upload.toml` now say so.

**G5-25 (a logged-in user is never limited) — fixed.** `checkSession` consults the limiter only on failure; `apiLogin` and `apiAct` consult the certificate's bucket first; `guard` takes `anonSem` for every device-class request. `hub-access.md`, the first hub step of `flows/hub-login.toml`, the file comment of `internal/hub/front.go`, the comment in `guard` and the comments of `anonSem` and `maxAnonymous` now say so.

**G5-26 (ten characters) — fixed, and a gap in the code.** `cli.app.readPassphrase` compares `len(pw)`, bytes. The invariant and the hub item of `secret-transfer.md` and the passphrase step of `flows/secret-push-pull.toml` now say bytes; the message still says characters. LEFTOVER part 7, item 9.

**G5-27 (cleared after use) — fixed, and a gap in the code.** The confirmation, a refused passphrase, a passphrase read from stdin (a string), the values `SealSecrets` reads, the base64 strings of the set, the local values a pull compares and the values left after a store error are not cleared. The secrets item of `secret-transfer.md` now lists what is cleared and what is not. LEFTOVER part 7, item 9.
