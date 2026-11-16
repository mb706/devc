# X6 — Explorer content: the daemon, sessions, input, requests

Read `docs/dev/chunks/X-content-guide.md` first. It is the work order for every content chunk and it is binding; this file only names your area. Then read `explorer/README.md` (format reference, including "Writing for the explorer").

## Area

- Packages, whole: `internal/session`, `internal/proto`, `internal/requests`, `internal/wsconn`.
- From `internal/daemon`: `daemon.go`, `api.go`, `socket.go`, `client.go`, `channel.go`, `muxguard.go`, `commands.go`, `receipts.go`, `adapter.go`, `turns.go`, `requests.go`, `terminal.go`, `web.go`.
- From `internal/cli`: `daemoncmds.go`, `sessioncmds.go`, `inputcmds.go`, `inbox.go`, `inboxgit.go`, `requestcmds.go`.

## Records to read for orientation

`STATUS.md` sections C04, C08, C09, C10a and C12r1; review C1 with its response.

## Concerns owed

Use these ids, and `order` values in the range 30 to 39.

- `daemon`: what the machine daemon owns, its socket and API, its workers per box, subscriptions, housekeeping and budgets, starting and stopping.
- `box-channel`: how the daemon and a box's agent talk: who listens, how the daemon knows whom it reached, the hello, multiplexing, the frame inspector, the control stream and raw streams, reconnection. A tour of that id exists; keep or replace it.
- `sessions-and-events`: exists from the first model. Verify every sentence and extend it: ownership of a session by a project, ingestion, reconciliation, the derived view and its states.
- `input-and-receipts`: sending, steering, answering, interrupting, keys; commands and their receipts; adapters and the verified paste; the gate that waits for a session at rest.
- `requests-and-approval`: what a box may ask for, the risk classes, where each may be decided, execution and the request log.
- `watching-sessions`: `ps`, the inbox, `events`, `attach`, seen marks, and the terminal the daemon serves to the web app.

## Flows owed

- `send-prompt`: exists; verify and extend.
- `event-ingestion`: a line in the spool to a journal entry to a subscriber.
- `question-answer`: a harness asks, the user answers from the CLI.
- `request-approve`: `devc request …` in a box to the decision and its execution.
- `channel-connect`: the daemon finds a running box and brings its channel up.
- `daemon-start`: start-up and reconciliation.
- `attach`: `devc attach` to the harness's terminal.

## Diagrams to consider

The states of a session as the daemon derives them. The states of a command and its receipt. The states of a request.

## Notes

- Concerns planned by other chunks are listed in `docs/dev/chunks/X-content-areas.md`. Link (`related`, `[[concern:…]]`) only to those that already exist; the last chunk adds the missing cross-links.
- `internal/proto` is the vocabulary of four processes. Say for each family of types who writes it and who must not trust it.
