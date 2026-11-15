// The inbox: every session that needs attention or runs, across machines,
// in the hub's sections (needs you, failed, finished, working, idle), with
// filters for machine, realm and project.

import { html } from './lib.js';
import { useStore, stale } from './store.js';
import { Icon, StateBadge, TopBar, Empty, sectionInfo, ago, useNow } from './ui.js';

const ORDER = ['needs-you', 'failed', 'finished', 'working', 'idle'];

// line is the one line a row shows: the question, a request, or the last
// message.
function line(s) {
  if (s.state === 'blocked' && s.question) return s.question.text;
  if (s.requests > 0) return s.requests === 1 ? '1 request waits for a decision' : `${s.requests} requests wait for a decision`;
  if (!s.live && s.end_reason) return s.message ? s.message : `Ended (${s.end_reason})`;
  return s.message || '';
}

export function SessionRow({ s, now, showRealm = true }) {
  const where = [s.harness || 'harness?', s.machine].join(' · ');
  const name = `${showRealm ? s.realm + '/' : ''}${s.project}`;
  const off = stale(s);
  return html`<li><a class=${'row' + (off ? ' stale' : '')} href=${`#/s/${encodeURIComponent(s.machine)}/${s.session}`}>
    <div class="row-top">
      <span class="row-title">${name}<span class="wt"> · ${s.worktree || 'main'}</span></span>
      <span class="row-time">${ago(s.last_event, now)}</span>
    </div>
    <div class="row-meta"><${StateBadge} s=${s} /><span class="where">${where}</span>
      ${off ? html`<span class="tag"><${Icon} name="offline" />machine offline</span>` : null}</div>
    ${line(s) ? html`<div class="row-text">${line(s)}</div>` : null}
  </a></li>`;
}

function uniq(list) {
  return [...new Set(list.filter(Boolean))].sort();
}

function setFilter(query, name, value) {
  const q = new URLSearchParams(query);
  if (value) q.set(name, value);
  else q.delete(name);
  if (name === 'realm') q.delete('project');
  const s = q.toString();
  location.hash = '#/' + (s ? '?' + s : '');
}

export function Inbox({ query }) {
  const st = useStore();
  const now = useNow();
  const q = new URLSearchParams(query);
  const f = { machine: q.get('machine') || '', realm: q.get('realm') || '', project: q.get('project') || '' };
  const all = Object.values(st.sessions).filter((s) => s.section);
  const shown = all.filter((s) => (!f.machine || s.machine === f.machine) && (!f.realm || s.realm === f.realm) && (!f.project || s.project === f.project));
  const by = {};
  for (const s of shown) (by[s.section] = by[s.section] || []).push(s);
  for (const k of Object.keys(by)) by[k].sort((a, b) => Date.parse(b.last_event) - Date.parse(a.last_event));
  const needs = (by['needs-you'] || []).length;
  const machines = uniq([...Object.keys(st.machines), ...all.map((s) => s.machine)]);
  const realms = uniq(all.map((s) => s.realm));
  const projects = uniq(all.filter((s) => !f.realm || s.realm === f.realm).map((s) => s.project));
  const filtered = f.machine || f.realm || f.project;
  const select = (name, label, options) => html`<label class="filter">
    <span class="sr">${label}</span>
    <select value=${f[name]} onChange=${(e) => setFilter(query, name, e.currentTarget.value)} aria-label=${label + ' filter'}>
      <option value="">${label}</option>
      ${options.map((o) => html`<option value=${o}>${o}</option>`)}
    </select></label>`;
  const counter = needs > 0
    ? html`<span class="needs-count" role="status"><${Icon} name="hand" />${needs} need${needs === 1 ? 's' : ''} you</span>`
    : html`<span class="needs-count calm" role="status"><${Icon} name="check" />Nothing needs you</span>`;
  return html`<div class="screen">
    <${TopBar} title="Inbox" sub=${counter} />
    <main class="content">
      <div class="filters" role="group" aria-label="Filters">
        ${select('machine', 'Machine', machines)}${select('realm', 'Realm', realms)}${select('project', 'Project', projects)}
        ${filtered ? html`<a class="btn small ghost" href="#/">Clear</a>` : null}
      </div>
      ${shown.length === 0
        ? html`<${Empty} icon="inbox">${filtered ? 'No session matches the filters.' : st.loadedAt || st.savedAt ? 'No sessions. Start one with New.' : 'Loading…'}<//>`
        : ORDER.filter((sec) => by[sec]).map((sec) => html`<section class="section" key=${sec}>
            <h2 class=${sectionInfo[sec].cls}><${Icon} name=${sectionInfo[sec].icon} />${sectionInfo[sec].label}<span class="n">${by[sec].length}</span></h2>
            <ul class="rows">${by[sec].map((s) => html`<${SessionRow} key=${s.machine + '/' + s.session} s=${s} now=${now} showRealm=${!f.realm} />`)}</ul>
          </section>`)}
    </main>
  </div>`;
}
