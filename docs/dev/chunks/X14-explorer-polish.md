# X14 — Explorer: polish after the content chunks

Nine authors have written the model with the tool and the app, and each reported what stood in their way. This chunk works through their list. Read `explorer/README.md` (design and format reference, binding), the "X1" to "X3" sections of `docs/dev/STATUS.md`, and the constraints and design direction in `docs/dev/chunks/X2-explorer-app-code.md`; they apply unchanged.

The model now holds about forty concerns and sixty flows. Build the site and look at it before changing anything: several items below are about how real content reads, which nobody could judge when the app was built.

## How to work

- Group the items by what they touch (the connection check, the queries, `check.sh`, the app's rendering, the documents) and fix them group by group.
- Where an item changes the connection check, run `./dev explore --check` before and after and compare: every flow that connected must still connect, and every path that changes must be at least as direct as before. If a change makes a flow's `unchecked` unnecessary, leave the model as it is and list the flow in your report; the next chunk edits the model.
- After the list, look at the whole app with the full model as the owner would: every view, both themes, 1280 and 1920 pixels. Fix what reads badly now that the content is real: concern pages with long side columns, the flows index with sixty entries, the runtime map with more actors and channels than it was designed with, the glossary with a hundred terms, search results across everything. Use your judgment and say what you changed.
- Update `explorer/README.md` ("Writing for the explorer" and the reference tables) and `docs/dev/chunks/X-content-guide.md` where an item says the documents should say something.
- Extend `explorer/check.sh` for what you add. It must pass.
- Do not change the model's content, and do not change the format in a way that would invalidate it.

Rules as always: plain HTML, CSS and JavaScript from `file://`; no dependencies; never `rm` with an unguarded variable; no commits; no background processes left behind; run the tool's unit tests if you change the tool.

At the end add an "X14" section to `docs/dev/STATUS.md` and report in at most 300 words: what you fixed, by group; what you chose not to do and why; which flows could now drop an `unchecked`; what you changed after looking at the whole with real content.

# The list

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

## From X9

- A step that names the function holding a command's run literal cannot be reached from anything, so a flow has to start at one of the literal's callees. Treat a command's constructor and its run literal as one for the connection check.
- The shortest-path rule again chose a misleading route (`data-push` through `Service.Pull` until `Service.Push` was named as a step). Same fix as for X7: prefer the nearest earlier step.
- `query flow <id>`: print the paths the tool found (third request).

## From X10

- Closures held in local variables and functions passed as values are not followed by the call graph; after a `cmd:` step the tool connects from the literal's other callees. Follow a function literal assigned to a local variable and called in the same function.
- A word counter for concern pages (prose only), reported by `--check` when a page passes the limit.
- Diagram labels such as `127.0.0.1:6001` wrap in the middle of a token; do not break inside code-like tokens.

## From X11

- A table wider than three columns is clipped in a concern page without any sign. Let wide tables scroll sideways, with a visible cue.
- The call graph cannot follow net/http's router into the hub's `guard`, so the front's checks can appear only as `checks`. A way to declare such a hand-over in the model (a route's handler is reached through a named wrapper) would let flows verify it.
- The guide should say that a step naming the function that holds a command's literal does connect when it is the actor's first step.
- A word counter in `--check` (second request).

## From X12

- `unchecked` does nothing on a step whose reference is a `file:` reference, so steps in JavaScript show no connection status at all. Show the author's reason there, marked as not verified.
- A method constant used as a map value counts as "listed" rather than "sent", so the hub's forwarding of commands needed `unchecked`. Classify a method that is looked up in a map and then passed to a call as sent by the function that does the call.
- The shortest-path rule sometimes reports a misleading connection (third report).
- A diagram edge from a node to itself shows no label.

