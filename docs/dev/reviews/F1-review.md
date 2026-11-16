1. **F1-01 — (d), medium — Failed group termination is discarded on cancellation.**  
   Files: [internal/execx/execx.go:279](/home/mewse/devcontainer/devc/internal/execx/execx.go:279)–282; [internal/daemon/relay.go:442](/home/mewse/devcontainer/devc/internal/daemon/relay.go:442)–447.

   `run` preserves `ErrGroupLeft` only when `cmd.Wait()` succeeded. Watchdog cancellation normally makes `cmd.Wait()` fail, so a surviving group is reported without `ErrGroupLeft` and `runPush` removes its quarantine, contrary to the E1-16 response.

   **Scenario:** The watchdog ends receive-pack while index-pack remains in uninterruptible filesystem I/O beyond the 30-second group wait. Its still-open quarantine files are unlinked and the relay lock is released despite the detected survivor.

   **Suggested fix:** Preserve the group error regardless of the command’s exit error, using `errors.Join` or giving `ErrGroupLeft` precedence. Leave the quarantine intact and prevent further relay writes until the surviving writer has ended.

2. **F1-02 — (e), medium — `--as-of` loses the legacy fallback in mixed-time histories.**  
   File: [internal/fleet/restore.go:409](/home/mewse/devcontainer/devc/internal/fleet/restore.go:409)–425.

   Once any generation has `Completed`, selection excludes every untimed generation. If all timestamped generations are newer than the requested time, restore omits the file even when an older usable generation remains archived.

   **Scenario:** A database’s generation 0 was completed before C19r3 and has no completion timestamp. Generation 1 completes after upgrading on Tuesday; pulling as of Monday now omits the database and incorrectly reports that its first archived generation is newer.

   **Suggested fix:** Preserve the accepted untimed-generation fallback when a file also has timestamped generations, and report that fallback in `AsOfCurrent`. Only classify a file as newer when its usable history establishes that.

## Unconfirmed

- **F1-U1 — (e), medium — Group signaling continues after the leader is reaped.**  
  Files: [internal/execx/execx.go:279](/home/mewse/devcontainer/devc/internal/execx/execx.go:279), [internal/execx/execx.go:294](/home/mewse/devcontainer/devc/internal/execx/execx.go:294)–300.

  `cmd.Wait()` releases the leader’s PID before `awaitGroup` finishes checking and signaling the numeric PGID. PID reuse could therefore make cleanup kill an unrelated newly created group, including another legitimate transfer; I have not confirmed this with bounded box effort. Keep the leader unreaped until group checking and signaling are finished.

2 findings must be fixed before the commit can stand.