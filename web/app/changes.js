// The changes tab: what the session's worktree changed, computed on the
// machine's relay: the uncommitted work of the last snapshot, or the branch
// against the upstream's default branch. A diffstat, then the unified diff
// file by file.

import { html, useState, useEffect } from './lib.js';
import * as api from './api.js';
import { Icon, Empty, ago, useNow } from './ui.js';

// files splits a unified diff at its "diff --git" lines.
function splitDiff(text) {
  const out = [];
  let cur = null;
  for (const line of text.split('\n')) {
    if (line.startsWith('diff --git ')) {
      cur = { head: line, lines: [] };
      out.push(cur);
      continue;
    }
    if (!cur) {
      cur = { head: '', lines: [] };
      out.push(cur);
    }
    cur.lines.push(line);
  }
  return out;
}

function lineClass(l) {
  if (l.startsWith('+++') || l.startsWith('---')) return 'dl-file';
  if (l.startsWith('+')) return 'dl-add';
  if (l.startsWith('-')) return 'dl-del';
  if (l.startsWith('@@')) return 'dl-hunk';
  if (l.startsWith('\\')) return 'dl-meta';
  return '';
}

function fileName(head) {
  const m = / b\/(.*)$/.exec(head);
  return m ? m[1] : head.replace(/^diff --git /, '');
}

function Bar({ f }) {
  const total = f.added + f.deleted;
  const n = Math.min(total, 10);
  const add = total ? Math.round((f.added / total) * n) : 0;
  return html`<span class="bar" aria-hidden="true">${[...Array(10)].map((_, i) => html`<i class=${i < add ? 'a' : i < n ? 'd' : ''}></i>`)}</span>`;
}

export function ChangesTab({ s }) {
  const [scope, setScope] = useState('snapshot');
  const [res, setRes] = useState(null);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(false);
  const [tick, setTick] = useState(0);
  const now = useNow();
  useEffect(() => {
    let gone = false;
    setLoading(true);
    setErr('');
    api.get(`/api/changes/${encodeURIComponent(s.machine)}/${s.session}?scope=${scope}`)
      .then((r) => { if (!gone) setRes(r); })
      .catch((e) => { if (!gone) { setErr(e.message); setRes(null); } })
      .finally(() => { if (!gone) setLoading(false); });
    return () => { gone = true; };
  }, [s.machine, s.session, scope, tick]);
  const scopes = [['snapshot', 'Uncommitted'], ['branch', 'Branch vs upstream']];
  return html`<div class="changes">
    <div class="seg" role="radiogroup" aria-label="Compare">
      ${scopes.map(([id, label]) => html`<button role="radio" aria-checked=${scope === id} class=${scope === id ? 'on' : ''} onClick=${() => setScope(id)}>${label}</button>`)}
      <button class="iconbtn" onClick=${() => setTick(tick + 1)} aria-label="Refresh" disabled=${loading}><${Icon} name="refresh" /></button>
    </div>
    ${err ? html`<${Empty} icon="diff">${s.stale ? 'The machine is offline.' : err}<//>` : null}
    ${!res && !err ? html`<${Empty} icon="diff">Computing the changes…<//>` : null}
    ${res ? html`<div class=${loading ? 'loading' : ''}>
      <div class="cmp">
        ${res.base ? html`<span><${Icon} name="branch" />${res.base_label}</span><${Icon} name="right" /><span>${res.head_label}</span>` : null}
        ${res.at ? html`<span class="dim">snapshot ${ago(res.at, now)}</span>` : null}
      </div>
      ${res.note ? html`<p class=${'note' + (res.stale ? ' warn' : '')}>${res.note}</p>` : null}
      ${res.base ? html`<p class="stat"><b>${res.files.length + (res.files_more || 0)}</b> file${res.files.length === 1 ? '' : 's'} changed, <span class="add">+${res.added}</span> <span class="del">−${res.deleted}</span></p>` : null}
      ${res.files.length ? html`<ul class="files">${res.files.map((f) => html`<li key=${f.path}>
        <span class="path">${f.old_path ? html`<span class="dim">${f.old_path} → </span>` : null}${f.path}</span>
        ${f.binary ? html`<span class="dim">binary</span>` : html`<span class="nums"><span class="add">+${f.added}</span> <span class="del">−${f.deleted}</span></span>`}
        <${Bar} f=${f} /></li>`)}
        ${res.files_more ? html`<li class="dim">and ${res.files_more} more</li>` : null}</ul>` : null}
      ${res.diff ? splitDiff(res.diff).filter((f) => f.head || f.lines.some((l) => l)).map((f, i) => html`<details class="dfile" open=${i < 20} key=${i}>
        <summary>${fileName(f.head) || 'diff'}</summary>
        <pre class="diff">${f.lines.map((l) => html`<span class=${lineClass(l)}>${l || ' '}</span>`)}</pre>
      </details>`) : null}
      ${res.truncated ? html`<p class="note warn">The diff is longer than the hub shows; the rest is cut off.</p>` : null}
    </div>` : null}
  </div>`;
}
