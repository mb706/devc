# X11 — Explorer content: the hub

Read `docs/dev/chunks/X-content-guide.md` first. It is the work order for every content chunk and it is binding; this file only names your area. Then read `explorer/README.md` (format reference, including "Writing for the explorer").

## Area

- Package `internal/hub` without `archive.go` and `retention.go`; `internal/hubnode`, whole.
- From `internal/daemon`: `hub.go`, `hublink.go`. From `internal/cli`: `hubcmds.go`, `hubaccess.go`.

## Records to read for orientation

`STATUS.md` sections C11a, C11b, C12r2; review C2 with its response; `docs/hub-deployment.md`; `docs/dev/plan.md` section 5.

## Concerns owed

Use these ids, and `order` values in the range 80 to 89.

- `hub-access`: who may talk to the hub and as what: the CA, client certificates for machines and devices, enrolment, login and its lockout, sessions, the access level and cross-site rule of every route, the limits on unauthenticated work.
- `hub-machine-link`: how a machine joins and stays connected, what travels each way, what the hub stores, the bounds on what a machine can make it fetch or keep, resynchronisation, pruning, two hubs and which one notifies.
- `hub-inbox-and-notifications`: the inbox, attention states, the notification records and their retries, ntfy, action tokens.
- `hub-commands-and-terminal`: commands from a browser to a machine and their receipts, what the hub may not approve, the terminal relay, the event stream to browsers.

## Flows owed

- `machine-join`: `devc hub join` to a connected machine.
- `device-enrol`: an enrolment link to a browser with a certificate.
- `hub-login`.
- `event-to-phone`: a question in a box to a notification on the phone and an entry in the inbox.
- `phone-answer`: an answer in the web app to the harness in the box.
- `hub-terminal`: the terminal of a session in the browser.

## Diagrams to consider

The route table as access level by kind of client. The states of a notification record. What the hub holds about a machine and how each part is bounded.

## Notes

- Concerns planned by other chunks are listed in `docs/dev/chunks/X-content-areas.md`. Link (`related`, `[[concern:…]]`) only to those that already exist; the last chunk adds the missing cross-links.
- The hub is reachable from the internet. `hub-access` must let a reader check, route by route, that nothing is served without a certificate and what each level may do.
