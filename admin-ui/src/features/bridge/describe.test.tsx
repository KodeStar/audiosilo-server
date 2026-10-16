import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { BridgeEstimate, BridgeJob } from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { bookDetail } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// The book page's description part (Bridge Phase 1c): the generated description
// with its mark and Show more, "Write a description" and its confirmation, and
// a describe job's status.

const BOOK_PATH = 'Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings';
const URL = `/library/book?library=1&path=${encodeURIComponent(BOOK_PATH)}`;

const bridgeOn = {
  ...serverInfo,
  capabilities: { ...serverInfo.capabilities, bridge: true },
  bridge: { version: '0.17.0', features: ['bridge', 'sidecars'] },
};

const LONG_TEXT =
  'A storm-wracked land where soldiers fight over gemhearts on the Shattered Plains. ' +
  'A slave once a surgeon, a scholar with a secret debt and a highprince who sees ' +
  'visions are drawn toward the same old war, each asking what an oath is worth. ' +
  'The opening follows them as the first threads begin to cross.';

/** A brief estimate of a book with generated characters and recaps, and no description. */
function estimate(over: Partial<BridgeEstimate> = {}): BridgeEstimate {
  return {
    kind: 'sidecars.generate',
    library_id: 1,
    path: BOOK_PATH,
    source: 'audio',
    duration_seconds: 163800,
    asr_hours: 11.4,
    agent_cost_usd: 18.2,
    subscription: false,
    agent_ready: true,
    can_contribute: false,
    figures: true,
    writable: true,
    can_describe: true,
    has: {
      community: { characters: false, recaps: false, description: false },
      local: { characters: true, recaps: true, description: false },
    },
    earlier: [],
    ...over,
  };
}

function job(over: Partial<BridgeJob> = {}): BridgeJob {
  return {
    id: 40,
    kind: 'sidecars.describe',
    library_id: 1,
    path: BOOK_PATH,
    params: {},
    state: 'running',
    progress: {},
    result: {},
    error: '',
    requested_by: { id: 1, username: 'chris' },
    priority: 0,
    attempts: 1,
    lease_until: null,
    cancel_requested: false,
    created_at: '2026-10-16T12:00:00Z',
    updated_at: '2026-10-16T12:01:00Z',
    claimed_at: '2026-10-16T12:00:05Z',
    finished_at: null,
    ...over,
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /server': { body: bridgeOn },
    'GET /admin/libraries/1/book': { body: bookDetail() },
    'GET /admin/bridge/jobs': { body: { jobs: [] } },
    'GET /admin/bridge/estimate': { body: estimate() },
    ...over,
  });
}

const queued = (calls: MockRequest[]) =>
  calls.filter((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs');

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('the generated description', () => {
  it('shows the text with its mark, clamped behind Show more', async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({
        'GET /admin/libraries/1/book': {
          body: bookDetail({ generated_description: { text: LONG_TEXT } }),
        },
        'GET /admin/bridge/estimate': {
          body: estimate({
            has: {
              community: { characters: false, recaps: false, description: false },
              local: { characters: true, recaps: true, description: true },
            },
          }),
        },
      }),
    );
    renderApp(URL);
    const part = await screen.findByRole('region', { name: 'Description' });
    expect(within(part).getByText(LONG_TEXT)).toHaveClass('line-clamp-4');
    // The mark the characters-and-recaps part uses too.
    expect(within(part).getByText('Generated on this server.')).toBeInTheDocument();
    const more = within(part).getByRole('button', { name: 'Show more' });
    expect(more).toHaveAttribute('aria-expanded', 'false');
    await user.click(more);
    expect(within(part).getByText(LONG_TEXT)).not.toHaveClass('line-clamp-4');
    expect(within(part).getByRole('button', { name: 'Show less' })).toBeInTheDocument();
    // It has one already: nothing to offer.
    expect(within(part).queryByRole('button', { name: 'Write a description' })).toBeNull();
  });

  it('shows it even while the Bridge is away', async () => {
    mockFetch(
      routes({
        'GET /server': { body: serverInfo },
        'GET /admin/libraries/1/book': {
          body: bookDetail({ generated_description: { text: 'Short enough to show whole.' } }),
        },
      }),
    );
    renderApp(URL);
    const part = await screen.findByRole('region', { name: 'Description' });
    expect(within(part).getByText('Short enough to show whole.')).not.toHaveClass('line-clamp-4');
    expect(within(part).queryByRole('button', { name: 'Show more' })).toBeNull();
  });
});

describe('write a description', () => {
  it('confirms, then queues sidecars.describe with no params', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(
      routes({
        'POST /admin/bridge/jobs': { status: 201, body: job({ state: 'queued' }) },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Write a description' }));
    const dialog = await screen.findByRole('dialog', { name: 'Write a description' });
    expect(within(dialog).getByText(/from the notes it kept/)).toBeInTheDocument();
    expect(within(dialog).getByText(/No new speech recognition runs/)).toBeInTheDocument();
    expect(within(dialog).getByText(/in the book's About in the player/)).toBeInTheDocument();
    expect(queued(calls)).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Write description' }));
    await waitFor(() => expect(queued(calls)).toHaveLength(1));
    expect(queued(calls)[0].body).toEqual({
      kind: 'sidecars.describe',
      library_id: 1,
      path: BOOK_PATH,
      params: {},
    });
  });

  it.each<[string, Partial<BridgeEstimate>]>([
    ['a Bridge that does not offer it (an older one, or away)', { can_describe: false }],
    [
      'a community description',
      {
        has: {
          community: { characters: false, recaps: false, description: true },
          local: { characters: true, recaps: true, description: false },
        },
      },
    ],
    [
      'nothing generated to write from',
      {
        has: {
          community: { characters: true, recaps: true, description: false },
          local: { characters: false, recaps: false, description: false },
        },
      },
    ],
  ])('is not offered with %s', async (_name, over) => {
    const calls = mockFetch(routes({ 'GET /admin/bridge/estimate': { body: estimate(over) } }));
    renderApp(URL);
    await screen.findByRole('heading', { level: 1, name: 'The Way of Kings' });
    await waitFor(() => expect(calls.some((c) => c.path === '/admin/bridge/estimate')).toBe(true));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole('button', { name: 'Write a description' })).toBeNull();
  });

  it('is not offered while a generate job is open (it writes one)', async () => {
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: { jobs: [job({ id: 39, kind: 'sidecars.generate', params: {} })] },
        },
      }),
    );
    renderApp(URL);
    // The generate job's own status shows; no describe action beside it.
    expect(await screen.findByRole('button', { name: /cancel generating/i })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Write a description' })).toBeNull();
  });

  it("follows a describe job's stage, and says when it failed", async () => {
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [job({ progress: { step: 'describing', stage: 'describing', cost_usd: 0.12 } })],
          },
        },
      }),
    );
    renderApp(URL);
    const part = await screen.findByRole('region', { name: 'Description' });
    expect(await within(part).findByText('Writing the description')).toBeInTheDocument();
    expect(within(part).getByText(/\$0\.12 so far/)).toBeInTheDocument();
    expect(
      within(part).getByRole('button', { name: 'Cancel writing the description' }),
    ).toBeInTheDocument();
    expect(within(part).queryByRole('button', { name: 'Write a description' })).toBeNull();
  });

  it('shows a failed describe job with its error', async () => {
    const error = "Couldn't write a description that passed the checks.";
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: { jobs: [job({ state: 'failed', error, finished_at: '2026-10-16T12:05:00Z' })] },
        },
      }),
    );
    renderApp(URL);
    const part = await screen.findByRole('region', { name: 'Description' });
    expect(await within(part).findByText(error)).toBeInTheDocument();
    // Not open any more: it can be asked again.
    expect(within(part).getByRole('button', { name: 'Write a description' })).toBeInTheDocument();
  });
});

describe("a generate job's description", () => {
  it('says the description came with it, or why not', async () => {
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                id: 41,
                kind: 'sidecars.generate',
                state: 'done',
                finished_at: '2026-10-16T14:00:00Z',
                result: {
                  characters: 3,
                  recaps: 2,
                  description: false,
                  description_error: 'the agent is rate limited',
                },
              }),
            ],
          },
        },
      }),
    );
    renderApp(URL);
    expect(
      await screen.findByText('No description this time: the agent is rate limited'),
    ).toBeInTheDocument();
  });
});
