// Small pieces shared by the screens: icons, state badges, times, the top
// bar and the navigation.

import { html, useState, useEffect } from './lib.js';
import { state, useStore } from './store.js';

// Icons: 24×24 strokes, drawn with currentColor. Every use sits beside a
// text label or carries one for screen readers.
const paths = {
  inbox: 'M3 13h5l2 3h4l2-3h5M5 5h14l2 8v6a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-6z',
  folder: 'M3 6a1 1 0 0 1 1-1h5l2 2h9a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1z',
  plus: 'M12 5v14M5 12h14',
  server: 'M4 4h16v6H4zM4 14h16v6H4zM8 7h.01M8 17h.01',
  back: 'M15 5l-7 7 7 7',
  hand: 'M12 8v4M12 16h.01M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z',
  failed: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM15 9l-6 6M9 9l6 6',
  done: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM8 12l3 3 5-6',
  working: 'M12 3v3M12 18v3M4.6 4.6l2.1 2.1M17.3 17.3l2.1 2.1M3 12h3M18 12h3M4.6 19.4l2.1-2.1M17.3 6.7l2.1-2.1',
  idle: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM10 9v6M14 9v6',
  ended: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM9 9h6v6H9z',
  terminal: 'M4 5h16v14H4zM8 10l3 2-3 2M13 15h3',
  diff: 'M6 3v12M18 9v12M6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM18 9a9 9 0 0 1-9 9',
  timeline: 'M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01',
  send: 'M4 12l16-8-6 16-2-7z',
  steer: 'M13 2 4 14h7l-1 8 9-12h-7z',
  stop: 'M6 6h12v12H6z',
  more: 'M5 12h.01M12 12h.01M19 12h.01',
  refresh: 'M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7',
  offline: 'M3 3l18 18M8.5 16.5a5 5 0 0 1 7 0M5 12.5a10 10 0 0 1 4-2.3M14.8 10.2a10 10 0 0 1 4.2 2.3M2 8.8a15 15 0 0 1 4.5-2.9M11 5a15 15 0 0 1 11 3.8M12 20h.01',
  lock: 'M6 11h12v10H6zM8 11V7a4 4 0 0 1 8 0v4',
  filter: 'M3 5h18l-7 9v5l-4 2v-7z',
  down: 'M6 9l6 6 6-6',
  up: 'M6 15l6-6 6 6',
  right: 'M9 6l6 6-6 6',
  branch: 'M6 3v12M6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM18 9c0 6-12 3-12 9',
  play: 'M7 4l13 8-13 8z',
  tool: 'M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.5 2.5-2.4-.6-.6-2.4z',
  question: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM9.5 9a2.5 2.5 0 1 1 3.5 2.3c-.6.3-1 .9-1 1.6V14M12 17h.01',
  note: 'M5 4h14v12l-4 4H5zM15 20v-4h4',
  key: 'M15 7a4 4 0 1 1-3.5 6L4 20.5V17h3v-3h3l1.5-1.5A4 4 0 0 1 15 7z',
  check: 'M5 12l5 5 9-10',
  x: 'M6 6l12 12M18 6 6 18',
};

export function Icon({ name, label, cls = '' }) {
  return html`<svg class=${'icon ' + cls} viewBox="0 0 24 24" aria-hidden=${label ? 'false' : 'true'} role=${label ? 'img' : null}
    fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    ${label ? html`<title>${label}</title>` : null}<path d=${paths[name] || paths.more} /></svg>`;
}

// Sections and states, each with an icon and a word: never colour alone.
export const sectionInfo = {
  'needs-you': { label: 'Needs you', icon: 'hand', cls: 'st-needs' },
  failed: { label: 'Failed', icon: 'failed', cls: 'st-failed' },
  finished: { label: 'Finished', icon: 'done', cls: 'st-finished' },
  working: { label: 'Working', icon: 'working', cls: 'st-working' },
  idle: { label: 'Idle', icon: 'idle', cls: 'st-idle' },
};

// stateOf describes a session's state in a word.
export function stateOf(s) {
  if (!s) return { label: 'Unknown', icon: 'more', cls: 'st-idle' };
  if (!s.live) {
    if (s.end_reason === 'lost') return { label: 'Lost', icon: 'failed', cls: 'st-failed' };
    return { label: 'Ended', icon: 'ended', cls: 'st-idle' };
  }
  if (s.state === 'blocked') {
    const why = s.question ? 'Question' : s.kind === 'permission' ? 'Permission' : 'Blocked';
    return { label: why, icon: 'hand', cls: 'st-needs' };
  }
  if (s.requests > 0) return { label: 'Request', icon: 'key', cls: 'st-needs' };
  switch (s.state) {
    case 'error': return { label: 'Error', icon: 'failed', cls: 'st-failed' };
    case 'done': return s.done_turn > s.seen_turn ? { label: 'Finished', icon: 'done', cls: 'st-finished' } : { label: 'Done', icon: 'done', cls: 'st-idle' };
    case 'working': return { label: 'Working', icon: 'working', cls: 'st-working' };
    case 'idle': return { label: 'Idle', icon: 'idle', cls: 'st-idle' };
  }
  return { label: 'Starting', icon: 'more', cls: 'st-idle' };
}

export function StateBadge({ s }) {
  const st = stateOf(s);
  return html`<span class=${'badge ' + st.cls}><${Icon} name=${st.icon} />${st.label}</span>`;
}

// --- Times ---

export function ago(t, now = Date.now()) {
  if (!t) return '';
  const ms = typeof t === 'number' ? t : Date.parse(t);
  if (!ms || ms < 1e11) return '';
  const s = Math.max(0, Math.round((now - ms) / 1000));
  if (s < 45) return 'just now';
  if (s < 90) return '1 min ago';
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  if (h < 36) return `${h} h ago`;
  const d = Math.round(h / 24);
  if (d < 45) return `${d} d ago`;
  return new Date(ms).toLocaleDateString();
}

export function duration(from, to = Date.now()) {
  const a = Date.parse(from);
  if (!a) return '';
  const b = typeof to === 'number' ? to : Date.parse(to);
  const s = Math.max(0, Math.round((b - a) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h} h ${m % 60} min`;
  return `${Math.floor(h / 24)} d ${h % 24} h`;
}

// useNow re-renders every interval so relative times stay true.
export function useNow(ms = 30000) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}

// --- Frame ---

export function needsYouCount(st = state) {
  let n = 0;
  for (const s of Object.values(st.sessions)) if (s.section === 'needs-you') n++;
  return n;
}

// Connection: the stream's state in words.
export function Connection() {
  const st = useStore();
  const now = useNow(15000);
  if (st.auth === 'offline' || (st.savedAt && st.stream !== 'live')) {
    const when = st.savedAt ? ` · saved ${ago(st.savedAt, now)}` : '';
    return html`<span class="conn conn-off" role="status"><${Icon} name="offline" />Offline${when}</span>`;
  }
  if (st.stream === 'live') return html`<span class="conn conn-live" role="status"><span class="dot" aria-hidden="true"></span>Live</span>`;
  if (st.stream === 'retrying') return html`<span class="conn conn-retry" role="status"><${Icon} name="refresh" />Reconnecting</span>`;
  return html`<span class="conn conn-retry" role="status"><${Icon} name="refresh" />Connecting</span>`;
}

export function TopBar({ title, sub, back, actions }) {
  return html`<header class="topbar">
    ${back ? html`<a class="iconbtn" href=${back} aria-label="Back"><${Icon} name="back" /></a>` : html`<a class="brand" href="#/" aria-label="devc inbox"><img src="/icons/icon.svg" alt="" width="28" height="28" /></a>`}
    <div class="title"><h1>${title}</h1>${sub ? html`<div class="sub">${sub}</div>` : null}</div>
    <div class="actions">${actions}<${Connection} /></div>
  </header>`;
}

export function Nav({ route }) {
  const st = useStore();
  const n = needsYouCount(st);
  const item = (href, icon, label, active, badge) => html`<a href=${href} class=${active ? 'active' : ''} aria-current=${active ? 'page' : null}>
    <${Icon} name=${icon} /><span>${label}</span>${badge ? html`<span class="count" aria-label=${badge + ' need you'}>${badge}</span>` : null}</a>`;
  return html`<nav class="nav" aria-label="Main">
    ${item('#/', 'inbox', 'Inbox', route === 'inbox', n)}
    ${item('#/projects', 'folder', 'Projects', route === 'projects', 0)}
    ${item('#/new', 'plus', 'New', route === 'new', 0)}
    ${item('#/machines', 'server', 'Machines', route === 'machines', 0)}
  </nav>`;
}

export function Toasts() {
  const st = useStore();
  return html`<div class="toasts" role="status" aria-live="polite">
    ${st.toasts.map((t) => html`<div key=${t.id} class=${'toast toast-' + t.kind}>${t.text}</div>`)}
  </div>`;
}

export function Empty({ icon = 'inbox', children }) {
  return html`<div class="empty"><${Icon} name=${icon} /><p>${children}</p></div>`;
}

// shortRef is project (and realm when the filter does not say it).
export const shortRef = (s) => (s.realm && s.project ? `${s.realm}/${s.project}` : s.ref || '');
