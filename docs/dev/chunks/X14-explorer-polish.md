# X14 — Explorer: polish after the content chunks

Small problems in the tool and the app that the content authors met. This list grows until the content is written; then one chunk works through it.

## From X4

- A tour step without `at` shows its symbol name twice (`explorer/app/js/views/flows.js`, the step list).
- A flow's final "reply" step cannot be connected when the command's run function is a function literal that prints the result; authors point it at the printed type instead. The check could accept the enclosing command's function.
- `explorer/check.sh --shot` captures only the visible part of a page, and `?step=N` does not scroll the sequence diagram to that step before the screenshot. Offer a full-page capture.
- The outline (app and `query outline`) drops a doc comment's first line when it starts with "(".
- With seven actors, the lane titles of a sequence diagram are cut after about nine characters.
- There is no reference form for a section of `docs/dev/STATUS.md`, where deviations are recorded (check whether `doc:` with a heading anchor already covers it, and document it).
