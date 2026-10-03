// AudioSilo admin-console service worker. Its only jobs are to make /admin an
// installable PWA (a registered SW with a fetch handler is part of the install
// criteria) and to keep the shell usable offline. It is served from the site
// root (/sw.js) so its scope ("/") covers /admin.
//
// It deliberately stays out of the way: the JSON API and the web player at /web
// (which ships its own SW) are never intercepted, and admin navigations are
// network-first so the console always reflects live server state.
//
// Every online admin navigation refreshes the cached shell ("/admin", the
// console's index.html), so it always matches the server's current build. The
// console's hashed /admin/assets/* files aren't listed (their names change every
// build); the stale-while-revalidate branch below caches them on first use, so
// the console works offline after one online visit. Bump VERSION whenever the
// shell list changes shape so old caches are dropped.
const VERSION = "audiosilo-admin-v3";
const SHELL = [
  "/admin",
  "/admin/theme-init.js",
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

  // Admin navigations: network-first, refreshing the cached shell on success and
  // falling back to it offline. Every route under /admin serves the same page.
  if (req.mode === "navigate") {
    if (url.pathname === "/admin" || url.pathname.startsWith("/admin/")) {
      e.respondWith(
        fetch(req)
          .then((res) => {
            // Only an HTML page is a shell: a navigation straight to a hashed
            // asset or theme-init.js must not overwrite it. waitUntil keeps the
            // worker alive until the write lands (respondWith settles first).
            const type = (res && res.headers.get("Content-Type")) || "";
            if (res && res.ok && type.startsWith("text/html")) {
              const copy = res.clone();
              e.waitUntil(caches.open(VERSION).then((c) => c.put("/admin", copy)));
            }
            return res;
          })
          .catch(() => caches.match("/admin")),
      );
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
            e.waitUntil(caches.open(VERSION).then((c) => c.put(req, copy)));
          }
          return res;
        })
        .catch(() => cached);
      return cached || net;
    }),
  );
});
