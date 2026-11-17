// The hub's event stream (GET /api/stream): inbox changes for everyone and
// the events of the sessions a screen has open. It resumes from the last
// change ID it saw, reconnects by itself with backoff, is reopened when the
// set of open sessions changes, and closes 30 seconds after the page is
// hidden (a stream that names a session keeps the hub from notifying about
// it, and a hidden page would otherwise hold notifications back) to reopen
// when the page is visible again. A reset (the hub no longer has the
// changes after our position) stops the stream, reads the snapshot and
// reopens from the snapshot's position, so no change is applied twice or
// lost meanwhile (finding C2-10).

import { state, set, applyChange, loadSnapshot, loadDetail, checkAuth, key } from './store.js';

let es = null;
let lastId = null;
let follow = new Set(); // "machine/session"
let retryTimer = 0;
let retryDelay = 1000;
let hiddenTimer = 0;
let started = false;

const KINDS = ['session', 'session.removed', 'machine', 'request', 'request.removed', 'event', 'session.reset'];

function url() {
  const q = new URLSearchParams();
  for (const k of [...follow].slice(0, 32)) q.append('session', k);
  if (lastId !== null) q.set('last_id', String(lastId));
  const s = q.toString();
  return '/api/stream' + (s ? '?' + s : '');
}

function close() {
  if (es) {
    es.close();
    es = null;
  }
  clearTimeout(retryTimer);
  retryTimer = 0;
}

function open() {
  close();
  if (document.visibilityState === 'hidden') {
    set({ stream: 'closed' });
    return;
  }
  set({ stream: state.stream === 'live' ? 'live' : 'connecting' });
  const src = new EventSource(url());
  es = src;
  const track = (e) => {
    if (e.lastEventId) lastId = Number(e.lastEventId);
  };
  src.addEventListener('ready', (e) => {
    track(e);
    retryDelay = 1000;
    set({ stream: 'live' });
  });
  src.addEventListener('reset', () => {
    if (es !== src) return;
    retryDelay = 1000;
    close();
    lastId = null;
    reconnect();
  });
  for (const kind of KINDS) {
    src.addEventListener(kind, (e) => {
      track(e);
      if (state.stream !== 'live') set({ stream: 'live' });
      let data;
      try {
        data = JSON.parse(e.data);
      } catch (err) {
        return;
      }
      applyChange(kind, data, Number(e.lastEventId) || 0);
    });
  }
  src.onopen = () => {
    // A stream that resumes sends nothing until something changes.
    retryDelay = 1000;
    set({ stream: 'live' });
  };
  src.onerror = () => {
    if (es !== src) return;
    if (src.readyState === EventSource.CLOSED) {
      // The hub answered with an error (logged out, revoked) or the
      // connection failed for good: find out why, then try again later.
      es = null;
      set({ stream: 'retrying' });
      checkAuth().then((ok) => {
        if (ok) schedule();
      });
    } else {
      set({ stream: 'retrying' });
    }
  };
}

function schedule() {
  clearTimeout(retryTimer);
  const wait = retryDelay * (0.8 + Math.random() * 0.4);
  retryDelay = Math.min(retryDelay * 2, 60000);
  retryTimer = setTimeout(reconnect, wait);
}

// resync reads the snapshot again (after a reset, or a long absence);
// only the newest of overlapping reads is applied.
let generation = 0;
async function resync() {
  const mine = ++generation;
  let pos;
  try {
    pos = await loadSnapshot(() => mine === generation);
  } catch (e) {
    return false;
  }
  if (pos === null || mine !== generation) return false;
  lastId = pos;
  for (const k of follow) {
    const i = k.indexOf('/');
    loadDetail(k.slice(0, i), k.slice(i + 1));
  }
  return true;
}

async function reconnect() {
  if (lastId === null && !(await resync())) {
    set({ stream: 'retrying' });
    if (!(await checkAuth())) {
      if (state.auth === 'offline') schedule();
      return;
    }
    schedule();
    return;
  }
  open();
}

// start begins following the hub (after a successful login check).
export async function start() {
  if (!started) {
    started = true;
    document.addEventListener('visibilitychange', onVisibility);
    window.addEventListener('online', () => reconnect());
  }
  lastId = null;
  await reconnect();
}

export function stop() {
  close();
  lastId = null;
  set({ stream: 'closed' });
}

function onVisibility() {
  clearTimeout(hiddenTimer);
  if (document.visibilityState === 'hidden') {
    hiddenTimer = setTimeout(() => {
      close();
      set({ stream: 'closed' });
    }, 30000);
  } else if (!es && state.auth === 'ok') {
    retryDelay = 1000;
    reconnect();
  }
}

// followSession makes the stream carry a session's events while a screen
// shows it; the returned function stops that.
export function followSession(machine, sid) {
  const k = key(machine, sid);
  follow.add(k);
  if (state.auth === 'ok' && lastId !== null) open();
  return () => {
    follow.delete(k);
    if (state.auth === 'ok' && lastId !== null && es) open();
  };
}
