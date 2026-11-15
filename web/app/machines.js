// Machines: each machine the hub knows, online or when it was last seen,
// and how many sessions run on it.

import { html } from './lib.js';
import { useStore } from './store.js';
import { Icon, TopBar, Empty, ago, useNow } from './ui.js';

export function Machines() {
  const st = useStore();
  const now = useNow();
  const list = Object.values(st.machines).sort((a, b) => (b.online - a.online) || a.name.localeCompare(b.name));
  const live = (m) => (m.boxes || []).reduce((n, b) => n + (b.live || 0), 0);
  const needs = (m) => Object.values(st.sessions).filter((s) => s.machine === m.name && s.section === 'needs-you').length;
  return html`<div class="screen">
    <${TopBar} title="Machines" />
    <main class="content">
      ${list.length === 0 ? html`<${Empty} icon="server">No machine has connected yet. Enrol one with <code>devc hub enroll machine</code>.<//>` : null}
      <ul class="rows">${list.map((m) => html`<li key=${m.name}><a class=${'row machine' + (m.online ? '' : ' stale')} href=${`#/?machine=${encodeURIComponent(m.name)}`}>
        <div class="row-top"><span class="row-title">${m.name}</span>
          ${m.online
            ? html`<span class="badge st-working"><span class="dot" aria-hidden="true"></span>Online</span>`
            : html`<span class="badge st-idle"><${Icon} name="offline" />Offline</span>`}</div>
        <div class="row-meta"><span class="where">${m.online ? `connected ${ago(m.connected, now)}` : m.last_seen ? `last seen ${ago(m.last_seen, now)}` : 'never connected'}</span>
          ${m.notifier ? html`<span class="tag">sends notifications</span>` : null}</div>
        <div class="row-meta"><span class="where">${live(m)} live session${live(m) === 1 ? '' : 's'} · ${(m.boxes || []).length} project${(m.boxes || []).length === 1 ? '' : 's'}${m.host ? ' · ' + m.host : ''}${m.version ? ' · devc ' + m.version : ''}</span>
          ${needs(m) ? html`<span class="badge st-needs"><${Icon} name="hand" />${needs(m)} need${needs(m) === 1 ? 's' : ''} you</span>` : null}</div>
      </a></li>`)}</ul>
    </main>
  </div>`;
}
