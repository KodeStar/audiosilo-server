import { StrictMode } from 'react';
import { render } from '@testing-library/react';
import { QueryClient } from '@tanstack/react-query';
import { createMemoryHistory } from '@tanstack/react-router';
import { App } from '@/app';
import type { OIDCReturn } from '@/lib/oidc-return';
import { createAppRouter } from '@/router';

/**
 * Renders the real console tree at `path` (relative to /admin); `oidcReturn` as
 * main.tsx passes a single sign-on's return; `strict` under StrictMode, as
 * main.tsx renders it (the dev server's extra effect and ref cycles).
 */
export function renderApp(
  path = '/',
  opts: { oidcReturn?: OIDCReturn; strict?: boolean } = {},
) {
  const history = createMemoryHistory({ initialEntries: [`/admin${path === '/' ? '' : path}`] });
  const router = createAppRouter({ history });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const app = (
    <App router={router} queryClient={queryClient} oidcReturn={opts.oidcReturn ?? null} />
  );
  const utils = render(opts.strict ? <StrictMode>{app}</StrictMode> : app);
  return { ...utils, router, queryClient };
}
