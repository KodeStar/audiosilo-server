import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminBook, BridgeJob, DuplicateGroup, SeriesSpellingGroup } from '@/api/types';
import { mockFetch, type MockReply, type MockRoute } from '@/test/fetch-mock';
import { issuesSummary, libraries, serverInfo } from '@/test/fixtures';
import { adminBook } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Health > Issues: categories, the triage queue, ignore with Undo, fixes,
// duplicates and the offline safety stop.

const broken = adminBook({
  path: 'Terry Pratchett/Guards! Guards!',
  title: 'Guards! Guards!',
  author: 'Terry Pratchett',
  scan_error: 'empty_file',
  scan_error_file: 'Terry Pratchett/Guards! Guards!/07.mp3',
});
const coverless = [
  adminBook({ path: 'A/One', title: 'Book One', has_cover: false }),
  adminBook({ path: 'A/Two', title: 'Book Two', has_cover: false }),
];

// A book ripped to disc folders, listed by its first disc.
const disc = adminBook({ path: 'Cowell/Dragonese/CD1', title: 'Dragonese' });

/** The issues with one book split across disc folders. */
function splitSummary() {
  const summary = issuesSummary();
  summary.categories.splice(2, 0, { kind: 'split_discs', count: 1, ignored: 0, samples: [] });
  return summary;
}

/** The issues with one series spelled several ways (its four books). */
function spellingsSummary() {
  const summary = issuesSummary();
  summary.categories.push({ kind: 'series_spellings', count: 4, ignored: 0, samples: [] });
  return summary;
}

/** A book page as POST .../book/rescan answers it. */
function rescanned(book: AdminBook) {
  return {
    book,
    fields: {},
    chapters: [],
    files: [],
    listeners: [],
    shares: [],
    folder: { path: '', override: '' },
    description: '',
    indexed_at: '',
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/books': (req) => {
      const issue = req.query.get('issue');
      if (issue === 'scan_error') return { body: { books: [broken] } };
      if (issue === 'no_cover') {
        return { body: { books: req.query.get('issue_ignored') ? [] : coverless } };
      }
      return { body: { books: [] } };
    },
    ...over,
  });
}

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('library health', () => {
  it('shows each category with its count and opens the first that needs attention', async () => {
    mockFetch(routes());
    renderApp('/health');
    expect(await screen.findByRole('heading', { name: 'Library health' })).toBeInTheDocument();
    expect(
      await screen.findByText('4 things could be better. None of it loses data.'),
    ).toBeInTheDocument();
    const card = screen.getByRole('button', { name: /Missing covers/ });
    expect(within(card).getByText('2')).toBeInTheDocument();
    expect(within(card).getByText('1 ignored')).toBeInTheDocument();
    // The first category with something in it is open: its book and why.
    expect(screen.getByRole('button', { name: /Files that couldn't be read/ })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    expect(await screen.findByText('07.mp3 is empty (0 bytes)')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Guards! Guards!' })).toBeInTheDocument();
  });

  it('ignores a book with an undo, and lists the ignored ones on request', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/issues/ignore': { status: 204 },
        'DELETE /admin/issues/ignore': { status: 204 },
      }),
    );
    const user = userEvent.setup();
    const { router } = renderApp('/health?issue=no_cover');
    const row = (await screen.findByText('Book One')).closest('li')!;
    await user.click(within(row).getByRole('button', { name: 'Ignore' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/issues/ignore')?.body,
      ).toEqual({
        kind: 'no_cover',
        books: [{ library_id: 1, path: 'A/One' }],
      }),
    );
    expect(await screen.findByText('Ignored Book One')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Undo' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/issues/ignore')).toBe(
        true,
      ),
    );

    await user.click(screen.getByRole('button', { name: 'Show ignored (1)' }));
    await waitFor(() =>
      expect(router.state.location.search).toMatchObject({ issue: 'no_cover', ignored: true }),
    );
    expect(await screen.findByText('Nothing ignored')).toBeInTheDocument();
    expect(calls.some((c) => c.query.get('issue_ignored') === 'true')).toBe(true);
  });

  it('ignores a selection from the floating bar', async () => {
    const calls = mockFetch(routes({ 'POST /admin/issues/ignore': { status: 204 } }));
    const user = userEvent.setup();
    renderApp('/health?issue=no_cover');
    await screen.findByText('Book One');
    await user.click(screen.getByRole('button', { name: 'Select all' }));
    const bar = screen.getByRole('toolbar', { name: 'Actions for the selected books' });
    expect(within(bar).getByText('2 selected')).toBeInTheDocument();
    await user.click(within(bar).getByRole('button', { name: 'Ignore' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/issues/ignore')?.body,
      ).toMatchObject({
        kind: 'no_cover',
        books: [
          { library_id: 1, path: 'A/One' },
          { library_id: 1, path: 'A/Two' },
        ],
      }),
    );
  });

  it('Shift-click selects the run of books from the last one clicked, as on Books', async () => {
    const four = ['One', 'Two', 'Three', 'Four'].map((n) =>
      adminBook({ path: `A/${n}`, title: `Book ${n}`, has_cover: false }),
    );
    mockFetch(
      routes({
        'GET /admin/books': (req) => ({
          body: { books: req.query.get('issue') === 'no_cover' ? four : [] },
        }),
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=no_cover');
    const box = (n: string) => screen.getByRole('checkbox', { name: `Select Book ${n}` });
    await screen.findByText('Book One');
    await user.click(box('Two'));
    // While selecting, Shift-mousedown on a row starts no text selection across the rows.
    const row = box('Four').closest('li')!;
    expect(fireEvent.mouseDown(row, { shiftKey: true })).toBe(false);
    expect(fireEvent.mouseDown(row)).toBe(true);
    await user.keyboard('{Shift>}');
    await user.click(box('Four'));
    await user.keyboard('{/Shift}');
    const bar = screen.getByRole('toolbar', { name: 'Actions for the selected books' });
    expect(within(bar).getByText('3 selected')).toBeInTheDocument();
    expect(box('One')).not.toBeChecked();
    expect(box('Three')).toBeChecked();
    // A Shift-click on a selected one clears the run back to it.
    await user.keyboard('{Shift>}');
    await user.click(box('Two'));
    await user.keyboard('{/Shift}');
    for (const n of ['Two', 'Three', 'Four']) expect(box(n)).not.toBeChecked();
  });

  it('reads an unreadable book again and says whether that fixed it', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/libraries/1/book/rescan': {
          body: {
            book: { ...broken, scan_error: undefined },
            fields: {},
            chapters: [],
            files: [],
            listeners: [],
            shares: [],
            folder: { path: '', override: '' },
            description: '',
            indexed_at: '',
          },
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=scan_error');
    await user.click(await screen.findByRole('button', { name: 'Read again' }));
    expect(await screen.findByText('Guards! Guards! reads fine now')).toBeInTheDocument();
    expect(calls.find((c) => c.path === '/admin/libraries/1/book/rescan')?.query.get('path')).toBe(
      'Terry Pratchett/Guards! Guards!',
    );
  });

  it('switches a selection to the community’s detailed chapters', async () => {
    const coarse = [
      adminBook({
        path: 'Fry/Mythos',
        title: 'Mythos',
        chapter_count: 34,
        chapters_check: 'refine',
      }),
      adminBook({
        path: 'Fry/Heroes',
        title: 'Heroes',
        chapter_count: 20,
        chapters_check: 'refine',
      }),
    ];
    const summary = issuesSummary();
    summary.categories.push({ kind: 'detailed_chapters', count: 2, ignored: 0, samples: [] });
    const calls = mockFetch(
      routes({
        'GET /admin/issues': { body: summary },
        'GET /admin/books': (req) => ({
          body: { books: req.query.get('issue') === 'detailed_chapters' ? coarse : [] },
        }),
        'POST /admin/books/bulk': { body: { updated: 2 } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=detailed_chapters');
    expect(
      await screen.findByText('Has 34 chapters; the community’s finer ones fit this copy'),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Select all' }));
    const bar = await screen.findByRole('toolbar');
    await user.click(within(bar).getByRole('button', { name: 'Use detailed chapters' }));
    expect(await screen.findByText('Using detailed chapters for 2 books')).toBeInTheDocument();
    const bulk = calls.find((c) => c.path === '/admin/books/bulk');
    expect(bulk?.body).toEqual({
      books: coarse.map((b) => ({ library_id: b.library_id, path: b.path })),
      chapter_source: 'community',
    });
  });

  describe('detect chapters', () => {
    const plain = [
      adminBook({ path: 'Mosse/The Black Gate', title: 'The Black Gate', duration: 4 * 3600 }),
      adminBook({ path: 'Mosse/Past Tense', title: 'Past Tense', duration: 2 * 3600 }),
    ];
    const bridgeServer = {
      ...serverInfo,
      capabilities: { ...serverInfo.capabilities, bridge: true },
      bridge: { version: '0.18.0', features: ['bridge', 'chapters', 'sidecars'] },
    };
    const bridgeStatus = {
      paired: true,
      paired_by: null,
      pairing: null,
      online: true,
      kinds: ['chapters.detect'],
      features: ['chapters'],
      libraries: [
        {
          id: 1,
          name: 'Audiobooks',
          root: '/books',
          mapping: {
            library_id: 1,
            mount_path: '/media/books',
            mapped: true,
            writable: true,
            checked_path: '',
            error: '',
          },
        },
      ],
      workers: [],
      estimates: { sidecars: { asr_seconds_per_audio_hour: 900 } },
    };
    // POST /admin/bridge/jobs answers the job itself (201 when it was queued).
    const queuedJob = (path: string): MockReply => ({
      status: 201,
      body: {
        id: 5,
        kind: 'chapters.detect',
        library_id: 1,
        path,
        params: {},
        state: 'queued',
        progress: {},
        result: {},
        error: '',
        requested_by: { id: 1, username: 'chris' },
        priority: 0,
        attempts: 0,
        lease_until: null,
        cancel_requested: false,
        created_at: '2026-10-17T09:00:00Z',
        updated_at: '2026-10-17T09:00:00Z',
        claimed_at: null,
        finished_at: null,
      } satisfies BridgeJob,
    });
    function detectRoutes(over: Record<string, MockRoute> = {}) {
      const summary = issuesSummary();
      summary.categories[4] = { kind: 'no_chapters', count: 2, ignored: 0, samples: [] };
      return routes({
        'GET /server': { body: bridgeServer },
        'GET /admin/issues': { body: summary },
        'GET /admin/books': (req) => ({
          body: { books: req.query.get('issue') === 'no_chapters' ? plain : [] },
        }),
        'GET /admin/bridge': { body: bridgeStatus },
        ...over,
      });
    }

    it('confirms with the transcription time and no agent cost, then queues chapters.detect', async () => {
      const calls = mockFetch(
        detectRoutes({
          'POST /admin/bridge/jobs': (req) => queuedJob((req.body as { path: string }).path),
        }),
      );
      const user = userEvent.setup();
      renderApp('/health?issue=no_chapters');
      const row = (await screen.findByRole('link', { name: 'The Black Gate' })).closest('li')!;
      // The footer says where the found chapters go.
      expect(
        screen.getByText(
          "Showing 2. Detect chapters saves the chapters it finds to the book's .audiosilo folder; nothing else on disk changes.",
        ),
      ).toBeInTheDocument();
      await user.click(within(row).getByRole('button', { name: 'Detect chapters' }));
      const dialog = await screen.findByRole('dialog');
      expect(within(dialog).getByText('None: no agent is used')).toBeInTheDocument();
      expect(await within(dialog).findByText(/^About /)).toBeInTheDocument();
      await user.click(within(dialog).getByRole('button', { name: 'Detect chapters' }));
      expect(await screen.findByText('Finding the chapters of 1 book')).toBeInTheDocument();
      const queued = calls.filter((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs');
      expect(queued.map((c) => c.body)).toEqual([
        { kind: 'chapters.detect', library_id: 1, path: 'Mosse/The Black Gate', params: {} },
      ]);
    });

    it('queues a selection from the bar, passing over a book whose generate job finds them', async () => {
      const calls = mockFetch(
        detectRoutes({
          'POST /admin/bridge/jobs': (req) =>
            (req.body as { path: string }).path === 'Mosse/Past Tense'
              ? { status: 409, body: { error: 'a generate job is open', code: 'generate_open' } }
              : queuedJob((req.body as { path: string }).path),
        }),
      );
      const user = userEvent.setup();
      renderApp('/health?issue=no_chapters');
      await screen.findByRole('link', { name: 'The Black Gate' });
      await user.click(screen.getByRole('button', { name: 'Select all' }));
      const bar = await screen.findByRole('toolbar');
      await user.click(within(bar).getByRole('button', { name: 'Detect chapters' }));
      const dialog = await screen.findByRole('dialog');
      expect(within(dialog).getByText('2 books')).toBeInTheDocument();
      await user.click(within(dialog).getByRole('button', { name: 'Detect chapters' }));
      expect(await screen.findByText('Finding the chapters of 1 book')).toBeInTheDocument();
      expect(
        screen.getByText('Being generated, which finds the chapters too: Past Tense.'),
      ).toBeInTheDocument();
      expect(
        calls.filter((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs'),
      ).toHaveLength(2);
    });

    it('never counts a parked detect job that comes back as queued, and passes over a book being described', async () => {
      mockFetch(
        detectRoutes({
          'POST /admin/bridge/jobs': (req) => {
            const path = (req.body as { path: string }).path;
            if (path === 'Mosse/Past Tense') {
              return {
                status: 409,
                body: { error: 'a description job is open', code: 'describe_open' },
              };
            }
            // A second press on a parked job answers that job (200, not created).
            const parked = (queuedJob(path) as { body: BridgeJob }).body;
            return { status: 200, body: { ...parked, state: 'needs_attention' } };
          },
        }),
      );
      const user = userEvent.setup();
      renderApp('/health?issue=no_chapters');
      await screen.findByRole('link', { name: 'The Black Gate' });
      await user.click(screen.getByRole('button', { name: 'Select all' }));
      const bar = await screen.findByRole('toolbar');
      await user.click(within(bar).getByRole('button', { name: 'Detect chapters' }));
      const dialog = await screen.findByRole('dialog');
      await user.click(within(dialog).getByRole('button', { name: 'Detect chapters' }));
      expect(await screen.findByText('Nothing was queued')).toBeInTheDocument();
      expect(
        screen.getByText(
          'Waiting for attention on the Bridge: The Black Gate. A description is being written (try again once it has finished): Past Tense.',
        ),
      ).toBeInTheDocument();
      expect(screen.queryByText(/Finding the chapters of/)).not.toBeInTheDocument();
    });

    it('offers no Detect chapters for a book whose chapter source was chosen, and says why', async () => {
      const chosen = [{ ...plain[0], chapter_choice: 'files' as const }, plain[1]];
      const calls = mockFetch(
        detectRoutes({
          'GET /admin/books': (req) => ({
            body: { books: req.query.get('issue') === 'no_chapters' ? chosen : [] },
          }),
          'POST /admin/bridge/jobs': (req) => queuedJob((req.body as { path: string }).path),
        }),
      );
      const user = userEvent.setup();
      renderApp('/health?issue=no_chapters');
      const row = (await screen.findByRole('link', { name: 'The Black Gate' })).closest('li')!;
      expect(
        within(row).queryByRole('button', { name: 'Detect chapters' }),
      ).not.toBeInTheDocument();
      expect(
        within(row).getByText(
          'Uses the file’s chapters by choice. Set it back to automatic on the book page to detect chapters.',
        ),
      ).toBeInTheDocument();
      const other = screen.getByRole('link', { name: 'Past Tense' }).closest('li')!;
      expect(within(other).getByRole('button', { name: 'Detect chapters' })).toBeInTheDocument();
      // The bar asks only for the book it can.
      await user.click(screen.getByRole('button', { name: 'Select all' }));
      const bar = await screen.findByRole('toolbar');
      await user.click(within(bar).getByRole('button', { name: 'Detect chapters' }));
      const dialog = await screen.findByRole('dialog');
      expect(within(dialog).getByText('Past Tense')).toBeInTheDocument();
      await user.click(within(dialog).getByRole('button', { name: 'Detect chapters' }));
      expect(await screen.findByText('Finding the chapters of 1 book')).toBeInTheDocument();
      expect(
        calls
          .filter((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs')
          .map((c) => (c.body as { path: string }).path),
      ).toEqual(['Mosse/Past Tense']);
    });

    it('stays open while it queues, so its outcome lands in the dialog that started it', async () => {
      // The books' POSTs wait until the test lets them through.
      let release: () => void = () => {};
      const held = new Promise<void>((resolve) => (release = resolve));
      mockFetch(
        detectRoutes({
          'POST /admin/bridge/jobs': async (req) => {
            await held;
            return queuedJob((req.body as { path: string }).path);
          },
        }),
      );
      const user = userEvent.setup();
      renderApp('/health?issue=no_chapters');
      await screen.findByRole('link', { name: 'The Black Gate' });
      await user.click(screen.getByRole('button', { name: 'Select all' }));
      const bar = await screen.findByRole('toolbar');
      await user.click(within(bar).getByRole('button', { name: 'Detect chapters' }));
      const confirm = { name: 'Detect chapters' };
      const dialog = await screen.findByRole('dialog', confirm);
      await user.click(within(dialog).getByRole('button', { name: 'Detect chapters' }));
      // Cancel and Escape do nothing while it queues.
      await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
      await user.keyboard('{Escape}');
      expect(screen.getByRole('dialog', confirm)).toBeInTheDocument();
      release();
      expect(await screen.findByText('Finding the chapters of 2 books')).toBeInTheDocument();
      await waitFor(() => expect(screen.queryByRole('dialog', confirm)).not.toBeInTheDocument());
      // The bar's selection was queued, so it is done with.
      expect(screen.queryByRole('toolbar')).not.toBeInTheDocument();
    });

    it('offers no fix while the Bridge does not offer chapter detection', async () => {
      mockFetch(detectRoutes({ 'GET /server': { body: serverInfo } }));
      renderApp('/health?issue=no_chapters');
      await screen.findByRole('link', { name: 'The Black Gate' });
      expect(screen.queryByRole('button', { name: 'Detect chapters' })).not.toBeInTheDocument();
      expect(
        screen.getByText('Showing 2. Fixes are saved in AudioSilo; nothing on disk changes.'),
      ).toBeInTheDocument();
    });
  });

  it('joins a book split across disc folders into one', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/issues': { body: splitSummary() },
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'split_discs'
            ? { body: { books: [disc] } }
            : { body: { books: [] } },
        'PUT /admin/libraries/1/folder-override': { body: { status: 'override set' } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=split_discs');
    expect(
      await screen.findByText('Dragonese reads as one book per disc folder'),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('button', { name: /One book split into disc folders/ }),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Join into one book' }));
    expect(await screen.findByText('Joining the discs of Dragonese')).toBeInTheDocument();
    const put = calls.find((c) => c.method === 'PUT');
    expect(put?.query.get('path')).toBe('Cowell/Dragonese');
    expect(put?.body).toEqual({ mode: 'book' });
    // The issues are fetched again once the folder is set.
    await waitFor(() =>
      expect(calls.filter((c) => c.path === '/admin/issues').length).toBeGreaterThan(1),
    );
  });

  it('sends one join while the first is in flight', async () => {
    let answer: (reply: MockReply) => void = () => {};
    const calls = mockFetch(
      routes({
        'GET /admin/issues': { body: splitSummary() },
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'split_discs'
            ? { body: { books: [disc] } }
            : { body: { books: [] } },
        'PUT /admin/libraries/1/folder-override': () =>
          new Promise<MockReply>((resolve) => (answer = resolve)),
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=split_discs');
    const join = await screen.findByRole('button', { name: 'Join into one book' });
    await user.click(join);
    await waitFor(() => expect(join).toBeDisabled());
    await user.click(join);
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(1);
    answer({ body: { status: 'override set' } });
    expect(await screen.findByText('Joining the discs of Dragonese')).toBeInTheDocument();
    await waitFor(() => expect(join).toBeEnabled());
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(1);
  });

  it('says when joining the discs failed', async () => {
    mockFetch(
      routes({
        'GET /admin/issues': { body: splitSummary() },
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'split_discs'
            ? { body: { books: [disc] } }
            : { body: { books: [] } },
        'PUT /admin/libraries/1/folder-override': { status: 500, body: { error: 'disk full' } },
      }),
    );
    renderApp('/health?issue=split_discs');
    await userEvent
      .setup()
      .click(await screen.findByRole('button', { name: 'Join into one book' }));
    expect(await screen.findByText("Couldn't join the discs of Dragonese")).toBeInTheDocument();
  });

  it('drops a book the re-read fixed from its list', async () => {
    let fixed = false;
    mockFetch(
      routes({
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'scan_error' && !fixed
            ? { body: { books: [broken] } }
            : { body: { books: [] } },
        'POST /admin/libraries/1/book/rescan': () => {
          fixed = true;
          return { body: rescanned({ ...broken, scan_error: undefined }) };
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=scan_error');
    await user.click(await screen.findByRole('button', { name: 'Read again' }));
    expect(await screen.findByText('Guards! Guards! reads fine now')).toBeInTheDocument();
    // The list is fetched again, not patched in place with a book it no longer holds.
    expect(await screen.findByText('All clear')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Guards! Guards!' })).not.toBeInTheDocument();
  });

  it('reads a selection again a couple of books at a time', async () => {
    const many = Array.from({ length: 5 }, (_, i) =>
      adminBook({ ...broken, path: `Broken/${i}`, title: `Broken ${i}` }),
    );
    let inFlight = 0;
    let most = 0;
    const calls = mockFetch(
      routes({
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'scan_error'
            ? { body: { books: many } }
            : { body: { books: [] } },
        'POST /admin/libraries/1/book/rescan': async (req) => {
          most = Math.max(most, ++inFlight);
          await new Promise((r) => setTimeout(r, 20));
          inFlight--;
          const b = many.find((m) => m.path === req.query.get('path'))!;
          return { body: rescanned({ ...b, scan_error: undefined }) };
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=scan_error');
    await screen.findByText('Broken 0');
    await user.click(screen.getByRole('button', { name: 'Select all' }));
    const bar = screen.getByRole('toolbar', { name: 'Actions for the selected books' });
    await user.click(within(bar).getByRole('button', { name: 'Read again' }));
    await waitFor(() =>
      expect(calls.filter((c) => c.path === '/admin/libraries/1/book/rescan')).toHaveLength(5),
    );
    await waitFor(() => expect(inFlight).toBe(0));
    expect(most).toBeLessThanOrEqual(2);
  });

  it('keeps the category it opened on its own once it is cleared', async () => {
    let ignored = false;
    const summary = () =>
      issuesSummary({
        categories: issuesSummary().categories.map((c) =>
          c.kind === 'scan_error' ? { ...c, count: ignored ? 0 : 1, ignored: ignored ? 1 : 0 } : c,
        ),
      });
    mockFetch(
      routes({
        'GET /admin/issues': () => ({ body: summary() }),
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'scan_error' && !ignored
            ? { body: { books: [broken] } }
            : { body: { books: [] } },
        'POST /admin/issues/ignore': () => {
          ignored = true;
          return { status: 204 };
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health');
    const row = (await screen.findByRole('link', { name: 'Guards! Guards!' })).closest('li')!;
    await user.click(within(row).getByRole('button', { name: 'Ignore' }));
    // It stays on the category just cleared, rather than jumping to the next one.
    expect(await screen.findByText('All clear')).toBeInTheDocument();
    expect(
      screen.getByRole('heading', { name: "Files that couldn't be read" }),
    ).toBeInTheDocument();
  });

  it('compares duplicates side by side and stops suggesting ones that differ', async () => {
    const group: DuplicateGroup = {
      reason: 'same_book',
      ignored: false,
      books: [
        { ...adminBook({ path: 'Dune', title: 'Dune', format: 'm4b' }), listeners: 2 },
        {
          ...adminBook({ path: 'Dune (mp3)', title: 'Dune', format: 'mp3', file_count: 12 }),
          listeners: 0,
        },
      ],
    };
    const calls = mockFetch(
      routes({
        'GET /admin/issues/duplicates': { body: { groups: [group] } },
        'POST /admin/issues/ignore': { status: 204 },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=duplicate');
    const card = (await screen.findByRole('heading', { name: 'Dune' })).closest('section')!;
    expect(within(card).getByText('Worth keeping')).toBeInTheDocument();
    expect(within(card).getByText('Another copy')).toBeInTheDocument();
    expect(within(card).getByText(/same title, author and length/)).toBeInTheDocument();
    await user.click(within(card).getByRole('button', { name: "They're different books" }));
    await waitFor(() =>
      expect(calls.find((c) => c.path === '/admin/issues/ignore')?.body).toEqual({
        kind: 'duplicate',
        books: [
          { library_id: 1, path: 'Dune' },
          { library_id: 1, path: 'Dune (mp3)' },
        ],
      }),
    );
  });

  it("groups copies across libraries only when asked, naming each copy's library (#165)", async () => {
    const across: DuplicateGroup = {
      reason: 'same_files',
      ignored: false,
      books: [
        { ...adminBook({ path: 'Dune', title: 'Dune', format: 'm4b' }), listeners: 1 },
        {
          ...adminBook({ path: 'Dune', title: 'Dune', format: 'm4b' }),
          library_id: 2,
          library_name: 'Kids',
          listeners: 0,
        },
      ],
    };
    const calls = mockFetch(
      routes({
        'GET /admin/issues/duplicates': (req) => ({
          body: { groups: req.query.get('across') === '1' ? [across] : [] },
        }),
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=duplicate');
    const toggle = await screen.findByRole('switch', { name: 'Include other libraries' });
    expect(toggle).not.toBeChecked();
    expect(await screen.findByText('All clear')).toBeInTheDocument();
    expect(
      calls
        .filter((c) => c.path === '/admin/issues/duplicates')
        .at(-1)
        ?.query.get('across'),
    ).toBeNull();
    expect(screen.getByText(/^Copies of one book in the same library/)).toBeInTheDocument();
    await user.click(toggle);
    expect(
      await screen.findByText(/^Copies of one book, in the same library or in different ones/),
    ).toBeInTheDocument();
    await waitFor(() => expect(toggle).toBeChecked());
    expect(
      calls
        .filter((c) => c.path === '/admin/issues/duplicates')
        .at(-1)
        ?.query.get('across'),
    ).toBe('1');
    const card = (await screen.findByRole('heading', { name: 'Dune' })).closest('section')!;
    expect(within(card).getAllByText('Library')).toHaveLength(2);
    expect(within(card).getByText('Kids')).toBeInTheDocument();
    expect(within(card).getByText(across.books[0].library_name)).toBeInTheDocument();
  });

  it('reaches the groups ignored across libraries, which the count (within each) leaves out', async () => {
    const across: DuplicateGroup = {
      reason: 'same_files',
      ignored: true,
      books: [
        { ...adminBook({ path: 'Dune', title: 'Dune', format: 'm4b' }), listeners: 1 },
        {
          ...adminBook({ path: 'Dune', title: 'Dune', format: 'm4b' }),
          library_id: 2,
          library_name: 'Kids',
          listeners: 0,
        },
      ],
    };
    mockFetch(
      routes({
        'GET /admin/issues/duplicates': (req) => ({
          body: {
            groups:
              req.query.get('across') === '1' && req.query.get('ignored') === 'true'
                ? [across]
                : [],
          },
        }),
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=duplicate');
    await screen.findByText('All clear');
    // The summary counts no ignored group: nothing to show until across is on.
    expect(screen.queryByRole('button', { name: /^Show ignored/ })).toBeNull();
    await user.click(await screen.findByRole('switch', { name: 'Include other libraries' }));
    await user.click(await screen.findByRole('button', { name: 'Show ignored' }));
    const card = (await screen.findByRole('heading', { name: 'Dune' })).closest('section')!;
    expect(within(card).getByText('Kids')).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Include other libraries' })).toBeChecked();
  });

  it('words each duplicate reason, and suggests no copy to keep when the lengths differ', async () => {
    const pair = (title: string, a: string, b: string, lengths = [36000, 36000]) => [
      { ...adminBook({ path: a, title, duration: lengths[0] }), listeners: 0 },
      { ...adminBook({ path: b, title, duration: lengths[1] }), listeners: 1 },
    ];
    // The reported pair: one ASIN, 5h 57m against 7h 13m.
    const burning = [21420, 25980];
    const groups: DuplicateGroup[] = [
      {
        reason: 'id_length_differs',
        ignored: false,
        books: pair('The Burning', 'The Burning [Dramatized Adaptation]', 'The Burning', burning),
      },
      { reason: 'same_id', ignored: false, books: pair('Dune', 'Dune', 'Dune (mp3)') },
      { reason: 'same_files', ignored: false, books: pair('Emma', 'Emma', 'Emma (copy)') },
      // Said to be different books: what to check is moot.
      { reason: 'id_length_differs', ignored: true, books: pair('Mythos', 'A', 'B', burning) },
    ];
    mockFetch(routes({ 'GET /admin/issues/duplicates': { body: { groups } } }));
    renderApp('/health?issue=duplicate');
    const card = async (title: string) =>
      (await screen.findByRole('heading', { name: title })).closest('section')!;

    const differs = await card('The Burning');
    expect(
      within(differs).getByText('2 books sharing an ASIN or ISBN, but of different lengths'),
    ).toBeInTheDocument();
    expect(within(differs).getByText(/may be different recordings/)).toBeInTheDocument();
    expect(within(differs).queryByText('Worth keeping')).not.toBeInTheDocument();
    expect(within(differs).queryByText('Another copy')).not.toBeInTheDocument();
    expect(
      within(differs).getByRole('button', { name: "They're different books" }),
    ).toBeInTheDocument();

    const sameId = await card('Dune');
    expect(
      within(sameId).getByText('2 copies sharing an ASIN or ISBN, of the same length'),
    ).toBeInTheDocument();
    expect(within(sameId).getByText('Worth keeping')).toBeInTheDocument();
    expect(within(sameId).queryByText(/may be different recordings/)).not.toBeInTheDocument();

    const sameFiles = await card('Emma');
    expect(within(sameFiles).getByText('2 copies with identical audio files')).toBeInTheDocument();
    expect(within(sameFiles).getByText('Another copy')).toBeInTheDocument();

    const ignored = await card('Mythos');
    expect(
      within(ignored).getByRole('button', { name: 'Suggest as duplicates again' }),
    ).toBeInTheDocument();
    expect(within(ignored).queryByText(/may be different recordings/)).not.toBeInTheDocument();
    expect(within(ignored).queryByText('Worth keeping')).not.toBeInTheDocument();
  });

  it('sets one spelling on every book of a series spelled several ways', async () => {
    const group: SeriesSpellingGroup = {
      library_id: 1,
      suggested: 'He Who Fights with Monsters',
      books: 4,
      spellings: [
        { name: 'He Who Fights With Monsters', books: 2 },
        { name: 'He Who Fights with Monsters', books: 1 },
        { name: 'He Who Fights WIth Monsters', books: 1 },
      ],
    };
    const tagged = [
      adminBook({ path: 'M/1', title: 'One', series: 'He Who Fights with Monsters' }),
      adminBook({ path: 'M/2', title: 'Two', series: 'He Who Fights With Monsters' }),
      adminBook({ path: 'M/3', title: 'Three', series: 'He Who Fights WIth Monsters' }),
      adminBook({ path: 'M/4', title: 'Four', series: 'He Who Fights With Monsters' }),
      // In the series through its list only: the rename leaves it be.
      adminBook({ path: 'Other', title: 'Other', series: 'Something Else' }),
    ];
    const calls = mockFetch(
      routes({
        'GET /admin/issues': { body: spellingsSummary() },
        'GET /admin/issues/series-spellings': { body: { groups: [group] } },
        // The series filter finds every spelling, as the server's does.
        'GET /admin/books': (req) => ({
          body: { books: req.query.get('series') ? tagged : [] },
        }),
        'POST /admin/books/bulk': { body: { updated: 0 } },
        'POST /admin/issues/ignore': { status: 204 },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=series_spellings');
    expect(
      await screen.findByText(
        '“He Who Fights With Monsters” and “He Who Fights WIth Monsters” look like He Who Fights with Monsters',
      ),
    ).toBeInTheDocument();
    expect(screen.getByText(/3 books are tagged with the other spelling/)).toBeInTheDocument();

    // Another spelling to keep: the count follows.
    const keep = screen.getByRole('combobox', { name: 'Spelling to keep' });
    await user.selectOptions(keep, 'He Who Fights With Monsters');
    expect(screen.getByText(/2 books are tagged with the other spelling/)).toBeInTheDocument();
    await user.selectOptions(keep, 'He Who Fights with Monsters');

    await user.click(screen.getByRole('button', { name: 'Use this spelling' }));
    await waitFor(() =>
      expect(calls.find((c) => c.path === '/admin/books/bulk')?.body).toEqual({
        books: [
          { library_id: 1, path: 'M/2' },
          { library_id: 1, path: 'M/3' },
          { library_id: 1, path: 'M/4' },
        ],
        set: { series: 'He Who Fights with Monsters' },
      }),
    );

    // Ignore stops suggesting it: every book of the series, whatever its spelling.
    await user.click(screen.getByRole('button', { name: 'Ignore' }));
    await waitFor(() =>
      expect(calls.find((c) => c.path === '/admin/issues/ignore')?.body).toEqual({
        kind: 'series_spellings',
        books: ['M/1', 'M/2', 'M/3', 'M/4'].map((path) => ({ library_id: 1, path })),
      }),
    );
  });

  it('celebrates an offline library as a safety stop, with a retry', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/issues': {
          body: issuesSummary({
            offline: [
              { library_id: 2, name: 'Kids', root: '/mnt/nas/kids', books: 849, listeners: 3 },
            ],
          }),
        },
        'GET /admin/libraries': { body: { libraries: libraries([{}, { available: false }]) } },
        'POST /admin/libraries/2/scan': { status: 202, body: { status: 'scan started' } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health');
    expect(await screen.findByText('Kids is offline. Nothing was deleted.')).toBeInTheDocument();
    expect(screen.getByText(/kept all 849 books/)).toBeInTheDocument();
    expect(screen.getByText(/Progress for 3 listeners is safe/)).toBeInTheDocument();
    const notice = screen
      .getByText('Kids is offline. Nothing was deleted.')
      .closest('div')!.parentElement!;
    await user.click(within(notice).getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(calls.some((c) => c.path === '/admin/libraries/2/scan')).toBe(true));
  });

  it('acts only on the selected books still listed', async () => {
    let ignoredOne = false;
    const calls = mockFetch(
      routes({
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'no_cover' && !req.query.get('issue_ignored')
            ? { body: { books: ignoredOne ? coverless.slice(1) : coverless } }
            : { body: { books: [] } },
        'POST /admin/issues/ignore': () => {
          ignoredOne = true;
          return { status: 204 };
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=no_cover');
    await screen.findByText('Book One');
    await user.click(screen.getByRole('button', { name: 'Select all' }));
    // Ignore Book One on its own row: it leaves the list but was selected.
    const row = screen.getByRole('link', { name: 'Book One' }).closest('li')!;
    await user.click(within(row).getByRole('button', { name: 'Ignore' }));
    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'Book One' })).not.toBeInTheDocument(),
    );
    const bar = screen.getByRole('toolbar', { name: 'Actions for the selected books' });
    expect(within(bar).getByText('1 selected')).toBeInTheDocument();
    await user.click(within(bar).getByRole('button', { name: 'Ignore' }));
    await waitFor(() =>
      expect(calls.filter((c) => c.path === '/admin/issues/ignore').at(-1)?.body).toEqual({
        kind: 'no_cover',
        books: [{ library_id: 1, path: 'A/Two' }],
      }),
    );
  });
});
