# C19r5 — Closing fixes

The reviewer's closing check of the last two fix commits (`docs/dev/reviews/F1-review.md` against `53b187d`, `F2-review.md` against `c336d39`) found no high-severity problem and five things to repair: four confirmed and one unconfirmed. This chunk repairs them. It is small on purpose; do not widen it.

Read `docs/dev/README.md`, the two reviews, and the "C19r3" and "C19r4" sections of `docs/dev/STATUS.md`.

Record the outcome per ID in `docs/dev/reviews/F-response.md`, one short paragraph each, with the test that covers it.

| Finding | Decision |
|---|---|
| F1-01 and F1-U1 process groups | Treat both as real and fix them together in the common runner. With the group option, the leader is reaped last: the runner learns that the leader has ended without reaping it, signals the group and watches it empty while the leader still holds the group ID (so the ID cannot have been reused), and only then reaps the leader. If the group cannot be ended within the wait, the runner reports that (`ErrGroupLeft`, joined with whatever else failed, never dropped), and the caller keeps the relay's lock and quarantine as they are until the group is gone: a watcher finishes the clean-up when the last member has ended, and pushes to that relay are refused with a clear message until then. Never signal a group ID after its leader was reaped. Test: a child that ignores termination and outlives its leader, with the runner failing for another reason at the same time; assert that the error is reported, that nothing is removed while the survivor lives, and that clean-up happens once it is gone. |
| F1-02 `--as-of` and untimed generations | A generation without a recorded time counts as older than every timed one. If all timed generations of a file are newer than the requested time, its newest untimed generation is restored, and the command lists those files separately ("restored from a generation without a recorded time"). A file is left out as "newer" only when it has no generation at or before the time and no untimed one. Test the reviewer's scenario. |
| F2-01 redirects of unchecked downloads | A download that is exempt from the checksum rule must end where it was asked for: for CRAN's archive, a redirect is followed only if the destination has the same path (the archive location of that package and version); anything else is refused. Check the other exempt download (GitHub tarballs) for the same weakness: a redirect must stay with the same owner, repository and ref. Test a redirect from the archive into the current-release directory. |
| F2-02 npm versions | What devc treats as an exact version must be a subset of what npm accepts as a version, because only those can never be tag names: the bare form `MAJOR.MINOR.PATCH[-prerelease][+build]`, numeric parts without leading zeros and at most 9007199254740991, identifiers of `[0-9A-Za-z-]`, the whole string at most 256 characters. Everything else is resolved online every time, and an alias stored earlier under such a name is ignored. Test with the tag name `9007199254740992.0.0` moving between two versions. Check the PyPI and CRAN notions of an exact version against the same idea and note what you find. |

Rules as always: build and test only through `./dev`; no commits; never `rm` with an unguarded variable (use `"${VAR:?}"`); clean up what you create and leave no loops or `tail -f` processes behind.

Be economical. Run the unit suite once at the end, and of the integration suite only the tests that cover what you changed (the git push tests, the fleet tests, and the source tests if there are integration tests for them). Do not run the whole integration suite.

Update `docs/dev/LEFTOVER.md` only where an item changes. Add a short "C19r5" section to `STATUS.md`.
