import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { keys } from '@/api/hooks';
import { setToken } from '@/api/token';
import type {
  AdminBookDetail,
  BridgeJob,
  MatchCandidate,
  OverrideField,
  ServerInfo,
} from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { bookDetail } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Suggest corrections (Bridge Phase 2b): the Community card's action, the
// review of the admin's edits against the community's values, the queued
// meta.correct job, and its issue once filed.

const BOOK_PATH = 'Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings';
const URL = `/library/book?library=1&path=${encodeURIComponent(BOOK_PATH)}`;

const server: ServerInfo = {
  ...serverInfo,
  capabilities: { ...serverInfo.capabilities, bridge: true, contributions: true },
  bridge: { version: '0.3.0', features: ['bridge', 'meta', 'contribute'] },
};

const candidate: MatchCandidate = {
  work_id: 'the-way-of-kings',
  title: 'The Way of Kings',
  authors: [{ id: 'a1', name: 'Brandon Sanderson' }],
  first_published: '2010-08-31',
  series: [{ name: 'The Stormlight Archive', position: '1' }],
  web_url: 'https://meta.audiosilo.app/works/the-way-of-kings',
  recordings: [
    {
      id: 'r1',
      narrators: [{ id: 'n1', name: 'Michael Kramer' }],
      asins: ['B003P2WO5E'],
      asin_refs: [{ region: 'us', asin: 'B003P2WO5E' }],
      asin_region: 'us',
      isbns: [],
    },
  ],
  recording_id: 'r1',
  score: 100,
};

/** The book with the admin's edits. */
function edited(edits: Partial<Record<OverrideField, string>>): AdminBookDetail {
  const d = bookDetail();
  for (const [f, v] of Object.entries(edits)) {
    d.fields[f as OverrideField] = { value: v, source: 'edited', scanned: '', locked: false };
  }
  return d;
}

function job(over: Partial<BridgeJob> = {}): BridgeJob {
  return {
    id: 51,
    kind: 'meta.correct',
    library_id: 1,
    path: BOOK_PATH,
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
    created_at: '2026-10-19T12:00:00Z',
    updated_at: '2026-10-19T12:00:00Z',
    claimed_at: null,
    finished_at: null,
    ...over,
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /server': { body: server },
    'GET /admin/libraries/1/book': {
      body: edited({ author: 'B. Sanderson', narrator: 'Kate Reading', title: 'The Way of Kings' }),
    },
    'GET /admin/libraries/1/book/match': { body: { candidates: [candidate] } },
    'GET /admin/bridge/jobs': { body: { jobs: [] } },
    ...over,
  });
}

const queued = (calls: MockRequest[]) =>
  calls.filter((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs');

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('suggest corrections', () => {
  it('reviews the edits that differ, lets one be unticked, and queues one meta.correct job', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(routes({ 'POST /admin/bridge/jobs': { status: 201, body: job() } }));
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    // Looked up by the book's ASIN.
    const lookup = calls.find((c) => c.path === '/admin/libraries/1/book/match');
    expect(lookup?.query.get('asin')).toBe('B003P2WO5E');
    const rows = await within(dialog).findByRole('list', { name: 'Corrections to suggest' });
    // The edited title agrees with the community: no row for it.
    expect(within(rows).getAllByRole('listitem')).toHaveLength(2);
    expect(within(rows).getByText('B. Sanderson')).toBeInTheDocument();
    expect(within(rows).getByText('Brandon Sanderson')).toBeInTheDocument();
    expect(within(rows).getAllByText('Reviewed by a maintainer')).toHaveLength(2);
    // Evidence is prefilled with where the values come from.
    const evidence = within(dialog).getByRole('textbox', { name: 'Evidence' });
    expect(evidence).toHaveValue(
      'Corrected in an AudioSilo server library (audiosilo-server 0.9.2).',
    );
    await user.click(within(rows).getByRole('checkbox', { name: 'Suggest Narrator' }));
    await user.type(evidence, ' See the publisher page.');
    await user.click(within(dialog).getByRole('button', { name: 'Suggest 1 correction' }));
    await waitFor(() => expect(queued(calls)).toHaveLength(1));
    expect(queued(calls)[0].body).toEqual({
      kind: 'meta.correct',
      library_id: 1,
      path: BOOK_PATH,
      params: {
        work_id: 'the-way-of-kings',
        recording_id: 'r1',
        asin: 'B003P2WO5E',
        region: 'us',
        changes: [{ field: 'author', current: 'Brandon Sanderson', corrected: 'B. Sanderson' }],
        evidence:
          'Corrected in an AudioSilo server library (audiosilo-server 0.9.2). See the publisher page.',
      },
    });
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Suggest corrections' })).toBeNull(),
    );
  });

  it('marks a title correction as applied automatically', async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({ 'GET /admin/libraries/1/book': { body: edited({ title: 'The Way of Kings!' }) } }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    expect(await within(dialog).findByText('Applied automatically')).toBeInTheDocument();
  });

  it('keeps an open review when the Bridge goes away, so nothing typed is lost', async () => {
    const user = userEvent.setup();
    let info = server;
    mockFetch(routes({ 'GET /server': () => ({ body: info }) }));
    const { queryClient } = renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    await user.type(
      await within(dialog).findByRole('textbox', { name: 'Evidence' }),
      ' See the publisher page.',
    );
    // The heartbeat lapses: the module's capability goes with the Bridge.
    info = {
      ...server,
      capabilities: { ...server.capabilities, bridge: false, contributions: false },
    };
    await queryClient.invalidateQueries({ queryKey: keys.server });
    // The card took the news: its action is gone...
    await waitFor(() =>
      expect(
        screen.queryByRole('button', { name: 'Suggest corrections', hidden: true }),
      ).toBeNull(),
    );
    // ...but the review stays, with what was typed.
    expect(screen.getByRole('dialog', { name: 'Suggest corrections' })).toBeInTheDocument();
    expect(within(dialog).getByRole('textbox', { name: 'Evidence' })).toHaveValue(
      'Corrected in an AudioSilo server library (audiosilo-server 0.9.2). See the publisher page.',
    );
  });

  it('requires the evidence', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(routes());
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    await user.clear(await within(dialog).findByRole('textbox', { name: 'Evidence' }));
    await user.click(within(dialog).getByRole('button', { name: 'Suggest 2 corrections' }));
    expect(await within(dialog).findByText('Say where the values come from.')).toBeInTheDocument();
    expect(queued(calls)).toHaveLength(0);
  });

  it("won't send what holds a hidden text-direction character, and says why", async () => {
    const user = userEvent.setup();
    const calls = mockFetch(
      routes({
        'GET /admin/libraries/1/book': {
          body: edited({ author: 'B. \u202eSanderson', narrator: 'Kate Reading' }),
        },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    const rows = await within(dialog).findByRole('list', { name: 'Corrections to suggest' });
    const author = within(rows).getByRole('checkbox', { name: 'Suggest Author' });
    expect(author).not.toBeChecked();
    expect(author).toHaveAttribute('data-disabled');
    expect(
      within(rows).getByText(
        "Holds a hidden text-direction character the community refuses, so it can't be sent.",
      ),
    ).toBeInTheDocument();
    await user.type(
      within(dialog).getByRole('textbox', { name: 'Evidence' }),
      ' See \u2066it\u2069.',
    );
    expect(
      within(dialog).getByText(
        'The evidence holds a hidden text-direction character the community refuses. Retype any text you pasted.',
      ),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Suggest 1 correction' }));
    expect(queued(calls)).toHaveLength(0);
  });

  it('reads character references as the server does, flagging what it would refuse', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(
      routes({
        'POST /admin/bridge/jobs': { status: 201, body: job() },
        'GET /admin/libraries/1/book': {
          body: edited({
            author: 'B. &#x202E;Sanderson',
            narrator: 'Michael Kramer&#32;',
            title: 'The Way &#1;of Kings',
            isbn: '9780765326355',
          }),
        },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    const rows = await within(dialog).findByRole('list', { name: 'Corrections to suggest' });
    for (const field of ['Title', 'Author', 'Narrator']) {
      const box = within(rows).getByRole('checkbox', { name: `Suggest ${field}` });
      expect(box).not.toBeChecked();
      expect(box).toHaveAttribute('data-disabled');
    }
    expect(within(rows).getByRole('checkbox', { name: 'Suggest ISBN' })).toBeChecked();
    expect(
      within(rows).getByText(
        "Holds a hidden text-direction character the community refuses, so it can't be sent.",
      ),
    ).toBeInTheDocument();
    expect(
      within(rows).getByText(
        "Holds a control character the community refuses, so it can't be sent.",
      ),
    ).toBeInTheDocument();
    expect(within(rows).getByText(/this changes nothing/)).toBeInTheDocument();

    // The evidence: an override or a control by reference, the form's blank.
    const evidence = within(dialog).getByRole('textbox', { name: 'Evidence' });
    const submit = within(dialog).getByRole('button', { name: 'Suggest 1 correction' });
    await user.type(evidence, ' See &#8238;it.');
    expect(
      within(dialog).getByText(
        'The evidence holds a hidden text-direction character the community refuses. Retype any text you pasted.',
      ),
    ).toBeInTheDocument();
    await user.clear(evidence);
    await user.type(evidence, 'See&#1;it.');
    expect(
      within(dialog).getByText(
        'The evidence holds a control character the community refuses. Retype any text you pasted.',
      ),
    ).toBeInTheDocument();
    await user.clear(evidence);
    await user.type(evidence, '_No response_');
    await user.click(submit);
    expect(await within(dialog).findByText('Say where the values come from.')).toBeInTheDocument();
    expect(queued(calls)).toHaveLength(0);

    // Markup by reference is fine, and only the ISBN goes.
    await user.clear(evidence);
    await user.type(evidence, 'The cover reads &lt;Kings&gt;.');
    await user.click(submit);
    await waitFor(() => expect(queued(calls)).toHaveLength(1));
    expect(queued(calls)[0].body).toMatchObject({
      params: {
        changes: [{ field: 'isbn', current: '', corrected: '9780765326355' }],
        evidence: 'The cover reads &lt;Kings&gt;.',
      },
    });
  });

  it('clamps a long book title in the header, keeping the whole of it', async () => {
    const user = userEvent.setup();
    const long = 'The Way of Kings '.repeat(30).trim();
    const d = edited({ author: 'B. Sanderson' });
    d.book = { ...d.book, title: long };
    mockFetch(routes({ 'GET /admin/libraries/1/book': { body: d } }));
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    const name = within(dialog).getByText(long);
    expect(name).toHaveClass('line-clamp-2');
    expect(name).toHaveAttribute('title', long);
    expect(dialog).toHaveAccessibleDescription(long);
  });

  it('says so when the community already agrees, and sends nothing', async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({ 'GET /admin/libraries/1/book': { body: edited({ title: 'The Way of Kings' }) } }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    expect(
      await within(dialog).findByText(/The community already has your edited values/),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Suggest 0 corrections' })).toBeDisabled();
  });

  it("says when the community doesn't find the book by its identifier", async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({
        'GET /admin/libraries/1/book/match': {
          body: { candidates: [{ ...candidate, recording_id: undefined }] },
        },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    expect(
      await within(dialog).findByText('The community has no record for this book'),
    ).toBeInTheDocument();
  });

  it('waits for unsaved edits, since the review reads the saved values', async () => {
    const user = userEvent.setup();
    mockFetch(routes());
    renderApp(URL);
    const action = await screen.findByRole('button', { name: 'Suggest corrections' });
    expect(action).toBeEnabled();
    await user.click(within(screen.getByRole('group', { name: 'Author' })).getByRole('button'));
    await user.type(
      within(screen.getByRole('group', { name: 'Author' })).getByRole('textbox'),
      ' Jr{Enter}',
    );
    expect(screen.getByRole('button', { name: 'Suggest corrections' })).toBeDisabled();
  });

  it("isn't offered without the meta family, or with no edited field", async () => {
    mockFetch(
      routes({
        'GET /server': {
          body: { ...server, bridge: { version: '0.3.0', features: ['bridge', 'contribute'] } },
        },
      }),
    );
    renderApp(URL);
    await screen.findByRole('heading', { level: 1, name: 'The Way of Kings' });
    expect(screen.queryByRole('button', { name: 'Suggest corrections' })).toBeNull();
  });

  it("shows the filed issue and who applies what, and the server's refusal in words", async () => {
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                state: 'done',
                finished_at: '2026-10-19T12:05:00Z',
                params: {
                  changes: [
                    { field: 'author', current: 'Brandon Sanderson', corrected: 'B. Sanderson' },
                  ],
                },
                result: {
                  issue_url: 'https://github.com/KodeStar/audiosilo-meta/issues/12',
                  issue_number: 12,
                  changes: [{}, {}],
                  auto: ['title'],
                  maintainer: ['author'],
                },
              }),
            ],
          },
        },
      }),
    );
    renderApp(URL);
    expect(await screen.findByText(/2 corrections filed/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /See issue #12/ })).toHaveAttribute(
      'href',
      'https://github.com/KodeStar/audiosilo-meta/issues/12',
    );
    expect(screen.getByText('Applied automatically once checked: Title')).toBeInTheDocument();
    expect(screen.getByText('Waiting for a maintainer: Author')).toBeInTheDocument();
  });

  it('starts a field suggested before unticked', async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                state: 'done',
                params: {
                  changes: [
                    { field: 'author', current: 'Brandon Sanderson', corrected: 'B. Sanderson' },
                  ],
                },
              }),
            ],
          },
        },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    const author = await within(dialog).findByRole('checkbox', { name: 'Suggest Author' });
    expect(author).not.toBeChecked();
    expect(within(dialog).getByText('Suggested before')).toBeInTheDocument();
    expect(within(dialog).getByRole('checkbox', { name: 'Suggest Narrator' })).toBeChecked();
  });

  it('shows the open job instead of the action while one works', async () => {
    mockFetch(
      routes({ 'GET /admin/bridge/jobs': { body: { jobs: [job({ state: 'running' })] } } }),
    );
    renderApp(URL);
    expect(
      await screen.findByRole('button', { name: 'Cancel the corrections' }),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Suggest corrections' })).toBeNull();
  });

  it("words the server's not_edited refusal", async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({
        'POST /admin/bridge/jobs': {
          status: 400,
          body: { error: 'not edited', code: 'not_edited', field: 'author' },
        },
      }),
    );
    renderApp(URL);
    await user.click(await screen.findByRole('button', { name: 'Suggest corrections' }));
    const dialog = await screen.findByRole('dialog', { name: 'Suggest corrections' });
    await user.click(await within(dialog).findByRole('button', { name: 'Suggest 2 corrections' }));
    expect(
      await screen.findByText(/One of these values isn't your edit any more/),
    ).toBeInTheDocument();
    // The dialog stays, so nothing typed is lost.
    expect(screen.getByRole('dialog', { name: 'Suggest corrections' })).toBeInTheDocument();
  });
});
