// A console tab left open across a server upgrade still runs the old build,
// whose lazy chunks the new server no longer has: the next page it opens fails
// to load (UPG-01). The tab then reloads itself into the new build, once per
// build: a guard in sessionStorage, keyed on the build that reloaded, stops a
// loop when the chunk is missing for another reason (a proxy, offline), and the
// router's error page (route-error.tsx) offers Reload instead.

/** This build's id: set by vite.config.ts at build time, new for every build. */
export const BUILD_ID: string = __ADMIN_BUILD_ID__;

const GUARD_KEY = 'audiosilo.admin.reloaded-for';

/**
 * Reloads the page unless this build already reloaded once in this tab;
 * true when it reloads. Without sessionStorage it never reloads (no guard, no
 * loop): the error page's Reload is the way on.
 */
export function reloadOnce(
  build: string = BUILD_ID,
  reload: () => void = () => window.location.reload(),
): boolean {
  try {
    if (sessionStorage.getItem(GUARD_KEY) === build) return false;
    sessionStorage.setItem(GUARD_KEY, build);
  } catch {
    return false;
  }
  reload();
  return true;
}

/**
 * A chunk that failed to load, in the words each engine uses: Chromium,
 * Safari and Firefox, plus a missing stylesheet chunk (Vite's own).
 */
export function isChunkLoadError(err: unknown): boolean {
  const message = err instanceof Error ? err.message : typeof err === 'string' ? err : '';
  return /Failed to fetch dynamically imported module|Importing a module script failed|error loading dynamically imported module|Unable to preload CSS/i.test(
    message,
  );
}

/**
 * Vite's `vite:preloadError` (a lazy chunk or its preloads failed): reload into
 * the new build once. When the guard says no, the error goes on to the page's
 * error boundary as before.
 */
export function installStaleBuildReload(
  target: Pick<Window, 'addEventListener'> = window,
  reload?: () => void,
) {
  target.addEventListener('vite:preloadError', (event) => {
    if (reloadOnce(BUILD_ID, reload)) event.preventDefault();
  });
}
