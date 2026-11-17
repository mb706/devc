# Writing the explorer's content: the guide for every content chunk

The explorer (`explorer/README.md`) shows two kinds of thing: facts the tool extracts from the code, and a model that people write. Chunks X4 onwards write the model, one area of the code each, and tidy the comments in that area's code. This guide applies to all of them. Each chunk's own work order names its packages and files and the pages it owes.

## The reader

The owner of this codebase: an experienced developer (mostly R and Python, comfortable reading Go) who designed the product, knows what devc is for, and did not write or read the code. They want to understand all of it well enough to trust it, change it and find their way in it. They will read your pages with the code open beside them.

## The standard: nothing that you did not see in the code

The reader will rely on what you write. One wrong sentence costs more than a missing one.

- Write only what you verified in the code during this chunk. Not what the plan or a work order intended, not what a name suggests, not how such systems usually work. The development records (`docs/dev/STATUS.md`, the work orders, the reviews) tell you where to look and why something is as it is; the code tells you what is true.
- If you are not sure, read the function. If you still cannot tell, leave the statement out.
- Every paragraph that says how something works carries references to the code that does it. A claim you cannot attach to a symbol is a claim to check again.
- Say what is, not what was. "The hub keeps checkpoints", not "retention was redesigned in C19r3". History belongs in a reference: `[[finding:E1-04]]`, `[[chunk:C19r3]]`.
- Where the code has a limit, a gap or something surprising, say so. `docs/dev/LEFTOVER.md` part 5 lists the accepted limits.

## How to learn your area without reading every line

1. Read the "Writing for the explorer" section of `explorer/README.md`, then the package comments of your packages.
2. Read the `STATUS.md` sections of the chunks that built your area (the work order names them): they record what was built, what deviates from the plan and why. Skim the review responses that touch your area.
3. Survey each package through the tool instead of opening every file (`./dev explore query …`, see `./dev explore query help`): the outline of a package or file with signatures and first doc sentences, its declarations without a doc comment (`undocumented`), who calls a function, what it calls, the path between two functions, who uses a field, a flow's connections with their paths (`flow`). Ask several questions in one run (`query outline internal/box undocumented internal/box`, or one per line on standard input with `query -`): each run loads the code once.
4. Trace each flow from its entry point (a command, a handler, a hook) and read the bodies of the functions along the way. This is where the reading time goes, and it should.

## What each chunk delivers

### 1. Comments in the code

Bring your area's comments to the level of a well-documented Go project, and no further:

- **Package comment.** It states the responsibility and the security rules the package carries (that is already the project's rule). For a package of several files, add which file holds what, in a few lines. Correct anything in it that is no longer true.
- **File comment**, above the `package` line and separated from it by a blank line, where the file's name does not say enough: `// This file implements …`. One to three lines.
- **Doc comments** on exported identifiers that lack one and on unexported functions and types that are not obvious, especially those your flows and concerns point at. Say why and what for, not what the next line does. Do not comment the obvious, and do not pad.
- Nothing in a comment may mention the explorer or exist only for it.
- The code itself must not change at all. Check with `./dev explore comments-only` before you finish (it compares the code with comments removed against the last commit), and run `./dev fmt` and `./dev vet`.

### 2. `packages.toml`

Each of your packages has a role that is one true sentence, and sits in the right layer and group.

### 3. Concerns

One page per concern your work order lists (you may split or merge with a reason). Use the headings of the README:

- **What it is for**: the problem, in the reader's terms. Three to six sentences.
- **How it works**: the mechanism, in the order a reader needs it. Name the main types and what owns what; say which process each part runs in; describe the life cycle. Explain a Linux or Go mechanism in one sentence where the design depends on it (a user namespace, `openat2`, an nftables set, a yamux stream).
- **What can go wrong, and what prevents it**: for the security-relevant concerns this is the centre of the page. What a box (or a machine, towards the hub) controls here, and the checks that stand in its way, each with its reference.
- **Decisions that are not obvious**: the things a reader would otherwise stop at and wonder about, each with its reason, and the finding or work order behind it where there is one.
- **Where to start reading**: three to six references, in order.

A page is 500 to 1,200 words of prose; quoted code blocks and tables do not count. `./dev explore --check` warns about a page beyond 1,200. If it wants to be longer, it is two concerns.

Front matter: `packages` and `files` (every non-test file of your area must belong to a concern), `key` symbols (the ten or so to know), `spec` sections, `tests` (the tests that show the concern working; the integration tests are executable examples and worth pointing at), `related`.

**Invariants.** Three to eight per concern: the properties that must hold, stated so that one could test them ("a box never receives a secret it was not started with"), each with the code that enforces it and a test where one exists.

**Diagrams.** A state machine for anything with three or more states (a session, a receipt, a request, a generation, a box). A structure diagram where a picture carries relationships better than a paragraph (the ref namespaces of a relay, the layers of a mount). No decoration.

### 4. Flows

The flows your work order lists, plus any you find the area needs.

- Steps are waypoints: the entry point, every crossing from one actor to another, every place where input is checked, every state that is written, and the outcome the user sees. Eight to twenty-five steps.
- A step's `text` is one or two sentences saying what happens there and why it matters. Subtleties (ordering that must hold, what happens on failure) go into `detail`.
- Put `checks` on steps that receive something from a less trusted actor.
- The tool must be able to connect consecutive steps. If it cannot, first suspect the flow: you may have skipped the function that links them, or the order may be wrong. Start each actor's chain at the entry function of its process or goroutine (after a `cmd:` message, the function that builds the command is fine). Use `unchecked` only for a connection that really is invisible to a call graph (a goroutine reading a queue, a file one process writes and another reads, a connection to a listener: `via = "net"`), and say which. `./dev explore query flow <id>` prints the paths the tool found: read them, a path can be found and still be the wrong one.
- A tour (`kind = "tour"`) is a reading order through code that is not a sequence in time: "how a path is resolved safely", "the proxy's request checks in order".

### 5. Glossary, layout, system, interfaces

- A glossary term for every devc word your pages use that a newcomer would not know (and check the existing ones before adding).
- `layout.toml`: the directories and files your area owns on disk, and its mounts.
- `system.toml`: the actors and channels your flows need, if they are missing or wrong. Keep ids stable.
- `interfaces.toml`: only if a table of your area is missing or misdescribed.

Others write in these shared files too. Add and correct; do not reorganise.

## Before you finish

1. `./dev explore --check`: no errors; no warnings from your files that you cannot justify in your report (`--only explorer/model/concerns/<id>.md` shows those of one file or directory).
2. `./dev explore`, then look at your pages: use `explorer/check.sh --shot` to take screenshots of each of your concern pages (`--full` for the whole page) and of each flow at two or three steps. Read them as the owner would. Fix what reads badly, what is cut off, and any diagram that does not read at a glance.
3. `./dev explore comments-only`, `./dev fmt`, `./dev vet`.
4. Read your concern pages once more against the code, sentence by sentence, as if someone else had written them.

Do not run the unit or integration suites: nothing but comments changes in the code. Do not change the tool, the app or the format; if something in them stands in your way, say so in your report.

## Report

At most 300 words: the concerns and flows you wrote; what you changed in comments (files touched, roughly how many comments); anything in the code that surprised you or looks wrong (a possible bug, a comment that contradicted the code, dead code): list these precisely, they are valuable; warnings that remain; what you could not verify and therefore left out.
