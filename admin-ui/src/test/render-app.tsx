import { render } from '@testing-library/react';
import { QueryClient } from '@tanstack/react-query';
import { createMemoryHistory } from '@tanstack/react-router';
import { App } from '@/app';
import { createAppRouter } from '@/router';

/**
 * Renders the real console tree at `path` (relative to /admin); `oidcDone` as
 * main.tsx passes a single sign-on's return.
 */
export function renderApp(path = '/', opts: { oidcDone?: boolean } = {}) {
  const history = createMemoryHistory({ initialEntries: [`/admin${path === '/' ? '' : path}`] });
  const router = createAppRouter({ history });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const utils = render(
    <App router={router} queryClient={queryClient} oidcDone={opts.oidcDone ?? false} />,
  );
  return { ...utils, router, queryClient };
}
