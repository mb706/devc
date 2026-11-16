# X1 — Explorer: the tool

The explorer is a local web app for understanding this codebase. Read `explorer/README.md` first: it is the design and the format reference for everything under `explorer/`, and it is binding. If you find that something in it cannot work as written, change the README together with the code and say so in your report.

This chunk builds the tool that extracts facts from the code, loads and checks the model, and writes the site's data. The app that renders the data is the next two chunks; here it is only a placeholder page.

## What to build

### 1. `explorer/tool` (Go, `package main`)

Standard library, plus what the module already depends on (`BurntSushi/toml` for the model files; `internal/cli` and through it cobra for the command tree). No new dependency, not even a development one. The tool must not change what goes into the `devc` binary.

**Loading and type-checking.** Use `go list -json -deps -export` (with `-tags integration`, and test variants) to find the module's packages, their files and the export data of everything they import, then parse with `go/parser` (keep comments) and type-check with `go/types`, importing dependencies from export data (`go/importer.ForCompiler(fset, "gc", lookup)`). Test files are included: in-package tests with their package, external test packages on their own. Tolerate type errors (count and report them) rather than failing; aim for none.

**Symbols.** Every package-level function, type, variable and constant; every method; struct fields and interface methods. For each: its reference string as defined in the README (this is its identity everywhere, also in URLs), kind, signature, doc comment, file and line range, whether it is exported, and for constants the value. External test packages use `<dir>_test` as the package part.

**Uses.** For every identifier that refers to a symbol of this module: the link in the source view (file, offset, length, target) and the "used by" record (target, file, line, enclosing symbol). Code in a function literal belongs to the enclosing declared function.

**Call graph.** An edge from the enclosing symbol to every function or method it calls or mentions (a function value counts). A call through an interface declared in this module gets edges to that method of every type in the module that implements the interface, marked as dynamic. Record which types implement which interfaces.

**Files.** For every Go file: line counts (code, comment, blank), its file comment (a comment above the `package` line that is not the package comment or a build constraint), whether it is a test. Also list the repository's other files that a reader may want to see (`web/` without `vendor/`, `assets/`, `examples/`, `test/` data and scripts, `dev`, the Containerfiles and templates), with their text. Leave out `legacy/`, `bin/`, `explorer/dist/`.

**Source annotations.** For each Go file, the token classes for highlighting (from `go/scanner`: keyword, string, comment, number) and the links. The app must not need a Go parser.

**The tables of entry points** (README, "What the tool extracts"):
- *Commands*: build the real command tree and walk it. `internal/cli` needs a small exported function that returns the root command for documentation purposes without running anything or touching the system; add it, with a doc comment that says what it is for. For each command: path, short and long text, flags (name, shorthand, default, usage), whether hidden, and the declared function its run function belongs to (map the function's program counter to file and line, then to the symbol).
- *RPC methods*: every `Method…` string constant of `internal/proto`. Classify each use: registered as a handler (a key in a map literal or a `case`; resolve the handler to a symbol where it is a function or method, else the enclosing function), called (an argument of a call), or merely listed. A surface in `interfaces.toml` names the functions in which its handlers are registered.
- *Routes*: the hub's route table (`internal/hub.routeTable`): method, path (evaluate the constant expression), access level, cross-site rule, handler.
- *Events*: the event type constants of `internal/proto`, with their uses grouped by package and function.
- *Configuration keys*: starting from the root types a `config` surface names, follow struct fields with `toml` tags through nested structs, slices and maps. For each key: path, Go type, the field's doc comment, the field's symbol (its "used by" list says where the key takes effect).
- *Environment variables*: string literals of the form `DEVC_[A-Z0-9_]+`, with where they occur.

**Where code runs.** For each actor in `system.toml` with `commands` or `entry`: the set of functions reachable in the call graph from those entry points (the command's run function; dynamic edges included). Store for each symbol the actors it is reachable from, and for each package the share per actor. This is an over-approximation; say so in the data so the app can say so.

**Documents.** `README.md`, `specs-v2.md`, `docs/**/*.md`, `explorer/README.md`, `examples/**/*.md`: text and headings (with numbers where headings are numbered). Index the review findings (`docs/dev/reviews/*-review.md`: ID, title, text; the matching paragraph of the response file) and the work orders (`docs/dev/chunks/`). Look at the files for the ID formats; earlier reviews use `A1`, later ones `C1-05`. Emit the patterns the app should turn into links inside comments and documents: a finding ID, "work order C10a", "plan 3.6", "README rule 2", "spec section 13".

**The model.** Load every file under `explorer/model/` as the README defines, strictly (unknown keys and duplicate ids are errors). Resolve every reference. An unresolved one is an error whose message names the file, the place in it, and the nearest names that do exist. Run the checks in the README's table. For flows, find the connection between consecutive steps as the README describes (a bounded breadth-first search in the call graph; for `via` kinds, the extracted tables) and keep the path found, so the app can show it.

**Output.** `explorer/dist/index.html`, the files of `explorer/app/`, and `data/`. The site must work when `index.html` is opened from disk: no `fetch`, no ES modules, no workers. Data is loaded with `<script>` tags that call a registration function. Split it so that the first load is small: a core file (packages, files, symbols with docs, the tables, the model), then one file per package (source, annotations, "used by", calls) loaded on demand, and the documents. Keep the core below about 6 MB. Record the commit, whether the tree was dirty, and the build time.

The data contract is `explorer/tool/schema.go`: plain Go types with JSON tags and a doc comment on every type and field. The next chunks are written against that file alone.

**Command line.** `go run ./explorer/tool [--check] [--strict] [--out dir]`. Messages are `file: place: what is wrong`, errors before warnings, with a summary line. Exit 1 on errors.

### 2. `./dev explore`

A subcommand of the `dev` script that runs the tool in the toolchain container and prints where the result is. Extra arguments are passed through. Add `/explorer/dist/` to `.gitignore`. Document it in `dev`'s usage text and in `docs/dev/README.md`'s list of commands if there is one.

### 3. A first model

The model is filled in by later chunks, area by area. This chunk writes enough to exercise every part of the tool, and it must be true: read the code for each thing you write.

- `packages.toml`: complete. Choose layers and groups so that no package imports upwards and the names mean something. Take each role from the package's comment.
- `system.toml`: zones, the actors that are devc code (with the commands that start them), the other processes and stores a reader needs to place (the harness, podman, tmux, git, sshd, the browser, ntfy, GitHub), and the channels between them. `docs/dev/plan.md` sections 2 and 3 describe the architecture.
- `interfaces.toml`: complete (every table the tool can extract has a surface).
- `layout.toml`: the top two levels of the configuration, state and runtime directories, from `internal/paths`, and the mounts of a box.
- `glossary.toml`: about ten terms.
- One concern (`concerns/sessions-and-events.md`: how a session's events get from a box to the daemon's view) and two flows: `flows/send-prompt.toml` (`devc send` from the command to the harness and the receipt back, across the daemon's API and the box channel) and one tour of your choice. They must pass the checks without `unchecked` unless there is a real reason.
- `overview.md`: a placeholder paragraph.

### 4. A placeholder app

`explorer/app/index.html` with one script: enough to prove that the data loads from disk. It lists the packages with their roles and, for a chosen file, shows the source with highlighting and working links. No design work; the next chunk replaces it.

### 5. Tests

Unit tests beside the tool for: parsing and resolving references (including the error for a near miss), front matter, the path search between steps, the classification of RPC uses, and the document patterns. Use a small synthetic package under `testdata` where the real repository is not needed. `./dev explore --check` must pass on the repository with no errors; list the warnings that remain in your report.

## Boundaries

- No changes to devc's behaviour. The only change outside `explorer/`, `dev`, `.gitignore` and the documents is the exported function in `internal/cli`.
- Do not write doc comments into the code beyond that; later chunks do it area by area.
- Do not run the integration suite: nothing it covers changes. Run the unit tests of `internal/cli` and of the tool, `./dev vet`, and gofmt.

## End of the chunk

Add an "X1" section to `docs/dev/STATUS.md`: what exists, how to run it, how long a build takes, the sizes of the output, deviations. Then report as the README of `docs/dev` says.
