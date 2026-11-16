# X8 — Explorer content: network, the SSH gate, secrets

Read `docs/dev/chunks/X-content-guide.md` first. It is the work order for every content chunk and it is binding; this file only names your area. Then read `explorer/README.md` (format reference, including "Writing for the explorer").

## Area

- Packages, whole: `internal/netpolicy`, `internal/gate`, `internal/secrets`.
- From `internal/boxside`: `net.go`, `netrelay.go`, `gate.go`. From `internal/daemon`: `netproxy.go`, `router.go`, `gate.go`, `secrets.go`. From `internal/box`: `netexceptions.go`. From `internal/config`: `secretfiles.go`.
- From `internal/cli`: `netcmds.go`, `netallow.go`, `sshcmds.go`.

## Records to read for orientation

`STATUS.md` sections C07, C07r, C10a (secrets), C10b, C13, C12r1, C19r1 and C19r3 (the network parts); findings C1-02, C1-12, D1-01, D1-02, D1-06, D1-21, E1-12, E1-15 with their responses.

## Concerns owed

Use these ids, and `order` values in the range 50 to 59.

- `network-levels`: what each level allows and how it is enforced: the rules the host installs in the box's network namespace, when they are installed, how readiness is bound to this start of this box, trusted resolvers, the canary, failing closed.
- `allowlist-proxy`: the proxy in the daemon, the relay through the box channel, how a destination is matched and the connected address checked, tunnels, exceptions and their expiry, the log.
- `forwards-and-local-urls`: `devc forward`, the router for local URLs and what it refuses.
- `ssh-gate`: how a box uses SSH hosts without ever holding a key: the gate key, the server started per connection, `gate-exec`, the filter, connection sharing, grants.
- `secrets`: where secrets live on the host, the two ways of delivering them, the rule that a box gets only what it was started with, redaction.

## Flows owed

- `network-install`: during `devc up` at level `internet`, from the engine's init to rules that are verified before the box starts.
- `allowlist-connect`: a request from a box at level `allowlist` to the upstream, with every check.
- `net-allow`: a temporary exception from `devc net allow` to its expiry.
- `local-url`: a browser on the host opens a box's port.
- `ssh-through-gate`: `ssh <host>` in a box to the remote shell.
- `secret-request`: a session asks the daemon for a secret at launch.

## Diagrams to consider

The rule set of a box at level `internet` as a decision path. The life of an exception. Where each key and secret lives and what the box holds instead.

## Notes

- Concerns planned by other chunks are listed in `docs/dev/chunks/X-content-areas.md`. Link (`related`, `[[concern:…]]`) only to those that already exist; the last chunk adds the missing cross-links.
