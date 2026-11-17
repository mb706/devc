# The devc explorer

A local web app for understanding this codebase: diagrams of what runs where and what depends on what, explanations of each concern, step-by-step flows through the code, and a source browser that ties them together.

```sh
./dev explore            # build explorer/dist/ (about 3 s once the tool is compiled)
xdg-open explorer/dist/index.html
./dev explore --check    # validate the model against the code, build nothing
./dev explore --check --only explorer/model/flows/box-up.toml   # only the messages about these files or directories
./dev explore --strict   # warnings fail too
./dev explore --model <dir> --out <dir>   # another model, another output directory (both below the repository)
./dev explore query help # questions about the code for people writing the model (several per run: query flow box-up symbol internal/box.Up)
./dev explore comments-only [commit]      # did the Go code change only in its comments since HEAD?
explorer/check.sh [--no-build] [dir]      # build, then check the app in headless Chrome (screenshots in dir)
explorer/check.sh --no-build --shot '#/concerns/network' [dir]   # screenshots of one page, both themes, two widths
explorer/check.sh --no-build --full --shot '#/concerns/network' [dir]   # the same, of the whole page
```

`explorer/dist/` is generated and ignored by git. It is a static site without a server and without network access: open `index.html` from disk. The explorer does not describe itself: the packages below `explorer/` are left out.

## How it is put together

| Part | Where | Committed |
|---|---|---|
| The code and its comments | the repository | yes |
| The model: what cannot be read off the code | `explorer/model/` | yes |
| The tool: extracts facts, checks the model against them, writes the site's data | `explorer/tool/` (Go, standard library and the module's own dependencies only) | yes |
| The app: renders the data | `explorer/app/` (HTML, CSS, JavaScript; no dependencies, no build step) | yes |
| The site | `explorer/dist/` | no |

Two rules keep it honest:

1. **Facts are extracted, never retyped.** Packages, files, symbols, signatures, doc comments, who refers to what, the call graph, commands, RPC methods, routes, configuration keys and where code runs all come from the code at build time.
2. **The model points at code by name, and the build checks every pointer.** A reference to a symbol that no longer exists is an error. A flow whose steps are not connected in the code is a warning. So a page cannot go stale without the build saying so.

### What belongs in the code, and what in the model

In the code, as in any well-documented Go project:

- A package comment that states the package's responsibility, the security rules it carries, and, for a package of several files, which file holds what.
- A file comment above the `package` line, separated from it by a blank line, for a file whose name does not say enough (`// This file implements …`).
- A doc comment on every exported identifier and on every unexported one that is not obvious.

Nothing in the code exists only for the explorer: no tags, no markers.

In the model: everything that spans packages. Where code runs and whom it trusts, the layers, the concerns, the flows, the glossary, the layout of state on disk.

## References

Everything in the model points at code and at other model objects with one syntax.

| Reference | Meaning |
|---|---|
| `internal/box.Up` | A package-level function, type, variable or constant: package directory, a dot, the name. |
| `internal/box.Manager.Up` | A method or a field: package, type, member. |
| `pkg:internal/box` | A package. |
| `file:internal/hub/front.go` | A file. For a file that is not Go, `file:web/app/store.js~applyEvent` points at the first line containing the text after `~`. |
| `cmd:devc up` | A command of the CLI. |
| `rpc:session.send` | An RPC method. |
| `http:POST /api/commands` | A route of the hub. |
| `event:question` | A session event type. |
| `config:project.network.level` | A configuration key. |
| `env:DEVC_BOX` | An environment variable. |
| `concern:network`, `flow:box-up`, `term:relay`, `actor:daemon`, `zone:box`, `channel:box-channel`, `surface:daemon-api`, `layer:foundation`, `group:git` | Objects of the model. |
| `diagram:session-states`, `diagram:sessions/session-states` | A concern's diagram: bare in the text of the concern that has it (or when only one concern has one of that id), else with the concern's id. |
| `doc:specs-v2.md#13`, `doc:specs-v2.md` | A heading of a document, by its number or its anchor; or the whole document. The leading words of an anchor are enough when only one heading starts with them: `doc:docs/dev/STATUS.md#x7` is the section "X7 Explorer content: git", where a chunk's deviations are recorded. |
| `finding:C1-05`, `chunk:C10a` | A review finding, a work order. A finding label that two reviews use keeps the plain label in the review whose name starts with its letter; in the other it is `<review>/<label>` (`finding:A/B1` is review A's B1, `finding:B1` review B's). |

In TOML, a reference is a string. In Markdown, write `[[internal/box.Up]]` or `[[internal/box.Up|bringing a box up]]`.

Line numbers are never written down. They are looked up at build time.

## The model

All files are in `explorer/model/`. Text fields are Markdown and may contain references.

### `system.toml`: where code runs

```toml
[[zone]]
id = "box"
title = "Box"
trust = "untrusted"        # untrusted | host | aux | hub | device | external
summary = "A project's container. Everything in it is under the agent's control."
col = 1                    # position on the runtime map, as a grid
row = 1
colspan = 1
rowspan = 2

[[actor]]
id = "box-agent"
zone = "box"
title = "Box agent"
kind = "process"           # process | store | person | service
summary = "…"
commands = ["devc box init"]        # commands that run as this actor
entry = ["internal/boxside.RunAgent"]  # other entry points, if any

[[channel]]
id = "box-channel"
from = "daemon"
to = "box-agent"
title = "Box channel"
transport = "…"
carries = "…"
boundary = true                      # crosses a trust boundary
checks = ["internal/proto.ParseSpoolLine"]   # where what arrives is validated
concerns = ["channel"]
```

An actor with `commands` or `entry` is devc code. A command stands for itself and its subcommands, except those another actor names: `commands = ["devc"]` with `["devc daemon"]` on another actor gives every command to the first but `devc daemon` and its subcommands, and naming `devc daemon status` on the first takes that one back. The tool computes which functions can be reached from those entry points (for a command whose run function is a function literal, from what that literal mentions, and what a local closure of the same function that it calls mentions), which gives every package and function the set of actors it may run in. Name an `entry` only where no command leads there; an entry that builds the whole command tree (`cli.Main`) reaches everything.

### `packages.toml`: the static structure

```toml
layers = ["foundation", "model", "engine", "feature", "orchestration", "entry"]   # bottom to top

[layer.foundation]
title = "Foundations"
summary = "…"

[[group]]
id = "git"
title = "Git"

[package."internal/gitx"]
layer = "feature"
group = "git"
role = "One sentence: what this package is responsible for."
```

Every package needs an entry. A package that imports one from a higher layer is reported.

### `concerns/<id>.md`: what the system does, concern by concern

TOML front matter between `+++` lines, then Markdown.

```
+++
id = "network"
title = "Network policy"
summary = "What a box can reach, and how that is enforced."
order = 50
packages = ["internal/netpolicy"]
files = ["internal/boxside/proxyrelay.go"]     # for packages that belong only in part
key = ["internal/netpolicy.Install"]           # the symbols to know
spec = ["doc:specs-v2.md#13"]
tests = ["test/integration.TestNetwork"]
related = ["channel"]

[[invariant]]
text = "A box at level internet never starts before its rules are in place."
refs = ["internal/netpolicy.Bind"]             # where it is enforced
tests = ["internal/netpolicy.TestBind"]        # optional: the tests that show it

[[diagram]]                                    # optional; see Diagrams
…
+++
## What it is for
## How it works
## What can go wrong, and what prevents it
## Decisions that are not obvious
## Where to start reading
```

Every file that is not a test should belong to at least one concern, through `packages` or `files`. Files that belong to none are listed by `--check`.

### `flows/<id>.toml`: what happens, step by step

```toml
id = "box-up"
kind = "flow"                 # flow: a sequence across actors. tour: a reading order.
title = "devc up"
summary = "…"
concern = "box-lifecycle"
trigger = "devc up demo/hello"
actors = ["user", "cli", "podman", "box-agent", "daemon"]    # the lifelines, in order

[[step]]
at = "cli"                              # something one actor does
ref = "internal/cli.newUpCmd"
text = "…"

[[step]]
from = "cli"                            # something that passes between two actors
to = "podman"
via = "exec"                            # exec | rpc:<method> | http:<route> | event:<type> | cmd:<command> | reply | file | signal | stream | net
label = "podman create"
ref = "internal/engine.Podman.Create"   # the code that sends
text = "…"

[[step]]
section = "Only at level internet"      # a heading between steps

[[step]]
at = "podman"
external = true                         # not devc code: no ref
text = "…"
```

A step may carry `detail` (longer Markdown shown when the step is open) and `checks` (references to the code that validates what crossed a boundary).

The build checks that consecutive steps of one actor are connected: there must be a path in the call graph from the earlier step's code to the later one's. For `via = "rpc:…"` the sending code must be among the method's callers and the next step's code among its handlers; likewise for routes, events and commands. A step that is connected in a way the tool cannot see says so: `unchecked = "started as a goroutine by the listener"`. The app shows the path it found between two steps; `./dev explore query flow <id>` prints them.

In detail:

- A step's `ref` should be a function or method. A type, field or constant works as the later step too: it is reached when a function on the path uses it.
- The path may start in the code of any earlier step of the same actor, not only the latest: a function returns to its caller, and an earlier step may have started the goroutine the later one runs in. The shortest path wins. Among equally short paths, the step whose function has not returned wins, the innermost first: the tool keeps the actor's steps as a stack, and a step that continues in the code of step 3 ends the steps after step 3 (their functions returned to it). Then the most recent step; and of steps that give the very same path, the most recent, whose code the path starts in. The connection says which step it starts from. Paths are at most 8 calls long; test code is never on them.
- Of the shortest paths, one through calls wins over one through calls via interfaces or mentions. An edge that only mentions a function (passes it as a value, stores it in a table) is no call: a connection through one is shown as weaker, with ⤳ for that edge.
- The order in which a flow's steps are found depends on where the actor's first step is: the tool looks for paths from the code of earlier steps, so a flow that starts in the middle of a call cannot later name the function that called it. Start an actor's chain at the entry function of the process or the goroutine: the command's run function, the handler, the loop.
- A message ends the receiver's thread of work and starts a new one: the receiver's next step must be reachable from what receives the message. `rpc:<method>`: from the method's handlers; the sending step's code must also be a caller of the method, call one, or be called by one with the method's name (`sender` in the data). A method stored as a value of a map in a variable is sent by the function that looks it up there and passes it to a call. `http:<route>`: from the route's handler, and from the functions the surface names in `through` (a wrapper the router calls before every handler). `event:<type>`: from the code that reads that type; the sender must write it. `cmd:<command>`: from the command's run function (a process started with that devc command). `reply`: the answer to an earlier call; the receiver continues in its own code.
- A command whose run function is a function literal (`RunE: func(…) error {…}` inside `newFooCmd`) starts in what the literal calls, and in what a local closure it calls calls (`send := func(…) {…}` in the same function). The function that holds the literal is the same code as the literal: a step naming `newFooCmd` connects right after the `cmd:` message, as the actor's first step, and as a `reply` at the end that prints the result.
- `exec` to devc code, `file`, `signal`, `stream` and `net` (a connection to a listener: TCP, a Unix socket) cannot be followed: the receiver's next step needs `unchecked`, with the reason. An `external = true` step (a person, a program that is not devc) has no code and breaks the actor's chain; so does a step whose `ref` is a `file:` reference. Such a step may still carry `unchecked`: the app shows the reason, marked as not verified.
- Tours (`kind = "tour"`) are reading orders; their steps are not checked for connections. A tour's step may name only a `ref`, or an actor with `at`.

### `glossary.toml`

```toml
[[term]]
id = "relay"
title = "Relay"
aka = ["relay repository"]
text = "…"
refs = ["internal/gitx.RelayService"]
see = ["bank"]
```

### `layout.toml`: state on disk

```toml
[[tree]]
id = "state"
title = "State directory"
root = "~/.local/share/devc"
summary = "…"

[[tree.entry]]
path = "durable/realms/<realm>/relays/<project>.git"
text = "…"
writer = "host"               # host | box | both | user
ref = "internal/paths.Layout.RelayDir"

[[mount]]
source = "<project directory>"
target = "/work"
mode = "rw"
text = "…"
ref = "internal/box.Spec"
```

### `interfaces.toml`: the tables of entry points

The tool extracts the tables; this file names them, says where to find them and adds a sentence each.

```toml
[[surface]]
id = "daemon-api"
kind = "rpc"                              # rpc | http | commands | events | config | env
title = "Daemon API"
summary = "…"
consts = "pkg:internal/proto"             # rpc, events: the package whose string constants …
prefix = "Method"                         # … with this prefix are the methods (event types)
handlers_in = ["internal/daemon.Daemon.handlers"]   # rpc: the functions that register handlers
listed_in = ["internal/daemon.hubMethods"]          # rpc, optional: an allowlist of the surface's methods

[[surface]]
id = "hub-routes"
kind = "http"
table = "internal/hub.routeTable"         # the function returning a slice literal of structs
fields = { method = "method", path = "path", access = "access", xsite = "xsite", handler = "handle" }
through = ["internal/hub.Server.guard"]   # optional: what every request passes before its handler

[[surface]]
id = "config-project"
kind = "config"
root = "internal/config.Project"          # the struct type of the file
key = "project"                           # the first part of its keys: config:project.network.level

[[surface]]
id = "env"
kind = "env"
pattern = "DEVC_[A-Z0-9_]+"               # names found in string literals and in the other files
```

A method belongs to every rpc surface whose `handlers_in` registers a handler for it or whose `listed_in` lists it. Uses of a method's constant are sorted: a key of a map literal or of an index assignment, a `case` or a comparison registers a handler; an argument of a call or the value of a struct literal's field sends; a value of a map literal held in a variable lists it, and a function that looks it up in that variable and passes what it found to a call (`m, ok := commandMethods[t]; …; l.forward(ctx, m, …)`) sends it; anything else (a slice of allowed methods, the argument of a call that builds a handler being registered) only lists it. Event types are sorted the same way into readers, writers and other uses.

### `overview.md`

The text of the first page, with the order in which to read the rest.

### Diagrams

The runtime map, the package map and the sequence diagrams of flows are drawn from the model. A concern may add diagrams of its own:

```toml
[[diagram]]
id = "session-states"
title = "Session states"
kind = "graph"                # boxes and arrows on a grid

[[diagram.node]]
id = "working"
label = "working"
col = 2
row = 1
ref = "internal/proto.StateWorking"
zone = "host"                 # optional colour

[[diagram.edge]]
from = "working"
to = "blocked"
label = "the harness asks for permission"
ref = "internal/boxside.hookNotification"
```

Every node and edge may carry a reference; clicking it opens the code.

## What the tool extracts

- Packages, files, line counts, package and file comments, imports.
- Every declaration with its signature, doc comment and position; struct fields and interface methods.
- Every use of a declaration: the links in the source view, "used by" lists, the call graph, and which types implement which interfaces. Tests are included.
- The command tree, from the running CLI (built with an empty configuration directory, so the harness commands are those of the built-in manifests): names, help texts, flags, and the function each command runs.
- RPC methods (the `Method…` constants of `internal/proto`) with the code that handles and the code that calls each.
- The hub's routes with access level and handler.
- Session event types with the code that writes and reads them.
- Configuration keys, from the `toml` tags of the configuration types, with the code that reads each field.
- Environment variables named `DEVC_…` and where they are used.
- Which functions are reachable from each actor's entry points.
- The documents: `README.md`, `specs-v2.md`, `docs/**`, with headings, review findings and work orders indexed, so that "finding C1-05" or "work order C10a" in a comment becomes a link; for every finding and work order, the places in Go comments that cite it.

The data contract between tool and app is the set of Go types in `explorer/tool/schema.go`.

## Checks

`./dev explore --check` reports:

| Problem | Severity |
|---|---|
| A reference that does not resolve | error |
| A package without an entry in `packages.toml` | error |
| An unknown key in a model file, a duplicate id | error |
| A package that imports from a higher layer | warning |
| A file that belongs to no concern | warning |
| Two flow steps with no connection the tool can find | warning |
| A package or multi-file package's file without a comment | warning |
| A concern page of more than 1,200 words of prose (code blocks and tables are not counted) | warning |
| A type error in the code | warning |

A package comment counts for its own file. Errors fail the build. `--strict` turns warnings into errors. `--only <file or directory>` (repeatable) shows only the messages about those files; errors elsewhere still fail.

## What the app shows

| View | Content |
|---|---|
| Overview | The system in one picture, and a reading order. |
| Runtime | Zones, actors and channels: what runs where, whom it trusts, what crosses each boundary and where it is checked. |
| Packages | The layers, with imports on demand; by size; coloured by group, by concern or by where the code runs. |
| Concerns | One page each: explanation, diagrams, invariants with the code that enforces them, key symbols, flows, tests. |
| Flows | A sequence diagram beside the steps, with the code of the current step next to it. |
| Interfaces | Commands, RPC methods, routes, events, configuration keys, environment variables, each leading to its code. |
| State | What is on disk and what is mounted into a box. |
| Glossary | The project's vocabulary. |
| Code | Every file, with links on every identifier, an outline, "used by" and "calls", and marks where a function is a step of a flow or part of a concern. |
| Documents | The spec, the guides and the development records. |

Search covers all of it.

## Keeping it current

- After renaming or removing something, run `./dev explore --check` and fix what it reports.
- New package: add it to `packages.toml` and to a concern.
- New behaviour worth explaining: extend the concern, add or extend a flow.
- The app and the tool have no knowledge of devc beyond the model: a new concern, flow or diagram needs no code change.

## Writing for the explorer

What an author of the model needs to know beyond the format: how long things can be before they are cut, what reads well, and how to look at the result. The numbers are for a laptop screen (1280 pixels wide, often with the inspector open, which leaves the view about 760) and a large monitor (1920).

### Preview every page you write

```sh
./dev explore --check                                    # errors and warnings only, about 3 s
./dev explore                                            # build explorer/dist
explorer/check.sh --no-build --shot '#/concerns/network' --shot '#/flows/box-up?step=7' "$(mktemp -d)"
```

`--shot` takes any route of the app (copy it from the address bar after `index.html`), loads it in headless Chrome in the light and the dark theme at 1280 × 800 and 1920 × 1080, saves four PNG files per route in the directory, and fails if the page logs an error. Look at all four: the 1280 shots show what is cut or wrapped; the 1920 shots, whether a diagram uses the room. For a page with the inspector open, add `?i=<reference>` to the route (`#/runtime?i=actor:daemon`). `--full` captures the whole page instead of the window (a concern page from its title to its last invariant; a flow's whole diagram), up to 16000 pixels tall. A flow route with `?step=<n>` shows that step in view. A site built elsewhere (`./dev explore --out bin/x`) is checked with `--site bin/x`. The full `explorer/check.sh` (about two minutes) visits every view and runs the app's assertions; run it once before you hand in.

Routes: `#/overview`, `#/runtime`, `#/concerns/<id>` (`?diagram=<id>` scrolls to a diagram, `?h=<anchor>` to a heading), `#/flows/<id>?step=<n>`, `#/state`, `#/glossary?sel=<id>`, `#/docs/<path>`, `#/docs/findings?sel=<id>`, `#/code/<file>?sym=<reference>`, `#/pkg/<path>`.

### Text

- **Markdown** in every text field: headings, paragraphs, `**bold**`, `*italic*`, `` `code` ``, lists (nested by indentation), tables (a table wider than the text grows into the free width of the page, then scrolls sideways with a note above it saying so), block quotes, fenced code (` ```go `, `toml`, `sh`, `json` are highlighted), links. Raw HTML is shown as text. `[[reference]]` becomes a link with the target's name; `[[reference|words]]` uses your words. A backticked name that is exactly one symbol, package or file (`` `internal/box.Up` ``, `` `internal/box` ``) becomes a link too; a bare name (`` `Up` ``) stays code. Mentions such as "finding C1-05", "work order C10a", "plan 3.6", "spec section 13" become links by themselves.
- **Summaries** (zones, actors, concerns, flows) are one sentence. What is shown where: the first sentence in lists and search results; the first paragraph, cut at 420 characters, in hover cards; three lines (about 150 characters) on concern and flow cards; two lines (about 200 characters) in the flow player's header. Longer summaries are cut with an ellipsis, not lost: the full text is on the object's own page.
- **Titles**: concerns and flows up to about 40 characters (they are headings and card titles); actors, zones and channels up to about 20, two or three words (actor titles wrap to two lines on a card 96 to 190 pixels wide; channel titles are labels on the map).
- **Invariants** are one sentence each, a property that holds, up to about 200 characters; put the reason in the concern's text. Name the code that enforces each one (`refs`) and, where there is one, the test that shows it (`tests`).
- **Glossary terms**: the first paragraph is the definition the hover card shows (cut at 420 characters); put examples and history in later paragraphs.
- **Key symbols** are shown with the first sentence of their doc comment: that sentence should say what the symbol is for.

### Concerns

- The page is the concern's text in a column of about 74 characters, with a side column that stays in view: "On this page" (its `##` sections and the invariants, the one being read marked), its flows, its code, key symbols (two lines of each doc comment), related concerns, spec sections and tests. With the inspector open the side column follows the page. The invariants come after the text, numbered, each with its code.
- **Length**: 500 to 1,200 words of prose; `--check` warns beyond 1,200. Quoted code blocks and tables do not count.
- Use `##` for sections (they get a rule above them and appear under "On this page" when there are three or more); `###` inside them. Write the five sections of the template; leave out one that has nothing to say rather than filling it.
- A diagram appears after the paragraph (or list) that first names it with `[[diagram:<id>]]`; a diagram the text does not name comes after the first section. Name it where the reader needs it.

### Diagrams (`kind = "graph"`)

- Nodes are boxes 104 to 190 pixels wide; edges are routed at right angles through the gaps between the boxes, so leave a cell empty where many edges must pass. Labels sit on the edges, where they cover no box.
- **Columns**: four fit everywhere (1280 with the inspector open); five at 1280 without it; six at 1920. A wider diagram scrolls sideways. **Rows**: any number, but up to five keeps a diagram on one screen.
- **Node labels**: up to about 25 characters (two lines). Give a node a `ref` when it stands for code: the box then shows the symbol's name under the label and opens the code on click. `zone` colours the box like that zone; use it where the zone is the point (box or host), not for decoration.
- **Edge labels**: up to about 24 characters; longer ones wrap into a box of up to 150 pixels and crowd the picture. An edge's `ref` makes its label and line open the code.
- A state machine of a dozen states works: edges both ways between two nodes run side by side, and an edge from a node to itself is a loop with its label beside it (on the side of the box with the fewest other edges). Place states so that most edges join neighbours: an edge that skips a column must go around the boxes between.

### Flows

- The player shows the sequence diagram beside the current step and the step's code in the inspector. Lifelines are drawn while each has at least 84 pixels: up to eight actors at 1280 with the inspector open, eleven at 1920; with more, or on a narrow window, the diagram becomes a list of steps. Seven actors or fewer read best.
- **Order of `actors`**: the initiator on the left, the box (or whatever is least trusted) on the right; keep the actors of one zone together, because consecutive actors of one zone share a band in that zone's colour, and a message that crosses a trust boundary is marked where it enters the receiver's band.
- **Length**: 10 to 30 steps read well; 40 still work. Put a `section` every five to ten steps; it is a heading across the lifelines and in the step panel.
- **`label`** (messages): what travels, short: `session.send {id, text}`, `podman create`. Up to about 30 characters fits between neighbouring lifelines; a longer label reaches over the lifelines beside it (readable, but busier) and is cut only at the diagram's edge, about 90 characters at 1280. The full label is always in the step panel.
- **Steps of one actor** show the name of their `ref` (`Daemon.apiSend`) on the lifeline; name the function that does the step, not its caller. Give an `external` step a short `label`; else the diagram shows its first words.
- **`text`**: one or two sentences, shown in full in the step panel (the first sentence in lists and tooltips). Longer explanations go in `detail`, shown under the text.
- **Connections**: the step panel shows the path the build found from the previous step of the same actor, or your `unchecked` reason under "not verified by the tool" (on a step in JavaScript, a `file:` reference, too). A path through a function passed as a value is marked "verified, weaker". A step whose connection was not found carries a mark in the diagram; fix it, or say why with `unchecked`. `./dev explore query flow <id>` prints every connection with its path; `./dev explore --check --only explorer/model/flows/<id>.toml` only your flow's warnings.
- **Messages without a table**: `via = "net"` for a connection to a listener (TCP, a Unix socket), `stream` for bytes on a connection that exists, `file` for a file one side writes and the other reads, `signal`, `exec`. The tool follows none of them: the receiver's next step needs `unchecked`.
- **Commands**: after `via = "cmd:devc foo"`, the receiver's step may name the function that builds the command (`newFooCmd`, when its run function is a literal) or anything the literal calls; a final `reply` that prints the result may name `newFooCmd` too.
- **Tours** have no diagram: a list of steps beside the step panel. Give each step a `ref` and a sentence of why to read it.

### The runtime map, the overview and the state view

- **Runtime**: zones are placed on the grid of `col`, `row`, `colspan` and `rowspan`; inside a zone the explorer places actors next to what they talk to. A zone of more than about twelve actors becomes tall; split it if it means two things. A channel's `title` is its label on the map (shown on hover), `carries` and `checks` are the rows of the Boundaries table under the map.
- **Overview**: the reading order is the concerns and flows in the order `overview.md` first names them (`[[concern:…]]`, `[[flow:…]]`), then the remaining concerns in their `order`, each followed by its flows and tours. Name the first five or so in the overview's text in the order a newcomer should read them.
- **State**: an entry's `path` is shown below its parents (missing parents are added without text); `<name>` parts are styled as placeholders. `writer` is shown in zone colours: `box` in the untrusted colour, `host` in the host's, `user` in the zone of the model's person actor, `both` in two. A mount's `mode` starting with `rw` is drawn as a heavy arrow (the box can write there), `ro` as a dashed one.

### Before you hand in

`./dev explore --strict` is clean for your files (no unresolved references, no flow step without a connection or an `unchecked` reason), every page you wrote has been looked at in both themes at both widths, and `explorer/check.sh` passes.
