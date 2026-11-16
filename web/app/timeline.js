// The timeline of an open session (finding C2-07): a bounded window of its
// events and the entries the screen shows, kept up to date event by event.
// A new event changes the last entry or adds one; it never rebuilds the
// history. Live events beyond the window push the oldest out (they can be
// paged in again); older pages are loaded on demand. Paging back beyond the
// window's size moves the window: the newest events are dropped and the
// window is no longer live until the screen returns to the latest events
// (finding D1-13). A run of tool calls is bounded too (finding D1-09).

// MAX_EVENTS is the most events an open session keeps.
export const MAX_EVENTS = 1000;

// TOOL_RUN_MAX is the most tool calls one folded run holds; a longer run
// continues in a new one.
export const TOOL_RUN_MAX = 200;

// entry turns one event into a timeline entry; null for events the
// timeline does not show (states, seen marks, harness details).
function entry(e) {
  const d = e.data || {};
  let it;
  switch (e.type) {
    case 'prompt': it = { kind: 'prompt', text: d.text || '', length: d.length }; break;
    case 'message': it = { kind: 'message', text: d.text || '' }; break;
    case 'note': it = { kind: 'note', text: d.text || '' }; break;
    case 'question': it = { kind: 'question', text: d.text || '', options: d.options || [] }; break;
    case 'answer': it = { kind: 'answer', choice: d.choice, text: d.text || '', via: d.via }; break;
    case 'request': it = { kind: 'request', id: d.id, what: d.summary || d.kind, risk: d.risk, status: d.status }; break;
    case 'session.start': it = { kind: 'start', harness: d.harness, worktree: d.worktree }; break;
    case 'session.end': it = { kind: 'end', reason: d.reason, status: d.status }; break;
    case 'state':
      if (d.state !== 'error') return null;
      it = { kind: 'error', text: d.message || 'The harness reported an error' };
      break;
    default:
      return null;
  }
  it.seq = e.seq;
  it.last = e.seq;
  it.at = e.at || e.ts;
  return it;
}

// addEvent adds one event to the entries. Consecutive tool calls fold into
// runs of at most TOOL_RUN_MAX (events the timeline does not show do not
// end a run); a run that grows becomes a new object, so its row is drawn
// again and no other.
export function addEvent(items, e) {
  if (e.type === 'tool') {
    const d = e.data || {};
    const t = { name: d.name, summary: d.summary || '', at: e.at || e.ts, seq: e.seq };
    const last = items[items.length - 1];
    if (last && last.kind === 'tools' && last.list.length < TOOL_RUN_MAX) {
      items[items.length - 1] = { ...last, list: [...last.list, t], at: t.at, last: e.seq };
    } else {
      items.push({ kind: 'tools', seq: e.seq, last: e.seq, list: [t], at: t.at });
    }
    return;
  }
  const it = entry(e);
  if (it) items.push(it);
}

export function buildItems(events) {
  const items = [];
  for (const e of events) addEvent(items, e);
  return items;
}

// newDetail is an open session's timeline state.
// live: the window ends at the session's newest event (false after paging
// back beyond the window's size).
export function newDetail() {
  return { events: [], items: [], ver: 0, more: false, partial: false, loading: false, older: false, error: '', status: 0, req: 0, buffer: [], reload: false, live: true };
}

// lastSeq is the seq of the newest event held (0: none).
export const lastSeq = (d) => (d.events.length ? d.events[d.events.length - 1].seq : 0);

// append adds a live event at the end and keeps the window.
export function append(d, e) {
  d.events.push(e);
  addEvent(d.items, e);
  trim(d);
  d.ver++;
}

// trim drops the oldest events beyond MAX_EVENTS, the entries that lie
// entirely before the first event kept, and the calls of a tool run that
// do.
export function trim(d) {
  if (d.events.length <= MAX_EVENTS) return;
  d.events.splice(0, d.events.length - MAX_EVENTS);
  const first = d.events[0].seq;
  let i = 0;
  while (i < d.items.length && d.items[i].last < first) i++;
  if (i) d.items.splice(0, i);
  const head = d.items[0];
  if (head && head.kind === 'tools' && head.seq < first) {
    const list = head.list.filter((t) => t.seq >= first);
    d.items[0] = { ...head, list, seq: list.length ? list[0].seq : first };
  }
  d.more = true;
}

// replace sets the window from a snapshot, then applies the events that
// arrived while it was read, in seq order. It returns false when they do
// not join the snapshot (a gap: read it again).
export function replace(d, events) {
  d.events = events.slice(-MAX_EVENTS);
  let ok = true;
  for (const e of d.buffer.sort((a, b) => a.seq - b.seq)) {
    const last = lastSeq(d);
    if (e.seq <= last) continue;
    if (last && e.seq !== last + 1) {
      ok = false;
      break;
    }
    d.events.push(e);
  }
  d.buffer = [];
  if (d.events.length > MAX_EVENTS) d.events.splice(0, d.events.length - MAX_EVENTS);
  d.items = buildItems(d.events);
  d.live = true;
  d.ver++;
  return ok;
}

// prepend adds an older page before the window. A window that would grow
// beyond MAX_EVENTS moves back instead: its newest events are dropped and
// it is no longer live (finding D1-13).
export function prepend(d, events) {
  const first = d.events.length ? d.events[0].seq : Infinity;
  const older = events.filter((e) => e.seq < first);
  let all = [...older, ...d.events];
  if (all.length > MAX_EVENTS) {
    all = all.slice(0, MAX_EVENTS);
    d.live = false;
  }
  d.events = all;
  d.items = buildItems(d.events);
  d.ver++;
}
