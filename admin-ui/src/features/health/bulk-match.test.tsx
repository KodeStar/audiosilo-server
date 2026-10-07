import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { MatchRun, MatchRunItem } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { issuesSummary } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Health > Not matched: the bulk match card, from starting a run to reviewing
// and applying it.

function run(over: Partial<MatchRun> = {}): MatchRun {
  return {
    id: 5,
    library_id: null,
    mode: 'match',
    region: 'uk',
    status: 'ready',
    started_by: 1,
    started_at: '2026-10-07T10:00:00Z',
    finished_at: '2026-10-07T10:05:00Z',
    total: 4,
    done: 4,
    scope: '',
    apply_total: 0,
    apply_done: 0,
    applied_at: null,
    counts: {
      auto: 1,
      pending: 1,
      review: 1,
      none: 2,
      error: 0,
      applied: 0,
      skipped: 0,
      failed: 0,
    },
    ...over,
  };
}

const confident: MatchRunItem = {
  id: 10,
  run_id: 5,
  library_id: 1,
  path: 'Andy Weir/The Martian',
  outcome: 'auto',
  score: 96,
  runner_up: 0,
  proposal: {
    work_id: 'the-martian',
    title: 'The Martian',
    authors: 'Andy Weir',
    narrators: 'R. C. Bray',
    asin_region: 'uk',
    cover_url: 'https://c/1.jpg',
    values: { asin: 'B0UK000001', narrator: 'R. C. Bray' },
  },
  applied: '',
  book: { title: 'The Martian', author: 'Andy Weir' },
  changes: {
    ids: { fields: ['asin'], cover: false },
    fill: { fields: ['narrator', 'asin'], cover: true },
    overwrite: { fields: ['narrator', 'asin'], cover: true },
  },
};

const doubtful: MatchRunItem = {
  ...confident,
  id: 11,
  path: 'Andy Weir/Artemis',
  outcome: 'review',
  score: 91,
  runner_up: 86,
  book: { title: 'Artemis', author: 'Andy Weir' },
};

function routes(runs: MatchRun[], over: Record<string, MockRoute> = {}) {
  const summary = issuesSummary();
  summary.categories.push({ kind: 'unmatched', count: 4, ignored: 0, samples: [] });
  return signedInRoutes({
    'GET /admin/issues': { body: summary },
    'GET /admin/match-runs': { body: { runs, region: 'uk' } },
    'GET /admin/match-runs/5/items': (req) => ({
      body: {
        items: req.query.get('outcome') === 'review' ? [doubtful] : [confident],
        next_after: 0,
      },
    }),
    ...over,
  });
}

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('bulk matching', () => {
  it('starts a run over the unmatched books, or a repick for the preferred marketplace', async () => {
    const calls = mockFetch(
      routes([], { 'POST /admin/match-runs': { status: 202, body: run({ status: 'matching' }) } }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    const card = (await screen.findByRole('heading', { name: 'Match automatically' })).closest(
      'section',
    )!;
    expect(
      await within(card).findByText(/title, author, series and length go to the community/),
    ).toBeInTheDocument();
    expect(within(card).getByText(/audible\.co\.uk ASIN/)).toBeInTheDocument();
    await user.click(within(card).getByRole('button', { name: 'Find matches' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/match-runs')?.body,
      ).toEqual({ mode: 'match' }),
    );
    await user.click(within(card).getByRole('button', { name: 'Use United Kingdom ASINs' }));
    await waitFor(() =>
      expect(
        calls.filter((c) => c.method === 'POST' && c.path === '/admin/match-runs')[1]?.body,
      ).toEqual({ mode: 'repick' }),
    );
  });

  it('shows a working run and stops it', async () => {
    const calls = mockFetch(
      routes([run({ status: 'matching', done: 1, total: 4 })], {
        'POST /admin/match-runs/5/cancel': { status: 204 },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    expect(await screen.findByText('Matching 1 of 4 books')).toBeInTheDocument();
    expect(screen.getByRole('progressbar', { name: 'Matching progress' })).toHaveAttribute(
      'aria-valuenow',
      '25',
    );
    await user.click(screen.getByRole('button', { name: 'Stop' }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.method === 'POST' && c.path === '/admin/match-runs/5/cancel'),
      ).toBe(true),
    );
  });

  it('reviews a ready run and applies the picks under the chosen scope', async () => {
    // The apply finishes before the card's next poll: it never sees it working.
    let current = run();
    const calls = mockFetch(
      routes([], {
        'GET /admin/match-runs': () => ({ body: { runs: [current], region: 'uk' } }),
        'POST /admin/match-runs/5/apply': () => {
          current = run({
            status: 'applied',
            applied_at: '2026-10-07T10:06:00Z',
            apply_total: 2,
            apply_done: 2,
          });
          return { status: 202, body: run({ status: 'applying' }) };
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    expect(await screen.findByText('1 confident')).toBeInTheDocument();
    expect(screen.getByText('1 to review')).toBeInTheDocument();
    expect(screen.getByText('2 not found')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Review and apply' }));

    const dialog = await screen.findByRole('dialog', { name: 'Review matches' });
    // The confident match is ticked, with its UK ASIN and what filling gaps writes.
    const row = (await within(dialog).findByText('B0UK000001 · UK')).closest('li')!;
    expect(within(row).getByRole('checkbox', { name: 'Apply to The Martian' })).toBeChecked();
    expect(within(row).getByText('Sets Cover, Narrator, and ASIN')).toBeInTheDocument();
    expect(within(row).getByText('96% match')).toBeInTheDocument();
    // ASIN and ISBN only writes less.
    await user.click(within(dialog).getByRole('radio', { name: /ASIN and ISBN only/ }));
    expect(within(row).getByText('Sets ASIN')).toBeInTheDocument();

    // Put the doubtful one in too.
    await user.click(within(dialog).getByRole('button', { name: /To review/ }));
    const doubtfulRow = (await within(dialog).findByText('Artemis')).closest('li')!;
    expect(within(doubtfulRow).getByText('next 86%')).toBeInTheDocument();
    const box = within(doubtfulRow).getByRole('checkbox', { name: 'Apply to Artemis' });
    expect(box).not.toBeChecked();
    await user.click(box);

    const issuesAsked = () => calls.filter((c) => c.path === '/admin/issues').length;
    const before = issuesAsked();
    await user.click(within(dialog).getByRole('button', { name: 'Apply to 2 books' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/match-runs/5/apply')?.body,
      ).toEqual({ scope: 'ids', exclude: [], include: [11] }),
    );
    expect(await screen.findByText('Applying matches')).toBeInTheDocument();
    // Once it's done the books it matched leave the list: the Health summary is asked again.
    expect(await screen.findByText(/Last run applied/)).toBeInTheDocument();
    await waitFor(() => expect(issuesAsked()).toBeGreaterThan(before));
  });

  it('says how the last run ended', async () => {
    mockFetch(routes([run({ status: 'failed', error: 'metadata_unavailable' })]));
    renderApp('/health?issue=unmatched');
    expect(
      await screen.findByText(/the community metadata service didn't answer/),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Find matches' })).toBeEnabled();
  });
});
