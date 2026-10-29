import { screen } from '@testing-library/react';
import { setToken } from '@/api/token';
import { BUILD_ID } from '@/lib/stale-build';
import { mockFetch } from '@/test/fetch-mock';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// UPG-01: a page that fails shows the console's error page inside the shell.

const failure = vi.hoisted(() => ({ message: 'boom' }));
vi.mock('@/features/health/issues-page', () => ({
  IssuesPage: () => {
    throw new TypeError(failure.message);
  },
}));

beforeEach(() => setToken('stored'));

describe('the route error page', () => {
  it('renders in place of the page, with the nav and Reload', async () => {
    failure.message = 'Cannot read properties of undefined';
    // React logs the error a boundary caught.
    const log = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockFetch(signedInRoutes());
    renderApp('/health');
    expect(await screen.findByRole('heading', { name: 'This page hit a problem' })).toBeVisible();
    expect(screen.getAllByRole('navigation', { name: 'Primary' }).length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: 'Reload' })).toBeVisible();
    expect(screen.getByRole('link', { name: 'Go to the overview' })).toBeVisible();
    expect(screen.queryByText('Something went wrong!')).toBeNull();
    log.mockRestore();
  });

  it('says the console was updated when a chunk is gone and this build already reloaded', async () => {
    failure.message = 'Failed to fetch dynamically imported module: /admin/assets/issues-page-x.js';
    // The guard already holds this build: no second reload, the page says why.
    sessionStorage.setItem('audiosilo.admin.reloaded-for', BUILD_ID);
    const log = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockFetch(signedInRoutes());
    renderApp('/health');
    expect(await screen.findByRole('heading', { name: 'AudioSilo was updated' })).toBeVisible();
    expect(screen.getAllByRole('navigation', { name: 'Primary' }).length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: 'Reload' })).toBeVisible();
    log.mockRestore();
  });
});
