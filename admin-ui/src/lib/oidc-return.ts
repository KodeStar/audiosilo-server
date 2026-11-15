// A single sign-on (Sign in with OpenID Connect, server#235) for the console
// ends on /admin/#oidc_token=<t>: a 60 s single-use pairing token the console
// trades for a session (POST /auth/exchange). The token rides in the fragment,
// so it never reaches a server log or a Referer; it leaves the address bar and
// the history entry as soon as it is read.

/** The token in a location hash ("#oidc_token=<t>"), or null. */
export function oidcTokenFrom(hash: string): string | null {
  const m = /^#oidc_token=([A-Za-z0-9_-]+)$/.exec(hash);
  return m ? m[1] : null;
}

/**
 * Reads the token a single sign-on landed on and takes it out of the address
 * bar (replacing the history entry, so Back can't bring it back).
 */
export function takeOIDCToken(
  loc: Pick<Location, 'hash' | 'pathname' | 'search'> = window.location,
  hist: Pick<History, 'replaceState' | 'state'> = window.history,
): string | null {
  const token = oidcTokenFrom(loc.hash);
  if (token) hist.replaceState(hist.state, '', loc.pathname + loc.search);
  return token;
}
