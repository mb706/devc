# X12 — Explorer content: fleet and the web app

Read `docs/dev/chunks/X-content-guide.md` first. It is the work order for every content chunk and it is binding; this file only names your area. Then read `explorer/README.md` (format reference, including "Writing for the explorer").

## Area

- Packages, whole: `internal/fleet`, `internal/archive`. From `internal/hub`: `archive.go`, `retention.go`, and the files that serve or stand in for the web app (`web.go`, `static.go`, `devmode.go`, `devmachine.go`, `devterm.go`; X11 does not cover these).
- From `internal/boxside`: `archiveworker.go`. From `internal/daemon`: `archive.go`. From `internal/cli`: `fleetcmds.go`.
- `web/`: the app's scripts, the service worker, `embed.go`, `check.sh`. The files are JavaScript: refer to them with `file:…~text`, and give each a comment at its top if it has none.

## Records to read for orientation

`STATUS.md` sections C17, C12, C12r2, C19r1, C19r3 and C19r5 (these parts); findings D1-03, D1-04, D1-08 to D1-20, E1-01 to E1-11, E1-13, E1-14, F1-02 with their responses.

## Concerns owed

Use these ids, and `order` values in the range 90 to 99.

- `fleet-archive`: what a machine archives on the hub and why; the uploader; file classes, generations and completion; what the hub checks and counts; retention by checkpoints; what a machine can and cannot do to another's archive and to its own past.
- `state-restore`: `state pull`, `--as-of`, export and import; what is restored from where and what is refused.
- `secret-transfer`: moving secrets between machines so that the hub never sees them.
- `web-app`: how the hub's web app is built and served, its store, the event stream and resynchronisation, the bounded timeline, commands with uncertain outcomes, the terminal, the content security policy.

## Flows owed

- `archive-upload`: one pass of the uploader for one file, from the scan to a completed generation and retention.
- `retention` (tour): how the hub chooses what to drop.
- `state-pull`: a new machine restores a realm.
- `secret-push-pull`.
- `web-app-start`: opening the app to a live inbox.
- `web-send`: sending a prompt from the app, including the case where the answer is lost.

## Diagrams to consider

The life of a generation, by class. What retention keeps on a timeline. The web app's data flow (stream, store, views).

## Notes

- Concerns planned by other chunks are listed in `docs/dev/chunks/X-content-areas.md`. Link (`related`, `[[concern:…]]`) only to those that already exist; the last chunk adds the missing cross-links.
