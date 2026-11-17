import { act, screen } from '@testing-library/react';
import { focusManager } from '@tanstack/react-query';
import { setToken } from '@/api/token';
import { mockFetch } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// server#197: a console tab left open across an upgrade hears of it from
// /server, read again when the tab comes back into focus.

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

it('says AudioSilo was updated when the server answers a new version, and offers Reload', async () => {
  let version = serverInfo.version;
  const calls = mockFetch(
    signedInRoutes({ 'GET /server': () => ({ body: { ...serverInfo, version } }) }),
  );
  renderApp();
  await screen.findByRole('heading', { level: 1, name: /chris\.$/ });
  const reads = () => calls.filter((c) => c.path === '/server').length;
  const before = reads();

  // Back in focus on the same version: read again, nothing to say.
  act(() => {
    focusManager.setFocused(false);
    focusManager.setFocused(true);
  });
  await vi.waitFor(() => expect(reads()).toBe(before + 1));
  expect(screen.queryByText('AudioSilo was updated')).toBeNull();

  version = '9.9.9';
  act(() => {
    focusManager.setFocused(false);
    focusManager.setFocused(true);
  });
  expect(await screen.findByText('AudioSilo was updated')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Reload' })).toBeVisible();
  focusManager.setFocused(undefined);
});
