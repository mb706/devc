// The session screen: header, tabs (timeline, terminal, changes), the
// pending question as a card with a button per option, pending requests,
// and the composer (send, steer, stop). Opening it marks the session seen.

import { html, useState, useEffect, useRef } from './lib.js';
import { state, useStore, key, loadDetail, dropDetail, markSeen, run, sendPrompt, decide } from './store.js';
import { followSession } from './stream.js';
import { Icon, StateBadge, TopBar, Empty, ago, duration, useNow } from './ui.js';
import { TerminalTab } from './terminal.js';
import { ChangesTab } from './changes.js';

// items turns events into timeline entries: consecutive tool calls fold
// into one run; states, seen marks and harness details are not shown.
function items(events) {
  const out = [];
  let tools = null;
  for (const e of events) {
    const d = e.data || {};
    let it = null;
    switch (e.type) {
      case 'tool':
        if (!tools) {
          tools = { kind: 'tools', seq: e.seq, list: [] };
          out.push(tools);
        }
        tools.list.push({ name: d.name, summary: d.summary || '', at: e.at || e.ts });
        tools.at = e.at || e.ts;
        continue;
      case 'prompt': it = { kind: 'prompt', text: d.text || '', length: d.length }; break;
      case 'message': it = { kind: 'message', text: d.text || '' }; break;
      case 'note': it = { kind: 'note', text: d.text || '' }; break;
      case 'question': it = { kind: 'question', text: d.text || '', options: d.options || [] }; break;
      case 'answer': it = { kind: 'answer', choice: d.choice, text: d.text || '', via: d.via }; break;
      case 'request': it = { kind: 'request', id: d.id, what: d.summary || d.kind, risk: d.risk, status: d.status }; break;
      case 'session.start': it = { kind: 'start', harness: d.harness, worktree: d.worktree }; break;
      case 'session.end': it = { kind: 'end', reason: d.reason, status: d.status }; break;
      case 'state':
        if (d.state === 'error') it = { kind: 'error', text: d.message || 'The harness reported an error' };
        else continue;
        break;
      default:
        continue;
    }
    it.seq = e.seq;
    it.at = e.at || e.ts;
    out.push(it);
    tools = null;
  }
  return out;
}

function ToolRun({ it }) {
  const [open, setOpen] = useState(false);
  const last = it.list[it.list.length - 1];
  const n = it.list.length;
  return html`<li class="tl tl-tools">
    <button class="toolrun" aria-expanded=${open} onClick=${() => setOpen(!open)}>
      <${Icon} name="tool" />
      <span class="tr-n">${n === 1 ? '1 tool call' : `${n} tool calls`}</span>
      ${!open ? html`<span class="tr-last">${last.name} ${last.summary}</span>` : null}
      <${Icon} name=${open ? 'up' : 'down'} cls="chev" />
    </button>
    ${open ? html`<ol class="toollist">${it.list.map((t, i) => html`<li key=${i}><b>${t.name}</b> <span>${t.summary}</span></li>`)}</ol>` : null}
  </li>`;
}

function Item({ it, s, now }) {
  switch (it.kind) {
    case 'tools': return html`<${ToolRun} it=${it} />`;
    case 'prompt': {
      const cut = it.length && it.length > [...it.text].length;
      return html`<li class="tl tl-prompt"><div class="bubble">${it.text}${cut ? '…' : ''}</div><div class="when">${ago(it.at, now)}</div></li>`;
    }
    case 'message': return html`<li class="tl tl-message"><div class="msg">${it.text}</div><div class="when">${s.harness} · ${ago(it.at, now)}</div></li>`;
    case 'note': return html`<li class="tl tl-note"><${Icon} name="note" />${it.text}</li>`;
    case 'question': return html`<li class="tl tl-question"><${Icon} name="question" /><div><b>Asked:</b> ${it.text}
      ${it.options.length ? html`<ol class="opts">${it.options.map((o, i) => html`<li key=${i}>${o}</li>`)}</ol>` : null}</div></li>`;
    case 'answer': return html`<li class="tl tl-note"><${Icon} name="check" />Answered${it.choice ? ` with option ${it.choice}` : ''}${it.text ? `: ${it.text}` : ''}</li>`;
    case 'request': return html`<li class="tl tl-request"><${Icon} name="key" /><span><b>Request</b> ${it.what}: ${it.status} <span class="tag">${it.risk} risk</span></span></li>`;
    case 'start': return html`<li class="tl tl-note"><${Icon} name="play" />Started ${it.harness} in ${it.worktree || 'main'} · ${ago(it.at, now)}</li>`;
    case 'end': return html`<li class="tl tl-end"><${Icon} name="ended" />Session ended (${it.reason}${it.status !== undefined && it.status !== null ? `, status ${it.status}` : ''}) · ${ago(it.at, now)}</li>`;
    case 'error': return html`<li class="tl tl-error"><${Icon} name="failed" />${it.text}</li>`;
  }
  return null;
}

function Outbox({ k }) {
  const st = useStore();
  const list = st.outbox[k] || [];
  const label = { sending: 'sending…', accepted: 'queued', delivering: 'typing…', delivered: 'delivered', refused: 'refused', failed: 'not sent', unknown: 'unconfirmed', 'typed-not-submitted': 'not submitted' };
  return list.map((it) => html`<li class="tl tl-prompt pending" key=${it.id}><div class="bubble">${it.text}</div>
    <div class="when">${it.mode === 'steer' ? 'steer · ' : ''}${label[it.outcome] || it.outcome}</div></li>`);
}

function Timeline({ s, d, k }) {
  const now = useNow();
  const end = useRef(null);
  const list = items(d.events);
  const count = d.events.length;
  useEffect(() => {
    // Stay at the bottom as new entries arrive, unless scrolled up.
    const el = end.current;
    if (!el) return;
    const sc = document.scrollingElement;
    if (sc.scrollHeight - sc.scrollTop - sc.clientHeight < 400) el.scrollIntoView({ block: 'end' });
  }, [count, (state.outbox[k] || []).length]);
  return html`<ol class="timeline" aria-label="Timeline">
    ${d.more ? html`<li class="tl-more"><button class="btn ghost small" disabled=${d.loading} onClick=${() => loadDetail(s.machine, s.session, d.events[0] ? d.events[0].seq : 0)}>Load earlier</button></li>` : null}
    ${d.partial ? html`<li class="tl tl-note"><${Icon} name="offline" />Older events are on the machine, which is offline.</li>` : null}
    ${list.map((it) => html`<${Item} key=${it.kind + it.seq} it=${it} s=${s} now=${now} />`)}
    <${Outbox} k=${k} />
    <li ref=${end} class="tl-end-marker" aria-hidden="true"></li>
  </ol>`;
}

function QuestionCard({ s }) {
  const q = s.question;
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const answer = async (params) => {
    setBusy(true);
    await run(s.machine, 'answer', { session: s.session, question: q.id, ...params }, { wait: 20 });
    setBusy(false);
    setText('');
  };
  return html`<section class="card qcard" aria-label="Question">
    <h2><${Icon} name="question" />Question</h2>
    <p class="qtext">${q.text}</p>
    ${q.options && q.options.length ? html`<div class="opts">${q.options.map((o, i) => html`<button key=${i} class="btn opt" disabled=${busy || s.stale}
        onClick=${() => answer({ choice: i + 1 })}><span class="num">${i + 1}</span>${o}</button>`)}</div>` : null}
    <form class="qform" onSubmit=${(e) => { e.preventDefault(); if (text.trim()) answer({ text: text.trim() }); }}>
      <input type="text" value=${text} onInput=${(e) => setText(e.currentTarget.value)} placeholder="Or answer in your own words" aria-label="Answer" disabled=${busy || s.stale} />
      <button class="btn primary" type="submit" disabled=${busy || s.stale || !text.trim()}>Answer</button>
    </form>
  </section>`;
}

function RequestCard({ r }) {
  const [busy, setBusy] = useState(false);
  const act = async (approve) => {
    setBusy(true);
    await decide(r, approve);
    setBusy(false);
  };
  const what = describeRequest(r);
  return html`<section class="card rcard" aria-label="Request">
    <h2><${Icon} name="key" />Request <span class=${'tag risk-' + r.risk}>${r.risk} risk</span></h2>
    <p>${what}</p>
    ${r.risk === 'low'
      ? html`<div class="row-actions"><button class="btn" disabled=${busy || r.stale} onClick=${() => act(false)}>Deny</button>
          <button class="btn primary" disabled=${busy || r.stale} onClick=${() => act(true)}>Approve</button></div>`
      : html`<p class="hint">High-risk requests are approved on the machine:</p><code class="cmd">devc requests approve ${r.id}</code>`}
  </section>`;
}

export function describeRequest(r) {
  const p = r.params || {};
  switch (r.kind) {
    case 'seal': return `Seal ${p.path}`;
    case 'savepoint': return `Savepoint${p.name ? ' ' + p.name : ''}`;
    case 'note': return `Note: ${p.text}`;
    case 'package': return `Install ${(p.packages || []).join(', ')} with ${p.manager}`;
    case 'net': return `Network access to ${p.target}${p.for ? ' for ' + p.for : ''}`;
    case 'grant': return `Grant ${p.grant}${p.name ? ' ' + p.name : ''}`;
    case 'pr': return `Pull request "${p.title}"${p.branch ? ' from ' + p.branch : ''}`;
  }
  return r.kind;
}

function Composer({ s, compact }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [open, setOpen] = useState(false);
  const ta = useRef(null);
  const disabled = !s.live || s.stale;
  const grow = () => {
    const el = ta.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = Math.min(el.scrollHeight, 180) + 'px';
  };
  const send = async (mode) => {
    const t = text.trim();
    if (!t) return;
    setBusy(true);
    setText('');
    requestAnimationFrame(grow);
    await sendPrompt(s.machine, s.session, t, mode);
    setBusy(false);
  };
  const stop = async () => {
    setBusy(true);
    await run(s.machine, 'interrupt', { session: s.session }, { wait: 10 });
    setBusy(false);
  };
  if (disabled) {
    return html`<div class="composer off">${s.stale ? html`<${Icon} name="offline" />The machine is offline; nothing can be sent until it is back.` : html`<${Icon} name="ended" />The session has ended.`}</div>`;
  }
  if (compact && !open && !text) {
    // A question waits: its card comes first; the composer folds away.
    return html`<div class="composer mini">
      <button type="button" class="btn danger" onClick=${stop} disabled=${busy} title="Interrupt (Escape)"><${Icon} name="stop" />Stop</button>
      <button type="button" class="btn ghost grow" onClick=${() => { setOpen(true); requestAnimationFrame(() => ta.current && ta.current.focus()); }}>Write a message instead…</button>
    </div>`;
  }
  return html`<form class="composer" onSubmit=${(e) => { e.preventDefault(); send('queue'); }}>
    <textarea ref=${ta} rows="1" value=${text} placeholder=${s.state === 'working' ? 'Queue a message, or steer now' : 'Message ' + (s.harness || 'the agent')}
      aria-label="Message" onInput=${(e) => { setText(e.currentTarget.value); grow(); }}
      onKeyDown=${(e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); send('queue'); } }}></textarea>
    <div class="cbtns">
      <button type="button" class="btn danger" onClick=${stop} disabled=${busy} title="Interrupt the current turn (Escape)"><${Icon} name="stop" />Stop</button>
      <button type="button" class="btn" onClick=${() => send('steer')} disabled=${busy || !text.trim()} title="Type it now, even while the agent works"><${Icon} name="steer" />Steer</button>
      <button type="submit" class="btn primary" disabled=${busy || !text.trim()} title="Queue it until the agent is ready"><${Icon} name="send" />Send</button>
    </div>
  </form>`;
}

function Menu({ s }) {
  const end = async () => {
    if (!confirm(`End the session ${s.project} · ${s.worktree || 'main'}? The harness is stopped.`)) return;
    await run(s.machine, 'stop', { session: s.session }, { wait: 20 });
  };
  return html`<details class="menu">
    <summary class="iconbtn" aria-label="More"><${Icon} name="more" /></summary>
    <div class="menu-list">
      <a href=${`#/p/${encodeURIComponent(s.machine)}/${s.realm}/${s.project}`}>Project ${s.realm}/${s.project}</a>
      <a href=${`#/new?machine=${encodeURIComponent(s.machine)}&ref=${s.ref}&worktree=${s.worktree || 'main'}&harness=${s.harness}`}>New session here</a>
      ${s.live && !s.stale ? html`<button onClick=${end}>End session</button>` : null}
    </div>
  </details>`;
}

export function SessionScreen({ machine, sid, tab }) {
  const st = useStore();
  const now = useNow(10000);
  const k = key(machine, sid);
  const s = st.sessions[k];
  const d = st.details[k];
  useEffect(() => {
    loadDetail(machine, sid);
    const unfollow = followSession(machine, sid);
    return () => {
      unfollow();
      dropDetail(machine, sid);
    };
  }, [machine, sid]);
  // A session that is not on the hub yet (just started) is asked for again.
  useEffect(() => {
    if (!d || d.status !== 404) return;
    const t = setTimeout(() => loadDetail(machine, sid), 2000);
    return () => clearTimeout(t);
  }, [d && d.status, d && d.loading]);
  useEffect(() => {
    if (s && document.visibilityState === 'visible') markSeen(s);
  });
  const base = `#/s/${encodeURIComponent(machine)}/${sid}`;
  if (!s) {
    return html`<div class="screen"><${TopBar} title="Session" back="#/" />
      <main class="content">${d && d.error ? html`<${Empty} icon="question">${d.status === 404 ? 'This session is not on the hub (yet). Waiting for it…' : d.error}<//>` : html`<${Empty} icon="working">Loading…<//>`}</main></div>`;
  }
  const pending = Object.values(st.requests).filter((r) => r.machine === machine && r.session === sid);
  const elapsed = s.live ? duration(s.started, now) : s.ended ? duration(s.started, s.ended) : '';
  const sub = html`<span class="hdr-sub"><${StateBadge} s=${s} /><span>${s.harness} · ${machine}</span>${elapsed ? html`<span class="elapsed">${s.live ? '' : 'ran '}${elapsed}</span>` : null}
    ${s.stale ? html`<span class="tag"><${Icon} name="offline" />offline</span>` : null}</span>`;
  const tabs = [['timeline', 'Timeline', 'timeline', base], ['terminal', 'Terminal', 'terminal', base + '/terminal'], ['changes', 'Changes', 'diff', base + '/changes']];
  return html`<div class=${'screen session tab-' + tab}>
    <div class="head">
      <${TopBar} back="#/" title=${html`${s.project}<span class="wt"> · ${s.worktree || 'main'}</span>`} sub=${sub} actions=${html`<${Menu} s=${s} />`} />
      <div class="tabs" role="tablist">${tabs.map(([id, label, icon, href]) => html`<a role="tab" href=${href} aria-selected=${tab === id} class=${tab === id ? 'active' : ''}><${Icon} name=${icon} />${label}</a>`)}</div>
    </div>
    <main class="content">
      ${tab === 'timeline' ? (d && d.events ? html`<${Timeline} s=${s} d=${d} k=${k} />` : html`<${Empty} icon="working">Loading…<//>`) : null}
      ${tab === 'terminal' ? html`<${TerminalTab} s=${s} />` : null}
      ${tab === 'changes' ? html`<${ChangesTab} s=${s} />` : null}
    </main>
    ${tab === 'timeline' ? html`<div class="dock">
      ${pending.map((r) => html`<${RequestCard} key=${r.id} r=${r} />`)}
      ${s.question && s.state === 'blocked' && s.live ? html`<${QuestionCard} key=${s.question.id} s=${s} />` : null}
      <${Composer} s=${s} compact=${!!(s.question && s.state === 'blocked')} />
    </div>` : null}
  </div>`;
}
