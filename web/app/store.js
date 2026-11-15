// The app's state: what the hub last told us, kept in memory and in local
// storage so that the screens show the last data even offline, and the
// actions that change it. Components re-render through useStore whenever
// anything changes (the data is small).

import { useState, useEffect } from './lib.js';
import * as api from './api.js';

const CACHE_KEY = 'devc.cache.v1';

export const state = {
  me: null, // the login (GET /api/me)
  auth: 'unknown', // unknown, ok, login, nocert, revoked, offline
  authDetail: '',
  totp: false,
  machines: {}, // name → machine row
  sessions: {}, // "machine/session" → session summary
  requests: {}, // "machine/id" → request summary
  details: {}, // "machine/session" → { events, more, partial, loading, error }
  outbox: {}, // "machine/session" → prompts sent from here, until they appear
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

// --- Local storage (per-device convenience; failures are ignored) ---

let saveTimer = 0;
function saveSoon() {
  if (saveTimer || state.auth !== 'ok') return;
  saveTimer = setTimeout(() => {
    saveTimer = 0;
    try {
      localStorage.setItem(CACHE_KEY, JSON.stringify({
        at: Date.now(), machines: state.machines, sessions: state.sessions, requests: state.requests,
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

// loadSnapshot reads the inbox, machines and requests; it returns the
// stream position to resume from.
export async function loadSnapshot() {
  const [inbox, machines, requests] = await Promise.all([
    api.get('/api/inbox'), api.get('/api/machines'), api.get('/api/requests'),
  ]);
  const sessions = {};
  for (const sec of inbox.sections) for (const s of sec.sessions) sessions[key(s.machine, s.session)] = s;
  // Sessions outside the inbox that a screen has open stay.
  for (const k of Object.keys(state.details)) if (!sessions[k] && state.sessions[k]) sessions[k] = state.sessions[k];
  const ms = {};
  for (const m of machines.machines) ms[m.name] = m;
  const rs = {};
  for (const r of requests.requests) rs[key(r.machine, r.id)] = r;
  set({ sessions, machines: ms, requests: rs, savedAt: 0, loadedAt: Date.now() });
  return Math.min(inbox.stream_id, machines.stream_id ?? inbox.stream_id, requests.stream_id);
}

export async function loadDetail(machine, sid, before = 0) {
  const k = key(machine, sid);
  const d = state.details[k] || { events: [], more: false };
  state.details[k] = { ...d, loading: true, error: '' };
  changed();
  try {
    const q = before ? `?before=${before}&limit=200` : '?limit=300';
    const res = await api.get(`/api/sessions/${encodeURIComponent(machine)}/${encodeURIComponent(sid)}${q}`);
    state.sessions[k] = res.session;
    const events = before ? [...res.events, ...d.events.filter((e) => e.seq >= before)] : res.events;
    state.details[k] = { events, more: res.more, partial: res.partial, loading: false, error: '' };
  } catch (e) {
    state.details[k] = { ...d, loading: false, error: e.message, status: e.status };
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

export function applyChange(kind, data) {
  switch (kind) {
    case 'session': {
      const k = key(data.machine, data.session);
      state.sessions[k] = data;
      break;
    }
    case 'session.removed':
      delete state.sessions[key(data.machine, data.session)];
      break;
    case 'machine':
      state.machines[data.name] = data;
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
      if (!d || d.loading) break;
      const last = d.events.length ? d.events[d.events.length - 1].seq : 0;
      if (data.event.seq <= last) break;
      if (data.event.seq !== last + 1 && last !== 0) {
        loadDetail(data.machine, data.session);
        break;
      }
      d.events = [...d.events, data.event];
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
  const [text, kind] = outcomes[rec.outcome] || [rec.outcome, 'info'];
  return [rec.detail && rec.outcome === 'refused' ? `${text}: ${rec.detail}` : text, kind];
}

// run sends a command, reports its receipt, and returns it (null on
// failure).
export async function run(machine, type, params, { wait = 0, quiet = false, id } = {}) {
  try {
    const rec = await api.command(machine, type, params, wait, id);
    const [text, kind] = describeReceipt(rec);
    if (!quiet || kind === 'error' || kind === 'warn') toast(text, kind);
    return rec;
  } catch (e) {
    toast(e.network ? 'The hub cannot be reached; nothing was sent' : e.message, 'error', 6000);
    return null;
  }
}

// send types a prompt into a session (queue or steer) and keeps it in the
// outbox until its prompt event arrives or the receipt says it failed.
export async function sendPrompt(machine, sid, text, mode) {
  const k = key(machine, sid);
  const id = api.newID();
  const item = { id, text, mode, outcome: 'sending', at: Date.now() };
  state.outbox[k] = [...(state.outbox[k] || []), item];
  changed();
  const rec = await run(machine, 'send', { session: sid, text, mode }, { wait: mode === 'steer' ? 20 : 0, quiet: true, id });
  item.outcome = rec ? rec.outcome : 'failed';
  changed();
  if (rec && !rec.final) {
    try {
      const fin = await api.receipt(machine, id, 60);
      item.outcome = fin.outcome;
      const [msg, kind] = describeReceipt(fin);
      if (kind !== 'ok') toast(msg, kind);
    } catch (e) { /* the timeline shows the prompt when it arrives */ }
    changed();
  }
  if (item.outcome !== 'delivered' && item.outcome !== 'accepted' && item.outcome !== 'delivering') {
    setTimeout(() => removeOutbox(k, id), 8000);
  }
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
  if (i >= 0) state.outbox[k] = list.filter((_, j) => j !== i);
}

// markSeen marks a session seen, once per turn: a live session up to its
// last finished turn (a turn still running is not seen yet), an ended one
// up to its last turn.
const seenSent = new Set();
export function markSeen(s) {
  const turn = s && (s.live ? s.done_turn : s.turn);
  if (!turn || s.seen_turn >= turn || s.stale) return;
  const k = `${s.machine}/${s.session}/${turn}`;
  if (seenSent.has(k)) return;
  seenSent.add(k);
  run(s.machine, 'seen', { session: s.session, turn }, { quiet: true }).then((rec) => {
    if (!rec) seenSent.delete(k);
  });
}

export async function decide(r, approve) {
  try {
    const res = await api.post(`/api/requests/${encodeURIComponent(r.machine)}/${encodeURIComponent(r.id)}/decide`, { approve });
    toast(approve ? `Approved: ${res.status}` : 'Denied', approve ? 'ok' : 'info');
    if (res.status !== 'pending') delete state.requests[key(r.machine, r.id)];
    changed();
  } catch (e) {
    toast(e.network ? 'The hub cannot be reached' : e.message, 'error', 6000);
  }
}
