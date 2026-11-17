// The session screen: header, tabs (timeline, terminal, changes), the
// pending question as a card with a button per option, pending requests,
// commands whose outcome is not known yet, and the composer (send, steer,
// stop). Opening it marks the session seen.

import { html, useState, useEffect, useRef, Component } from './lib.js';
import { state, useStore, key, loadDetail, dropDetail, markSeen, run, sendPrompt, decide, stale, retryPending, dismissPending } from './store.js';
import { followSession } from './stream.js';
import { newer } from './timeline.js';
import { Icon, StateBadge, TopBar, Empty, ago, duration, useNow } from './ui.js';
import { TerminalTab } from './terminal.js';
import { ChangesTab } from './changes.js';

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

// Item draws one timeline entry. Entries do not change once made (a tool
// run that grows is a new entry), so a row is drawn again only when its
// entry, the harness name or the minute changes: a new event does not
// redraw the history (finding C2-07).
class Item extends Component {
  shouldComponentUpdate(next) {
    return next.it !== this.props.it || next.harness !== this.props.harness || next.now !== this.props.now;
  }

  render({ it, harness, now }) {
    return itemView(it, harness, now);
  }
}

function itemView(it, harness, now) {
  switch (it.kind) {
    case 'tools': return html`<${ToolRun} it=${it} />`;
    case 'prompt': {
      const cut = it.length && it.length > [...it.text].length;
      return html`<li class="tl tl-prompt"><div class="bubble">${it.text}${cut ? '…' : ''}</div><div class="when">${ago(it.at, now)}</div></li>`;
    }
    case 'message': return html`<li class="tl tl-message"><div class="msg">${it.text}</div><div class="when">${harness} · ${ago(it.at, now)}</div></li>`;
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

// PendingActions offers what can be done about a command whose outcome is
// not known: send it again with the same ID (the machine applies it once),
// or forget it.
function PendingActions({ id, outcome }) {
  if (outcome !== 'unknown' && outcome !== 'unsent') return null;
  return html`<span class="pend-actions">
    <button class="btn small" onClick=${() => retryPending(id)}>${outcome === 'unsent' ? 'Send again' : 'Retry'}</button>
    <button class="btn small ghost" onClick=${() => dismissPending(id)}>Dismiss</button></span>`;
}

const outboxLabel = { sending: 'sending…', accepted: 'queued', delivering: 'typing…', delivered: 'delivered', refused: 'refused', failed: 'not sent',
  unknown: 'unknown: the app asks the machine when it can', unsent: 'did not arrive', 'typed-not-submitted': 'not submitted' };

function Outbox({ k }) {
  const st = useStore();
  const list = st.outbox[k] || [];
  return list.map((it) => html`<li class=${'tl tl-prompt pending' + (it.outcome === 'unknown' || it.outcome === 'unsent' ? ' uncertain' : '')} key=${it.id}><div class="bubble">${it.text}</div>
    <div class="when">${it.mode === 'steer' ? 'steer · ' : ''}${outboxLabel[it.outcome] || it.outcome}${it.seen ? ' · a prompt like it is in the session (the receipt decides)' : ''}</div>
    ${st.pending[it.id] ? html`<${PendingActions} id=${it.id} outcome=${it.outcome} />` : null}</li>`);
}

// Pending lists the session's other commands whose outcome is not known.
function Pending({ s }) {
  const st = useStore();
  const list = Object.values(st.pending).filter((p) => p.type !== 'send' && p.type !== 'start' && p.machine === s.machine && p.params.session === s.session && (p.outcome === 'unknown' || p.outcome === 'unsent'));
  if (!list.length) return null;
  return html`<section class="card pcard" aria-label="Unconfirmed">
    ${list.map((p) => html`<div class="pend" key=${p.id}><${Icon} name="question" /><span class="grow"><b>${p.label}</b>: ${p.outcome === 'unsent' ? 'did not arrive' : 'outcome unknown; asking the machine'}</span>
      <${PendingActions} id=${p.id} outcome=${p.outcome} /></div>`)}
  </section>`;
}

// RENDER_STEP entries are drawn at first, and more on request: the DOM
// stays small however long the session is.
const RENDER_STEP = 300;

function Timeline({ s, d, k }) {
  const now = useNow();
  const end = useRef(null);
  const top = useRef(null);
  const seen = useRef({ first: 0, n: 0 });
  const [shown, setShown] = useState(RENDER_STEP);
  const list = d.items;
  // Entries paged in at the top are shown at once (and the view stays at
  // them), within the window.
  const first = d.events.length ? d.events[0].seq : 0;
  let want = shown;
  if (seen.current.first && first < seen.current.first) want = Math.min(list.length, shown + Math.max(0, list.length - seen.current.n));
  useEffect(() => {
    if (want !== shown) setShown(want);
    const older = seen.current.first && first < seen.current.first;
    seen.current = { first, n: list.length };
    if (older && top.current) top.current.scrollIntoView({ block: 'start' });
  });
  const from = Math.max(0, list.length - want);
  useEffect(() => {
    // Stay at the bottom as new entries arrive, unless scrolled up or
    // looking at older history.
    const el = end.current;
    if (!el || !d.live) return;
    const sc = document.scrollingElement;
    if (sc.scrollHeight - sc.scrollTop - sc.clientHeight < 400) el.scrollIntoView({ block: 'end' });
  }, [d.ver, (state.outbox[k] || []).length]);
  let head = null;
  if (from > 0) head = html`<li class="tl-more"><button class="btn ghost small" onClick=${() => setShown(want + RENDER_STEP)}>Show earlier</button></li>`;
  else if (d.more) head = html`<li class="tl-more"><button class="btn ghost small" disabled=${d.loading || d.older} onClick=${() => loadDetail(s.machine, s.session, d.events[0] ? d.events[0].seq : 0)}>Load earlier</button></li>`;
  // The window is detached from the live end (findings D1-13, E1-13):
  // newer events are counted, not shown, until the screen returns to the
  // latest.
  const n = newer(d);
  const latest = d.live ? null : html`<li class="tl-more tl-latest"><span class="hint">${n > 0 ? `Showing older events; ${n} newer ${n === 1 ? 'event is' : 'events are'} not shown.` : 'Showing older events.'}</span>
    <button class="btn small" disabled=${d.loading} onClick=${() => { setShown(RENDER_STEP); loadDetail(s.machine, s.session); }}><${Icon} name="down" />Back to the latest</button></li>`;
  return html`<ol class="timeline" aria-label="Timeline">
    <li ref=${top} class="tl-top-marker" aria-hidden="true"></li>
    ${head}
    ${d.partial ? html`<li class="tl tl-note"><${Icon} name="offline" />Older events are on the machine, which is offline.</li>` : null}
    ${list.slice(from).map((it) => html`<${Item} key=${it.kind + it.seq} it=${it} harness=${s.harness} now=${now} />`)}
    ${latest}
    ${d.live ? html`<${Outbox} k=${k} />` : null}
    <li ref=${end} class="tl-end-marker" aria-hidden="true"></li>
  </ol>`;
}

function QuestionCard({ s }) {
  const q = s.question;
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const off = stale(s);
  const answer = async (params) => {
    setBusy(true);
    await run(s.machine, 'answer', { session: s.session, ref: s.ref, question: q.id, ...params }, { wait: 20 });
    setBusy(false);
    setText('');
  };
  return html`<section class="card qcard" aria-label="Question">
    <h2><${Icon} name="question" />Question</h2>
    <p class="qtext">${q.text}</p>
    ${q.options && q.options.length ? html`<div class="opts">${q.options.map((o, i) => html`<button key=${i} class="btn opt" disabled=${busy || off}
        onClick=${() => answer({ choice: i + 1 })}><span class="num">${i + 1}</span>${o}</button>`)}</div>` : null}
    <form class="qform" onSubmit=${(e) => { e.preventDefault(); if (text.trim()) answer({ text: text.trim() }); }}>
      <input type="text" value=${text} onInput=${(e) => setText(e.currentTarget.value)} placeholder="Or answer in your own words" aria-label="Answer" disabled=${busy || off} />
      <button class="btn primary" type="submit" disabled=${busy || off || !text.trim()}>Answer</button>
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
  const off = stale(r);
  return html`<section class="card rcard" aria-label="Request">
    <h2><${Icon} name="key" />Request <span class=${'tag risk-' + r.risk}>${r.risk} risk</span></h2>
    <p>${what}</p>
    ${r.risk === 'low'
      ? html`<div class="row-actions"><button class="btn" disabled=${busy || off} onClick=${() => act(false)}>Deny</button>
          <button class="btn primary" disabled=${busy || off} onClick=${() => act(true)}>Approve</button></div>`
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
    case 'comment': return `Comment on #${p.number}: ${p.body}`;
  }
  return r.kind;
}

function Composer({ s, compact }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [open, setOpen] = useState(false);
  const ta = useRef(null);
  const off = stale(s);
  const disabled = !s.live || off;
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
    await sendPrompt(s, t, mode);
    setBusy(false);
  };
  const stop = async () => {
    setBusy(true);
    await run(s.machine, 'interrupt', { session: s.session, ref: s.ref }, { wait: 10 });
    setBusy(false);
  };
  if (disabled) {
    return html`<div class="composer off">${off ? html`<${Icon} name="offline" />The machine is offline; nothing can be sent until it is back.` : html`<${Icon} name="ended" />The session has ended.`}</div>`;
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
    await run(s.machine, 'stop', { session: s.session, ref: s.ref }, { wait: 20 });
  };
  return html`<details class="menu">
    <summary class="iconbtn" aria-label="More"><${Icon} name="more" /></summary>
    <div class="menu-list">
      <a href=${`#/p/${encodeURIComponent(s.machine)}/${s.realm}/${s.project}`}>Project ${s.realm}/${s.project}</a>
      <a href=${`#/new?machine=${encodeURIComponent(s.machine)}&ref=${s.ref}&worktree=${s.worktree || 'main'}&harness=${s.harness}`}>New session here</a>
      ${s.live && !stale(s) ? html`<button onClick=${end}>End session</button>` : null}
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
    ${stale(s) ? html`<span class="tag"><${Icon} name="offline" />offline</span>` : null}</span>`;
  const tabs = [['timeline', 'Timeline', 'timeline', base], ['terminal', 'Terminal', 'terminal', base + '/terminal'], ['changes', 'Changes', 'diff', base + '/changes']];
  return html`<div class=${'screen session tab-' + tab}>
    <div class="head">
      <${TopBar} back="#/" title=${html`${s.project}<span class="wt"> · ${s.worktree || 'main'}</span>`} sub=${sub} actions=${html`<${Menu} s=${s} />`} />
      <div class="tabs" role="tablist">${tabs.map(([id, label, icon, href]) => html`<a role="tab" href=${href} aria-selected=${tab === id} class=${tab === id ? 'active' : ''}><${Icon} name=${icon} />${label}</a>`)}</div>
    </div>
    <main class="content">
      ${tab === 'timeline' ? (d && (d.events.length || !d.loading) ? html`<${Timeline} s=${s} d=${d} k=${k} />` : html`<${Empty} icon="working">Loading…<//>`) : null}
      ${tab === 'terminal' ? html`<${TerminalTab} s=${s} />` : null}
      ${tab === 'changes' ? html`<${ChangesTab} s=${s} />` : null}
    </main>
    ${tab === 'timeline' ? html`<div class="dock">
      <${Pending} s=${s} />
      ${pending.map((r) => html`<${RequestCard} key=${r.id} r=${r} />`)}
      ${s.question && s.state === 'blocked' && s.live ? html`<${QuestionCard} key=${s.question.id} s=${s} />` : null}
      <${Composer} s=${s} compact=${!!(s.question && s.state === 'blocked')} />
    </div>` : null}
  </div>`;
}
