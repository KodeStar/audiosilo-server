/**
 * Whether the server's first-run setup page is open: a server started with
 * --setup and no admin yet. GET /setup answers 200 only then (404 when it was
 * never enabled, a redirect to /admin once an admin exists), so the sign-in page
 * asks it rather than the server saying anything new to a signed-out visitor.
 * Any failure reads as closed.
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
