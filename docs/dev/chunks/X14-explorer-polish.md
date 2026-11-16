# X14 — Explorer: polish after the content chunks

Small problems in the tool and the app that the content authors met. This list grows until the content is written; then one chunk works through it.

## From X4

- A tour step without `at` shows its symbol name twice (`explorer/app/js/views/flows.js`, the step list).
- A flow's final "reply" step cannot be connected when the command's run function is a function literal that prints the result; authors point it at the printed type instead. The check could accept the enclosing command's function.
- `explorer/check.sh --shot` captures only the visible part of a page, and `?step=N` does not scroll the sequence diagram to that step before the screenshot. Offer a full-page capture.
- The outline (app and `query outline`) drops a doc comment's first line when it starts with "(".
- With seven actors, the lane titles of a sequence diagram are cut after about nine characters.
- There is no reference form for a section of `docs/dev/STATUS.md`, where deviations are recorded (check whether `doc:` with a heading anchor already covers it, and document it).

## From X5

- `explorer/README.md` should say how a `cmd:` step connects to a run function written as a function literal.
- A `reply` step passed the connection check although its code is reached only through a function value: the check is looser than it looks. Decide whether such a connection should be shown as weaker.
- Two concern pages run to about 1,400 words (`isolation`, `launching-a-harness`); check that long pages still read well, with the side column in view.

## From X6

- `explorer/check.sh --shot` cannot capture a whole concern page (same request as X4 and X5: a full-page capture).
- Each `./dev explore query` starts the container and loads everything, so a loop over files takes minutes. Accept several questions in one run (from arguments or standard input).
- A query that lists the declarations of a package or file that have no doc comment.

## From X7

- A query that prints a flow's connection paths (authors had to read `dist/data/core.js` to see them).
- The shortest-path rule can name a misleading starting step: in `turn-snapshot`, `Relay.Update` is shown as coming from `ClearIncoming`. Prefer the nearest earlier step of the same actor, and fall back to earlier ones only when that fails.
- `--check` needs a filter by file or directory: warnings from other areas hide an author's own.

## From X8

- The tool has no `via` for a plain network connection (a TCP connection to a listener); authors use `stream` or `unchecked`. Consider `via = "net"`.
- The content guide's 1,200-word limit should say that quoted code blocks and tables do not count.
- The README should say that the order in which steps are found depends on the entry function a flow starts at (start a flow at the entry function of the process, not in the middle of a call).

