import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { keys } from '@/api/hooks';
import { setToken } from '@/api/token';
import type { BridgeEstimate, BridgeJob, BridgeStatus } from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { bookDetail } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// The book page's characters-and-recaps action: the gate, the confirmation
// with its estimate, the queue order, and a job's live status.

const BOOK_PATH = 'Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings';
const URL = `/library/book?library=1&path=${encodeURIComponent(BOOK_PATH)}`;

const bridgeOn = {
  ...serverInfo,
  capabilities: { ...serverInfo.capabilities, bridge: true },
  bridge: { version: '0.2.0', features: ['bridge', 'sidecars'] },
};

/** The contributions module on (Phase 2b): the contribute switch is offered. */
const contributionsOn = {
  ...bridgeOn,
  capabilities: { ...bridgeOn.capabilities, contributions: true },
  bridge: { version: '0.3.0', features: ['bridge', 'sidecars', 'contribute'] },
};

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
    can_contribute: true,
    figures: true,
    writable: true,
    can_describe: false,
    has: {
      community: { characters: false, recaps: false, description: false },
      local: { characters: false, recaps: false, description: false },
    },
    earlier: [],
    ...over,
  };
}

function job(over: Partial<BridgeJob> = {}): BridgeJob {
  return {
    id: 31,
    kind: 'sidecars.generate',
    library_id: 1,
    path: BOOK_PATH,
    params: { source: 'auto', contribute: 'local' },
    state: 'running',
    progress: {},
    result: {},
    error: '',
    requested_by: { id: 1, username: 'chris' },
    priority: 0,
    attempts: 1,
    lease_until: null,
    cancel_requested: false,
    created_at: '2026-10-15T12:00:00Z',
    updated_at: '2026-10-15T12:10:00Z',
    claimed_at: '2026-10-15T12:00:05Z',
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

/** The estimates asked for, brief (the card) or full (the dialog). */
const estimates = (calls: MockRequest[], brief: boolean) =>
  calls.filter(
    (c) => c.path === '/admin/bridge/estimate' && (c.query.get('brief') === '1') === brief,
  );

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('generate characters and recaps', () => {
  it('stays hidden without an online Bridge offering it, and asks nothing', async () => {
    const calls = mockFetch(routes({ 'GET /server': { body: serverInfo } }));
    renderApp(URL);
    await screen.findByRole('heading', { level: 1, name: 'The Way of Kings' });
    expect(screen.queryByRole('button', { name: /generate characters/i })).toBeNull();
    expect(calls.some((c) => c.path.startsWith('/admin/bridge'))).toBe(false);
  });

  it('says the community covers the book, with a link to the card, instead of the action', async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({
        'GET /admin/bridge/estimate': {
          body: estimate({
            has: {
              community: { characters: true, recaps: true, description: false },
              // Files generated here before: masked by the community's, so no note and no Regenerate.
              local: { characters: true, recaps: true, description: false },
            },
          }),
        },
      }),
    );
    renderApp(URL);
    await screen.findByText('The community already has characters and recaps for this book.');
    expect(screen.queryByRole('button', { name: /generate characters/i })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Regenerate' })).toBeNull();
    expect(screen.queryByText('Generated on this server.')).toBeNull();
    const card = document.getElementById('characters-and-recaps')!;
    const scrolled = vi.spyOn(card, 'scrollIntoView');
    const link = screen.getByRole('link', { name: 'See them' });
    expect(link).toHaveAttribute('href', '#characters-and-recaps');
    await user.click(link);
    expect(scrolled).toHaveBeenCalled();
    expect(card).toHaveFocus();
  });

  it('offers Generate while the community has only one of characters and recaps', async () => {
    mockFetch(
      routes({
        'GET /admin/bridge/estimate': {
          body: estimate({
            has: {
              community: { characters: true, recaps: false, description: false },
              local: { characters: false, recaps: false, description: false },
            },
          }),
        },
      }),
    );
    renderApp(URL);
    await screen.findByRole('button', { name: /generate characters/i });
    expect(screen.queryByText(/already has characters and recaps/)).toBeNull();
  });

  it('shows no covered line while the Bridge is away', async () => {
    const calls = mockFetch(routes({ 'GET /server': { body: serverInfo } }));
    renderApp(URL);
    await screen.findByRole('heading', { level: 1, name: 'The Way of Kings' });
    expect(calls.some((c) => c.path === '/admin/bridge/estimate')).toBe(false);
    expect(screen.queryByText(/already has characters and recaps/)).toBeNull();
  });

  it('shows the estimate and queues the earlier books first, then this one', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(
      routes({
        'GET /server': { body: contributionsOn },
        'GET /admin/bridge/estimate': {
          body: estimate({
            source: 'ebook',
            ebook: `${BOOK_PATH}/The Way of Kings.epub`,
            asr_hours: 0,
            agent_cost_usd: 4,
            earlier: [
              {
                library_id: 1,
                path: 'B/Prequel 0',
                title: 'Prequel',
                series_index: '0.5',
                asr_hours: 2,
                agent_cost_usd: 3,
              },
            ],
          }),
        },
        'POST /admin/bridge/jobs': (req) => ({
          status: 201,
          body: job({ path: (req.body as { path: string }).path, state: 'queued' }),
        }),
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Generate characters and recaps' }));
    const dialog = await screen.findByRole('dialog', { name: 'Generate characters and recaps' });
    expect(await within(dialog).findByText('The ebook beside it')).toBeInTheDocument();
    expect(within(dialog).getByText('The Way of Kings.epub')).toBeInTheDocument();
    // A Bridge that can contribute, with the module on: the switch starts on.
    expect(within(dialog).getByRole('switch')).toBeChecked();
    expect(
      within(dialog).getByText('On this server, and offered to the community metadata'),
    ).toBeInTheDocument();
    const earlierBox = within(dialog).getByRole('checkbox', {
      name: /also generate for 1 earlier book in the series first/i,
    });
    expect(earlierBox).toBeChecked();
    expect(within(dialog).getByText('Prequel')).toBeInTheDocument();
    // The figures quote every run the button queues: the earlier book's audio
    // and both agent runs.
    expect(within(dialog).getByText('About 2h for 2 books')).toBeInTheDocument();
    expect(within(dialog).getByText('About $7.00 for 2 books')).toBeInTheDocument();
    // This book alone: the ebook needs no transcription.
    await user.click(earlierBox);
    expect(within(dialog).getByText('None needed')).toBeInTheDocument();
    expect(within(dialog).getByText('About $4.00')).toBeInTheDocument();
    await user.click(earlierBox);
    // The card asked for the brief estimate; the dialog for the full one, once.
    expect(estimates(calls, true).length).toBeGreaterThan(0);
    expect(estimates(calls, false)).toHaveLength(1);

    await user.click(within(dialog).getByRole('button', { name: 'Generate for 2 books' }));
    await waitFor(() => expect(queued(calls)).toHaveLength(2));
    expect(queued(calls).map((c) => c.body)).toEqual([
      {
        kind: 'sidecars.generate',
        library_id: 1,
        path: 'B/Prequel 0',
        params: { source: 'auto', contribute: 'issue' },
      },
      {
        kind: 'sidecars.generate',
        library_id: 1,
        path: BOOK_PATH,
        params: { source: 'auto', contribute: 'issue' },
      },
    ]);
  });

  it("won't queue while the Bridge can't write to the library", async () => {
    const user = userEvent.setup();
    mockFetch(routes({ 'GET /admin/bridge/estimate': { body: estimate({ writable: false }) } }));
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Generate characters and recaps' }));
    const dialog = await screen.findByRole('dialog');
    expect(
      await within(dialog).findByText("The Bridge can't write to this library"),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Generate' })).toBeDisabled();
  });

  it('says why a queue was refused, inline', async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({
        'POST /admin/bridge/jobs': {
          status: 409,
          body: { error: 'library not writable', code: 'library_not_writable' },
        },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Generate characters and recaps' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(await within(dialog).findByRole('button', { name: 'Generate' }));
    expect(
      await within(dialog).findByText(/The Bridge can't write to this library\. Check its mapping/),
    ).toBeInTheDocument();
  });

  it('never counts a job that is being cancelled as queued, and stops there', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(
      routes({
        'GET /admin/bridge/estimate': {
          body: estimate({
            agent_cost_usd: null,
            earlier: [
              {
                library_id: 1,
                path: 'B/Prequel 0',
                title: 'Prequel',
                series_index: '0.5',
                asr_hours: 1,
                agent_cost_usd: null,
              },
            ],
          }),
        },
        // The open job comes back for the earlier book, being cancelled.
        'POST /admin/bridge/jobs': {
          status: 200,
          body: job({ path: 'B/Prequel 0', cancel_requested: true }),
        },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Generate characters and recaps' }));
    const dialog = await screen.findByRole('dialog', { name: 'Generate characters and recaps' });
    expect(await within(dialog).findByText('No estimate yet')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Generate for 2 books' }));
    expect(
      await within(dialog).findByText('Characters and recaps for Prequel 0 is still stopping'),
    ).toBeInTheDocument();
    expect(queued(calls)).toHaveLength(1);
  });

  it('offers contributing only when the Bridge can, and then queues for this server only', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(
      routes({
        'GET /server': { body: contributionsOn },
        'GET /admin/bridge/estimate': { body: estimate({ can_contribute: false }) },
        'POST /admin/bridge/jobs': { status: 201, body: job({ state: 'queued' }) },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Generate characters and recaps' }));
    const dialog = await screen.findByRole('dialog', { name: 'Generate characters and recaps' });
    expect(await within(dialog).findByText('On this server only')).toBeInTheDocument();
    expect(within(dialog).queryByRole('switch')).toBeNull();
    expect(within(dialog).queryByText('Also contribute to the community')).toBeNull();
    await user.click(within(dialog).getByRole('button', { name: 'Generate' }));
    await waitFor(() => expect(queued(calls)).toHaveLength(1));
    expect(queued(calls)[0].body).toMatchObject({ params: { contribute: 'local' } });
  });

  it('offers no contribute switch while the contributions module is off, even when the Bridge can', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(
      routes({
        'GET /admin/bridge/estimate': { body: estimate({ can_contribute: true }) },
        'POST /admin/bridge/jobs': { status: 201, body: job({ state: 'queued' }) },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Generate characters and recaps' }));
    const dialog = await screen.findByRole('dialog', { name: 'Generate characters and recaps' });
    expect(await within(dialog).findByText('On this server only')).toBeInTheDocument();
    expect(within(dialog).queryByRole('switch')).toBeNull();
    await user.click(within(dialog).getByRole('button', { name: 'Generate' }));
    await waitFor(() => expect(queued(calls)).toHaveLength(1));
    expect(queued(calls)[0].body).toMatchObject({ params: { contribute: 'local' } });
  });

  it('keeps the results on this server when the switch is turned off', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(
      routes({
        'GET /server': { body: contributionsOn },
        'POST /admin/bridge/jobs': { status: 201, body: job({ state: 'queued' }) },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Generate characters and recaps' }));
    const dialog = await screen.findByRole('dialog', { name: 'Generate characters and recaps' });
    const toggle = await within(dialog).findByRole('switch', {
      name: 'Also contribute to the community',
    });
    expect(toggle).toBeChecked();
    await user.click(toggle);
    expect(within(dialog).getByText('On this server only')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Generate' }));
    await waitFor(() => expect(queued(calls)).toHaveLength(1));
    expect(queued(calls)[0].body).toMatchObject({ params: { contribute: 'local' } });
  });

  it("tells a Bridge that hasn't sent its figures from an agent that isn't ready", async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({
        'GET /admin/bridge/estimate': {
          body: estimate({
            figures: false,
            agent_ready: false,
            can_contribute: false,
            asr_hours: 0,
            agent_cost_usd: null,
          }),
        },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Generate characters and recaps' }));
    const dialog = await screen.findByRole('dialog', { name: 'Generate characters and recaps' });
    expect(
      await within(dialog).findByText("The Bridge hasn't reported on its agent yet"),
    ).toBeInTheDocument();
    expect(within(dialog).queryByText("The Bridge's agent isn't ready")).toBeNull();
  });

  it('keeps the open dialog when the Bridge goes offline, and says so', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(
      routes({ 'POST /admin/bridge/jobs': { status: 201, body: job({ state: 'queued' }) } }),
    );
    const { queryClient } = renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Generate characters and recaps' }));
    const dialog = await screen.findByRole('dialog', { name: 'Generate characters and recaps' });
    await within(dialog).findByText('On this server only');
    // The next GET /server says the Bridge is away.
    act(() => queryClient.setQueryData(keys.server, serverInfo));
    expect(await within(dialog).findByText('The Bridge is offline')).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'Generate characters and recaps' })).toBe(dialog);
    // What is queued waits for it.
    await user.click(within(dialog).getByRole('button', { name: 'Generate' }));
    await waitFor(() => expect(queued(calls)).toHaveLength(1));
  });

  it("words a book that isn't indexed any more", async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({
        'GET /admin/bridge/estimate': (req) =>
          req.query.get('brief') === '1'
            ? { body: estimate() }
            : { status: 404, body: { error: 'no book', code: 'book_not_found' } },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Generate characters and recaps' }));
    const dialog = await screen.findByRole('dialog', { name: 'Generate characters and recaps' });
    expect(
      await within(dialog).findByText(
        "That book isn't in the library any more. It may have moved or been removed.",
      ),
    ).toBeInTheDocument();
  });

  it('shows a running job live: stage, count, ETA, cost, and Cancel', async () => {
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                progress: {
                  step: 'asr',
                  stage: 'asr',
                  // The Bridge's own English wording of the stage: not shown.
                  message: 'Transcribing (12 of 40)',
                  done: 12,
                  total: 40,
                  eta_seconds: 5400,
                  cost_usd: 3.42,
                },
              }),
            ],
          },
        },
      }),
    );
    renderApp(URL);
    const line = await screen.findByText('Transcribing');
    expect(line.closest('p')).toHaveTextContent(
      'Transcribing · 12 of 40 · about 1h 30m left · $3.42 so far',
    );
    expect(screen.queryByText('Transcribing (12 of 40)')).toBeNull();
    expect(
      screen.getByRole('button', { name: 'Cancel generating characters and recaps' }),
    ).toBeInTheDocument();
    // No second job while one is open.
    expect(screen.queryByRole('button', { name: 'Generate characters and recaps' })).toBeNull();
  });

  it("shows the book's chapter detection instead of Generate while it runs, and offers Generate once it is parked", async () => {
    let state: BridgeJob['state'] = 'running';
    const detect = () =>
      job({ id: 44, kind: 'chapters.detect', params: {}, state, progress: { stage: 'asr' } });
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': () => ({ body: { jobs: [detect()] } }),
        // The estimate names the detect job while it refuses a generate job.
        'GET /admin/bridge/estimate': () => ({
          body: estimate({ open_job: state === 'running' ? detect() : undefined }),
        }),
      }),
    );
    const { queryClient } = renderApp(URL);
    expect(
      await screen.findByText(
        'The book’s chapters are being found. Generate once that has finished: it reuses the transcription.',
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Generate characters and recaps' })).toBeNull();
    // Parked (no chapters heard), it gives way: a generate job cancels it.
    state = 'needs_attention';
    await act(() => queryClient.invalidateQueries());
    expect(
      await screen.findByRole('button', { name: 'Generate characters and recaps' }),
    ).toBeInTheDocument();
    expect(screen.queryByText(/chapters are being found/)).toBeNull();
  });

  it('shows a parked job with its reason, hint, and a link into the Bridge', async () => {
    const status: Partial<BridgeStatus> = {
      paired: true,
      online: true,
      kinds: [],
      features: [],
      libraries: [],
      workers: [],
      pairing: null,
      paired_by: null,
      ui_url: 'http://192.168.1.20:8090/',
    };
    mockFetch(
      routes({
        'GET /admin/bridge': { body: status },
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                state: 'needs_attention',
                error: 'parked',
                progress: {
                  step: 'fact_pass',
                  park: { code: 'budget_exceeded', hint: 'Raise agent.book_budget_usd' },
                },
              }),
            ],
          },
        },
      }),
    );
    renderApp(URL);
    expect(await screen.findByText('The job reached its spending limit.')).toBeInTheDocument();
    expect(screen.getByText('Raise agent.book_budget_usd')).toBeInTheDocument();
    expect(await screen.findByRole('link', { name: 'Open in the Bridge' })).toHaveAttribute(
      'href',
      'http://192.168.1.20:8090/#/jobs/31',
    );
  });

  it("says the Bridge is retrying once a park's retry time has passed, and words a refused credential", async () => {
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                state: 'needs_attention',
                progress: {
                  step: 'fact_pass',
                  park: { code: 'agent_credential_refused', retry_at: '2026-01-01T00:00:00Z' },
                },
              }),
            ],
          },
        },
      }),
    );
    renderApp(URL);
    expect(
      await screen.findByText(
        "The agent's credential was refused. Replace it on the Bridge's Accounts tab, or in the agent container if it has one of its own, which it uses first; the job then carries on by itself.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByText('Trying again now.')).toBeInTheDocument();
    expect(screen.queryByText(/Tries again/)).toBeNull();
  });

  it('says what a finished job made', async () => {
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                state: 'done',
                finished_at: '2026-10-15T14:00:00Z',
                result: { characters: 41, recaps: 38, cost_usd: 12.5 },
              }),
            ],
          },
        },
        'GET /admin/bridge/estimate': {
          body: estimate({
            has: {
              community: { characters: false, recaps: false, description: false },
              local: { characters: true, recaps: true, description: false },
            },
          }),
        },
      }),
    );
    renderApp(URL);
    expect(
      await screen.findByText(/41 characters and 38 recaps · cost \$12\.50/),
    ).toBeInTheDocument();
    expect(await screen.findByRole('button', { name: 'Regenerate' })).toBeInTheDocument();
  });
});

describe('the Bridge page', () => {
  it('shows why a generate job was cancelled', async () => {
    mockFetch(
      signedInRoutes({
        'GET /admin/bridge': {
          body: {
            paired: true,
            paired_by: null,
            pairing: null,
            online: true,
            kinds: [],
            features: [],
            libraries: [],
            workers: [],
          },
        },
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                state: 'cancelled',
                cancel_requested: true,
                error: 'the book moved onto another job of this kind',
              }),
            ],
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const jobs = await screen.findByRole('list', { name: 'Jobs' });
    expect(
      within(jobs).getByText('the book moved onto another job of this kind'),
    ).toBeInTheDocument();
  });

  it('words a chapter detection job: its stage, what it found, and why it stopped', async () => {
    mockFetch(
      signedInRoutes({
        'GET /admin/bridge': {
          body: {
            paired: true,
            paired_by: null,
            pairing: null,
            online: true,
            kinds: ['chapters.detect'],
            features: ['chapters'],
            libraries: [],
            workers: [],
          },
        },
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({ id: 41, kind: 'chapters.detect', progress: { stage: 'chapter_detecting' } }),
              job({
                id: 42,
                kind: 'chapters.detect',
                state: 'done',
                finished_at: '2026-10-15T14:00:00Z',
                result: { chapters: 24, files: ['x'], source: 'audio' },
              }),
              job({
                id: 43,
                kind: 'chapters.detect',
                state: 'needs_attention',
                progress: { park: { code: 'chapters_not_detected' } },
              }),
            ],
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const jobs = await screen.findByRole('list', { name: 'Jobs' });
    expect(within(jobs).getAllByText('Chapters')).toHaveLength(3);
    expect(within(jobs).getByText('Finding the chapters in the narration')).toBeInTheDocument();
    expect(within(jobs).getByText(/^24 chapters found/)).toBeInTheDocument();
    expect(
      within(jobs).getByText(/^The chapters couldn't be found in the narration\./),
    ).toBeInTheDocument();
  });

  it('words a generate job: its book, its park, and a link into the Bridge; and the estimates', async () => {
    const status: BridgeStatus = {
      paired: true,
      paired_at: '2026-10-15T09:00:00Z',
      paired_by: { id: 1, username: 'chris' },
      pairing: null,
      online: true,
      heartbeat_at: new Date().toISOString(),
      version: '0.2.0',
      kinds: ['bridge.verify', 'sidecars.generate'],
      features: ['bridge', 'sidecars'],
      libraries: [{ id: 1, name: 'Fiction', root: '/books/fiction', mapping: null }],
      workers: [],
      ui_url: 'http://192.168.1.20:8090/',
      estimates: {
        sidecars: {
          asr_seconds_per_audio_hour: 900,
          agent_usd_per_audio_hour: 1.6,
          agent_usd_per_ebook: null,
          samples: 12,
          agent_backend: 'claude',
          agent_ready: true,
          subscription: true,
          monthly_budget_usd: 100,
          month_spent_usd: 12.3,
        },
      },
    };
    mockFetch(
      signedInRoutes({
        'GET /admin/bridge': { body: status },
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                state: 'needs_attention',
                progress: { step: 'fact_pass', park: { code: 'core_needed' } },
              }),
            ],
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const jobs = await screen.findByRole('list', { name: 'Jobs' });
    expect(within(jobs).getByText('Characters and recaps')).toBeInTheDocument();
    expect(within(jobs).getByRole('link', { name: BOOK_PATH })).toHaveAttribute(
      'href',
      `/admin/library/book?library=1&path=${encodeURIComponent(BOOK_PATH).replace(/%20/g, '+')}`,
    );
    expect(
      within(jobs).getByText(
        'The community has no entry for this book yet. Add one on the Bridge to contribute.',
      ),
    ).toBeInTheDocument();
    expect(within(jobs).getByRole('link', { name: 'Open in the Bridge' })).toHaveAttribute(
      'href',
      'http://192.168.1.20:8090/#/jobs/31',
    );
    expect(screen.getByText('$12.30 of $100')).toBeInTheDocument();
    expect(screen.getByText('Subscription')).toBeInTheDocument();
    expect(screen.getByText('12 finished books')).toBeInTheDocument();
    expect(screen.getByText('claude')).toBeInTheDocument();
  });
});
