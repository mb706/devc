// Projects: every machine's projects, and one project's sessions,
// worktrees (ahead and behind) and pending requests.

import { html, useEffect, useState } from './lib.js';
import * as api from './api.js';
import { useStore, loadProjects, decide } from './store.js';
import { Icon, TopBar, Empty, ago, useNow } from './ui.js';
import { SessionRow } from './inbox.js';
import { describeRequest } from './session.js';

function BoxBadge({ state }) {
  const map = { running: ['play', 'st-working', 'Running'], stopped: ['stop', 'st-idle', 'Stopped'], absent: ['x', 'st-idle', 'No box'] };
  const [icon, cls, label] = map[state] || ['more', 'st-idle', state || 'Unknown'];
  return html`<span class=${'badge ' + cls}><${Icon} name=${icon} />${label}</span>`;
}

export function Projects() {
  const st = useStore();
  useEffect(() => { loadProjects(); }, []);
  const list = st.projects;
  return html`<div class="screen">
    <${TopBar} title="Projects" actions=${html`<button class="iconbtn" aria-label="Refresh" onClick=${() => loadProjects(true)}><${Icon} name="refresh" /></button>`} />
    <main class="content">
      ${!list ? html`<${Empty} icon="folder">Asking the machines…<//>` : list.length === 0 ? html`<${Empty} icon="server">No machine has connected to this hub yet.<//>` : null}
      ${(list || []).map((m) => html`<section class="section" key=${m.machine}>
        <h2><${Icon} name="server" />${m.machine}${!m.online ? html`<span class="tag"><${Icon} name="offline" />offline</span>` : null}</h2>
        ${m.error && m.online ? html`<p class="note warn">${m.error}</p>` : null}
        ${!m.online ? html`<p class="note">The machine is offline; its projects show when it is back.</p>` : null}
        <ul class="rows">${m.projects.map((p) => html`<li key=${p.ref}><a class="row" href=${`#/p/${encodeURIComponent(m.machine)}/${p.realm}/${p.project}`}>
          <div class="row-top"><span class="row-title">${p.realm}/${p.project}</span>
            ${p.needs_you ? html`<span class="badge st-needs"><${Icon} name="hand" />${p.needs_you} need${p.needs_you === 1 ? 's' : ''} you</span>` : null}</div>
          <div class="row-meta"><${BoxBadge} state=${p.box} /><span class="where">${p.live} live session${p.live === 1 ? '' : 's'} · ${p.worktrees.length} worktree${p.worktrees.length === 1 ? '' : 's'}</span></div>
        </a></li>`)}</ul>
      </section>`)}
    </main>
  </div>`;
}

function Divergence({ d, label }) {
  if (!d) return null;
  if (!d.ahead && !d.behind) return html`<span class="div"><span class="dim">${label}</span> even</span>`;
  return html`<span class="div"><span class="dim">${label}</span>${d.ahead ? html` <span class="add" title="ahead">↑${d.ahead}</span>` : null}${d.behind ? html` <span class="del" title="behind">↓${d.behind}</span>` : null}</span>`;
}

function RequestRow({ r }) {
  const [busy, setBusy] = useState(false);
  const act = async (approve) => { setBusy(true); await decide(r, approve); setBusy(false); };
  return html`<li class="req">
    <div class="req-top"><span><${Icon} name="key" />${describeRequest(r)}</span><span class=${'tag risk-' + r.risk}>${r.risk} risk</span></div>
    ${r.risk === 'low'
      ? html`<div class="row-actions"><button class="btn" disabled=${busy || r.stale} onClick=${() => act(false)}>Deny</button>
          <button class="btn primary" disabled=${busy || r.stale} onClick=${() => act(true)}>Approve</button></div>`
      : html`<div class="hint">Approve it on the machine: <code class="cmd">devc requests approve ${r.id}</code></div>`}
  </li>`;
}

export function Project({ machine, realm, project }) {
  const st = useStore();
  const now = useNow();
  const ref = `${realm}/${project}`;
  const [older, setOlder] = useState([]);
  useEffect(() => { loadProjects(); }, []);
  useEffect(() => {
    let gone = false;
    api.get(`/api/sessions?machine=${encodeURIComponent(machine)}&realm=${realm}&project=${project}&limit=30`)
      .then((r) => { if (!gone) setOlder(r.sessions); }).catch(() => {});
    return () => { gone = true; };
  }, [machine, realm, project]);
  const mp = (st.projects || []).find((m) => m.machine === machine);
  const p = mp && mp.projects.find((x) => x.ref === ref);
  const sessions = {};
  for (const s of older) sessions[s.session] = s;
  for (const s of Object.values(st.sessions)) if (s.machine === machine && s.ref === ref) sessions[s.session] = s;
  const list = Object.values(sessions).sort((a, b) => (b.live - a.live) || (Date.parse(b.last_event) - Date.parse(a.last_event)));
  const reqs = Object.values(st.requests).filter((r) => r.machine === machine && r.ref === ref);
  const newHref = (wt) => `#/new?machine=${encodeURIComponent(machine)}&ref=${ref}${wt ? '&worktree=' + wt : ''}`;
  return html`<div class="screen">
    <${TopBar} back="#/projects" title=${ref} sub=${html`<span class="hdr-sub">${p ? html`<${BoxBadge} state=${p.box} />` : null}<span>${machine}</span></span>`} />
    <main class="content">
      ${!mp ? html`<${Empty} icon="folder">Loading…<//>` : !p ? html`<${Empty} icon="folder">${mp.online ? `${machine} has no project ${ref}.` : `${machine} is offline.`}<//>` : null}
      ${p && p.problem ? html`<p class="note warn">${p.problem}</p>` : null}
      ${p ? html`<div class="facts">
        <span><${Icon} name="branch" />default ${p.default_branch || '?'}</span>
        ${p.harnesses.map((h) => html`<span class=${'chip' + (h.ready ? '' : ' off')} title=${h.ready ? 'ready' : 'not installed in the box'}>${h.id}${h.ready ? '' : ' (not ready)'}</span>`)}
      </div>` : null}
      ${reqs.length ? html`<section class="section"><h2 class="st-needs"><${Icon} name="key" />Requests<span class="n">${reqs.length}</span></h2>
        <ul class="reqs">${reqs.map((r) => html`<${RequestRow} key=${r.id} r=${r} />`)}</ul></section>` : null}
      <section class="section"><h2><${Icon} name="timeline" />Sessions<span class="n">${list.length}</span>
        <a class="btn small" href=${newHref('')}><${Icon} name="plus" />New</a></h2>
        ${list.length ? html`<ul class="rows">${list.map((s) => html`<${SessionRow} key=${s.session} s=${s} now=${now} showRealm=${false} />`)}</ul>` : html`<p class="note">No sessions in the last 30 days.</p>`}
      </section>
      ${p ? html`<section class="section"><h2><${Icon} name="branch" />Worktrees<span class="n">${p.worktrees.length}</span></h2>
        ${p.box !== 'running' && p.worktrees_at ? html`<p class="note">The box is ${p.box}; as of ${ago(p.worktrees_at, now)}.</p>` : null}
        ${p.error ? html`<p class="note warn">${p.error}</p>` : null}
        <ul class="wts">${p.worktrees.map((w) => html`<li key=${w.name}>
          <div class="wt-top"><b>${w.name}</b><span class="dim">${w.branch || 'detached'}</span>
            ${w.dirty ? html`<span class="tag warn"><${Icon} name="note" />uncommitted</span>` : html`<span class="tag"><${Icon} name="check" />clean</span>`}</div>
          <div class="wt-meta"><${Divergence} d=${w.origin} label="banked" /><${Divergence} d=${w.upstream} label="upstream" />
            ${w.sessions ? html`<span>${w.sessions} live</span>` : null}
            <a class="btn small ghost" href=${newHref(w.name)}>Start here</a></div>
          ${w.error ? html`<div class="note warn">${w.error}</div>` : null}
        </li>`)}</ul></section>` : null}
    </main>
  </div>`;
}
