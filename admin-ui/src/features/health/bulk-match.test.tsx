import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { MatchRun, MatchRunItem } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { issuesSummary } from '@/test/fixtures';
import { bookDetail, books } from '@/test/library-fixtures';
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
    values: { asin: 'B0UK000001', narrator: 'R. C. Bray', series: 'Mars', series_index: '1' },
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

  it('starts a refresh over the matched books', async () => {
    const calls = mockFetch(
      routes([], { 'POST /admin/match-runs': { status: 202, body: run({ status: 'matching' }) } }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    await user.click(await screen.findByRole('button', { name: 'Fill in matched books' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/match-runs')?.body,
      ).toEqual({ mode: 'refresh' }),
    );
  });

  it("reviews a refresh run without the ids scope, and says when an identifier isn't known", async () => {
    const unknown: MatchRunItem = {
      ...confident,
      id: 12,
      path: 'Someone/Obscure',
      outcome: 'none',
      score: 0,
      detail: 'identifier_unknown',
      book: { title: 'Obscure', author: 'Someone' },
      changes: {},
    };
    const calls = mockFetch(
      routes([run({ mode: 'refresh', counts: { ...run().counts, review: 0, none: 1 } })], {
        'GET /admin/match-runs/5/items': (req) => ({
          body: {
            items: req.query.get('outcome') === 'none' ? [unknown] : [confident],
            next_after: 0,
          },
        }),
        'POST /admin/match-runs/5/apply': { status: 202, body: run({ status: 'applying' }) },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    await user.click(await screen.findByRole('button', { name: 'Review and apply' }));
    const dialog = await screen.findByRole('dialog', { name: 'Fill in matched books' });
    expect(await within(dialog).findByText('The Martian')).toBeInTheDocument();
    expect(within(dialog).queryByRole('radio', { name: /ASIN and ISBN only/ })).toBeNull();
    expect(within(dialog).getByRole('radio', { name: /Fill in what's missing/ })).toBeChecked();
    await user.click(within(dialog).getByRole('button', { name: /Not found/ }));
    expect(
      await within(dialog).findByText("The community doesn't know this book's ASIN or ISBN"),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Apply to 1 book' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/match-runs/5/apply')?.body,
      ).toEqual({ scope: 'fill', exclude: [], include: [] }),
    );
  });

  it('shows the series a scope replaces, old to new', async () => {
    const polluted: MatchRunItem = {
      ...confident,
      changes: {
        fill: { fields: [], cover: false },
        overwrite: {
          fields: ['series', 'more_series'],
          cover: false,
          series: {
            from: [
              { name: 'Mars (Full-Cast Editions)', position: 1 },
              { name: 'Mars (Narrated by Someone Else)', position: 1 },
            ],
            to: [{ name: 'Mars', position: 1 }],
          },
        },
      },
    };
    mockFetch(
      routes([run({ mode: 'refresh' })], {
        'GET /admin/match-runs/5/items': { body: { items: [polluted], next_after: 0 } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    await user.click(await screen.findByRole('button', { name: 'Review and apply' }));
    const dialog = await screen.findByRole('dialog', { name: 'Fill in matched books' });
    // Fill leaves the series be: the proposal's series shows as before.
    expect(await within(dialog).findByText('Series: Mars #1')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('radio', { name: /Use community values/ }));
    expect(
      await within(dialog).findByText(
        'Series: Mars (Full-Cast Editions) #1; Mars (Narrated by Someone Else) #1 → Mars #1',
      ),
    ).toBeInTheDocument();
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
    // Where the book is, and where the match sits in its series, to tell them apart.
    expect(within(row).getByText('Andy Weir/The Martian')).toHaveAttribute(
      'title',
      '/mnt/tank/fiction/Andy Weir/The Martian',
    );
    expect(
      within(row).getByRole('checkbox', { name: 'Apply to The Martian' }),
    ).toHaveAccessibleDescription('Andy Weir/The Martian');
    expect(within(row).getByText('Series: Mars #1')).toBeInTheDocument();
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

  // The match dialog over the review: one candidate, no recordings (so no cover).
  const byHandRoutes: Record<string, MockRoute> = {
    'GET /admin/libraries/1/book/match': {
      body: {
        candidates: [
          {
            work_id: 'the-martian',
            title: 'The Martian',
            authors: [{ id: 'a1', name: 'Andy Weir' }],
            first_published: '2011',
            series: [],
            web_url: 'https://meta.audiosilo.app/works/the-martian',
            recordings: [],
            score: 100,
          },
        ],
      },
    },
    'POST /admin/meta/covers': { body: { covers: [''] } },
  };

  /** Matches the review's confident book by hand, accepting the default ticks. */
  async function matchByHand(user: ReturnType<typeof userEvent.setup>, row: HTMLElement) {
    await user.click(within(row).getByRole('button', { name: 'Match The Martian by hand' }));
    const match = await screen.findByRole('dialog', { name: 'Match with community metadata' });
    await within(match).findByRole('radio', { name: /The Martian/ });
    await user.click(within(match).getByRole('button', { name: 'Compare fields' }));
    const compare = await screen.findByRole('dialog', { name: 'Compare with community metadata' });
    await user.click(within(compare).getByRole('button', { name: /^Accept \d+ field/ }));
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Compare with community metadata' })).toBeNull(),
    );
  }

  it('matches a book by hand over the review, which keeps its place and leaves it out', async () => {
    const detail = bookDetail();
    const calls = mockFetch(
      routes([run()], {
        'GET /admin/libraries/1/book': { body: detail },
        'PATCH /admin/libraries/1/book': { body: detail },
        ...byHandRoutes,
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    await user.click(await screen.findByRole('button', { name: 'Review and apply' }));
    const review = await screen.findByRole('dialog', { name: 'Review matches' });
    const row = (await within(review).findByText('B0UK000001 · UK')).closest('li')!;
    // The book page is a new tab away, for a closer look without leaving the review.
    expect(
      within(row).getByRole('link', { name: 'Open the book page for The Martian in a new tab' }),
    ).toHaveAttribute('target', '_blank');

    // Escape closes the match, not the review under it.
    await user.click(within(row).getByRole('button', { name: 'Match The Martian by hand' }));
    expect(
      await screen.findByRole('dialog', { name: 'Match with community metadata' }),
    ).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Match with community metadata' })).toBeNull(),
    );
    expect(screen.getByRole('dialog', { name: 'Review matches' })).toBeInTheDocument();

    await matchByHand(user, row);
    expect(calls.some((c) => c.method === 'PATCH' && c.path === '/admin/libraries/1/book')).toBe(
      true,
    );

    // Back on the review: the book reads as matched since the run, and the apply leaves it out.
    const after = (await within(review).findByText('B0UK000001 · UK')).closest('li')!;
    expect(
      within(after).getByText('Matched since this run: an apply leaves it as it is'),
    ).toBeInTheDocument();
    expect(within(after).queryByRole('checkbox')).toBeNull();
    expect(within(review).getByRole('button', { name: 'Apply to 0 books' })).toBeDisabled();
  });

  it('asks the server again for a book matched by hand that took no ASIN or ISBN', async () => {
    const unmatched = bookDetail({ book: { ...books[0], asin: '' } });
    const calls = mockFetch(
      routes([run()], {
        'GET /admin/libraries/1/book': { body: unmatched },
        'PATCH /admin/libraries/1/book': { body: unmatched },
        ...byHandRoutes,
      }),
    );
    const autoAsked = () =>
      calls.filter(
        (c) => c.path === '/admin/match-runs/5/items' && c.query.get('outcome') === 'auto',
      ).length;
    const issuesAsked = () => calls.filter((c) => c.path === '/admin/issues').length;
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    await user.click(await screen.findByRole('button', { name: 'Review and apply' }));
    const review = await screen.findByRole('dialog', { name: 'Review matches' });
    const row = (await within(review).findByText('B0UK000001 · UK')).closest('li')!;
    const [items, issues] = [autoAsked(), issuesAsked()];
    await matchByHand(user, row);

    // Still unmatched to the server, so not "matched since": the review asks again,
    // the Health page under it too, and the book stays left out of the apply.
    await waitFor(() => expect(autoAsked()).toBeGreaterThan(items));
    await waitFor(() => expect(issuesAsked()).toBeGreaterThan(issues));
    const after = (await within(review).findByText('B0UK000001 · UK')).closest('li')!;
    expect(within(after).queryByText(/Matched since this run/)).toBeNull();
    expect(within(after).getByRole('checkbox')).not.toBeChecked();
    expect(within(review).getByRole('button', { name: 'Apply to 0 books' })).toBeDisabled();
  });

  it("shows a gone book's path once, as its title", async () => {
    const gone: MatchRunItem = {
      ...confident,
      id: 12,
      path: 'Andy Weir/Project Hail Mary',
      book: { title: '', author: '' },
      gone: true,
      changes: {},
    };
    mockFetch(
      routes([run()], {
        'GET /admin/match-runs/5/items': { body: { items: [gone], next_after: 0 } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    await user.click(await screen.findByRole('button', { name: 'Review and apply' }));
    const dialog = await screen.findByRole('dialog', { name: 'Review matches' });
    expect(await within(dialog).findByText('Andy Weir/Project Hail Mary')).toHaveAttribute(
      'title',
      '/mnt/tank/fiction/Andy Weir/Project Hail Mary',
    );
  });

  it('says how the last run ended', async () => {
    mockFetch(routes([run({ status: 'failed', error: 'metadata_unavailable' })]));
    renderApp('/health?issue=unmatched');
    expect(
      await screen.findByText(/the community metadata service didn't answer/),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Find matches' })).toBeEnabled();
  });

  it('says a run that found nothing found nothing, not that it applied none', async () => {
    const empty = run();
    empty.counts = { ...empty.counts, auto: 0, pending: 0, review: 0, none: 4 };
    mockFetch(routes([empty]));
    renderApp('/health?issue=unmatched');
    expect(await screen.findByText('The last run found nothing to apply.')).toBeInTheDocument();
    expect(screen.queryByText(/Last run applied/)).not.toBeInTheDocument();
  });

  it("starts over on the run's own library", async () => {
    const calls = mockFetch(
      routes([run({ library_id: 3, library_name: 'Fiction' })], {
        'POST /admin/match-runs': { status: 202, body: run({ status: 'matching' }) },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    await user.click(await screen.findByRole('button', { name: 'Start over' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/match-runs')?.body,
      ).toEqual({ library_id: 3, mode: 'match' }),
    );
  });

  it('words a review item held for its length with both lengths (R-44)', async () => {
    const shorter: MatchRunItem = {
      ...doubtful,
      detail: 'length_differs',
      proposal: { ...doubtful.proposal, runtime_min: 300 },
      book: { title: 'Artemis', author: 'Andy Weir', duration: 634 * 60 },
    };
    mockFetch(
      routes([run()], {
        'GET /admin/match-runs/5/items': (req) => ({
          body: {
            items: req.query.get('outcome') === 'review' ? [shorter] : [confident],
            next_after: 0,
          },
        }),
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    await user.click(await screen.findByRole('button', { name: 'Review and apply' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(await within(dialog).findByRole('button', { name: /To review/ }));
    const row = (await within(dialog).findByText('Artemis')).closest('li')!;
    expect(
      within(row).getByText(/^Held for review: the book is 10h 34m long, this recording 5h/),
    ).toBeInTheDocument();
  });

  it('says nothing about length for an item held only for its score', async () => {
    mockFetch(routes([run()]));
    const user = userEvent.setup();
    renderApp('/health?issue=unmatched');
    await user.click(await screen.findByRole('button', { name: 'Review and apply' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(await within(dialog).findByRole('button', { name: /To review/ }));
    const row = (await within(dialog).findByText('Artemis')).closest('li')!;
    expect(within(row).queryByText(/Held for review/)).toBeNull();
  });
});
