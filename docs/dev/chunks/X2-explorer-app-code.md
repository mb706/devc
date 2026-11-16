# X2 — Explorer: the app, part 1 (shell and the views of extracted facts)

Read `explorer/README.md` (design and format reference, binding), the "X1" section of `docs/dev/STATUS.md`, and `explorer/tool/schema.go`, which is the data contract. The placeholder in `explorer/app/` shows how data is loaded from disk; replace it.

The reader of this app is the owner of the codebase, an experienced developer who did not write the code and wants to understand all of it. Everything here serves one thing: getting from a question ("what is this, who calls it, where does it run, why is it like this") to the answer without losing one's place.

This chunk builds the shell, the shared parts, and every view that needs only extracted facts. The next chunk (X3) builds the views of the model (runtime map, concerns, flows, state, glossary, overview, documents) on top of what you build, so the shared parts must be solid and easy to reuse.

## Constraints

- Plain HTML, CSS and JavaScript. No framework, no library, no build step, nothing fetched from a network, no web fonts. It runs from `file://`: classic scripts only (no ES modules, no `fetch`, no workers).
- Readable code: one file per view or shared part, a short comment at the top of each saying what it is, small functions. A later reader will extend it.
- The app knows nothing about devc beyond what is in the data. No package name, zone name or command name is written into the app.
- Works in current Chrome and Firefox, at widths from a laptop (1280) to a large monitor. A phone layout is not needed.
- Light and dark themes, following the system, with a switch. Both must look finished.

## Design direction

A technical atlas: calm, precise, dense where it lists and spacious where it explains.

- Neutral surfaces, hairline borders, small radii, almost no shadows. One accent colour for interaction. The only other strong colours are the zone colours.
- **Zone colours are the visual language of the whole app.** Each zone's `trust` value has one colour (untrusted, host, aux, hub, device, external), defined once as CSS variables for both themes with checked contrast. Wherever something belongs to a zone or runs in an actor of a zone, it carries that colour: a strip, a dot, a tint. A reader should learn within minutes that the warm colour means "inside the box, under the agent's control".
- Identifiers are always monospace. Text is the system font, about 15px for explanations (lines no longer than about 75 characters), 13 to 14px for tables and code.
- Diagrams are built from HTML boxes (so text wraps and everything is selectable and clickable) with connectors drawn in an SVG layer above or below them.

If a skill named `dataviz` or `artifact-diagramming` is available to you, load it before you choose colours and build the diagram parts, and use what applies.

## The shell

- **Navigation** across the top or down the left: Overview, Runtime, Packages, Concerns, Flows, Interfaces, State, Glossary, Code, Documents. Views that X3 builds show a short "not built yet" page for now. The build's commit, date and dirty flag are visible but quiet.
- **Routing** with the URL fragment, so that back, forward, reload and bookmarks work: for example `#/pkg/internal/box`, `#/code/internal/box/spec.go?sym=internal/box.Spec`, `#/interfaces/commands?q=push`. Symbol references are used in URLs as they are.
- **The inspector**: a panel on the right that shows one thing (a symbol with its code, a file, a package summary, and in X3 an actor, a term, a finding) without leaving the current view. It has its own back stack, can be resized and closed, and its target is part of the URL. "Open in full" turns the inspected thing into the main view. This is what lets a reader keep a diagram or an explanation in view while looking at code.
- **References.** One function renders any reference of the README's syntax as a chip: a short label (for a symbol, the last element of the package path and the name), an icon for its kind, the zone colour where it has one. Click: inspect. Click with Ctrl or Cmd, or middle click: open in full. Unknown references cannot occur (the tool checks them), but render defensively.
- **Hover cards.** Hovering over any reference, anywhere, shows after a short delay: what it is, its signature if it is a symbol, the first paragraph of its doc comment or summary, where it is defined, and where it runs.
- **Search**: `/` or Ctrl+K opens it. It searches symbols, files, packages, commands, RPC methods, routes, configuration keys, environment variables, and (once X3 has built them) concerns, flows, terms and document headings, grouped by kind. Rank exact matches first, then prefixes, then word starts, then substrings, then subsequences; prefer exported symbols and shorter names. A second mode searches the text of all code: it loads the per-package data as needed, shows progress, accepts a regular expression, and lists matching lines by file.
- **Loading.** Per-package data arrives on demand. Show a loading state; never block the page.
- **Settings**, kept in `localStorage` (guard every access; the page must work when storage is unavailable): theme, whether tests are shown, the inspector's width, and an "open in editor" link template with presets for VS Code and Cursor (`vscode://file/{path}:{line}`), default none. The repository's absolute path comes from the build data. "Copy path:line" is always available.

## Shared parts that X3 will reuse

Build these as separate, documented files:

1. The reference chip and the hover card.
2. The inspector.
3. The source view (below), usable both full-width and inside the inspector.
4. A **connector layer**: given pairs of elements, draw connectors between them in SVG (straight, or gently curved, with an optional arrowhead and label), redrawn on resize and scroll, with classes for highlighting. The package map uses it now; the runtime map, concern diagrams and sequence diagrams will use it in X3.
5. A renderer for Go doc comments: paragraphs, indented code blocks, lists, headings, and the patterns from the build data (finding IDs, work orders, plan, rule and spec references) turned into references.
6. A small table component with a filter box and sortable columns.

## Views

### Code

- **Tree** on the left: directories, packages, files. Each file shows its line count and the first sentence of its file comment; each package the first sentence of its role. A filter box. Test files can be hidden. Files that are not Go are in the tree too.
- **Source** in the middle: line numbers (clicking one puts it in the URL), highlighting from the build's token classes, and a link on every identifier the build resolved. Click an identifier: go to its definition (in this view; the inspector is for looking from elsewhere). Hovering shows the card. Comments get the document patterns as links. Long files must stay fast: render lazily if needed, but in-page search (Ctrl+F) of the browser should still find text; if you virtualise, provide your own find-in-file.
- A header that stays in place: path as breadcrumbs, the package, the file comment, copy and editor links.
- **Outline** on the right: the file's declarations, types with their methods, with a filter. The declaration around the current scroll position is marked.
- **Symbol panel**, when a symbol is selected: signature; doc comment; where it runs (actors, by zone colour, with the note that this is an over-approximation); *Used by*, grouped by package and function, each with its line of code, tests separately; *Calls*; for an interface, the types that implement it, and for a type, the interfaces it implements; for a constant, its value. Every entry is a reference.
- Files that are not Go are shown with line numbers and simple highlighting of comments and strings.

### Packages

- **Layers**: one row per layer from the model, top layer first, with its title and summary. Each package is a tile: name, role (cut to two lines), a bar for its size, a strip in its group's colour, dots in zone colours for where it runs.
  - Hovering or selecting a tile draws its imports (downwards) and its importers (upwards) with the connector layer and dims the rest. All edges at once is an option that is off by default.
  - Colour by group, by where the code runs, or by test share. A legend.
  - Click: package summary in the inspector. Open in full: the package page.
- **Size**: a treemap of packages and their files by lines of code, in the same colours; clicking a file opens it.
- **Package page**: role, layer, group, where it runs; the full package comment; a table of files (description, lines, tests); its neighbours (what it imports and what imports it, as tiles); its declarations grouped by file, types with methods, each with the first sentence of its doc comment (unexported ones can be hidden); its tests; and what it contributes to the tables of entry points (commands, handlers, routes, configuration types).

### Interfaces

One tab per surface from the model, each a filterable table whose rows lead to code:

- **Commands**: the tree, with short text, flags, the function it runs, and hidden commands marked (many are the entry points of processes inside the box and of helpers; show which actor a command starts where the model says so).
- **RPC methods**, by surface: name, doc comment of the constant, handlers, callers.
- **Routes**: method, path, access level, cross-site rule, handler.
- **Events**: type, doc comment, where written, where read.
- **Configuration**: per root type a tree of keys with Go type, doc comment and the number of places that use the field; selecting a key lists them.
- **Environment variables**: name and uses.

## Checking your work

- Write `explorer/check.sh` on the model of `web/check.sh`: build the site, open it headless from its `file://` URL, visit every route of this chunk in both themes, fail on any console error or uncaught exception, run a few assertions in the page (search finds a known symbol; following a link in the source lands on the definition; the inspector's back works; a package page lists its files), and save screenshots to a temporary directory.
- Look at the screenshots. Iterate until each view is something you would be content to use for a week: alignment, spacing, contrast in both themes, no overflow, no clipped text, diagrams that read at a glance. Check a very large file, a package with many files, and the largest table.
- Performance: the first paint within about a second from disk on this machine; opening the largest source file within about a second.

## Boundaries

- You may adjust how the tool copies or lists the app's files, and add fields to `schema.go` if a view needs a fact the tool has and does not emit (say which, in your report). Do not change the model's format.
- Do not write content into `explorer/model/`.
- No integration suite. Run the tool's unit tests if you touch it, and `explorer/check.sh`.

## End of the chunk

Add an "X2" section to `docs/dev/STATUS.md`: the files of the app and what each is, the shared parts and how X3 should use them, how to run the check. Then report as the README of `docs/dev` says.
