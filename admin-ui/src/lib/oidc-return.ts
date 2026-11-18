// A single sign-on (Sign in with OpenID Connect, server#235) for the console
// ends on /admin/#oidc=done, or on the console page it started from with the
// same marker (oidcStartHref, server#406). The sign-in itself waits in an HttpOnly cookie the
// server set on its own path, never in the address (a link carrying it would
// sign whoever follows it in); the console trades it for a session at
// POST /auth/oidc/finish. The marker leaves the address bar and the history
// entry as soon as it is read.

/** Where a single sign-on for the console starts (handlers_oidc.go handleOIDCStart). */
export const OIDC_START = '/api/v1/auth/oidc/start?return=admin';

/**
 * The console's single sign-on start, carrying the page asked for (its path
 * and query, under /admin/) as `next`, so the sign-in lands back on it rather
 * than on the start page (server#406; the server takes only a console path,
 * else lands on the start page). The start page itself goes without.
 */
export function oidcStartHref(
  loc: Pick<Location, 'pathname' | 'search'> = window.location,
): string {
  const next = loc.pathname + loc.search;
  if (!loc.pathname.startsWith('/admin/') || next === '/admin/') return OIDC_START;
  return `${OIDC_START}&next=${encodeURIComponent(next)}`;
}

/** Whether a location hash is a single sign-on's return ("#oidc=done"). */
export function isOIDCReturn(hash: string): boolean {
  return hash === '#oidc=done';
}

/**
 * Reports whether the console was opened by a single sign-on's return and takes
 * the marker out of the address bar (replacing the history entry, so Back or a
 * reload doesn't finish again).
 */
export function takeOIDCReturn(
  loc: Pick<Location, 'hash' | 'pathname' | 'search'> = window.location,
  hist: Pick<History, 'replaceState' | 'state'> = window.history,
): boolean {
  const done = isOIDCReturn(loc.hash);
  if (done) hist.replaceState(hist.state, '', loc.pathname + loc.search);
  return done;
}
