// A console tab left open across a server upgrade still runs the old build,
// whose lazy chunks the new server no longer has: the next page it opens fails
// to load (UPG-01). The router's error page (route-error.tsx) then reloads the
// tab into the new build, once per build: a guard in sessionStorage, keyed on
// the build that reloaded, stops a loop when the chunk is missing for another
// reason (a proxy, offline), and the page offers Reload instead. Only a page
// that failed to open reloads: a chunk fetched ahead on a link's hover or focus
// (a route's preload) fails quietly, so pointing at a link never reloads the
// tab and loses what was typed. Hence no window-wide vite:preloadError handler,
// which hears those preloads too (and whose preventDefault hands the lazy
// screen an undefined module).

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
