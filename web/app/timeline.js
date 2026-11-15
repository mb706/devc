// The timeline of an open session (finding C2-07): a bounded window of its
// most recent events and the entries the screen shows, kept up to date
// event by event. A new event changes the last entry or adds one; it never
// rebuilds the history. Live events beyond the window push the oldest out
// (they can be paged in again); older pages are loaded on demand.

// MAX_EVENTS is the most events an open session keeps.
export const MAX_EVENTS = 1000;

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
// one run (events the timeline does not show do not end a run); a run that
// grows becomes a new object, so its row is drawn again and no other.
export function addEvent(items, e) {
  if (e.type === 'tool') {
    const d = e.data || {};
    const t = { name: d.name, summary: d.summary || '', at: e.at || e.ts };
    const last = items[items.length - 1];
    if (last && last.kind === 'tools') {
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
export function newDetail() {
  return { events: [], items: [], ver: 0, more: false, partial: false, loading: false, older: false, error: '', status: 0, req: 0, buffer: [], reload: false };
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

// trim drops the oldest events beyond MAX_EVENTS, and the entries that
// lie entirely before the first event kept.
export function trim(d) {
  if (d.events.length <= MAX_EVENTS) return;
  d.events.splice(0, d.events.length - MAX_EVENTS);
  const first = d.events[0].seq;
  let i = 0;
  while (i < d.items.length && d.items[i].last < first) i++;
  if (i) d.items.splice(0, i);
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
  d.ver++;
  return ok;
}

// prepend adds an older page before the window, within MAX_EVENTS.
export function prepend(d, events) {
  const first = d.events.length ? d.events[0].seq : Infinity;
  let older = events.filter((e) => e.seq < first);
  const room = Math.max(0, MAX_EVENTS - d.events.length);
  if (older.length > room) older = older.slice(older.length - room);
  d.events = [...older, ...d.events];
  d.items = buildItems(d.events);
  d.ver++;
}
