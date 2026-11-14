import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminSettings } from '@/api/types';
import { mockFetch } from '@/test/fetch-mock';
import { settingsWithMetadata, systemStatus } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { patchEcho, signedInRoutes } from '@/test/routes';

// Settings > Community metadata offers the marketplace nearest the admin while
// none was ever chosen (server#192): an offer with its own buttons, never a
// changed field (the card stays saved, so leaving the page asks nothing).

beforeEach(() => setToken('stored'));
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function open(settings: AdminSettings) {
  // A save answers as a server would: the marketplace counts as chosen.
  const calls = mockFetch(
    signedInRoutes({
      'GET /admin/system': { body: systemStatus() },
      'GET /admin/settings': { body: settings },
      'PATCH /admin/settings': patchEcho(settings, {
        metadata: { ...settings.metadata, region_chosen: true },
      }),
    }),
  );
  renderApp('/server?topic=metadata');
  return calls;
}

const patches = (calls: ReturnType<typeof mockFetch>) =>
  calls.filter((c) => c.method === 'PATCH').map((c) => c.body);

describe('the nearest marketplace in Settings', () => {
  it('offers it once, saving it in one click, with nothing unsaved before', async () => {
    vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['en-GB']);
    const calls = open(settingsWithMetadata({ region_chosen: false }));
    expect(
      await screen.findByText(
        'No marketplace chosen yet. Nearest to you: United Kingdom (audible.co.uk).',
      ),
    ).toBeVisible();
    // The field still shows what is saved, so the card has nothing to save.
    expect(screen.getByRole('combobox', { name: 'Audible marketplace' })).toHaveValue('');
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Use United Kingdom' }));
    await waitFor(() => expect(patches(calls)).toEqual([{ metadata: { region: 'uk' } }]));
    await waitFor(() => expect(screen.queryByText(/Nearest to you/)).toBeNull());
    expect(screen.getByRole('combobox', { name: 'Audible marketplace' })).toHaveValue('uk');
  });

  it('Keep no preference saves the US-first default as a choice', async () => {
    vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['de-AT']);
    const calls = open(settingsWithMetadata({ region_chosen: false }));
    await screen.findByText(/Nearest to you: Germany \(audible\.de\)/);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Keep no preference' }));
    await waitFor(() => expect(patches(calls)).toEqual([{ metadata: { region: '' } }]));
    await waitFor(() => expect(screen.queryByText(/Nearest to you/)).toBeNull());
  });

  it.each([
    ['one already chosen', { region_chosen: true }, ['en-GB']],
    ['an older server that says nothing', {}, ['en-GB']],
    ['an admin nearest the US store', { region_chosen: false }, ['en-US']],
  ])('offers nothing for %s', async (_, metadata, langs) => {
    vi.spyOn(navigator, 'languages', 'get').mockReturnValue(langs);
    open(settingsWithMetadata(metadata));
    expect(await screen.findByRole('combobox', { name: 'Audible marketplace' })).toBeVisible();
    expect(screen.queryByText(/Nearest to you/)).toBeNull();
  });
});
