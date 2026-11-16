// The app's state: what the hub last told us, kept in memory and in local
// storage so that the screens show the last data even offline, and the
// actions that change it. Components re-render through useStore whenever
// anything changes (the data is small; an open session's timeline is a
// bounded window, timeline.js).

import { useState, useEffect } from './lib.js';
import * as api from './api.js';
import { newDetail, append, replace, prepend, lastSeq, MAX_EVENTS } from './timeline.js';

const CACHE_KEY = 'devc.cache.v1';

export const state = {
  me: null, // the login (GET /api/me)
  auth: 'unknown', // unknown, ok, login, nocert, revoked, offline
  authDetail: '',
  totp: false,
  machines: {}, // name → machine row
  sessions: {}, // "machine/session" → session summary
  requests: {}, // "machine/id" → request summary
  details: {}, // "machine/session" → timeline state (timeline.js)
  outbox: {}, // "machine/session" → prompts sent from here, until they appear
  pending: {}, // command ID → a command whose outcome is not known yet
  sessionVer: {}, // "machine/session" → stream position its summary is from
  projects: null, // GET /api/projects answer, by machine
  projectsAt: 0,
  stream: 'closed', // closed, connecting, live, retrying
  savedAt: 0, // when the cached data was saved (0: fresh from the hub)
  loadedAt: 0,
  toasts: [],
};

const listeners = new Set();
let queued = false;

export function changed() {
  if (queued) return;
  queued = true;
  queueMicrotask(() => {
    queued = false;
    for (const f of listeners) f();
    saveSoon();
  });
}

export function set(patch) {
  Object.assign(state, patch);
  changed();
}

export function useStore() {
  const [, force] = useState(0);
  useEffect(() => {
    const f = () => force((n) => n + 1);
    listeners.add(f);
    return () => listeners.delete(f);
  }, []);
  return state;
}

export const key = (machine, id) => `${machine}/${id}`;

// online and stale come from the current machine map, not from what a
// summary said when it arrived (finding C2-12): a machine that goes or
// comes back changes every row of it at once.
export function online(machine) {
  const m = state.machines[machine];
  return !!(m && m.online);
}

export function stale(x) {
  if (!x) return true;
  return state.machines[x.machine] ? !state.machines[x.machine].online : !!x.stale;
}

// --- Local storage (per-device convenience; failures are ignored) ---

let saveTimer = 0;
function saveSoon() {
  if (saveTimer || state.auth !== 'ok') return;
  saveTimer = setTimeout(() => {
    saveTimer = 0;
    try {
      localStorage.setItem(CACHE_KEY, JSON.stringify({
        at: Date.now(), machines: state.machines, sessions: state.sessions, requests: state.requests, pending: state.pending,
      }));
    } catch (e) { /* storage full or blocked */ }
  }, 1000);
}

export function loadCache() {
  try {
    const c = JSON.parse(localStorage.getItem(CACHE_KEY) || 'null');
    if (c && c.sessions) {
      Object.assign(state, { machines: c.machines || {}, sessions: c.sessions || {}, requests: c.requests || {}, savedAt: c.at || 0 });
    }
    if (c && c.pending && typeof c.pending === 'object') {
      state.pending = c.pending;
      // A prompt whose outcome is unknown shows in its session again.
      for (const p of Object.values(state.pending)) {
        if (p.type === 'send') addOutbox(key(p.machine, p.params.session), { id: p.id, text: p.params.text, mode: p.params.mode, outcome: p.outcome, at: p.at });
      }
      if (Object.keys(state.pending).length) watchPending();
    }
  } catch (e) { /* ignore */ }
}

export function clearCache() {
  try { localStorage.removeItem(CACHE_KEY); } catch (e) { /* ignore */ }
}

// --- Toasts ---

let toastSeq = 0;
export function toast(text, kind = 'info', ms = 4000) {
  const id = ++toastSeq;
  state.toasts = [...state.toasts, { id, text, kind }];
  changed();
  setTimeout(() => {
    state.toasts = state.toasts.filter((t) => t.id !== id);
    changed();
  }, ms);
}

// --- Loading ---

// checkAuth asks who we are; it sorts failures into the reasons the login
// screen explains.
export async function checkAuth() {
  try {
    const me = await api.get('/api/me');
    set({ me, auth: 'ok', authDetail: '' });
    return true;
  } catch (e) {
    if (e.status === 401) set({ auth: 'login', totp: !!e.body.totp, authDetail: '' });
    else if (e.status === 404) set({ auth: 'nocert', authDetail: e.message });
    else if (e.status === 403) set({ auth: 'revoked', authDetail: e.message });
    else set({ auth: 'offline', authDetail: e.message });
    return false;
  }
}

// loadSnapshot reads the inbox, machines and requests and returns the
// stream position to resume from. current() is asked before the answer is
// applied: a snapshot that a newer one superseded is dropped (null).
export async function loadSnapshot(current = () => true) {
  const [inbox, machines, requests] = await Promise.all([
    api.get('/api/inbox'), api.get('/api/machines'), api.get('/api/requests'),
  ]);
  if (!current()) return null;
  const sessions = {};
  for (const sec of inbox.sections) for (const s of sec.sessions) sessions[key(s.machine, s.session)] = s;
  // Sessions outside the inbox that a screen has open stay.
  for (const k of Object.keys(state.details)) if (!sessions[k] && state.sessions[k]) sessions[k] = state.sessions[k];
  const ms = {};
  for (const m of machines.machines) ms[m.name] = m;
  const rs = {};
  for (const r of requests.requests) rs[key(r.machine, r.id)] = r;
  const pos = Math.min(inbox.stream_id, machines.stream_id ?? inbox.stream_id, requests.stream_id);
  const vers = {};
  for (const k of Object.keys(sessions)) vers[k] = Math.max(state.sessionVer[k] || 0, inbox.stream_id || 0);
  set({ sessions, sessionVer: vers, machines: ms, requests: rs, savedAt: 0, loadedAt: Date.now() });
  resolvePending();
  return pos;
}

// putSession stores a session summary that is current as of stream
// position ver, unless a newer one is stored already (finding D1-12: a
// detail read that began before a change must not undo it).
function putSession(k, s, ver) {
  const have = state.sessionVer[k] || 0;
  if (ver && have > ver && state.sessions[k]) return false;
  state.sessions[k] = s;
  if (ver) state.sessionVer[k] = Math.max(have, ver);
  return true;
}

// loadDetail reads a session's recent events (before: an older page).
// While a full read runs, the stream's events for the session are held
// back and merged by seq afterwards; an answer to a request that a newer
// one replaced, or for a screen that closed, is ignored (finding C2-10).
let reqSeq = 0;
export async function loadDetail(machine, sid, before = 0) {
  const k = key(machine, sid);
  let d = state.details[k];
  if (!d) {
    d = newDetail();
    state.details[k] = d;
  }
  const token = ++reqSeq;
  if (before) {
    if (d.loading || d.older) return;
    d.older = true;
  } else {
    d.req = token;
    d.loading = true;
    d.buffer = [];
    d.reload = false;
  }
  d.error = '';
  d.ver++;
  changed();
  const current = () => state.details[k] === d && (before ? d.events.length && d.events[0].seq === before : d.req === token);
  try {
    const q = before ? `?before=${before}&limit=200` : '?limit=300';
    const res = await api.get(`/api/sessions/${encodeURIComponent(machine)}/${encodeURIComponent(sid)}${q}`);
    if (!current()) {
      if (before && state.details[k] === d) d.older = false;
      return;
    }
    putSession(k, res.session, res.stream_id);
    d.partial = res.partial;
    d.status = 0;
    if (before) {
      d.older = false;
      prepend(d, res.events);
      d.more = res.more;
    } else {
      d.loading = false;
      d.more = res.more;
      const joined = replace(d, res.events);
      if (!joined || d.reload) {
        loadDetail(machine, sid);
      }
    }
  } catch (e) {
    if (state.details[k] !== d) return;
    if (before) d.older = false;
    else if (d.req === token) d.loading = false;
    d.error = e.message;
    d.status = e.status;
    d.ver++;
  }
  changed();
}

export function dropDetail(machine, sid) {
  delete state.details[key(machine, sid)];
  changed();
}

export async function loadProjects(force = false) {
  if (!force && state.projects && Date.now() - state.projectsAt < 15000) return;
  try {
    const res = await api.get('/api/projects');
    set({ projects: res.machines, projectsAt: Date.now() });
  } catch (e) {
    if (!state.projects) set({ projects: [] });
    toast(`Projects: ${e.message}`, 'error');
  }
}

// --- Stream messages (stream.js hands them here) ---

// applyChange applies one stream message; id is its stream position.
export function applyChange(kind, data, id = 0) {
  switch (kind) {
    case 'session': {
      putSession(key(data.machine, data.session), data, id);
      break;
    }
    case 'session.removed': {
      const k = key(data.machine, data.session);
      delete state.sessions[k];
      delete state.sessionVer[k];
      break;
    }
    case 'machine':
      state.machines[data.name] = data;
      if (data.online) resolvePending(data.name);
      break;
    case 'request': {
      const k = key(data.machine, data.id);
      if (data.status === 'pending') state.requests[k] = data;
      else delete state.requests[k];
      break;
    }
    case 'request.removed':
      delete state.requests[key(data.machine, data.id)];
      break;
    case 'event': {
      const k = key(data.machine, data.session);
      const d = state.details[k];
      if (!d) break;
      if (!d.live) {
        // The window shows older history: the live end moves on without
        // it ("Back to the latest" reads it again).
        settleOutbox(k, data.event);
        break;
      }
      if (d.loading) {
        // Held back until the snapshot is in; bounded like the window.
        if (d.buffer.length < MAX_EVENTS) d.buffer.push(data.event);
        else d.reload = true;
        break;
      }
      const last = lastSeq(d);
      if (data.event.seq <= last) break;
      if (last !== 0 && data.event.seq !== last + 1) {
        loadDetail(data.machine, data.session);
        break;
      }
      append(d, data.event);
      settleOutbox(k, data.event);
      break;
    }
    case 'session.reset':
      if (state.details[key(data.machine, data.session)]) loadDetail(data.machine, data.session);
      break;
    default:
      return;
  }
  changed();
}

// --- Commands ---

const outcomes = {
  delivered: ['Delivered', 'ok'],
  accepted: ['Queued: it is typed when the session is ready', 'info'],
  delivering: ['Typing it now…', 'info'],
  'typed-not-submitted': ['Typed, but the harness did not take it: check the terminal', 'warn'],
  unknown: ['Sent; the machine could not confirm it arrived', 'warn'],
  refused: ['Refused', 'error'],
};

export function describeReceipt(rec) {
  if (rec.outcome === 'refused' && /^stale answer/.test(rec.detail || '')) {
    // The question changed before the answer could be typed (C12r1).
    return ['The question changed before your answer arrived; nothing was typed', 'warn'];
  }
  const [text, kind] = outcomes[rec.outcome] || [rec.outcome, 'info'];
  return [rec.detail && rec.outcome === 'refused' ? `${text}: ${rec.detail}` : text, kind];
}

// What the pending list calls each command type.
const labels = { send: 'Message', answer: 'Answer', interrupt: 'Stop', keys: 'Keys', start: 'New session', stop: 'End session' };

// run sends a command and reports its receipt. When its outcome cannot be
// known (the hub or the machine went away mid-way), the command is kept
// with its ID and parameters as pending, shown as unknown, and resolved by
// asking for its receipt; a retry reuses the ID (finding C2-11). It
// returns the receipt, { outcome: 'unknown' } while pending, or null when
// nothing was sent.
export async function run(machine, type, params, { wait = 0, quiet = false, id = api.newID() } = {}) {
  try {
    const rec = await api.command(machine, type, params, wait, id);
    const [text, kind] = describeReceipt(rec);
    if (!quiet || kind === 'error' || kind === 'warn') toast(text, kind);
    if (!rec.final && type !== 'seen') addPending({ id, machine, type, params, wait, outcome: rec.outcome, at: Date.now(), known: true });
    return rec;
  } catch (e) {
    if (e.uncertain) {
      // A seen mark is harmless to repeat: it is not tracked, and is sent
      // again the next time the session is shown.
      if (type === 'seen') return null;
      addPending({ id, machine, type, params, wait, outcome: 'unknown', at: Date.now() });
      toast('The outcome is not known yet; the app asks the machine when it can', 'warn', 6000);
      return { command: id, outcome: 'unknown', final: false };
    }
    toast(e.message, 'error', 6000);
    return null;
  }
}

// --- Commands whose outcome is not known yet ---

let pendingTimer = 0;

function addPending(p) {
  p.label = labels[p.type] || p.type;
  state.pending[p.id] = p;
  watchPending();
  changed();
}

function watchPending() {
  if (pendingTimer) return;
  pendingTimer = setInterval(() => {
    if (!Object.keys(state.pending).length) {
      clearInterval(pendingTimer);
      pendingTimer = 0;
      return;
    }
    resolvePending();
  }, 10000);
}

// finish ends a pending command with its final receipt.
function finish(p, rec) {
  delete state.pending[p.id];
  if (p.type === 'send') {
    setOutbox(key(p.machine, p.params.session), p.id, rec.outcome);
  }
  const [text, kind] = describeReceipt(rec);
  if (p.type !== 'send' || kind !== 'ok') toast(`${p.label}: ${text}`, kind, 6000);
}

// resolvePending asks the machines that are online for the receipts of
// pending commands: a final receipt ends one; a receipt the machine does
// not have means it never arrived ('unsent': the user may send it again
// with the same ID, or dismiss it).
let resolving = false;
export async function resolvePending(machine = '') {
  if (resolving) return;
  resolving = true;
  try {
    for (const p of Object.values(state.pending)) {
      if ((machine && p.machine !== machine) || !online(p.machine) || p.outcome === 'unsent') continue;
      try {
        const rec = await api.receipt(p.machine, p.id, 0);
        if (!state.pending[p.id]) continue;
        if (rec.final) finish(p, rec);
        else {
          p.outcome = rec.outcome;
          p.known = true;
          if (p.type === 'send') setOutbox(key(p.machine, p.params.session), p.id, rec.outcome);
        }
      } catch (e) {
        if (e.status === 404 && state.pending[p.id] && !p.known) {
          p.outcome = 'unsent';
          if (p.type === 'send') setOutbox(key(p.machine, p.params.session), p.id, 'unsent');
        }
      }
      changed();
    }
  } finally {
    resolving = false;
  }
}

// retryPending sends a pending command again with its own ID: the machine
// applies an ID once.
export async function retryPending(id) {
  const p = state.pending[id];
  if (!p) return;
  p.outcome = 'unknown';
  if (p.type === 'send') setOutbox(key(p.machine, p.params.session), id, 'sending');
  changed();
  try {
    const rec = await api.command(p.machine, p.type, p.params, p.wait, id);
    if (rec.final) finish(p, rec);
    else {
      p.outcome = rec.outcome;
      p.known = true;
      if (p.type === 'send') setOutbox(key(p.machine, p.params.session), id, rec.outcome);
    }
  } catch (e) {
    if (!e.uncertain) {
      delete state.pending[id];
      if (p.type === 'send') setOutbox(key(p.machine, p.params.session), id, 'failed');
      toast(e.message, 'error', 6000);
    } else if (p.type === 'send') setOutbox(key(p.machine, p.params.session), id, 'unknown');
  }
  changed();
}

// dismissPending forgets a pending command (its prompt leaves the outbox).
export function dismissPending(id) {
  const p = state.pending[id];
  if (!p) return;
  delete state.pending[id];
  if (p.type === 'send') removeOutbox(key(p.machine, p.params.session), id);
  changed();
}

// --- The outbox: prompts sent from here, until their prompt event ---

function addOutbox(k, item) {
  const list = state.outbox[k] || [];
  if (!list.some((i) => i.id === item.id)) state.outbox[k] = [...list, item];
}

function setOutbox(k, id, outcome) {
  state.outbox[k] = (state.outbox[k] || []).map((i) => (i.id === id ? { ...i, outcome } : i));
  if (outcome !== 'delivered' && outcome !== 'accepted' && outcome !== 'delivering' && outcome !== 'sending' && !state.pending[id]) {
    setTimeout(() => removeOutbox(k, id), 8000);
  }
}

// sendPrompt types a prompt into a session (queue or steer) and keeps it
// in the outbox until its prompt event arrives, the receipt says it
// failed, or (when its outcome is unknown) the user dismisses it.
export async function sendPrompt(s, text, mode) {
  const k = key(s.machine, s.session);
  const id = api.newID();
  addOutbox(k, { id, text, mode, outcome: 'sending', at: Date.now() });
  changed();
  const rec = await run(s.machine, 'send', { session: s.session, ref: s.ref, text, mode }, { wait: mode === 'steer' ? 20 : 0, quiet: true, id });
  setOutbox(k, id, rec ? rec.outcome : 'failed');
  changed();
  return rec;
}

function removeOutbox(k, id) {
  state.outbox[k] = (state.outbox[k] || []).filter((i) => i.id !== id);
  changed();
}

function settleOutbox(k, ev) {
  if (ev.type !== 'prompt' || !state.outbox[k]) return;
  const text = (ev.data && ev.data.text) || '';
  const list = state.outbox[k];
  const i = list.findIndex((it) => text && it.text.trim().startsWith(text.replace(/ …\[truncated\]$/, '').trim().slice(0, 200)));
  if (i >= 0) {
    const id = list[i].id;
    state.outbox[k] = list.filter((_, j) => j !== i);
    // The prompt is in the session: whatever the receipt says later.
    if (state.pending[id]) delete state.pending[id];
  }
}

// markSeen marks a session seen, once per turn: a live session up to its
// last finished turn (a turn still running is not seen yet), an ended one
// up to its last turn.
const seenSent = new Set();
export function markSeen(s) {
  const turn = s && (s.live ? s.done_turn : s.turn);
  if (!turn || s.seen_turn >= turn || stale(s)) return;
  const k = `${s.machine}/${s.session}/${turn}`;
  if (seenSent.has(k)) return;
  seenSent.add(k);
  run(s.machine, 'seen', { session: s.session, ref: s.ref, turn }, { quiet: true }).then((rec) => {
    if (!rec) seenSent.delete(k);
  });
}

// decide approves or denies a low-risk request. A busy machine leaves it
// pending (try again); an unknown outcome leaves it shown until the
// machine's request list says what became of it.
export async function decide(r, approve) {
  try {
    const res = await api.post(`/api/requests/${encodeURIComponent(r.machine)}/${encodeURIComponent(r.id)}/decide`, { approve });
    toast(approve ? `Approved: ${res.status}` : 'Denied', approve ? 'ok' : 'info');
    if (res.status !== 'pending') delete state.requests[key(r.machine, r.id)];
    changed();
  } catch (e) {
    if (e.status === 429) toast('The machine is busy with other requests; this one is still pending. Try again in a moment.', 'warn', 6000);
    else if (e.uncertain) toast('The machine went away; the decision may or may not have arrived. The request stays here until the machine says.', 'warn', 8000);
    else toast(e.network ? 'The hub cannot be reached' : e.message, 'error', 6000);
  }
}
