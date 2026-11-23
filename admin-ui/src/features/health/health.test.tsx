import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { onTestFinished } from 'vitest';
import { setToken } from '@/api/token';
import type { AdminBook, BridgeJob, DuplicateGroup, SeriesSpellingGroup } from '@/api/types';
import { mockFetch, type MockReply, type MockRoute } from '@/test/fetch-mock';
import { issuesSummary, libraries, serverInfo } from '@/test/fixtures';
import { adminBook } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';
import { shiftClick } from '@/test/user';

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

  it('lists a book whose ASIN the community database lacks, to match by hand or add (server#370)', async () => {
    const summary = issuesSummary();
    summary.categories.push({ kind: 'unknown_asin', count: 1, ignored: 0, samples: [] });
    const unknown = adminBook({
      path: 'X/Rare',
      title: 'Rare Book',
      author: 'R. Author',
      asin: 'B0RARE0001',
    });
    mockFetch(
      routes({
        'GET /admin/issues': { body: summary },
        'GET /admin/books': (req) => ({
          body: { books: req.query.get('issue') === 'unknown_asin' ? [unknown] : [] },
        }),
      }),
    );
    renderApp('/health?issue=unknown_asin');
    const card = await screen.findByRole('button', { name: /ASIN not in the community database/ });
    expect(card).toHaveAttribute('aria-pressed', 'true');
    const row = (
      await screen.findByText("Its ASIN, B0RARE0001, isn't in the community database yet")
    ).closest('li')!;
    expect(
      within(row).getByRole('link', { name: /^Add Rare Book to the community database/ }),
    ).toHaveAttribute('href', 'https://meta.audiosilo.app/add?q=B0RARE0001');
    expect(within(row).getByRole('button', { name: 'Review match' })).toBeInTheDocument();
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
    await shiftClick(user, box('Four'));
    const bar = screen.getByRole('toolbar', { name: 'Actions for the selected books' });
    expect(within(bar).getByText('3 selected')).toBeInTheDocument();
    expect(box('One')).not.toBeChecked();
    expect(box('Three')).toBeChecked();
    // A Shift-click on a selected one clears the run back to it.
    await shiftClick(user, box('Two'));
    for (const n of ['Two', 'Three', 'Four']) expect(box(n)).not.toBeChecked();
  });

  it('shows a file that is not audio as a plain line, the raw message behind Details', async () => {
    const notAudio = {
      ...broken,
      scan_error: 'probe_failed' as const,
      scan_error_detail: '[mp3 @ 0xcbcc3c000] Failed to find two consecutive MPEG audio frames.',
    };
    mockFetch(routes({ 'GET /admin/books': () => ({ body: { books: [notAudio] } }) }));
    const user = userEvent.setup();
    renderApp('/health?issue=scan_error');
    expect(
      await screen.findByText(
        "07.mp3 couldn't be read as audio. It may be damaged or not an audio file.",
      ),
    ).toBeInTheDocument();
    const raw = '[mp3] Failed to find two consecutive MPEG audio frames.';
    expect(screen.getByText(raw)).not.toBeVisible();
    const toggle = screen.getByRole('button', { name: 'Details' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await user.click(toggle);
    expect(screen.getByText(raw)).toBeVisible();
    expect(screen.getByRole('button', { name: 'Hide details' })).toHaveAttribute(
      'aria-expanded',
      'true',
    );
    expect(screen.queryByText(/0xcbcc3c000/)).not.toBeInTheDocument();
  });

  it('says a file that was not read could not be read, and why, with no Details', async () => {
    const stuck = {
      ...broken,
      scan_error: 'unreadable' as const,
      scan_error_detail:
        "too many earlier reads from the same drive or share are stuck, so it wasn't tried",
    };
    mockFetch(routes({ 'GET /admin/books': () => ({ body: { books: [stuck] } }) }));
    renderApp('/health?issue=scan_error');
    expect(
      await screen.findByText(
        "07.mp3 couldn't be read: too many earlier reads from the same drive or share are stuck, so it wasn't tried",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Details' })).not.toBeInTheDocument();
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
      expect(within(dialog).queryByText(/No worker is connected/)).not.toBeInTheDocument();
      await user.click(within(dialog).getByRole('button', { name: 'Detect chapters' }));
      expect(await screen.findByText('Finding the chapters of 1 book')).toBeInTheDocument();
      const queued = calls.filter((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs');
      expect(queued.map((c) => c.body)).toEqual([
        { kind: 'chapters.detect', library_id: 1, path: 'Mosse/The Black Gate', params: {} },
      ]);
    });

    it('says the job waits when the Bridge waits for an ASR worker and none is connected (BRIDGE-10)', async () => {
      mockFetch(
        detectRoutes({
          'GET /admin/bridge': {
            body: {
              ...bridgeStatus,
              setup: {
                items: [
                  { id: 'asr', done: false, required: true, detail: 'waiting for an ASR worker' },
                ],
                can_contribute: false,
              },
            },
          },
        }),
      );
      const user = userEvent.setup();
      renderApp('/health?issue=no_chapters');
      const row = (await screen.findByRole('link', { name: 'The Black Gate' })).closest('li')!;
      await user.click(within(row).getByRole('button', { name: 'Detect chapters' }));
      const dialog = await screen.findByRole('dialog');
      expect(
        await within(dialog).findByText(
          'No worker is connected: the job waits until one connects, then takes about the time above.',
        ),
      ).toBeInTheDocument();
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

    it('passes over a book whose chapters were edited since the list was read (409 chapters_edited)', async () => {
      mockFetch(
        detectRoutes({
          'POST /admin/bridge/jobs': (req) =>
            (req.body as { path: string }).path === 'Mosse/Past Tense'
              ? { status: 409, body: { error: 'chapters edited', code: 'chapters_edited' } }
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
      await user.click(within(dialog).getByRole('button', { name: 'Detect chapters' }));
      expect(await screen.findByText('Finding the chapters of 1 book')).toBeInTheDocument();
      expect(
        screen.getByText('Their chapters were edited on the book page: Past Tense.'),
      ).toBeInTheDocument();
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

    // A detection as it comes on its book's row (the book's newest job).
    const rowJob = (path: string, over: Partial<BridgeJob>): BridgeJob => ({
      ...(queuedJob(path) as { body: BridgeJob }).body,
      path,
      ...over,
    });

    it("shows each row's detection, holding Detect back while one is open (BRIDGE-08)", async () => {
      let state: BridgeJob['state'] = 'queued';
      const calls = mockFetch(
        detectRoutes({
          'GET /admin/books': (req) => ({
            body: {
              books:
                req.query.get('issue') === 'no_chapters'
                  ? [
                      { ...plain[0], detect_job: rowJob(plain[0].path, { id: 7, state }) },
                      {
                        ...plain[1],
                        detect_job: rowJob(plain[1].path, {
                          id: 6,
                          state: 'failed',
                          error: 'transcription failed',
                        }),
                      },
                    ]
                  : [],
            },
          }),
        }),
      );
      const user = userEvent.setup();
      const { queryClient } = renderApp('/health?issue=no_chapters');
      const gate = (await screen.findByRole('link', { name: 'The Black Gate' })).closest('li')!;
      const past = screen.getByRole('link', { name: 'Past Tense' }).closest('li')!;
      // The queued one says so, with no second Detect; the failed one says why, and offers it again.
      expect(await within(gate).findByText('Chapter detection')).toBeInTheDocument();
      expect(within(gate).queryByRole('button', { name: 'Detect chapters' })).toBeNull();
      expect(within(past).getByText('transcription failed')).toBeInTheDocument();
      expect(within(past).getByRole('button', { name: 'Detect chapters' })).toBeInTheDocument();
      // The rows carry them: no job list is read for the list, whatever its size.
      expect(calls.some((c) => c.path === '/admin/bridge/jobs')).toBe(false);
      // The bar's Detect leaves the open one out.
      await user.click(screen.getByRole('button', { name: 'Select all' }));
      const bar = await screen.findByRole('toolbar');
      await user.click(within(bar).getByRole('button', { name: 'Detect chapters' }));
      // One book: the dialog names it (two would be counted).
      const dialog = await screen.findByRole('dialog');
      expect(within(dialog).getByText('Past Tense')).toBeInTheDocument();
      await user.keyboard('{Escape}');
      // The job ends: the row offers Detect again, and the counts are read
      // again, as the book may have its chapters now.
      const before = calls.filter((c) => c.path === '/admin/issues').length;
      state = 'done';
      await queryClient.invalidateQueries({ queryKey: ['admin', 'books', 'list'] });
      await waitFor(() =>
        expect(calls.filter((c) => c.path === '/admin/issues').length).toBeGreaterThan(before),
      );
      expect(
        await within(gate).findByRole('button', { name: 'Detect chapters' }),
      ).toBeInTheDocument();
    });

    it('offers Detect again beside a detection that is being cancelled (BRIDGE-08)', async () => {
      mockFetch(
        detectRoutes({
          'GET /admin/books': (req) => ({
            body: {
              books:
                req.query.get('issue') === 'no_chapters'
                  ? [
                      {
                        ...plain[0],
                        detect_job: rowJob(plain[0].path, {
                          id: 7,
                          state: 'running',
                          cancel_requested: true,
                        }),
                      },
                    ]
                  : [],
            },
          }),
        }),
      );
      renderApp('/health?issue=no_chapters');
      const gate = (await screen.findByRole('link', { name: 'The Black Gate' })).closest('li')!;
      // The stopping job still shows; the server ends it for a new request.
      expect(await within(gate).findByText('Chapter detection')).toBeInTheDocument();
      expect(within(gate).getByRole('button', { name: 'Detect chapters' })).toBeInTheDocument();
    });

    it('keeps the rows when reading the list again fails', async () => {
      let down = false;
      mockFetch(
        detectRoutes({
          'GET /admin/books': (req) =>
            down
              ? { status: 503, body: { error: 'starting' } }
              : { body: { books: req.query.get('issue') === 'no_chapters' ? plain : [] } },
        }),
      );
      const { queryClient } = renderApp('/health?issue=no_chapters');
      expect(await screen.findByRole('link', { name: 'The Black Gate' })).toBeInTheDocument();
      down = true;
      await queryClient.refetchQueries({ queryKey: ['admin', 'books', 'list'] });
      await waitFor(() =>
        expect(
          queryClient.getQueryCache().findAll({ queryKey: ['admin', 'books', 'list'] })[0]?.state
            .status,
        ).toBe('error'),
      );
      expect(screen.getByRole('link', { name: 'The Black Gate' })).toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Past Tense' })).toBeInTheDocument();
    });

    it('reads the list again once a detection is queued, so its row says so (BRIDGE-08)', async () => {
      let queued = false;
      mockFetch(
        detectRoutes({
          'GET /admin/books': (req) => ({
            body: {
              books:
                req.query.get('issue') === 'no_chapters'
                  ? [
                      queued
                        ? { ...plain[0], detect_job: rowJob(plain[0].path, { id: 5 }) }
                        : plain[0],
                      plain[1],
                    ]
                  : [],
            },
          }),
          'POST /admin/bridge/jobs': (req) => {
            queued = true;
            return queuedJob((req.body as { path: string }).path);
          },
        }),
      );
      const user = userEvent.setup();
      renderApp('/health?issue=no_chapters');
      const gate = (await screen.findByRole('link', { name: 'The Black Gate' })).closest('li')!;
      expect(within(gate).queryByText('Chapter detection')).not.toBeInTheDocument();
      await user.click(within(gate).getByRole('button', { name: 'Detect chapters' }));
      const dialog = await screen.findByRole('dialog');
      await user.click(within(dialog).getByRole('button', { name: 'Detect chapters' }));
      expect(await within(gate).findByText('Chapter detection')).toBeInTheDocument();
      await waitFor(() =>
        expect(within(gate).queryByRole('button', { name: 'Detect chapters' })).toBeNull(),
      );
    });

    it('shows no detection on the ignored list, though the list before it had one (BRIDGE-08)', async () => {
      mockFetch(
        detectRoutes({
          'GET /admin/books': (req) => ({
            body: {
              books:
                req.query.get('issue') !== 'no_chapters'
                  ? []
                  : req.query.get('issue_ignored') === 'true'
                    ? [plain[1]]
                    : [{ ...plain[0], detect_job: rowJob(plain[0].path, { id: 7 }) }],
            },
          }),
        }),
      );
      const { router } = renderApp('/health?issue=no_chapters');
      const gate = (await screen.findByRole('link', { name: 'The Black Gate' })).closest('li')!;
      expect(await within(gate).findByText('Chapter detection')).toBeInTheDocument();
      await act(() =>
        router.navigate({
          to: '/health',
          search: { issue: 'no_chapters', ignored: true },
        } as never),
      );
      expect(await screen.findAllByRole('button', { name: 'Show again' })).not.toHaveLength(0);
      expect(screen.queryByText('Chapter detection')).not.toBeInTheDocument();
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
      key: 'dune',
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
      key: 'fiction-kids',
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

  it('ignores and restores one of two comparisons that share the same copy', async () => {
    const book = (library: number, path: string) => ({
      ...adminBook({ library_id: library, path, title: path }),
      listeners: 0,
    });
    const a = book(1, 'Best');
    const b = book(2, 'Kids copy');
    const c = book(1, 'Other copy');
    const pair = { key: 'across', reason: 'same_id', ignored: false, books: [a, b] };
    const local = { key: 'local', reason: 'same_id', ignored: false, books: [a, c] };
    let ignored = false;
    const calls = mockFetch(
      routes({
        'GET /admin/issues/duplicates': () => ({
          body: { groups: ignored ? [local] : [pair, local], total: ignored ? 1 : 2 },
        }),
        'POST /admin/issues/ignore': () => {
          ignored = true;
          return { status: 204 };
        },
        'DELETE /admin/issues/ignore': () => {
          ignored = false;
          return { status: 204 };
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=duplicate&across=1');
    const cards = await screen.findAllByRole('heading', { name: 'Best' });
    expect(cards).toHaveLength(2);
    await user.click(
      within(cards[0].closest('section')!).getByRole('button', { name: "They're different books" }),
    );
    await waitFor(() => expect(screen.getAllByRole('heading', { name: 'Best' })).toHaveLength(1));
    await user.click(screen.getByRole('button', { name: 'Undo' }));
    await waitFor(() => expect(screen.getAllByRole('heading', { name: 'Best' })).toHaveLength(2));
    const expected = {
      kind: 'duplicate',
      books: [
        { library_id: 1, path: 'Best' },
        { library_id: 2, path: 'Kids copy' },
      ],
    };
    expect(calls.filter((c) => c.path === '/admin/issues/ignore').map((c) => c.body)).toEqual([
      expected,
      expected,
    ]);
  });

  it('sends one decision and Undo for a group larger than the bulk limit', async () => {
    const books = Array.from({ length: 1001 }, (_, i) => ({
      ...adminBook({ path: `Copy ${i}`, title: 'Dune' }),
      listeners: 0,
    }));
    const key = 'a'.repeat(64);
    const calls = mockFetch(
      routes({
        'GET /admin/issues/duplicates': {
          body: { groups: [{ key, reason: 'same_files', ignored: false, books }], total: 1 },
        },
        'POST /admin/issues/ignore': { status: 204 },
        'DELETE /admin/issues/ignore': { status: 204 },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=duplicate');
    await user.click(await screen.findByRole('button', { name: "They're different books" }));
    await user.click(await screen.findByRole('button', { name: 'Undo' }));
    const expected = {
      kind: 'duplicate',
      books: [{ library_id: 1, path: 'Copy 0' }],
      group_key: key,
    };
    await waitFor(() =>
      expect(calls.filter((c) => c.path === '/admin/issues/ignore').map((c) => c.body)).toEqual([
        expected,
        expected,
      ]),
    );
  });

  it('ignores an across-libraries link when there is only one book library', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/libraries': { body: { libraries: libraries().slice(0, 1) } },
        'GET /admin/issues/duplicates': { body: { groups: [] } },
      }),
    );
    const { router } = renderApp('/health?issue=duplicate&across=1');
    await waitFor(() =>
      expect(calls.some((r) => r.path === '/admin/issues/duplicates')).toBe(true),
    );
    await waitFor(() => expect(router.state.location.search).toEqual({ issue: 'duplicate' }));
    expect(screen.queryByRole('switch', { name: 'Include other libraries' })).toBeNull();
    expect(calls.some((r) => r.query.has('across'))).toBe(false);
  });

  it('asks for the duplicates across libraries only once the libraries say there are several', async () => {
    let finish!: () => void;
    const pending = new Promise<void>((resolve) => {
      finish = resolve;
    });
    const calls = mockFetch(
      routes({
        'GET /admin/libraries': async () => {
          await pending;
          return { body: { libraries: libraries() } };
        },
        'GET /admin/issues/duplicates': { body: { groups: [] } },
      }),
    );
    renderApp('/health?issue=duplicate&across=1');
    expect(await screen.findByRole('heading', { name: 'Likely duplicates' })).toBeInTheDocument();
    expect(calls.some((c) => c.path === '/admin/issues/duplicates')).toBe(false);
    await act(async () => finish());
    await waitFor(() =>
      expect(calls.some((c) => c.path === '/admin/issues/duplicates')).toBe(true),
    );
    expect(
      calls.filter((c) => c.path === '/admin/issues/duplicates').map((c) => c.query.get('across')),
    ).toEqual(['1']);
  });

  it('keeps "Include other libraries" in the URL, so a reload keeps it (ADMIN-11)', async () => {
    const calls = mockFetch(routes({ 'GET /admin/issues/duplicates': { body: { groups: [] } } }));
    renderApp('/health?issue=duplicate&across=1');
    const toggle = await screen.findByRole('switch', { name: 'Include other libraries' });
    await waitFor(() => expect(toggle).toBeChecked());
    // Asked with it from the start: the list waits for the libraries to decide it.
    const asked = calls.filter((c) => c.path === '/admin/issues/duplicates');
    expect(asked.length).toBeGreaterThan(0);
    expect(asked.map((c) => c.query.get('across'))).toEqual(asked.map(() => '1'));
    await userEvent.setup().click(toggle);
    await waitFor(() => expect(toggle).not.toBeChecked());
    expect(window.location.search).not.toContain('across');
  });

  it('reaches the groups ignored across libraries, which the count (within each) leaves out', async () => {
    const across: DuplicateGroup = {
      key: 'fiction-kids',
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
        key: 'burning',
        reason: 'id_length_differs',
        ignored: false,
        books: pair('The Burning', 'The Burning [Dramatized Adaptation]', 'The Burning', burning),
      },
      { key: 'dune', reason: 'same_id', ignored: false, books: pair('Dune', 'Dune', 'Dune (mp3)') },
      {
        key: 'emma',
        reason: 'same_files',
        ignored: false,
        books: pair('Emma', 'Emma', 'Emma (copy)'),
      },
      // Said to be different books: what to check is moot.
      {
        key: 'mythos',
        reason: 'id_length_differs',
        ignored: true,
        books: pair('Mythos', 'A', 'B', burning),
      },
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

  it('loads duplicate groups a page at a time, numbering them out of the total', async () => {
    const pair = (n: number): DuplicateGroup => ({
      key: String(n),
      reason: 'same_files',
      ignored: false,
      books: [
        { ...adminBook({ path: `Book ${n}`, title: `Book ${n}` }), listeners: 0 },
        { ...adminBook({ path: `Copies/Book ${n}`, title: `Book ${n}` }), listeners: 0 },
      ],
    });
    const calls = mockFetch(
      routes({
        'GET /admin/issues/duplicates': (req) =>
          req.query.get('cursor') === 'c1'
            ? { body: { groups: [pair(3)], total: 3 } }
            : { body: { groups: [pair(1), pair(2)], total: 3, next_cursor: 'c1' } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=duplicate');
    const first = (await screen.findByRole('heading', { name: 'Book 1' })).closest('section')!;
    expect(within(first).getByText('1 of 3')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Book 3' })).toBeNull();
    const asked = calls.filter((c) => c.path === '/admin/issues/duplicates');
    expect(asked[0].query.get('limit')).toBe('50');
    expect(asked[0].query.get('cursor')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Show more' }));
    const third = (await screen.findByRole('heading', { name: 'Book 3' })).closest('section')!;
    expect(within(third).getByText('3 of 3')).toBeInTheDocument();
    expect(
      calls
        .filter((c) => c.path === '/admin/issues/duplicates')
        .at(-1)
        ?.query.get('cursor'),
    ).toBe('c1');
    // The last page: nothing more to load.
    expect(screen.queryByRole('button', { name: 'Show more' })).toBeNull();
  });

  it('keeps the loaded duplicate groups when a later page fails', async () => {
    const pair = (n: number): DuplicateGroup => ({
      key: String(n),
      reason: 'same_files',
      ignored: false,
      books: [
        { ...adminBook({ path: `Book ${n}`, title: `Book ${n}` }), listeners: 0 },
        { ...adminBook({ path: `Copies/Book ${n}`, title: `Book ${n}` }), listeners: 0 },
      ],
    });
    mockFetch(
      routes({
        'GET /admin/issues/duplicates': (req) =>
          req.query.get('cursor') === 'c1'
            ? { status: 500, body: { error: 'could not find duplicates' } }
            : { body: { groups: [pair(1)], total: 2, next_cursor: 'c1' } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=duplicate');
    await screen.findByRole('heading', { name: 'Book 1' });
    await user.click(screen.getByRole('button', { name: 'Show more' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Show more' })).toBeEnabled());
    // Still listed, with Show more to ask again: not replaced by the error.
    expect(screen.getByRole('heading', { name: 'Book 1' })).toBeInTheDocument();
  });

  it('shows a few copies of a large group until asked, and ignores them all', async () => {
    const books = Array.from({ length: 14 }, (_, i) => ({
      ...adminBook({ path: `Copy ${i + 1}`, title: 'Dune' }),
      listeners: 0,
    }));
    const calls = mockFetch(
      routes({
        'GET /admin/issues/duplicates': {
          body: {
            groups: [{ key: 'emma', reason: 'same_files', ignored: false, books }],
            total: 1,
          },
        },
        'POST /admin/issues/ignore': { status: 204 },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=duplicate');
    const card = (await screen.findByRole('heading', { name: 'Dune' })).closest('section')!;
    expect(within(card).getAllByText('Open book')).toHaveLength(10);
    await user.click(within(card).getByRole('button', { name: 'Show all 14 copies' }));
    expect(within(card).getAllByText('Open book')).toHaveLength(14);
    expect(within(card).queryByRole('button', { name: /^Show all/ })).toBeNull();
    await user.click(within(card).getByRole('button', { name: "They're different books" }));
    await waitFor(() =>
      expect(
        (calls.find((c) => c.path === '/admin/issues/ignore')?.body as { books: unknown[] })?.books,
      ).toHaveLength(14),
    );
  });

  it("asks for a copy's cover only once its column nears the viewport", async () => {
    // An observer that only says "in view" when the test says so.
    const observed: { el: Element; cb: IntersectionObserverCallback }[] = [];
    class FakeObserver {
      private cb: IntersectionObserverCallback;
      constructor(cb: IntersectionObserverCallback) {
        this.cb = cb;
      }
      observe(el: Element) {
        observed.push({ el, cb: this.cb });
      }
      disconnect() {}
      unobserve() {}
      takeRecords() {
        return [];
      }
    }
    vi.stubGlobal('IntersectionObserver', FakeObserver);
    onTestFinished(() => {
      vi.unstubAllGlobals();
    });
    const group: DuplicateGroup = {
      key: 'dune',
      reason: 'same_files',
      ignored: false,
      books: [
        { ...adminBook({ path: 'Dune', title: 'Dune' }), listeners: 0 },
        { ...adminBook({ path: 'Dune (copy)', title: 'Dune' }), listeners: 0 },
      ],
    };
    const calls = mockFetch(
      routes({ 'GET /admin/issues/duplicates': { body: { groups: [group], total: 1 } } }),
    );
    renderApp('/health?issue=duplicate');
    await screen.findByRole('heading', { name: 'Dune' });
    await waitFor(() => expect(observed.length).toBeGreaterThanOrEqual(2));
    await new Promise((r) => setTimeout(r, 50));
    const covers = () =>
      calls
        .filter((c) => c.path === '/admin/covers')
        .flatMap((c) => (c.body as { books: { path: string }[] }).books.map((b) => b.path));
    expect(covers()).not.toContain('Dune (copy)');
    const column = screen.getAllByText('Open book')[1].closest('div.p-5')!;
    const entry = observed.find((o) => o.el === column)!;
    entry.cb([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver);
    await waitFor(() => expect(covers()).toContain('Dune (copy)'));
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
        'POST /admin/books/merge': {
          body: {
            books: [
              { library_id: 1, path: 'M/1', previous: null },
              { library_id: 1, path: 'M/3', previous: null },
            ],
          },
        },
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

    await user.click(screen.getByRole('button', { name: 'Use this spelling' }));
    await waitFor(() =>
      expect(calls.find((c) => c.path === '/admin/books/merge')?.body).toEqual({
        library_id: 1,
        field: 'series',
        sources: ['He Who Fights with Monsters', 'He Who Fights WIth Monsters'],
        target: 'He Who Fights With Monsters',
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
