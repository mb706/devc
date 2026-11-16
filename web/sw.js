// The devc web app's service worker: it caches the app shell, and nothing
// else, so the app opens offline and shows the data it saved. API answers
// are never cached (the hub marks them no-store, and this worker does not
// touch /api/ or /machine at all). The app's own files are fetched from the
// network first (a revalidation, answered 304 when unchanged) and taken
// from the cache only when the hub cannot be reached; vendored files carry
// their version in the path and come from the cache.

const CACHE = 'devc-shell-4';

// SHELL lists every file of the app; a Go test checks that it matches the
// embedded files.
const SHELL = [
  '/',
  '/manifest.webmanifest',
  '/app/app.css',
  '/app/api.js',
  '/app/changes.js',
  '/app/cspshim.js',
  '/app/inbox.js',
  '/app/lib.js',
  '/app/login.js',
  '/app/machines.js',
  '/app/main.js',
  '/app/newsession.js',
  '/app/projects.js',
  '/app/session.js',
  '/app/store.js',
  '/app/stream.js',
  '/app/terminal.js',
  '/app/timeline.js',
  '/app/ui.js',
  '/icons/icon.svg',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  '/icons/maskable-512.png',
  '/vendor/htm-3.1.1/htm.module.js',
  '/vendor/preact-10.29.8/hooks.module.js',
  '/vendor/preact-10.29.8/preact.module.js',
  '/vendor/xterm-6.0.0/xterm.css',
  '/vendor/xterm-6.0.0/xterm.mjs',
];

self.addEventListener('install', (event) => {
  event.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith('devc-shell-') && k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

function shellPath(url) {
  if (url.origin !== self.location.origin) return null;
  const p = url.pathname;
  if (p === '/' || p === '/index.html') return '/';
  return SHELL.includes(p) ? p : null;
}

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  const path = shellPath(url);
  // Anything else (the API, the event stream, terminals) goes to the
  // network untouched.
  if (!path) return;
  if (path.startsWith('/vendor/')) {
    event.respondWith(caches.match(path).then((hit) => hit || fetch(req)));
    return;
  }
  event.respondWith(
    fetch(req)
      .then((res) => {
        if (res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(path, copy));
        }
        return res;
      })
      .catch(() => caches.match(path).then((hit) => hit || Response.error())),
  );
});
