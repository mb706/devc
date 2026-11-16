# The devc explorer

A local web app for understanding this codebase: diagrams of what runs where and what depends on what, explanations of each concern, step-by-step flows through the code, and a source browser that ties them together.

```sh
./dev explore            # build explorer/dist/ (about 3 s once the tool is compiled)
xdg-open explorer/dist/index.html
./dev explore --check    # validate the model against the code, build nothing
./dev explore --strict   # warnings fail too
./dev explore --model <dir> --out <dir>   # another model, another output directory (both below the repository)
./dev explore query help # questions about the code for people writing the model
./dev explore comments-only [commit]      # did the Go code change only in its comments since HEAD?
explorer/check.sh [--no-build] [dir]      # build, then check the app in headless Chrome (screenshots in dir)
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
| `doc:specs-v2.md#13`, `doc:specs-v2.md` | A heading of a document, by its number or its anchor; or the whole document. |
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

An actor with `commands` or `entry` is devc code. A command stands for itself and its subcommands, except those another actor names: `commands = ["devc"]` with `["devc daemon"]` on another actor gives every command to the first but `devc daemon` and its subcommands, and naming `devc daemon status` on the first takes that one back. The tool computes which functions can be reached from those entry points (for a command whose run function is a function literal, from what that literal mentions), which gives every package and function the set of actors it may run in. Name an `entry` only where no command leads there; an entry that builds the whole command tree (`cli.Main`) reaches everything.

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
via = "exec"                            # exec | rpc:<method> | http:<route> | event:<type> | cmd:<command> | reply | file | signal | stream
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

The build checks that consecutive steps of one actor are connected: there must be a path in the call graph from the earlier step's code to the later one's. For `via = "rpc:…"` the sending code must be among the method's callers and the next step's code among its handlers; likewise for routes, events and commands. A step that is connected in a way the tool cannot see says so: `unchecked = "started as a goroutine by the listener"`. The app shows the path it found between two steps.

In detail:

- A step's `ref` should be a function or method. A type, field or constant works as the later step too: it is reached when a function on the path uses it.
- The path may start in the code of any earlier step of the same actor, not only the latest: a function returns to its caller, and an earlier step may have started the goroutine the later one runs in. The shortest path wins (among equals, from the latest step), and the connection says which step it starts from. Paths are at most 8 calls long; test code is never on them.
- A message ends the receiver's thread of work and starts a new one: the receiver's next step must be reachable from what receives the message. `rpc:<method>`: from the method's handlers; the sending step's code must also be a caller of the method, call one, or be called by one with the method's name (`sender` in the data). `http:<route>`: from the route's handler. `event:<type>`: from the code that reads that type; the sender must write it. `cmd:<command>`: from the command's run function (a process started with that devc command). `reply`: the answer to an earlier call; the receiver continues in its own code.
- `exec` to devc code, `file`, `signal` and `stream` cannot be followed: the receiver's next step needs `unchecked`, with the reason. An `external = true` step (a person, a program that is not devc) has no code and breaks the actor's chain.
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

A method belongs to every rpc surface whose `handlers_in` registers a handler for it or whose `listed_in` lists it. Uses of a method's constant are sorted: a key of a map literal or of an index assignment, a `case` or a comparison registers a handler; an argument of a call or the value of a struct literal's field sends; anything else (a slice of allowed methods, the argument of a call that builds a handler being registered) only lists it. Event types are sorted the same way into readers, writers and other uses.

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
- The documents: `README.md`, `specs-v2.md`, `docs/**`, with headings, review findings and work orders indexed, so that "finding C1-05" or "work order C10a" in a comment becomes a link.

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
| A type error in the code | warning |

A package comment counts for its own file. Errors fail the build. `--strict` turns warnings into errors.

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
