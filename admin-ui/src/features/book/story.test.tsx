import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { BookMeta, BridgeJob, MetaWork } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { formatDate } from '@/lib/format';
import { bookDetail } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';
import { recapHeading, recapRows, roleKey, storySources, summaryParts } from './story-model';

// The book page's Characters and recaps card (Bridge Phase 1e): the entries
// /meta serves, where each dimension came from, recaps closed until opened,
// a generate job's contribution link, and the empty states.

const BOOK_PATH = 'Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings';
const URL = `/library/book?library=1&path=${encodeURIComponent(BOOK_PATH)}`;
const ISSUE = 'https://github.com/KodeStar/audiosilo-meta-community/issues/12';

const attribution = {
  credit: 'AudioSilo Meta community contributors',
  license: 'CC BY-SA 4.0',
  license_url: 'https://creativecommons.org/licenses/by-sa/4.0/',
  source_url: 'https://meta.audiosilo.app/works/the-way-of-kings',
};

function work(over: Partial<MetaWork> = {}): MetaWork {
  return {
    id: 'the-way-of-kings',
    title: 'The Way of Kings',
    authors: [],
    language: 'en',
    characters: [
      {
        id: 'kaladin',
        name: 'Kaladin',
        aliases: ['Kal', 'Stormblessed'],
        role: 'protagonist',
        reveal: { chapter: 1 },
        description: 'A surgeon turned spearman.',
      },
      { id: 'szeth', name: 'Szeth', role: 'antagonist', reveal: { chapter: 0 } },
    ],
    recaps: [
      { through: { chapter: 5 }, text: 'Kaladin reaches the Shattered Plains.' },
      { through: { chapter: 2 }, text: 'Kaladin is sold to Sadeas.' },
    ],
    recap_summary: { in_short: 'A war on the Plains.', ending: 'Kaladin saves the army.' },
    ...over,
  };
}

const meta = (w?: MetaWork, matched = true): BookMeta => ({ matched, ...(w ? { work: w } : {}) });

const bridgeOn = {
  ...serverInfo,
  capabilities: { ...serverInfo.capabilities, bridge: true },
  bridge: { version: '0.2.0', features: ['bridge', 'sidecars'] },
};

function generateJob(over: Partial<BridgeJob> = {}): BridgeJob {
  return {
    id: 31,
    kind: 'sidecars.generate',
    library_id: 1,
    path: BOOK_PATH,
    params: { source: 'auto', contribute: 'issue' },
    state: 'done',
    progress: {},
    result: { characters: 2, recaps: 2, contributed: { recaps: ISSUE } },
    error: '',
    requested_by: { id: 1, username: 'chris' },
    priority: 0,
    attempts: 1,
    lease_until: null,
    cancel_requested: false,
    created_at: '2026-10-15T12:00:00Z',
    updated_at: '2026-10-15T12:10:00Z',
    claimed_at: '2026-10-15T12:00:05Z',
    finished_at: '2026-10-15T12:10:00Z',
    ...over,
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/libraries/1/book': { body: bookDetail() },
    'GET /libraries/1/meta': { body: meta(work({ attribution })) },
    ...over,
  });
}

/** The card, once it has an answer. */
async function card() {
  const heading = await screen.findByRole('heading', { level: 3, name: 'Characters and recaps' });
  const section = heading.closest('section')!;
  await within(section).findByRole('tab', { name: /Characters/ });
  return section;
}

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('the Characters and recaps card', () => {
  it("lists the community's characters with their source and credit", async () => {
    mockFetch(routes());
    renderApp(URL);
    const c = await card();
    expect(c).toHaveAttribute('id', 'characters-and-recaps');
    expect(within(c).getByText('Kaladin')).toBeInTheDocument();
    expect(within(c).getByText('Protagonist')).toBeInTheDocument();
    expect(within(c).getByText('Also known as Kal and Stormblessed')).toBeInTheDocument();
    expect(within(c).getByText('A surgeon turned spearman.')).toBeInTheDocument();
    expect(within(c).getByText('From chapter 1')).toBeInTheDocument();
    expect(within(c).getByText('From the start')).toBeInTheDocument();
    expect(within(c).getByText('Community')).toBeInTheDocument();
    expect(within(c).getByRole('link', { name: attribution.credit })).toHaveAttribute(
      'href',
      attribution.source_url,
    );
    expect(within(c).getByRole('link', { name: 'CC BY-SA 4.0' })).toHaveAttribute(
      'href',
      attribution.license_url,
    );
    expect(within(c).queryByText('Generated on this server')).toBeNull();
  });

  it('keeps each recap closed until it is opened, in chapter order', async () => {
    const user = userEvent.setup();
    mockFetch(routes());
    renderApp(URL);
    const c = await card();
    await user.click(within(c).getByRole('tab', { name: /Recaps/ }));
    const rows = within(c).getAllByRole('button', { name: /^Through chapter/ });
    expect(rows.map((r) => r.textContent)).toEqual(['Through chapter 2', 'Through chapter 5']);
    // Nothing is given away before it's asked for.
    expect(within(c).queryByText('Kaladin is sold to Sadeas.')).toBeNull();
    expect(within(c).queryByText('Kaladin reaches the Shattered Plains.')).toBeNull();
    expect(within(c).queryByText('Kaladin saves the army.')).toBeNull();

    // The tab counts the whole-book summary's row too.
    expect(within(c).getByRole('tab', { name: /Recaps/ })).toHaveTextContent('Recaps3');
    expect(rows[0]).toHaveAttribute('aria-expanded', 'false');
    await user.click(rows[0]);
    expect(rows[0]).toHaveAttribute('aria-expanded', 'true');
    expect(within(c).getByText('Kaladin is sold to Sadeas.')).toBeInTheDocument();
    // Only that chapter opens.
    expect(within(c).queryByText('Kaladin reaches the Shattered Plains.')).toBeNull();
    await user.click(rows[0]);
    expect(within(c).queryByText('Kaladin is sold to Sadeas.')).toBeNull();

    // The whole-book summary sits behind its own reveal.
    await user.click(within(c).getByRole('button', { name: 'The whole book' }));
    expect(within(c).getByText('A war on the Plains.')).toBeInTheDocument();
    expect(within(c).getByText('Kaladin saves the army.')).toBeInTheDocument();
  });

  it('says what was generated on this server, when, and the contribution waiting for review', async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({
        'GET /server': { body: bridgeOn },
        'GET /admin/bridge/estimate': {
          body: {
            kind: 'sidecars.generate',
            library_id: 1,
            path: BOOK_PATH,
            source: 'audio',
            subscription: false,
            agent_ready: true,
            can_contribute: true,
            figures: true,
            writable: true,
            can_describe: false,
            has: {
              community: { characters: true, recaps: false, description: false },
              local: { characters: false, recaps: true, description: false },
            },
            earlier: [],
          },
        },
        'GET /admin/bridge/jobs': { body: { jobs: [generateJob()] } },
        // Community characters; recaps and their summary generated here fill the gap.
        'GET /libraries/1/meta': {
          body: meta(
            work({
              attribution,
              generated: {
                characters: false,
                recaps: true,
                recap_summary: true,
                description: false,
                generated_at: '2026-10-15T12:10:00Z',
              },
            }),
          ),
        },
      }),
    );
    renderApp(URL);
    const c = await card();
    // Characters: the community's, and no contribution link beside them.
    expect(within(c).getByText('Community')).toBeInTheDocument();
    expect(within(c).queryByRole('link', { name: /waiting for review/ })).toBeNull();
    await user.click(within(c).getByRole('tab', { name: /Recaps/ }));
    expect(within(c).getByText('Generated on this server')).toBeInTheDocument();
    expect(within(c).getByText(formatDate('2026-10-15T12:10:00Z', 'en'))).toBeInTheDocument();
    expect(
      within(c).getByRole('link', { name: 'Contributed, waiting for review' }),
    ).toHaveAttribute('href', ISSUE);
    expect(within(c).queryByText('Community')).toBeNull();
  });

  it('credits a community summary kept beside a generated list to the community', async () => {
    const user = userEvent.setup();
    mockFetch(
      routes({
        'GET /libraries/1/meta': {
          body: meta(
            work({
              attribution,
              // The community had only the summary: the list was generated here.
              generated: {
                characters: false,
                recaps: true,
                description: false,
                generated_at: '2026-10-15T12:10:00Z',
              },
            }),
          ),
        },
      }),
    );
    renderApp(URL);
    const c = await card();
    await user.click(within(c).getByRole('tab', { name: /Recaps/ }));
    const panel = within(c).getByRole('tabpanel');
    // The list's source heads the tab; the summary's row carries its own, with the credit.
    expect(within(panel).getByText('Generated on this server')).toBeInTheDocument();
    const row = within(panel).getByRole('button', { name: 'The whole book' }).closest('li')!;
    expect(within(row).getByText('Community')).toBeInTheDocument();
    expect(within(row).getByRole('link', { name: attribution.credit })).toHaveAttribute(
      'href',
      attribution.source_url,
    );
    expect(within(row).queryByText('Generated on this server')).toBeNull();
  });

  it("gives the summary no badge of its own when it shares the list's source", async () => {
    const user = userEvent.setup();
    mockFetch(routes());
    renderApp(URL);
    const c = await card();
    await user.click(within(c).getByRole('tab', { name: /Recaps/ }));
    const row = within(c).getByRole('button', { name: 'The whole book' }).closest('li')!;
    expect(within(row).queryByText('Community')).toBeNull();
  });

  it('shows an empty state per dimension when the book has neither', async () => {
    const user = userEvent.setup();
    mockFetch(routes({ 'GET /libraries/1/meta': { body: meta(undefined, false) } }));
    renderApp(URL);
    const c = await card();
    expect(within(c).getByText('No characters yet.')).toBeInTheDocument();
    await user.click(within(c).getByRole('tab', { name: /Recaps/ }));
    expect(within(c).getByText('No recaps yet.')).toBeInTheDocument();
  });

  it('cuts a long list at ten, with Show all', async () => {
    const user = userEvent.setup();
    const many = Array.from({ length: 12 }, (_, i) => ({
      id: `c${i}`,
      name: `Person ${i + 1}`,
      reveal: { chapter: i + 1 },
    }));
    mockFetch(routes({ 'GET /libraries/1/meta': { body: meta(work({ characters: many })) } }));
    renderApp(URL);
    const c = await card();
    expect(within(c).getByText('Person 10')).toBeInTheDocument();
    expect(within(c).queryByText('Person 11')).toBeNull();
    await user.click(within(c).getByRole('button', { name: 'Show all 12 characters' }));
    expect(within(c).getByText('Person 12')).toBeInTheDocument();
  });

  it('is absent while community metadata is off, and asks nothing', async () => {
    const calls = mockFetch(
      routes({
        'GET /server': {
          body: { ...serverInfo, capabilities: { ...serverInfo.capabilities, metadata: false } },
        },
      }),
    );
    renderApp(URL);
    await screen.findByRole('heading', { level: 1, name: 'The Way of Kings' });
    expect(screen.queryByRole('heading', { level: 3, name: 'Characters and recaps' })).toBeNull();
    expect(calls.some((r) => r.path === '/libraries/1/meta')).toBe(false);
  });
});

describe('story model', () => {
  it('names one source per dimension, a contribution only beside generated entries', () => {
    expect(storySources(undefined)).toEqual({
      characters: { source: 'none' },
      recaps: { source: 'none' },
      summary: { source: 'none' },
    });
    expect(storySources(work(), { characters: ISSUE, recaps: ISSUE })).toEqual({
      characters: { source: 'community' },
      recaps: { source: 'community' },
      summary: { source: 'community' },
    });
    const generated = work({
      generated: { characters: true, recaps: false, description: false, generated_at: 'T' },
      recaps: [],
      recap_summary: { in_short: 'Only a summary.' },
    });
    expect(storySources(generated, { characters: ISSUE })).toEqual({
      characters: { source: 'generated', generatedAt: 'T', contributed: ISSUE },
      // No list; the summary has a source of its own.
      recaps: { source: 'none' },
      summary: { source: 'community' },
    });
    // A generated list beside the community's summary (recap_summary absent),
    // and both generated (recap_summary true, the same contribution).
    const list = { characters: false, recaps: true, description: false, generated_at: 'T' };
    expect(storySources(work({ generated: list }), { recaps: ISSUE })).toMatchObject({
      recaps: { source: 'generated', contributed: ISSUE },
      summary: { source: 'community' },
    });
    expect(
      storySources(work({ generated: { ...list, recap_summary: true } }), { recaps: ISSUE }),
    ).toMatchObject({
      recaps: { source: 'generated', contributed: ISSUE },
      summary: { source: 'generated', generatedAt: 'T', contributed: ISSUE },
    });
  });

  it('orders recaps by chapter and knows its roles and summaries', () => {
    expect(recapRows(work()).map((r) => r.through.chapter)).toEqual([2, 5]);
    expect(roleKey('minor')).toBe('book.story.role.minor');
    expect(roleKey('sidekick')).toBeUndefined();
    expect(roleKey(undefined)).toBeUndefined();
    expect(summaryParts(work({ recap_summary: { in_short: ' ' } }))).toBeUndefined();
    expect(summaryParts(work({ recap_summary: { in_short: ' Short. ', ending: '' } }))).toEqual({
      inShort: 'Short.',
      ending: '',
    });
  });

  it('heads a chapter-0 recap by its scope: earlier books for series, before this book otherwise', () => {
    expect(recapHeading({ through: { chapter: 4 }, text: 'x' })).toEqual({
      key: 'book.story.through',
      n: 4,
    });
    expect(recapHeading({ through: { chapter: 0 }, scope: 'series', text: 'x' })).toEqual({
      key: 'book.story.previously',
    });
    expect(recapHeading({ through: { chapter: 0 }, scope: 'book', text: 'x' })).toEqual({
      key: 'book.story.beforeBook',
    });
    expect(recapHeading({ through: { chapter: 0 }, text: 'x' })).toEqual({
      key: 'book.story.beforeBook',
    });
  });
});
