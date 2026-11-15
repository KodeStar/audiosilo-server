// A single sign-on (Sign in with OpenID Connect, server#235) for the console
// ends on /admin/#oidc=done. The sign-in itself waits in an HttpOnly cookie the
// server set on its own path, never in the address (a link carrying it would
// sign whoever follows it in); the console trades it for a session at
// POST /auth/oidc/finish. The marker leaves the address bar and the history
// entry as soon as it is read.

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
