import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { Import, ImportDetail, ImportSummary } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { activity, samDetail, users } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Settings > Import (connect, map, review, apply, undo) and a person's
// "Imported history" on their Listening tab.

const TOKEN = 'abs-secret-token';

function summary(over: Partial<ImportSummary> = {}): ImportSummary {
  return {
    items: 12,
    matched: { path: 8, asin: 1, isbn: 0, title: 1 },
    unmatched: 2,
    sessions: 140,
    skipped_after_cutoff: 3,
    listened: 45 * 3600,
    estimated: 0,
    progress: 10,
    finished: 4,
    bookmarks: 6,
    first_listen: '2023-02-01T10:00:00Z',
    last_listen: '2026-05-01T10:00:00Z',
    ...over,
  };
}

function imp(over: Partial<Import> = {}): Import {
  return {
    id: 7,
    user_id: 2,
    username: 'sam',
    source: 'abs',
    source_url: 'https://abs.example.com',
    source_user: 'samantha',
    status: 'review',
    cutoff: '2026-06-01T00:00:00Z',
    cutoff_utc_offset: 0,
    created_at: new Date().toISOString(),
    applied_at: null,
    error: '',
    error_code: '',
    summary: summary(),
    ...over,
  };
}

function detail(over: Partial<ImportDetail> = {}): ImportDetail {
  return {
    ...imp(),
    unmatched_items: [
      {
        title: 'The Long Way to a Small, Angry Planet',
        author: 'Becky Chambers',
        listened: 5 * 3600,
        sessions: 12,
        reason: 'no_match',
      },
      { title: 'Dune', author: 'Frank Herbert', listened: 1800, sessions: 2, reason: 'contested' },
    ],
    ...over,
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({ 'GET /admin/users': { body: { users } }, ...over });
}

beforeEach(() => setToken('stored'));

/** Fills step 1 and connects. */
async function connect(user: ReturnType<typeof userEvent.setup>) {
  await user.type(
    await screen.findByRole('textbox', { name: 'Audiobookshelf address' }),
    'https://abs.example.com',
  );
  await user.type(screen.getByLabelText('API token'), TOKEN);
  await user.click(screen.getByRole('button', { name: 'Connect' }));
}

describe('import: connect, map, review, apply', () => {
  it('walks the whole flow and never keeps the token', async () => {
    let started = false;
    let applied = false;
    let listPolls = 0;
    const calls = mockFetch(
      routes({
        'POST /admin/imports/abs/users': {
          body: {
            version: '2.17.2',
            users: [
              { abs_id: 'u1', username: 'samantha', type: 'user', suggested_user_id: 2 },
              { abs_id: 'u2', username: 'root', type: 'root', suggested_user_id: null },
            ],
          },
        },
        'POST /admin/imports/abs': () => {
          started = true;
          return { status: 202, body: { imports: [imp({ status: 'fetching', summary: null })] } };
        },
        // The list is what's polled: fetching on the first ask after the
        // start, ready on the next poll.
        'GET /admin/imports': () => {
          if (started) listPolls++;
          return {
            body: {
              imports: !started
                ? []
                : [
                    applied
                      ? imp({ status: 'applied', applied_at: new Date().toISOString() })
                      : listPolls > 1
                        ? imp()
                        : imp({ status: 'fetching', summary: null }),
                  ],
            },
          };
        },
        'GET /admin/imports/7': { body: detail() },
        'POST /admin/imports/7/apply': () => {
          applied = true;
          return { body: imp({ status: 'applied', applied_at: new Date().toISOString() }) };
        },
      }),
    );
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    expect(
      await screen.findByText('Bring listening history over from Audiobookshelf'),
    ).toBeInTheDocument();

    await connect(user);
    expect(calls.find((c) => c.path === '/admin/imports/abs/users')?.body).toEqual({
      url: 'https://abs.example.com',
      token: TOKEN,
    });

    // Step 2: samantha is suggested for sam; root isn't imported.
    const sam = await screen.findByRole('combobox', { name: 'AudioSilo person for samantha' });
    expect(sam).toHaveValue('2');
    expect(screen.getByRole('combobox', { name: 'AudioSilo person for root' })).toHaveValue('');
    expect(
      screen.getByRole('radio', { name: /Before each person started using AudioSilo/ }),
    ).toBeChecked();
    await user.click(screen.getByRole('button', { name: 'Fetch history for 1 person' }));
    await waitFor(() =>
      expect(calls.find((c) => c.path === '/admin/imports/abs')?.body).toEqual({
        url: 'https://abs.example.com',
        token: TOKEN,
        users: [{ abs_user_id: 'u1', abs_username: 'samantha', user_id: 2 }],
        cutoff: 'auto',
      }),
    );

    // Back on step 1 with nothing filled in: the token is gone.
    expect(await screen.findByLabelText('API token')).toHaveValue('');
    expect(JSON.stringify({ ...localStorage, ...sessionStorage })).not.toContain(TOKEN);

    // Step 3: fetching, then (polled) ready for review.
    const card = await screen.findByRole('region', { name: 'samantha → sam' });
    expect(await within(card).findByText(/Fetching samantha's history/)).toBeInTheDocument();
    // Nothing but the list is asked about while it is fetched.
    expect(calls.some((c) => c.path === '/admin/imports/7')).toBe(false);
    expect(await within(card).findByText('45h', {}, { timeout: 4000 })).toBeInTheDocument();
    expect(within(card).getByText('140')).toBeInTheDocument();
    expect(
      within(card).getByText('10 of 12 (8 by path, 1 by ASIN, 1 by title and author)'),
    ).toBeInTheDocument();
    expect(within(card).getByText(/Sessions from before/)).toBeInTheDocument();

    // The unmatched books, with why.
    await user.click(within(card).getByText('2 books not matched'));
    expect(within(card).getByText('The Long Way to a Small, Angry Planet')).toBeVisible();
    expect(within(card).getByText('Becky Chambers · Not found in your libraries')).toBeVisible();
    expect(within(card).getByText(/Two Audiobookshelf books matched the same book/)).toBeVisible();

    // Apply, after a confirm that says a re-import replaces this one.
    await user.click(within(card).getByRole('button', { name: 'Apply for sam' }));
    const dialog = await screen.findByRole('dialog', {
      name: "Import samantha's history for sam?",
    });
    expect(
      within(dialog).getByText(/Importing for sam again later replaces this import/),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Apply import' }));
    expect(await screen.findByText('History imported for sam')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'POST' && c.path === '/admin/imports/7/apply')).toBe(
      true,
    );

    // It moves to the history, with Undo.
    const history = await screen.findByRole('region', { name: 'Past imports' });
    expect(
      await within(history).findByRole('button', { name: 'Undo the import from samantha for sam' }),
    ).toBeInTheDocument();
    // No token went anywhere but the two requests that need it.
    const withToken = calls.filter((c) => JSON.stringify(c.body ?? '').includes(TOKEN));
    expect(withToken.map((c) => c.path)).toEqual([
      '/admin/imports/abs/users',
      '/admin/imports/abs',
    ]);
  });

  it.each([
    ['invalid_url', 400, 'Audiobookshelf address', /starting with http:\/\/ or https:\/\//],
    ['abs_unreachable', 502, 'Audiobookshelf address', /couldn't reach that address/],
    ['not_abs', 502, 'Audiobookshelf address', /but not Audiobookshelf/],
    ['abs_unauthorized', 502, 'API token', /refused this token/],
    ['invalid_import', 400, 'API token', /doesn't look like an API token/],
  ])('explains %s on the field it is about', async (code, status, field, text) => {
    mockFetch(
      routes({
        'POST /admin/imports/abs/users': { status, body: { error: 'nope', code } },
      }),
    );
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    await connect(user);
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.getByLabelText(field)).toHaveAttribute('aria-invalid', 'true');
  });

  it('asks for the address and token before connecting', async () => {
    const calls = mockFetch(routes());
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Connect' }));
    expect(screen.getByText('Enter the Audiobookshelf address.')).toBeInTheDocument();
    expect(screen.getByText('Paste an API token from Audiobookshelf.')).toBeInTheDocument();
    expect(calls.some((c) => c.path === '/admin/imports/abs/users')).toBe(false);
  });

  it('refuses a mapping with nobody, or with one person twice', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/imports/abs/users': {
          body: {
            version: '2.17.2',
            users: [
              { abs_id: 'u1', username: 'samantha', type: 'user', suggested_user_id: null },
              { abs_id: 'u2', username: 'sammy', type: 'user', suggested_user_id: null },
            ],
          },
        },
        'POST /admin/imports/abs': { status: 202, body: { imports: [] } },
      }),
    );
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    await connect(user);
    await user.click(await screen.findByRole('button', { name: 'Fetch history for 0 people' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Choose an AudioSilo person for at least one account.',
    );
    await user.selectOptions(
      screen.getByRole('combobox', { name: 'AudioSilo person for samantha' }),
      'sam',
    );
    await user.selectOptions(
      screen.getByRole('combobox', { name: 'AudioSilo person for sammy' }),
      'sam',
    );
    expect(
      screen.getAllByText('Another account already goes to this person. Pick one of them.'),
    ).toHaveLength(2);
    await user.click(screen.getByRole('button', { name: 'Fetch history for 2 people' }));
    expect(
      await screen.findByText('Each AudioSilo person can take the history of one account only.'),
    ).toBeInTheDocument();
    // A day must be given when importing before a date.
    await user.selectOptions(
      screen.getByRole('combobox', { name: 'AudioSilo person for sammy' }),
      '',
    );
    await user.click(screen.getByRole('radio', { name: /Before a date/ }));
    await user.click(screen.getByRole('button', { name: 'Fetch history for 1 person' }));
    expect(await screen.findByText('Choose a date.')).toBeInTheDocument();
    expect(calls.some((c) => c.path === '/admin/imports/abs')).toBe(false);
  });

  it('says why a start was refused', async () => {
    mockFetch(
      routes({
        'POST /admin/imports/abs/users': {
          body: {
            version: '2.17.2',
            users: [{ abs_id: 'u1', username: 'sam', type: 'user', suggested_user_id: 2 }],
          },
        },
        'POST /admin/imports/abs': {
          status: 409,
          body: { error: 'running', code: 'import_running' },
        },
      }),
    );
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    await connect(user);
    await user.click(await screen.findByRole('button', { name: 'Fetch history for 1 person' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'An import for this person is already being fetched or applied.',
    );
  });
});

describe('import: review', () => {
  it('shows a failed import with its error and what to do, and discards it', async () => {
    const failed = imp({
      status: 'failed',
      summary: null,
      error: 'Audiobookshelf refused the token for this user.',
      error_code: 'abs_unauthorized',
    });
    let gone = false;
    const calls = mockFetch(
      routes({
        'GET /admin/imports': () => ({ body: { imports: gone ? [] : [failed] } }),
        'GET /admin/imports/7': { body: { ...failed, unmatched_items: [] } },
        'DELETE /admin/imports/7': () => {
          gone = true;
          return { status: 204 };
        },
      }),
    );
    renderApp('/server?topic=import');
    const card = await screen.findByRole('region', { name: 'samantha → sam' });
    expect(
      await within(card).findByText('Audiobookshelf refused the token for this user.'),
    ).toBeInTheDocument();
    expect(within(card).getByText(/Use an admin or root account's token/)).toBeInTheDocument();
    expect(within(card).queryByRole('button', { name: /Apply/ })).not.toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(within(card).getByRole('button', { name: 'Discard' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/imports/7')).toBe(true),
    );
    await waitFor(() =>
      expect(screen.queryByRole('region', { name: 'samantha → sam' })).not.toBeInTheDocument(),
    );
  });

  it('changes the cutoff and shows the recount', async () => {
    let recounted = false;
    const all = { cutoff: null, summary: summary({ sessions: 180, skipped_after_cutoff: 0 }) };
    const calls = mockFetch(
      routes({
        'GET /admin/imports': () => ({ body: { imports: [imp(recounted ? all : {})] } }),
        'GET /admin/imports/7': { body: detail() },
        'PATCH /admin/imports/7': () => {
          recounted = true;
          return { body: detail(all) };
        },
      }),
    );
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    const card = await screen.findByRole('region', { name: 'samantha → sam' });
    await user.click(
      await within(card).findByRole('button', { name: 'Change which listening to import for sam' }),
    );
    await user.click(within(card).getByRole('radio', { name: /Everything/ }));
    await user.click(within(card).getByRole('button', { name: 'Recount' }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ cutoff: null }),
    );
    expect(await within(card).findByText('180')).toBeInTheDocument();
    expect(within(card).getByText('All sessions, whenever they were')).toBeInTheDocument();
  });

  it('shows and keeps the cutoff day in server time', async () => {
    // 1 June, 00:00 at UTC+10: 31 May in the browser almost anywhere else.
    const zoned = { cutoff: '2026-05-31T14:00:00Z', cutoff_utc_offset: 600 };
    const calls = mockFetch(
      routes({
        'GET /admin/imports': { body: { imports: [imp(zoned)] } },
        'GET /admin/imports/7': { body: detail(zoned) },
        'PATCH /admin/imports/7': { body: detail(zoned) },
      }),
    );
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    const card = await screen.findByRole('region', { name: 'samantha → sam' });
    expect(await within(card).findByText('Sessions from before Jun 1, 2026')).toBeInTheDocument();
    await user.click(
      await within(card).findByRole('button', { name: 'Change which listening to import for sam' }),
    );
    expect(within(card).getByLabelText('Import sessions before')).toHaveValue('2026-06-01');
    await user.click(within(card).getByRole('button', { name: 'Recount' }));
    await waitFor(() =>
      expect(within(card).queryByRole('button', { name: 'Recount' })).not.toBeInTheDocument(),
    );
    expect(calls.some((c) => c.method === 'PATCH')).toBe(false);
  });

  it('sends a new day as the day itself, and "auto" as is', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/imports': { body: { imports: [imp()] } },
        'GET /admin/imports/7': { body: detail() },
        'PATCH /admin/imports/7': { body: detail() },
      }),
    );
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    const card = await screen.findByRole('region', { name: 'samantha → sam' });
    const change = { name: 'Change which listening to import for sam' };
    const patches = () => calls.filter((c) => c.method === 'PATCH').map((c) => c.body);

    await user.click(await within(card).findByRole('button', change));
    expect(within(card).getByRole('radio', { name: /Before a date/ })).toBeChecked();
    fireEvent.change(within(card).getByLabelText('Import sessions before'), {
      target: { value: '2026-05-01' },
    });
    await user.click(within(card).getByRole('button', { name: 'Recount' }));
    await waitFor(() => expect(patches()).toEqual([{ cutoff: '2026-05-01' }]));

    await user.click(await within(card).findByRole('button', change));
    await user.click(within(card).getByRole('radio', { name: /Before each person started/ }));
    await user.click(within(card).getByRole('button', { name: 'Recount' }));
    await waitFor(() => expect(patches()).toEqual([{ cutoff: '2026-05-01' }, { cutoff: 'auto' }]));
  });

  it('says what an apply replaces', async () => {
    mockFetch(
      routes({
        'GET /admin/imports': {
          body: {
            imports: [imp(), imp({ id: 3, status: 'applied', applied_at: '2026-03-02T10:00:00Z' })],
          },
        },
        'GET /admin/imports/7': { body: detail() },
      }),
    );
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    const card = await screen.findByRole('region', { name: 'samantha → sam' });
    await user.click(await within(card).findByRole('button', { name: 'Apply for sam' }));
    const dialog = await screen.findByRole('dialog');
    expect(
      within(dialog).getByText(/This replaces the Audiobookshelf history imported for sam on/),
    ).toBeInTheDocument();
  });
});

describe('import: undo', () => {
  it('undoes an applied import after confirming', async () => {
    let undone = false;
    const calls = mockFetch(
      routes({
        'GET /admin/imports': () => ({
          body: {
            imports: [
              imp({
                status: undone ? 'undone' : 'applied',
                applied_at: '2026-03-02T10:00:00Z',
              }),
            ],
          },
        }),
        'POST /admin/imports/7/undo': () => {
          undone = true;
          return { body: imp({ status: 'undone' }) };
        },
      }),
    );
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    const history = await screen.findByRole('region', { name: 'Past imports' });
    await user.click(
      await within(history).findByRole('button', { name: 'Undo the import from samantha for sam' }),
    );
    const dialog = await screen.findByRole('dialog', { name: 'Undo the import for sam?' });
    expect(within(dialog).getByText(/Progress sam changed since then is kept/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Undo import' }));
    expect(await screen.findByText('Import for sam undone')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'POST' && c.path === '/admin/imports/7/undo')).toBe(true);
    expect(
      await within(history).findByRole('button', {
        name: 'Delete the import from samantha for sam',
      }),
    ).toBeInTheDocument();
  });

  it('keeps the dialog open and says why an undo was refused', async () => {
    mockFetch(
      routes({
        'GET /admin/imports': {
          body: { imports: [imp({ status: 'applied', applied_at: '2026-03-02T10:00:00Z' })] },
        },
        'POST /admin/imports/7/undo': {
          status: 409,
          body: { error: 'not applied', code: 'import_not_applied' },
        },
      }),
    );
    renderApp('/server?topic=import');
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole('button', { name: 'Undo the import from samantha for sam' }),
    );
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Undo import' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      "This import isn't applied, so there's nothing to undo.",
    );
  });
});

describe("a person's imported history", () => {
  function personRoutes(over: Record<string, MockRoute> = {}) {
    return routes({
      'GET /admin/users/2': { body: samDetail() },
      'GET /admin/users/2/progress': { body: { progress: [] } },
      'GET /admin/sessions': { body: { sessions: [], next_before: null } },
      'GET /admin/listening': { body: activity() },
      ...over,
    });
  }

  it('is hidden when they have none', async () => {
    const calls = mockFetch(personRoutes());
    renderApp('/people/user/2');
    expect(await screen.findByRole('region', { name: 'Recent sessions' })).toBeInTheDocument();
    await waitFor(() =>
      expect(calls.find((c) => c.path === '/admin/imports')?.query.get('user_id')).toBe('2'),
    );
    expect(screen.queryByRole('region', { name: 'Imported history' })).not.toBeInTheDocument();
  });

  it('lists their imports with Undo and a way to the import settings', async () => {
    mockFetch(
      personRoutes({
        'GET /admin/imports': {
          body: { imports: [imp({ status: 'applied', applied_at: '2026-03-02T10:00:00Z' })] },
        },
      }),
    );
    const { router } = renderApp('/people/user/2');
    const section = await screen.findByRole('region', { name: 'Imported history' });
    expect(within(section).getByText('From samantha on Audiobookshelf')).toBeInTheDocument();
    expect(within(section).getByText(/45h of listening/)).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(
      within(section).getByRole('button', { name: 'Undo the import from samantha for sam' }),
    );
    expect(
      await screen.findByRole('dialog', { name: 'Undo the import for sam?' }),
    ).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await user.click(within(section).getByRole('link', { name: /Import settings/ }));
    await waitFor(() => expect(router.state.location.search).toEqual({ topic: 'import' }));
  });
});
