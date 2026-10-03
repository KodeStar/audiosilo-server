import { render } from '@testing-library/react';
import { QueryClient } from '@tanstack/react-query';
import { createMemoryHistory } from '@tanstack/react-router';
import { App } from '@/app';
import { createAppRouter } from '@/router';

/** Renders the real console tree at `path` (relative to /admin). */
export function renderApp(path = '/') {
  const history = createMemoryHistory({ initialEntries: [`/admin${path === '/' ? '' : path}`] });
  const router = createAppRouter({ history });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const utils = render(<App router={router} queryClient={queryClient} />);
  return { ...utils, router, queryClient };
}
