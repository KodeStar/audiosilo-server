// AudioSilo admin-console service worker. Its only jobs are to make /admin an
// installable PWA (a registered SW with a fetch handler is part of the install
// criteria) and to keep the shell usable offline. It is served from the site
// root (/sw.js) so its scope ("/") covers /admin.
//
// It deliberately stays out of the way: the JSON API and the web player at /web
// (which ships its own SW) are never intercepted, and admin navigations are
// network-first so the console always reflects live server state.
const VERSION = "audiosilo-admin-v1";
const SHELL = [
  "/admin",
  "/assets/style.css",
  "/assets/admin.js",
  "/assets/i18n.js",
  "/assets/i18n-dict.js",
  "/assets/favicon.svg",
  "/assets/icon-192.png",
  "/assets/icon-512.png",
  "/manifest.webmanifest",
];

self.addEventListener("install", (e) => {
  e.waitUntil(
    caches
      .open(VERSION)
      .then((c) => c.addAll(SHELL))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== VERSION).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (e) => {
  const req = e.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  // Leave the API and the web player (its own SW) alone.
  if (url.pathname.startsWith("/api/") || url.pathname.startsWith("/web/")) return;

  // Admin navigations: network-first, fall back to the cached shell offline.
  if (req.mode === "navigate") {
    if (url.pathname === "/admin" || url.pathname.startsWith("/admin/")) {
      e.respondWith(fetch(req).catch(() => caches.match("/admin")));
    }
    return; // other navigations pass straight through to the network
  }

  // Static assets: stale-while-revalidate.
  e.respondWith(
    caches.match(req).then((cached) => {
      const net = fetch(req)
        .then((res) => {
          if (res && res.ok) {
            const copy = res.clone();
            caches.open(VERSION).then((c) => c.put(req, copy));
          }
          return res;
        })
        .catch(() => cached);
      return cached || net;
    }),
  );
});
