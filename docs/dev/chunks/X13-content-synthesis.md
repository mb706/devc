# X13 — Explorer content: synthesis

Read `docs/dev/chunks/X-content-guide.md` first; it applies here too. Then `explorer/README.md`, and the "X4" to "X12" sections of `docs/dev/STATUS.md`.

Nine chunks have each written one area: its concerns, flows, glossary terms and comments. Nobody has yet looked at the whole. This chunk makes the explorer one thing: the picture of the system, the first page, the paths through it, and the pages that belong to no single area. It is what the owner reads first.

Your material is the model as it stands. Read every concern page and every flow before you write. Where you state something about the code yourself, the guide's standard applies: verify it in the code.

## What to deliver

### 1. The system map (`system.toml`)

- Check every zone, actor and channel against the code and against the flows: each actor's summary is true, each has the commands or entry points that start it, every channel a flow uses exists, and no channel is left that nothing uses or explains. `checks` on every boundary channel name the code that validates what arrives.
- Then make the picture work. Build the site, look at the Runtime view, and arrange the grid (zones, order of actors) until the map reads at a glance: untrusted on one side, the host in the middle, the hub and the devices on the other, as few crossing connectors as the system allows. Iterate with screenshots.

### 2. The first page (`overview.md`)

- What devc is and the idea behind it, in about 150 words, for someone who knows the product.
- How to read the explorer: the zone colours, what a reference chip does, what the tool checked and what it could not.
- The reading order: the tours first, then the concerns in an order that builds (each needs only what came before). Set the concerns' `order` values to match.

### 3. Tours

- `start-here`: one task from beginning to end, as a tour through the area flows' most important waypoints: a project is configured, the box comes up, a harness is launched, a prompt is sent, the agent works and commits, an event reaches the phone, a question is answered from there, the commits are banked and pushed, the box goes down. Twenty to thirty stops. Each stop says what happens and points to the flow that tells it in full.
- `trust-boundaries`: every place where something crosses from a less trusted side to a more trusted one, ordered from the most exposed, with the code that checks it: what a box sends the host (spool, channel, git, proxy, gate, data), what a machine sends the hub, what a browser sends the hub, what the hub sends a machine.
- Any other tour the whole needs and no area wrote.

### 4. Concerns that belong to no area

- `security-model` (first in the order): the threat model in the reader's terms; the six security rules of `docs/dev/README.md`, each as an invariant with the code that enforces it and where that is concentrated; for each zone what it is trusted with; the accepted limits, grouped, with links to `docs/dev/LEFTOVER.md` part 5. This page is the index to every other page's "what can go wrong" section.
- `testing-and-tools` (last): how the tests are organised (unit tests with fakes and which fakes exist, the fake harness, the integration suite and its helpers), how to run them, which integration test shows which concern (a table), what is knowingly untested (link `LEFTOVER.md`), and the development tools: `dev`, the explorer's own tool.

### 5. One whole

- **Vocabulary.** One word per thing across all pages; the glossary has no duplicates and no term that contradicts a page. Where two pages disagree about a fact, find out from the code which is right and fix the other.
- **Cross-links.** `related` on every concern; references to concerns and flows of other areas where a page mentions them in words.
- **Coverage.** `./dev explore --check` reports no file without a concern. Add the file comments that are still missing where a file's name does not say enough.
- **The package roles** read as one list: same style, no two packages claiming the same thing.

## Before you finish

As in the guide. In addition: read the Overview, the Runtime view, `start-here` and `security-model` in the built site as the owner would on their first visit, and fix whatever makes them stop.

## Report

As in the guide, plus: every contradiction you found between pages and which side the code was on, and every statement you removed because the code did not support it.
