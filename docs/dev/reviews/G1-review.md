**Part A**

Flow step numbers below count `[[step]]` entries, including section entries.

1. **G1-01 — mismatch — `explorer/model/concerns/box-lifecycle.md`: “adding one needs a new start.”**

   A running box can delete its readiness marker. A subsequent `up`, with an unchanged definition hash, calls `afterStart` without restarting the container and rewrites its capability record from the **current** configuration (`internal/box/lifecycle.go:334`, `:345`, `:669`, `:701`). Thus a newly configured daemon-delivered secret can become available without a new container start.

   **Correction:** Finishing an already-running box’s startup also replaces its capability record; the record does not invariably preserve the grants of its actual start.

2. **G1-02 — misleading — `explorer/model/concerns/toolbox-and-harnesses.md`: “A harness credential is written only into the box’s memory-backed secrets directory, as a 0400 file.”**

   Shared-home logins write credentials into durable realm state, which boxes mount read-write (`internal/harness/login.go:91`, `internal/harness/box.go:221`). File delivery also permits persistent storage when explicitly enabled (`internal/secrets/secrets.go:235`).

   **Correction:** The 0400 runtime-file rule applies to file delivery of token/key credentials. Shared-home credentials reside in writable durable state, and the persistent-secrets override relaxes the memory-backed requirement.

3. **G1-03 — mismatch — `explorer/model/concerns/box-init-and-agent.md`: “The readiness marker … was written by the box init of that start.”**

   The host checks only whether `ready` is a regular file; it checks neither its contents nor its writer (`internal/box/lifecycle.go:638`, `:660`). The runtime mount is box-writable (`internal/box/spec.go:333`). `TestInitWritesReadyAndWaits` checks ordinary marker creation, not authenticity (`internal/boxside/boxside_test.go:158`).

   **Correction:** Clearing the old marker prevents its accidental reuse; it does not authenticate the replacement. The same false guarantee appears in `box-lifecycle.md`, the readiness glossary/layout entries, channel `podman-box`, and the X5-added comment at `internal/box/lifecycle.go:626`.

4. **G1-04 — wrong — `explorer/model/concerns/box-lifecycle.md`: “A plain `podman start` outside devc. No capability record exists for it.”**

   Capability records are removed by devc’s publication and cleanup paths, not automatically when a container stops (`internal/box/lifecycle.go:526`, `:821`). An out-of-band stop can leave the record behind; restarting the same container preserves its ID, which is the daemon’s record-identity check (`internal/daemon/secrets.go:30`).

   **Correction:** A plain start has no usable record if devc previously removed it. An out-of-band restart can retain an accepted record from an earlier run; the separate internet-level binding check still applies.

5. **G1-05 — omission — `explorer/model/concerns/box-lifecycle.md`: “`down`, `rm` and a recreation leave a running box running when its work could not be banked, unless `--no-bank` was given.”**

   The banking hook explicitly skips banking and permits stopping when configuration problems affect the project, without requiring `--no-bank` (`internal/cli/gitcmds.go:135`). Both `Down` and `Remove` accept that result (`internal/box/lifecycle.go:758`, `:854`).

   **Correction:** `down` and `rm` also stop without banking when configuration problems prevent it, after printing a warning. The invariant needs this exception.

6. **G1-06 — mismatch — `explorer/model/concerns/isolation.md`: “No mount source or nested mount point is reached through a symlink.”**

   Trusted roots may themselves be symlinks: `EnsureBeneath` explicitly resolves them (`internal/paths/paths.go:662`). Moreover, checking nested mount points is separate from Podman’s later mount operation; another running box can change a realm-shared parent between them (`internal/box/dirs.go:151`, `internal/box/lifecycle.go:521`, `:550`).

   **Correction:** devc rejects symlinks below trusted roots during its checks. Trusted-root symlinks are allowed, and those checks do not eliminate races involving realm-shared parents.

7. **G1-07 — mismatch — `explorer/model/concerns/isolation.md`: “No process in a box holds a capability.”**

   `Manager.Root` deliberately executes with `Privileged = true`, and the engine renders `--privileged` (`internal/box/exec.go:213`, `internal/engine/ops.go:312`). The cited specification test checks container-creation arguments, not this exec path (`internal/box/spec_test.go:147`).

   **Correction:** Ordinary box processes start without capabilities. A host-authorized `devc root` command is an explicit exception.

8. **G1-08 — misleading — `explorer/model/flows/harness-login.toml`, steps 20–22: “At the next devc up.”**

   For an already-running, ready box, `up` returns without running the preparers or delivering the newly stored credential (`internal/box/lifecycle.go:331`). Delivery occurs through `publish` on creation/start paths (`internal/box/lifecycle.go:502`).

   **Correction:** A newly stored file-delivered credential reaches an existing running box after a stop/start or recreation. Plain `up` on a ready box does not refresh it.

9. **G1-09 — mismatch — `explorer/model/concerns/box-lifecycle.md`, diagram `box-states`: `starting → absent`, “a check fails: removed.”**

   A failed restart of an existing stopped container uses `rollbackStop`, retaining the container (`internal/box/lifecycle.go:355`, `:585`). Failure while finishing an already-running box leaves it running (`internal/box/lifecycle.go:345`). The cited rollback test explicitly covers the retained-container case (`internal/box/lifecycle_test.go:778`).

   **Correction:** Removal applies to a container created by this invocation. An existing container is stopped again, or left running when this invocation did not start it.

10. **G1-10 — misleading — `explorer/model/concerns/box-lifecycle.md`: “`Manager.Remove` does the same with `podman rm --force`.”**

    When the container is already absent, `Remove` skips `afterStop`; `Down` calls it in that situation (`internal/box/lifecycle.go:847`, `:867`, `:743`). Ending open sessions happens in the skipped hook (`internal/cli/sessioncmds.go:904`).

    **Correction:** Removing an existing container runs the after-stop hooks. Removing an already-absent container performs cleanup but does not end its open sessions through those hooks.

11. **G1-11 — misleading — `explorer/model/concerns/box-lifecycle.md`: Root “first appends the command to the env’s `root-log.jsonl`.”**

    Logging is conditional on `r.Env != ""`; projects configured directly with `image` normally have no env and bypass it (`internal/box/exec.go:208`).

    **Correction:** Root commands are logged for projects using an env. Direct-image projects execute them without an env root-log entry.

12. **G1-12 — wrong — `explorer/model/concerns/host-foundations.md`: “A file is truncated or replaced while being tailed … reports `ErrShrunk`.”**

    `ReadFrom` checks only whether the current size is below the supplied offset. It tracks no inode identity and does not detect replacement or truncate-and-regrow when the resulting size reaches that offset (`internal/safefs/safefs.go:210`, `:224`).

    **Correction:** `ErrShrunk` detects a file shorter than the offset at the size check. Other replacements and truncations require additional caller-side detection.

13. **G1-13 — wrong — `explorer/model/layout.toml`, project mount: “host code never runs tools in it or walks it”; projects tree: “never walks it.”**

    On SELinux hosts in labels mode, publication runs host `restorecon -R` over the project directory (`internal/box/lifecycle.go:518`, `:601`, `:613`).

    **Correction:** Host code avoids ordinary project-tree traversal, with an explicit exception for recursive relabeling by `restorecon`.

14. **G1-14 — mismatch — `explorer/model/concerns/launching-a-harness.md`: “A session whose process ends in its window gets a `session.end` … unless one is there already.”**

    The process starts before a separate tmux invocation tags its window (`internal/boxside/session.go:168`, `:187`). If it exits first, the global pane-died hook can receive an empty session option, and `SessionEnded` writes no event for an invalid ID (`internal/tmux/tmux.go:169`, `internal/boxside/session.go:220`). `TestSessionEnded` supplies an already-valid ID directly (`internal/boxside/session_test.go:125`).

    **Correction:** The pane-died mechanism records exits once the window is tagged. A sufficiently early exit can escape that mechanism.

15. **G1-15 — mismatch — `explorer/model/concerns/launching-a-harness.md`: StartSession checks that the executable “is not devc”; also `flows/session-launch.toml`, step 9.**

    `StartSession` calls `notSelf` with an empty self path (`internal/boxside/session.go:148`). That explicitly disables the same-file comparison (`internal/boxside/launch.go:554`).

    **Correction:** Before opening the window, StartSession checks that the executable is a regular executable file. The launcher performs the devc-self comparison later.

16. **G1-16 — omission — `explorer/model/concerns/launching-a-harness.md`: “While the wrapper reports a session’s state, the hooks’ state events for it are dropped.”**

    Question hooks return before the OSC filtering branch (`internal/boxside/hook.go:71`). They deliberately append blocked state and, after an answer, working state even while the wrapper is active (`internal/boxside/question.go:171`, `:232`). `TestHookStatesWithWrapper` exercises only `UserPromptSubmit` (`internal/boxside/input_test.go:217`).

    **Correction:** OSC filtering suppresses ordinary mapped hook states. Question-hook blocked/working states are exceptions; the same qualification is needed in the status-marker layout entry and status-scanner flow.

17. **G1-17 — wrong — `explorer/model/concerns/launching-a-harness.md`: “The question goes out before it is recorded. The daemon then knows about the waiting hook by the time a client sees the question.”**

    `askQuestion` starts the RPC in a goroutine, then immediately records the question. It waits for no acknowledgment or registration barrier (`internal/boxside/question.go:217`, `:222`). The spool write can therefore precede even starting the RPC.

    **Correction:** The RPC and question recording are initiated concurrently. Their ordering at the daemon is not guaranteed.

18. **G1-18 — omission — `explorer/model/flows/box-up.toml`, step 8 and the transition to step 10.**

    The flow mentions `--recreate` but omits the operations required before publication: before-stop hooks, dependent removal, old-container removal, after-stop hooks and cleanup (`internal/box/lifecycle.go:294`). A stopped container retained without recreation follows `startStopped`, skipping container creation (`internal/box/lifecycle.go:349`).

    **Correction:** Add the recreation branch before publication, including its banking refusal point. Show that starting an existing retained container skips `podman create`.

19. **G1-19 — mismatch — `explorer/model/concerns/toolbox-and-harnesses.md`: “A vendor installer sees no part of the host but the new version directory it fills.”**

    Installers also receive read-only mounts for `install.needs`; the built-in pi manifest requests Node.js (`internal/toolbox/harness.go:470`, `:484`, `:552`; `assets/harness/pi.toml:21`).

    **Correction:** The installer receives its new version directory read-write and any required toolbox components read-only.

20. **G1-20 — comment — `internal/box/lifecycle.go:1`: “up … down, rm, rebuild and ls, and the box lock they all hold.”**

    `Manager.List` takes no box lock (`internal/box/lifecycle.go:963`); the concern page correctly says that `ls` does not hold it.

    **Correction:** The mutating lifecycle operations hold the box lock; `ls` reads without it.

**Part B**

- **Foundations, toolbox, harnesses (X4), 1 — confirmed, low.** Login options name the installer image directly, while image preparation occurs in installation, not login (`internal/harness/login.go:129`; `internal/toolbox/harness.go:450`).
- **Foundations, toolbox, harnesses (X4), 2 — confirmed, medium.** Vendor status output from the shared-home container is printed directly, without terminal escaping (`internal/harness/login.go:179`; `internal/cli/harnesscmds.go:701`).
- **Foundations, toolbox, harnesses (X4), 3 — confirmed, low.** GC enters a blocking `flock` with no timeout or context, so its CLI timeout cannot interrupt that wait (`internal/toolbox/harness.go:725`; `internal/toolbox/toolbox.go:304`).
- **Foundations, toolbox, harnesses (X4), 4 — confirmed, medium.** After checking whether each PID still exists, the function unconditionally sends SIGKILL by numeric PID, without pinning its identity (`internal/doctor/statx.go:181`).
- **Foundations, toolbox, harnesses (X4), 5 — confirmed, low.** Publication completes and `ok` becomes true before `EnsureShim` can return an error, leaving the new version current despite command failure (`internal/toolbox/harness.go:531`).
- **Foundations, toolbox, harnesses (X4), 6 — not a defect.** The current built-in Codex manifest explicitly uses shared-home authentication; the alleged Codex-specific env-token statement is absent from the current C03 description (`assets/harness/codex.toml:33`; `docs/dev/STATUS.md:153`).
- **Boxes and the box side (X5), 1 — confirmed, medium.** Generated tmux configuration enables both passthrough and clipboard writes for attached terminals (`internal/tmux/tmux.go:459`, `:465`).
- **Boxes and the box side (X5), 2 — not a defect.** Hardening is requested through container arguments and enforced by Podman/kernel mechanisms; the self-test is an optional diagnostic, explicitly invoked by the escape test (`internal/box/spec.go:232`, `:436`; `test/integration/escape_test.go:78`).
- **Boxes and the box side (X5), 3 — confirmed, low.** The absent-container branch bypasses `afterStop`, which is where open sessions are ended (`internal/box/lifecycle.go:847`, `:867`; `internal/cli/sessioncmds.go:904`).
- **Boxes and the box side (X5), 4 — not a defect.** This helper deletes files rather than preserving their ownership, and explicitly receives DAC_OVERRIDE and FOWNER for that job (`internal/box/lifecycle.go:922`).
- **Boxes and the box side (X5), 5 — not a defect.** The implementation deliberately appends JSON records to `root-log.jsonl`; the `env.toml` statement is stale specification text (`internal/box/exec.go:218`; `specs-v2.md:183`).

Estimated statements checked: about 700; findings: 20 in Part A, plus 7 confirmed suspected defects in Part B.