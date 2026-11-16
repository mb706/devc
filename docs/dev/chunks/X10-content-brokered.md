# X10 — Explorer content: GitHub, service logins, the desktop

Read `docs/dev/chunks/X-content-guide.md` first. It is the work order for every content chunk and it is binding; this file only names your area. Then read `explorer/README.md` (format reference, including "Writing for the explorer").

## Area

- Packages, whole: `internal/github`, `internal/svclogin`, `internal/gui`.
- From `internal/boxside`: `github.go`. From `internal/daemon`: `github.go`, `services.go`. From `internal/gitx`: `github.go`. From `internal/config`: `services.go`.
- From `internal/cli`: `ghcmds.go`, `svccmds.go`, `guicmds.go`.

## Records to read for orientation

`STATUS.md` sections C15, C18, C19, C19r2 and C19r4 (these parts); findings D2-01 to D2-05, D2-07 to D2-09, D2-16, E2-01, E2-02, E2-07 with their responses.

## Concerns owed

Use these ids, and `order` values in the range 70 to 79.

- `github`: the three ways a box deals with GitHub: reading through the daemon with the host's credential, asking for a pull request or comment, and holding a short-lived App token; what each exposes.
- `service-logins`: services without tokens: definitions, the default-deny proxy, where the credential is added and removed, cookie logins in a throwaway container, the rule checks on a request.
- `desktop`: the sidecar that shares only the box's network namespace, the display's secrets, limits, reuse and the browser view.

## Flows owed

- `gh-read`: `devc gh pr view` in a box.
- `request-pr`: a box asks for a pull request; the host creates it after approval.
- `app-token`: `git push github` from a box with an App token, from the start record to the minted token.
- `service-request`: a request from a box to a service, with every check and the credential's path.
- `service-login`: `devc service login` for a cookie service.
- `gui-up`: `devc gui up` to a viewer in the browser.

## Diagrams to consider

For each credential (the GitHub token, the App key, a service credential, the display cookie): where it lives, who can read it, and what the box gets. The request checks of the service proxy in order.

## Notes

- Concerns planned by other chunks are listed in `docs/dev/chunks/X-content-areas.md`. Link (`related`, `[[concern:…]]`) only to those that already exist; the last chunk adds the missing cross-links.
