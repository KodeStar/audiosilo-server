/**
 * Whether the server's first-run setup page is open: a server started with
 * --setup and no admin yet. GET /setup answers 200 only then, and otherwise
 * redirects to /admin (never enabled or closed alike; an older server 404s a
 * never-enabled one), so the sign-in page asks it rather than the server saying
 * anything new to a signed-out visitor, and the usual answer is a redirect, not
 * a failed request in the browser's console. Any failure reads as closed.
 */
export async function setupPageOpen(): Promise<boolean> {
  try {
    const res = await fetch('/setup', {
      method: 'HEAD',
      redirect: 'manual',
      credentials: 'omit',
      cache: 'no-store',
    });
    return res.status === 200;
  } catch {
    return false;
  }
}
