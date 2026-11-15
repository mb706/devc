// The terminal tab: the session's tmux window in xterm.js. It observes by
// default: read-only, at the pane's own size (the phone never resizes the
// pane; the font shrinks to fit or the view scrolls sideways). When the hub
// allows it, the user can take control; a key bar then offers the keys a
// phone keyboard lacks.

import { html, useState, useEffect, useRef } from './lib.js';
import { useStore, stale } from './store.js';
import { Icon, Empty } from './ui.js';
import { installCspShim } from './cspshim.js';

const XTERM = '/vendor/xterm-6.0.0/xterm.mjs';
const XTERM_CSS = '/vendor/xterm-6.0.0/xterm.css';
const FONT = 'ui-monospace, SFMono-Regular, Menlo, Consolas, "DejaVu Sans Mono", "Liberation Mono", monospace';

let xtermModule = null;
async function loadXterm() {
  if (xtermModule) return xtermModule;
  installCspShim();
  if (!document.querySelector(`link[href="${XTERM_CSS}"]`)) {
    const l = document.createElement('link');
    l.rel = 'stylesheet';
    l.href = XTERM_CSS;
    document.head.appendChild(l);
  }
  xtermModule = await import(XTERM);
  return xtermModule;
}

const theme = {
  background: '#0f141a', foreground: '#d8dee6', cursor: '#d8dee6', cursorAccent: '#0f141a', selectionBackground: '#3a4656',
};

// fitSize is the font size at which cols columns fill width.
function fitSize(width, cols) {
  return Math.max(5, Math.min(15, Math.floor((width / (cols * 0.602)) * 10) / 10));
}

const KEYS = [
  ['Esc', '\x1b'], ['Tab', '\t'], ['Ctrl', null], ['↑', 'A'], ['↓', 'B'], ['←', 'D'], ['→', 'C'], ['Enter', '\r'],
];

export function TerminalTab({ s }) {
  const st = useStore();
  const host = useRef(null);
  const term = useRef(null);
  const ws = useRef(null);
  const ctrl = useRef(false);
  const [mode, setMode] = useState('observe');
  const [status, setStatus] = useState('connecting');
  const [reason, setReason] = useState('');
  const [size, setSize] = useState(null);
  const [zoom, setZoom] = useState(false);
  const [ctrlOn, setCtrlOn] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const allowControl = st.me && st.me.terminal_control;
  const usable = s.live && !stale(s);

  const send = (data) => {
    const w = ws.current;
    if (w && w.readyState === WebSocket.OPEN && mode === 'control') w.send(JSON.stringify({ type: 'input', data }));
  };

  useEffect(() => {
    if (!usable) return undefined;
    let closed = false;
    let t = null;
    setStatus('connecting');
    setReason('');
    (async () => {
      let mod;
      try {
        mod = await loadXterm();
      } catch (e) {
        setStatus('ended');
        setReason('The terminal could not be loaded.');
        return;
      }
      if (closed) return;
      t = new mod.Terminal({
        disableStdin: mode !== 'control', cursorBlink: false, fontFamily: FONT, fontSize: 11, scrollback: 0,
        theme, allowProposedApi: false, convertEol: false, cols: 80, rows: 24,
      });
      term.current = t;
      t.open(host.current);
      if (mode === 'control') {
        t.onData((d) => {
          if (ctrl.current && d.length === 1) {
            const c = d.toUpperCase().charCodeAt(0);
            if (c >= 64 && c <= 95) d = String.fromCharCode(c - 64);
            ctrl.current = false;
            setCtrlOn(false);
          }
          send(d);
        });
      }
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const w = new WebSocket(`${proto}//${location.host}/api/terminal/${encodeURIComponent(s.machine)}/${s.session}?mode=${mode}`);
      w.binaryType = 'arraybuffer';
      ws.current = w;
      // The hub counts a stream as hidden until told otherwise: the state
      // goes out when the socket opens, after the terminal opened, and on
      // every change (finding C2-17).
      const visible = () => {
        if (w.readyState === WebSocket.OPEN) w.send(JSON.stringify({ type: 'visible', visible: document.visibilityState === 'visible' }));
      };
      document.addEventListener('visibilitychange', visible);
      w.onopen = visible;
      w.onmessage = (ev) => {
        if (typeof ev.data !== 'string') {
          t.write(new Uint8Array(ev.data));
          return;
        }
        let m;
        try {
          m = JSON.parse(ev.data);
        } catch (e) {
          return;
        }
        if (m.type === 'open' || m.type === 'size') {
          t.resize(m.cols, m.rows);
          setSize({ cols: m.cols, rows: m.rows });
          if (m.type === 'open') {
            setStatus('open');
            visible();
          }
        } else if (m.type === 'end') {
          setReason(m.reason || '');
        }
      };
      w.onclose = () => {
        document.removeEventListener('visibilitychange', visible);
        if (!closed) setStatus('ended');
      };
      w.onerror = () => {
        if (!closed) setReason((r) => r || 'The connection to the hub failed.');
      };
    })();
    return () => {
      closed = true;
      if (ws.current) ws.current.close();
      ws.current = null;
      if (t) t.dispose();
      term.current = null;
    };
  }, [s.machine, s.session, mode, usable, attempt]);

  // The font follows the pane's size and the screen's width.
  useEffect(() => {
    const t = term.current;
    if (!t || !size || !host.current) return undefined;
    const apply = () => {
      const width = host.current.parentElement.clientWidth - 2;
      t.options.fontSize = zoom ? 13 : fitSize(width, size.cols);
    };
    apply();
    window.addEventListener('resize', apply);
    return () => window.removeEventListener('resize', apply);
  }, [size, zoom, status]);

  if (!usable) {
    return html`<${Empty} icon="terminal">${stale(s) ? 'The machine is offline.' : 'The session has ended; its terminal is gone.'}<//>`;
  }
  const takeControl = () => {
    if (!confirm('Take control of this terminal? What you type goes straight to the session.')) return;
    setMode('control');
  };
  const key = (seq) => {
    const t = term.current;
    if (seq === null) {
      ctrl.current = !ctrl.current;
      setCtrlOn(ctrl.current);
      if (t) t.focus();
      return;
    }
    if (seq.length === 1 && seq >= 'A' && seq <= 'D') {
      const app = t && t.modes && t.modes.applicationCursorKeysMode;
      seq = (app ? '\x1bO' : '\x1b[') + seq;
    }
    send(seq);
    if (t) t.focus();
  };
  const label = mode === 'control' ? 'In control' : 'Observing, read-only';
  return html`<div class="term">
    <div class="term-bar">
      <span class=${'term-state' + (mode === 'control' ? ' control' : '')}><${Icon} name=${mode === 'control' ? 'key' : 'terminal'} />${label}${size ? html`<span class="dim"> · ${size.cols}×${size.rows}</span>` : null}</span>
      <span class="term-btns">
        <button class="btn small" onClick=${() => setZoom(!zoom)} aria-pressed=${zoom}>${zoom ? 'Fit width' : 'Zoom'}</button>
        ${allowControl ? (mode === 'control'
          ? html`<button class="btn small" onClick=${() => setMode('observe')}>Release</button>`
          : html`<button class="btn small" onClick=${takeControl}>Take control</button>`) : null}
      </span>
    </div>
    ${status !== 'open' ? html`<div class=${'term-msg' + (status === 'ended' ? ' ended' : '')} role="status">
      ${status === 'connecting' ? 'Connecting to the terminal…' : html`<span>${reason || 'The terminal closed.'}</span>
        <button class="btn small" onClick=${() => setAttempt(attempt + 1)}><${Icon} name="refresh" />Reconnect</button>`}
    </div>` : null}
    <div class=${'term-view' + (zoom ? ' zoom' : '')}><div ref=${host} class="term-host"></div></div>
    ${mode === 'control' && status === 'open' ? html`<div class="keybar" role="toolbar" aria-label="Keys">
      ${KEYS.map(([label, seq]) => html`<button class=${'key' + (seq === null && ctrlOn ? ' on' : '')} aria-pressed=${seq === null ? ctrlOn : null}
        onClick=${() => key(seq)}>${label}</button>`)}
    </div>` : null}
  </div>`;
}
