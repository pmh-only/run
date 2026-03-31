const CACHE = 'run-v1';

// Pre-cache static assets on install
const PRECACHE = [
  '/static/main.js',
  '/static/admin.js',
  'https://cdn.jsdelivr.net/npm/xterm@5.3.0/css/xterm.min.css',
  'https://cdn.jsdelivr.net/npm/xterm@5.3.0/lib/xterm.min.js',
  'https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.8.0/lib/xterm-addon-fit.min.js',
  '/static/icons/icon.svg',
  '/static/icons/icon-96.png',
  '/static/icons/icon-192.png',
  '/static/icons/icon-512.png',
];

self.addEventListener('install', function (event) {
  event.waitUntil(
    caches.open(CACHE).then(function (cache) { return cache.addAll(PRECACHE); })
  );
  self.skipWaiting();
});

self.addEventListener('activate', function (event) {
  event.waitUntil(
    caches.keys().then(function (keys) {
      return Promise.all(
        keys.filter(function (k) { return k !== CACHE; }).map(function (k) { return caches.delete(k); })
      );
    })
  );
  self.clients.claim();
});

self.addEventListener('fetch', function (event) {
  var request = event.request;
  if (request.method !== 'GET') return;

  var url = new URL(request.url);

  // Pass through: auth, API, WebSocket upgrade, healthz
  if (url.pathname.startsWith('/auth/') ||
      url.pathname.startsWith('/api/') ||
      url.pathname === '/terminal' ||
      url.pathname === '/healthz') return;

  // HTML pages (auth-gated): network-first so redirects to /auth/login work
  if (request.headers.get('accept') && request.headers.get('accept').indexOf('text/html') !== -1) {
    event.respondWith(
      fetch(request).catch(function () { return caches.match(request); })
    );
    return;
  }

  // Static assets: cache-first, populate cache on miss
  event.respondWith(
    caches.match(request).then(function (cached) {
      if (cached) return cached;
      return fetch(request).then(function (resp) {
        if (resp && resp.status === 200) {
          var clone = resp.clone();
          caches.open(CACHE).then(function (cache) { cache.put(request, clone); });
        }
        return resp;
      });
    })
  );
});
