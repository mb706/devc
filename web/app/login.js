// Login, and what to do when the hub does not accept this device.

import { html, useState } from './lib.js';
import * as api from './api.js';
import { useStore, set, checkAuth } from './store.js';
import { Icon } from './ui.js';

const problems = {
  nocert: ['No device certificate', 'The hub did not receive a client certificate from this browser. Install the certificate bundle made by "devc hub enroll device <name>" on this device (on Android: Settings → Security → Encryption & credentials → Install a certificate → VPN & app user certificate), then reload and choose it when the browser asks.'],
  revoked: ['Certificate not accepted', 'The hub knows this device certificate but does not accept it: it was revoked or has expired. Enrol the device again with "devc hub enroll device <name>".'],
  offline: ['The hub cannot be reached', 'Either this device is offline, or the hub refused the connection, which it does when the browser offers no client certificate or one it does not accept. If the browser never asked for a certificate, install the bundle made by "devc hub enroll device <name>".'],
};

export function Login({ onDone }) {
  const st = useStore();
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    setErr('');
    try {
      await api.post('/api/login', { password, totp: code.trim() || undefined });
      setPassword('');
      setCode('');
      if (await checkAuth()) onDone();
    } catch (ex) {
      setErr(ex.network ? 'The hub cannot be reached.' : ex.message);
      if (ex.status === 401) set({ totp: st.totp || /code/.test(ex.message) });
    }
    setBusy(false);
  };
  const problem = problems[st.auth];
  return html`<div class="screen login">
    <main class="content narrow">
      <div class="login-head"><img src="/icons/icon.svg" alt="" width="56" height="56" /><h1>devc</h1></div>
      ${problem ? html`<section class="card warn" role="alert"><h2><${Icon} name="lock" />${problem[0]}</h2><p>${problem[1]}</p>
          ${st.authDetail ? html`<p class="dim">The hub said: ${st.authDetail}</p>` : null}
          <button class="btn" onClick=${() => checkAuth().then((ok) => ok && onDone())}><${Icon} name="refresh" />Try again</button></section>`
        : html`<form class="form card" onSubmit=${submit}>
          <h2><${Icon} name="lock" />Log in</h2>
          <label>Password<input type="password" autocomplete="current-password" value=${password} onInput=${(e) => setPassword(e.currentTarget.value)} required /></label>
          ${st.totp ? html`<label>Code from your authenticator app<input type="text" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9 ]*" maxlength="8" value=${code} onInput=${(e) => setCode(e.currentTarget.value)} required /></label>` : null}
          ${err ? html`<p class="field-err" role="alert">${err}</p>` : null}
          <button class="btn primary wide" type="submit" disabled=${busy || !password}>${busy ? 'Logging in…' : 'Log in'}</button>
          <p class="hint">The session is tied to this device's certificate and lasts 30 days without use.</p>
        </form>`}
    </main>
  </div>`;
}
