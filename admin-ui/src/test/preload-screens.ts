import { createMemoryHistory } from '@tanstack/react-router';
import { PAGES } from '@/features/section-page';
import { createAppRouter } from '@/router';

/**
 * Loads every screen the console fetches on first visit (the sections' and the
 * lazy routes'), so a test file's first render finds them in. Each test file
 * starts with a cold module graph: without this, its first test pays for
 * compiling and running its screen inside its own findBy and test time-outs,
 * which a busy machine (a full parallel run, CI) can pass (server#308).
 */
export async function preloadScreens(): Promise<void> {
  const router = createAppRouter({ history: createMemoryHistory() });
  const routes = Object.values(router.routesById).map((r) =>
    (r.options.component as { preload?: () => Promise<unknown> } | undefined)?.preload?.(),
  );
  const sections = Object.values(PAGES).flatMap((pages) =>
    Object.values(pages).map((s) => s.preload()),
  );
  await Promise.all([...routes, ...sections]);
}
