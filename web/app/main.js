// The devc web app (work order C12): routing, start-up and the frame.
// Routes live in the hash, so a notification's link (#/s/<machine>/<id>)
// opens the session.

import { html, render, useState, useEffect } from './lib.js';
import { state, useStore, loadCache, checkAuth, set } from './store.js';
import * as stream from './stream.js';
import { Nav, Toasts } from './ui.js';
import { Inbox } from './inbox.js';
import { SessionScreen } from './session.js';
import { Projects, Project } from './projects.js';
import { NewSession } from './newsession.js';
import { Machines } from './machines.js';
import { Login } from './login.js';

const NAME = '[a-z0-9][a-z0-9._-]{0,62}';
const SID = 's[0-9a-v]{20}';

function parse(hash) {
  const raw = (hash || '').replace(/^#/, '') || '/';
  const [path, query = ''] = raw.split('?');
  let m;
  if ((m = new RegExp(`^/s/(${NAME})/(${SID})(?:/(terminal|changes))?$`).exec(path))) {
    return { name: 'session', machine: m[1], sid: m[2], tab: m[3] || 'timeline', query };
  }
  if ((m = new RegExp(`^/p/(${NAME})/(${NAME})/(${NAME})$`).exec(path))) {
    return { name: 'project', machine: m[1], realm: m[2], project: m[3], query };
  }
  const simple = { '/': 'inbox', '/projects': 'projects', '/new': 'new', '/machines': 'machines', '/login': 'login' };
  return { name: simple[path] || 'inbox', query };
}

function useRoute() {
  const [route, setRoute] = useState(parse(location.hash));
  useEffect(() => {
    const f = () => {
      setRoute(parse(location.hash));
      window.scrollTo(0, 0);
    };
    window.addEventListener('hashchange', f);
    return () => window.removeEventListener('hashchange', f);
  }, []);
  return route;
}

let following = false;
async function begin() {
  if (await checkAuth()) {
    following = true;
    await stream.start();
  } else if (state.auth === 'offline') {
    // Keep trying; the cached data stays on screen meanwhile.
    setTimeout(begin, 5000 + Math.random() * 5000);
  }
}

function App() {
  const st = useStore();
  const route = useRoute();
  const needLogin = st.auth === 'login' || st.auth === 'nocert' || st.auth === 'revoked' || (st.auth === 'offline' && !st.savedAt && !st.loadedAt);
  if (route.name === 'login' || needLogin) {
    return html`<${Login} onDone=${() => { if (!following) begin(); else stream.start(); if (route.name === 'login') location.hash = '#/'; }} /><${Toasts} />`;
  }
  let screen;
  switch (route.name) {
    case 'session': screen = html`<${SessionScreen} key=${route.machine + route.sid} machine=${route.machine} sid=${route.sid} tab=${route.tab} />`; break;
    case 'project': screen = html`<${Project} key=${route.machine + route.realm + route.project} ...${route} />`; break;
    case 'projects': screen = html`<${Projects} />`; break;
    case 'new': screen = html`<${NewSession} key=${route.query} query=${route.query} />`; break;
    case 'machines': screen = html`<${Machines} />`; break;
    default: screen = html`<${Inbox} query=${route.query} />`;
  }
  const nav = route.name === 'session' ? 'session' : route.name === 'project' ? 'projects' : route.name;
  return html`<div class=${'app route-' + route.name}>${screen}<${Nav} route=${nav} /><${Toasts} /></div>`;
}

loadCache();
const root = document.getElementById('app');
root.textContent = '';
render(html`<${App} />`, root);
begin();

if ('serviceWorker' in navigator) {
  navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(() => { /* the app works without it */ });
}

// For the development check (web/check.sh): the app started without
// throwing.
document.documentElement.dataset.app = 'started';
set({});
