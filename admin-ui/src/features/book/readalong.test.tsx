import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type {
  AdminBook,
  BridgeEstimate,
  BridgeJob,
  ReadalongEstimate,
  ServerInfo,
} from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { bookDetail, books } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// The book page's Read-along card (Bridge Phase 3c): its states, the offer and
// its confirmation with the estimate, the job it queues, what holds it back,
// and Generate held back by an open read-along.

const BOOK = books[0];
const URL = `/library/book?library=1&path=${encodeURIComponent(BOOK.path)}`;
const EBOOK = { path: `${BOOK.path}/The Way of Kings.epub`, size: 1_060_000 };

const bridgeOn: ServerInfo = {
  ...serverInfo,
  capabilities: { ...serverInfo.capabilities, bridge: true, readalong: true },
  bridge: { version: '0.4.0', features: ['bridge', 'sidecars', 'chapters', 'readalong'] },
};

function readalongEstimate(over: Partial<ReadalongEstimate> = {}): ReadalongEstimate {
  return {
    kind: 'readalong.align',
    asr_hours: 2.5,
    figures: true,
    writable: true,
    ebook: EBOOK,
    readalong: null,
    ...over,
  };
}

const generateEstimate: BridgeEstimate = {
  kind: 'sidecars.generate',
  library_id: 1,
  path: BOOK.path,
  source: 'audio',
  duration_seconds: 163800,
  asr_hours: 11.4,
  agent_cost_usd: 18.2,
  subscription: false,
  agent_ready: true,
  can_contribute: false,
  figures: true,
  writable: true,
  can_describe: false,
  has: {
    community: { characters: false, recaps: false, description: false },
    local: { characters: false, recaps: false, description: false },
  },
  earlier: [],
};

function job(over: Partial<BridgeJob> = {}): BridgeJob {
  return {
    id: 41,
    kind: 'readalong.align',
    library_id: 1,
    path: BOOK.path,
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
    created_at: '2026-11-02T10:00:00Z',
    updated_at: '2026-11-02T10:05:00Z',
    claimed_at: '2026-11-02T10:00:05Z',
    finished_at: null,
    ...over,
  };
}

function routes(
  book: Partial<AdminBook> = { ebook: EBOOK },
  over: Record<string, MockRoute> = {},
  estimate: ReadalongEstimate = readalongEstimate(),
) {
  return signedInRoutes({
    'GET /server': { body: bridgeOn },
    'GET /admin/libraries/1/book': { body: bookDetail({ book: { ...BOOK, ...book } }) },
    'GET /admin/bridge/jobs': { body: { jobs: [] } },
    // Each kind's estimate on the one route, by its kind.
    'GET /admin/bridge/estimate': (req) => ({
      body: req.query.get('kind') === 'readalong.align' ? estimate : generateEstimate,
    }),
    ...over,
  });
}

const queued = (calls: MockRequest[]) =>
  calls.filter((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs');

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('the Read-along card', () => {
  it('is not shown for a book with no ebook, and asks for no read-along estimate', async () => {
    const calls = mockFetch(routes({}));
    renderApp(URL);
    await screen.findByRole('heading', { level: 1, name: 'The Way of Kings' });
    expect(screen.queryByRole('heading', { name: 'Read-along' })).toBeNull();
    expect(calls.some((c) => c.query.get('kind') === 'readalong.align')).toBe(false);
  });

  it('is not shown for a book with an ebook but no map and no Bridge that makes one', async () => {
    mockFetch(routes(undefined, { 'GET /server': { body: serverInfo } }));
    renderApp(URL);
    await screen.findByRole('heading', { level: 1, name: 'The Way of Kings' });
    expect(screen.queryByRole('heading', { name: 'Read-along' })).toBeNull();
  });

  it('shows a ready map with its coverage, rounded down, and offers Make again', async () => {
    mockFetch(
      routes({
        ebook: EBOOK,
        readalong: { state: 'ready', coverage: 0.996, generated_at: '2026-11-02T10:11:12Z' },
      }),
    );
    renderApp(URL);
    expect(await screen.findByText('Ready: lines up 99% of the ebook’s text')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Make again' })).toBeInTheDocument();
  });

  it('says why a stale map is out of date, with Make again', async () => {
    mockFetch(routes({ ebook: EBOOK, readalong: { state: 'stale', reason: 'ebook_changed' } }));
    renderApp(URL);
    expect(
      await screen.findByText(/Out of date: the ebook changed since the read-along was made/),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Make again' })).toBeInTheDocument();
  });

  it('shows a ready map without a Bridge, offering nothing', async () => {
    mockFetch(
      routes(
        { ebook: EBOOK, readalong: { state: 'ready', coverage: 0.5 } },
        { 'GET /server': { body: serverInfo } },
      ),
    );
    renderApp(URL);
    expect(await screen.findByText('Ready: lines up 50% of the ebook’s text')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Make/ })).toBeNull();
  });

  it('confirms with the estimate, then queues readalong.align for this book', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(
      routes(undefined, {
        'POST /admin/bridge/jobs': { status: 201, body: job({ state: 'queued' }) },
      }),
    );
    renderApp(URL);
    expect(await screen.findByText(/No read-along yet/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Make read-along' }));
    const dialog = await screen.findByRole('dialog', { name: 'Make a read-along' });
    expect(
      await within(dialog).findByText(
        'Up to 2.5h, less if the Bridge still has this book’s transcript',
      ),
    ).toBeInTheDocument();
    expect(within(dialog).getByText('None: no agent is used')).toBeInTheDocument();
    expect(within(dialog).getByText('The Way of Kings.epub')).toBeInTheDocument();
    expect(within(dialog).getByText(/less than half of the ebook lines up/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Make read-along' }));
    await waitFor(() => expect(queued(calls)).toHaveLength(1));
    expect(queued(calls)[0].body).toEqual({
      kind: 'readalong.align',
      library_id: 1,
      path: BOOK.path,
      params: {},
    });
    expect(await screen.findByText('Read-along queued')).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Make a read-along' })).toBeNull(),
    );
  });

  it("won't queue while the Bridge can't write to the library", async () => {
    const user = userEvent.setup();
    mockFetch(routes(undefined, {}, readalongEstimate({ writable: false })));
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Make read-along' }));
    const dialog = await screen.findByRole('dialog');
    expect(
      await within(dialog).findByText("The Bridge can't write to this library"),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Make read-along' })).toBeDisabled();
  });

  it('says a refusal inline in the card words', async () => {
    const user = userEvent.setup();
    mockFetch(
      routes(undefined, {
        'POST /admin/bridge/jobs': {
          status: 409,
          body: { error: 'a generate job is open', code: 'generate_open' },
        },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Make read-along' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(await within(dialog).findByRole('button', { name: 'Make read-along' }));
    expect(
      await within(dialog).findByText(/Characters and recaps are being generated for the book/),
    ).toBeInTheDocument();
  });

  it("follows the book's read-along job with its stage, and offers no Make while it runs", async () => {
    mockFetch(
      routes(undefined, {
        'GET /admin/bridge/jobs': {
          body: { jobs: [job({ progress: { stage: 'readalong_aligning', done: 3, total: 12 } })] },
        },
      }),
    );
    renderApp(URL);
    const card = await screen.findByRole('region', { name: 'Read-along' });
    expect(
      await within(card).findByText('Lining up the ebook with the narration'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Make read-along' })).toBeNull();
    expect(
      screen.getByRole('button', { name: 'Cancel making the read-along' }),
    ).toBeInTheDocument();
  });

  it('words a finished job by the share of the text it lined up', async () => {
    mockFetch(
      routes(undefined, {
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                state: 'done',
                result: { coverage: { text: 0.996, audio: 0.98 }, transcribed: false },
                finished_at: '2026-11-02T10:30:00Z',
              }),
            ],
          },
        },
      }),
    );
    renderApp(URL);
    expect(
      await screen.findByText(/99% of the text lined up and the transcript was reused/),
    ).toBeInTheDocument();
  });

  it('waits for an open generate job, showing its status instead of Make', async () => {
    mockFetch(
      routes(undefined, {
        'GET /admin/bridge/jobs': {
          body: { jobs: [job({ kind: 'sidecars.generate', id: 40, state: 'needs_attention' })] },
        },
      }),
    );
    renderApp(URL);
    expect(
      await screen.findByText(/Make the read-along once that has finished/),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Make read-along' })).toBeNull();
  });
});

describe('Generate while a read-along is made', () => {
  it('holds Generate back while the read-along job works, saying why', async () => {
    const metadataOn: ServerInfo = {
      ...bridgeOn,
      capabilities: { ...bridgeOn.capabilities, metadata: true },
    };
    mockFetch(
      routes(undefined, {
        'GET /server': { body: metadataOn },
        'GET /admin/bridge/jobs': { body: { jobs: [job({ state: 'running' })] } },
      }),
    );
    renderApp(URL);
    expect(
      await screen.findByText(/A read-along is being made for the book\. Generate once/),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Generate characters and recaps' })).toBeNull();
  });
});
