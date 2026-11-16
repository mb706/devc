# X4 — Explorer content: foundations, configuration, toolbox, harnesses, host setup

Read `docs/dev/chunks/X-content-guide.md` first. It is the work order for every content chunk and it is binding; this file only names your area. Then read `explorer/README.md` (format reference, including "Writing for the explorer").

## Area

- Packages, whole: `cmd/devc`, `internal/names`, `internal/paths`, `internal/safefs`, `internal/flock`, `internal/execx`, `internal/extract`, `internal/display`, `internal/version`, `internal/config`, `internal/toolbox`, `internal/harness`, `internal/service`, `internal/doctor`, `assets`, `examples`.
- From `internal/cli`: `root.go`, `commands.go`, `setupcmds.go`, `toolcmds.go`, `harnesscmds.go`, `servicecmds.go`.

## Records to read for orientation

`STATUS.md` sections C01, C03, C03r and C16; `docs/dev/plan.md` sections 1 to 3; reviews A and their response for these packages.

## Concerns owed

Use these ids, and `order` values in the range 10 to 19.

- `configuration`: the configuration directory as the only source of policy; realms, projects, envs; loading, validation and lookup; grants; the rules for names.
- `host-foundations`: how host code touches files and processes: the layout of state (`paths`), reading what a box controls (`safefs`), locks, running commands (`execx`), unpacking archives (`extract`).
- `toolbox-and-harnesses`: what the toolbox is and how it reaches a box; harness manifests; installing a harness in a throwaway container; rendering managed settings; shims and launch descriptions; logins; versions and their collection.
- `host-setup`: `devc init`, `devc doctor` and its script for root, installing the daemon as a service.
- `command-line`: how the binary decides what it is (CLI, git remote helper, shim, relay hook), how the command tree is assembled, how a `realm/project` reference is resolved, output and exit conventions.

## Flows owed

- `harness-install`: `devc harness install <id>` to a version in the toolbox.
- `harness-login`: `devc login <realm> <harness>`.
- `config-load` (tour): from a project reference to validated configuration.
- `safe-read` (tour): opening a file beneath a directory a box controls.
- `binary-dispatch` (tour): the roles of `cmd/devc`.
- `doctor-fix`: what `devc doctor --fix` does itself and what it leaves to the printed script.

## Diagrams to consider

The configuration tree and the types it loads into. What the toolbox holds and where each part is mounted.

## Notes

- Concerns planned by other chunks are listed in `docs/dev/chunks/X-content-areas.md`. Link (`related`, `[[concern:…]]`) only to those that already exist; the last chunk adds the missing cross-links.
- You are the first content chunk. If the guide or the format stands in your way, say exactly where in your report: the next eight chunks follow the same path.
- `layout.toml`: complete the configuration, state and runtime trees down to the level `internal/paths` defines.
