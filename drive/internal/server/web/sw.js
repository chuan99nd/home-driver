// Service worker: caches the app shell ONLY, so Drive opens like an app.
// It never caches or even touches user files, API responses, share links,
// sign-in or the curator: those requests are not intercepted at all.
const CACHE = 'drive-shell-v2';
const SHELL = [
  '/', '/static/tokens.css', '/static/app.css', '/static/app.js', '/static/ui.js', '/static/i18n.js',
  '/static/icons.svg', '/static/icon.svg', '/static/icon-192.png', '/static/manifest.webmanifest',
];

self.addEventListener('install', (event) => {
  event.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  const url = new URL(req.url);
  const isShell = url.origin === self.location.origin && req.method === 'GET' &&
    ((url.pathname === '/' && !url.search) || url.pathname.startsWith('/static/'));
  if (!isShell) return; // the network handles it, untouched

  // Network first, so a new server version is picked up immediately; the
  // cache is only the fallback when the server cannot be reached.
  event.respondWith(
    fetch(req).then((resp) => {
      if (resp.ok) {
        const copy = resp.clone();
        caches.open(CACHE).then((c) => c.put(req, copy));
      }
      return resp;
    }).catch(() => caches.match(req, { ignoreSearch: false }).then((hit) => hit || Response.error())),
  );
});
