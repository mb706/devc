# X15 — Explorer content: corrections after the accuracy review

A second model checked the explorer's model against the code, statement by statement, in five passes (`docs/dev/reviews/G1-review.md` to `G5-review.md`; about 4,300 statements checked, 118 findings). Each review has two parts: Part A lists statements in the model, and a few comments in the code, that are wrong, misleading, incomplete or attached to the wrong code; Part B says for each suspected defect in `docs/dev/LEFTOVER.md` part 7 whether it is real.

This work order is used by several chunks; each is given its review files. Read `docs/dev/chunks/X-content-guide.md` (the standard for the content, binding) and `explorer/README.md` first.

## Part A: make every finding's statement true

For each finding in your review files:

1. **Check it yourself in the code.** The reviewer is exact but not infallible, and its line numbers may have moved. Read the code it cites and the statement it objects to.
2. **If the reviewer is right, fix the statement** where it stands: the concern's text, an invariant and its references, a diagram, a flow step (its text, its reference, its order, a missing step), a glossary term, an actor or channel, a package role, a code comment. Fix the cause, not only the quoted sentence: if the same mistake appears elsewhere on the page, in a flow or in the glossary, fix it there too.
3. **Do not answer a finding by deleting what was said.** Say what the code does, as precisely as the page said the wrong thing. If the truth is that something is not protected, or is protected on one path only, the page says exactly that: this is what the owner most needs to know.
4. **If the reviewer is wrong,** change nothing and say why in the response, with file and line.
5. **Where a finding shows that the code contradicts its own stated rule** (the page described the rule; the code does something weaker), the page must describe the code, and the gap goes into `docs/dev/LEFTOVER.md` part 7 under a heading "Found by the accuracy review", in the form of the entries there. Do not fix the code.

An invariant that the code does not hold on every path is not an invariant. Restate it as what does hold, or move it into "What can go wrong" as a known gap with its reference.

Write `docs/dev/reviews/<review>-response.md` for each review file: one short paragraph per finding ID saying what you changed, or why not.

## Part B: record the verdicts

In `docs/dev/LEFTOVER.md` part 7, add the reviewer's verdict to each item your reviews cover: `Confirmed (high|medium|low)`, `Not a defect: <reason>`, or `Undecided without running it`, in bold at the start of the item, keeping the item's text. Where the reviewer adds evidence that sharpens an item, add it in a sentence. Where a concern page or a flow says something that a confirmed defect contradicts, correct the page as under Part A.

## Rules

- Comments in the code may change; code may not. `./dev explore comments-only`, `./dev fmt` and `./dev vet` before you finish, if you touched a comment.
- `./dev explore --check` must report no errors and no new warnings. Every flow that connected must still connect.
- Look at each page and flow you changed in the built site.
- Do not change the tool, the app or the format. Do not rewrite pages beyond what the findings and their causes require.
- Never `rm` with an unguarded variable. No commits. No test suites.

## Report

At most 250 words: how many findings you fixed, how many you rejected and the IDs of those; the findings that turned out to be gaps in the code (IDs and one line each); anything you noticed beyond the findings that is also wrong.
