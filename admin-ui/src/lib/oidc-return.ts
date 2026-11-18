// A single sign-on (Sign in with OpenID Connect, server#235) for the console
// ends on /admin/#oidc=done&flow=<key>, or on the console page it started from
// with the same marker (oidcStartHref, server#406). The sign-in itself waits in
// an HttpOnly cookie the server set on its own path, one per flow, never in the
// address (a link carrying it would sign whoever follows it in); the console
// trades it for a session at POST /auth/oidc/finish, naming the flow's key, so
// two tabs that came back at once each finish their own. The marker leaves the
// address bar and the history entry as soon as it is read.

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

/**
 * A single sign-on's return: `flow` is the key of the flow it finished
 * (handlers_oidc.go oidcFlowParam), absent from a server before it.
 */
export interface OIDCReturn {
  flow?: string;
}

const RETURN_HASH = /^#oidc=done(?:&flow=([A-Za-z0-9_-]{16}))?$/;

/** The single sign-on's return a location hash is ("#oidc=done", "&flow=<key>" after), or null. */
export function parseOIDCReturn(hash: string): OIDCReturn | null {
  const m = RETURN_HASH.exec(hash);
  if (!m) return null;
  return m[1] ? { flow: m[1] } : {};
}

/**
 * The single sign-on's return the console was opened by (null when none), with
 * the marker taken out of the address bar (replacing the history entry, so Back
 * or a reload doesn't finish again).
 */
export function takeOIDCReturn(
  loc: Pick<Location, 'hash' | 'pathname' | 'search'> = window.location,
  hist: Pick<History, 'replaceState' | 'state'> = window.history,
): OIDCReturn | null {
  const done = parseOIDCReturn(loc.hash);
  if (done) hist.replaceState(hist.state, '', loc.pathname + loc.search);
  return done;
}
