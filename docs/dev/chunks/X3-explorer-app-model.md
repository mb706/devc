# X3 — Explorer: the app, part 2 (the views of the model)

Read `explorer/README.md` (design and format reference, binding), the "X1" and "X2" sections of `docs/dev/STATUS.md`, `explorer/tool/schema.go`, and the app's shared parts that X2 built (reference chips, hover cards, the inspector, the source view, the connector layer, the doc renderer, the table). Read `docs/dev/chunks/X2-explorer-app-code.md` for the constraints and the design direction; they apply here unchanged.

X2 built the views of extracted facts. This chunk builds the views that explain: where code runs, what each concern is about, what happens step by step. These are the pages the owner will read first and return to most, so they deserve the most care. The model currently holds a first version (one concern, two flows, the system map, the layout, a glossary); later chunks fill it in area by area, and each author will check their pages in what you build. Nothing in the app may depend on particular content.

## Shared parts to add

1. **A Markdown renderer** for the model's text and for the documents: headings with anchors, paragraphs, emphasis, inline code, fenced code blocks, lists (nested), tables, block quotes, links, and the README's `[[reference]]` and `[[reference|text]]` forms rendered as reference chips. Also the document patterns from the build data (finding IDs, work orders and so on), and a file path or a backticked name that resolves to exactly one file or symbol becomes a reference. Escape everything else; the documents are trusted, but the renderer must not execute anything.
2. **A diagram renderer for `kind = "graph"`**: nodes placed on the grid the model gives (HTML boxes, zone colours, a reference on click), edges with labels through the connector layer. Edges between the same two nodes must not overlap, labels must not sit on boxes, and an edge that returns to its own node must be drawn. It must look right for a state machine of a dozen states.
3. **Backlinks**: for every symbol, file and package, where the model mentions it (concerns: key symbols, invariants, text; flow steps; glossary; layout; diagrams). Compute it once from the data.

## Views

### Overview

The model's `overview.md`; a small runtime map that leads to the Runtime view; a legend for the zone colours and the shapes; the reading order, as a list of concerns, flows and tours with a checkbox each (kept in `localStorage`); the numbers of the build (packages, lines, tests, commands). This is the first page a reader sees: it should make the system graspable in a minute and make the next click obvious.

### Runtime

The map of zones, actors and channels.

- Zones are regions on the grid the model gives, tinted in their trust colour, titled, with their summary available. Actors are cards inside their zone, with an icon for their kind and a mark where they are devc code.
- Channels are connectors between actors. One that crosses a trust boundary is drawn differently from one that does not (heavier, with a mark where it crosses), because these are where the system defends itself.
- With two dozen channels the map must stay legible: labels appear on hover or selection, parallel connectors are spread, and hovering an actor highlights its channels and dims the rest.
- Selecting an actor shows, in the inspector: its summary; the commands and entry points that start it; the code that can run in it, as packages with their share (and the note that this is an over-approximation); its channels in and out; the concerns and flows it takes part in.
- Selecting a channel shows: transport, what it carries, and where what arrives is checked (the `checks` references), plus the flows that use it.
- A **Boundaries** table below or beside the map: every boundary channel with what crosses and where it is checked.
- Filters: show only what one concern or one flow involves.

### Concerns

- **Index**: the concerns in the model's order as cards: title, summary, the packages as small tiles, how many flows and invariants.
- **Page**: the text takes the main column. A column beside it, which stays in view, holds: the packages and files that belong to the concern (a small version of the layer picture with them lit), the key symbols with the first sentence of each doc comment, the flows, the related concerns, the spec sections and the tests. Invariants are listed under their own heading: the statement, then the code that enforces it as references. A diagram appears where the text names it (`[[diagram:id]]`); diagrams the text does not place come after the first section.

### Flows

- **Index**: flows grouped by concern, tours separately, each with its trigger and its actors as zone-coloured chips.
- **Player** for a flow, three parts side by side on a wide screen:
  1. The **sequence diagram**: one lifeline per actor, headed in its zone's colour; one row per step; a step of one actor is a box on its lifeline; a step between two actors is an arrow with its label and a small mark for its `via` kind; a `section` is a heading across all lifelines; a step that crosses a trust boundary is marked. The current step is highlighted, and the diagram scrolls to keep it in view.
  2. The **step**: its text and detail; its `checks`; and the **connection from the previous step**: the path the tool found (a chain of references through the call graph, or the RPC method, route, event or command that links the two), or the `unchecked` reason, clearly marked as not verified by the tool.
  3. The **code** of the step's reference, in the inspector.
  Next and previous with buttons and with the arrow keys or `j` and `k`. The step is part of the URL.
- A **tour** uses the same player without the diagram.
- On a narrower window the diagram becomes a compact list of steps.

### State

The trees of `layout.toml` as outlines that fold: path, who writes it (in zone colours), what it is, and the reference to the code that knows the path. The mounts of a box as a table: source, target, mode, text. If it helps, draw the two sides (host paths, box paths) with connectors between them.

### Glossary

The terms in alphabetical order with a filter: text, other names, references, related terms. `[[term:…]]` anywhere shows the definition on hover.

### Documents

- A list grouped as: product (README, spec, guides), development (plan, status, leftover, development rules), work orders, reviews, the explorer's own README.
- A document is rendered with its headings as an outline beside it.
- **Findings**: one table of all review findings from the build data: ID, title, review, and the response. Each links to the places in the code whose comments cite it. This is how a reader finds out why a piece of hardening exists.

### In the Code view (extending X2)

- In the symbol panel and on the package page: **In the model**, from the backlinks.
- In the source: a small mark in the margin of a declaration that is a flow step, a key symbol or the enforcement point of an invariant, leading back to it. A reader browsing code should see where they are in the larger picture.
- Search covers concerns, flows, terms and document headings.

## Checking your work

Extend `explorer/check.sh` with the routes of this chunk, in both themes, and with assertions: stepping through a flow changes the step, the URL and the inspected code; a reference in a concern's text opens the inspector; the runtime map draws a connector for every channel; a `[[term:…]]` shows its definition; the findings table links to code.

Look at the screenshots and iterate as X2 did. For this chunk in particular:

- The runtime map and the sequence diagram must read at a glance. If they do not, change the drawing, not the content.
- Test with more content than the model has today: make a temporary copy of the model in a scratch directory with a concern three times as long, a flow of forty steps across seven actors, a diagram with fifteen nodes, and a glossary of a hundred terms, build from it (`--model` if the tool has such an option; add one if not), and check that the views hold. Do not commit the temporary content.

## Boundaries

- As in X2. You may add what the views need to `schema.go` and the tool (say what), and a `--model <dir>` option. Do not change the model's format without updating `explorer/README.md`, and do not rewrite the model's content beyond fixing an outright error you come across.
- Write a short section "Writing for the explorer" at the end of `explorer/README.md`: what an author needs to know that the format reference does not say (how long a summary can be before it is cut, how many columns a diagram can use, how a flow reads best, how to preview). The authors of the next chunks depend on it.

## End of the chunk

Add an "X3" section to `docs/dev/STATUS.md`. Then report as the README of `docs/dev` says.
