# X5 — Explorer content: boxes: life cycle, isolation, and the box side

Read `docs/dev/chunks/X-content-guide.md` first. It is the work order for every content chunk and it is binding; this file only names your area. Then read `explorer/README.md` (format reference, including "Writing for the explorer").

## Area

- Packages, whole: `internal/box`, `internal/engine`, `internal/tmux`, `internal/ptywrap`.
- From `internal/boxside`: `boxside.go`, `init.go`, `identity.go`, `selftest.go`, `clone_*.go`, `probe_*.go`, `agent.go`, `agentclient.go`, `helper.go`, `launch.go`, `session.go`, `pty.go`, `hook.go`, `spool.go`, `input.go`, `question.go`, `extension.go`, `display.go`.
- From `internal/cli`: `boxcmds.go`. From `internal/toolbox`: `seccomp.go` belongs to your `isolation` concern too (its comments are X4's).

## Records to read for orientation

`STATUS.md` sections C02, C03, C03r, C04, C07, C07r, C09 and C12r1; reviews A and B with their responses.

## Concerns owed

Use these ids, and `order` values in the range 20 to 29.

- `box-lifecycle`: from `project.toml` to a running container and back: the spec, images, directories, `up`, `down`, `rm`, recreation, drift, the per-box lock, the start record and the capabilities a box was started with.
- `isolation`: what a box cannot do and how each restriction is put in place: the user namespace and its ID mapping, dropped capabilities, no new privileges, the seccomp profile, what is mounted and how, SELinux labels, resource limits, the identity fix, the self-test.
- `box-init-and-agent`: the first process in a box, the agent and its socket, what the agent serves, how tools in the box reach it.
- `launching-a-harness`: shims and launch descriptions, tmux, the status wrapper with its pseudo-terminal and scanner, hooks and the spool, questions, the box's half of input.

## Flows owed

- `box-up`: `devc up` to a running, checked box.
- `box-init`: inside the box, from the first process to an agent that answers.
- `session-launch`: typing a harness's name in a box (or `devc run`) to a harness running in tmux under the wrapper.
- `hook-to-spool`: a harness hook to a line in the spool.
- `box-down`: `devc down` and what is preserved first.
- `status-scanner` (tour): how the wrapper reads status reports out of terminal output.

## Diagrams to consider

The states of a box (absent, created, running, drifted, …) and what moves it between them. The process tree in a running box. The mounts of a box by source and mode (also as `[[mount]]` entries in `layout.toml`).

## Notes

- Concerns planned by other chunks are listed in `docs/dev/chunks/X-content-areas.md`. Link (`related`, `[[concern:…]]`) only to those that already exist; the last chunk adds the missing cross-links.
- `isolation` is the page that carries the sandbox's central claim. For each restriction, name the exact argument or file that applies it and the check that fails closed when it cannot be applied.
