import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminBookDetail, CommunityChapters } from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { adminBook, bookDetail } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';
import { communityState } from './book-model';

const BOOK_PATH = 'Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings';
const URL = `/library/book?library=1&path=${encodeURIComponent(BOOK_PATH)}`;

function check(over: Partial<CommunityChapters>): CommunityChapters {
  return {
    status: 'refine',
    checked_at: '2026-10-08T10:00:00Z',
    detail: {
      local_duration: 55545.9,
      community_duration: 56177.8,
      local_chapters: 2,
      community_chapters: 172,
      anchors: 2,
      snapped: 140,
      approximate: 0,
    },
    ...over,
  };
}

function routes(detail: AdminBookDetail, over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/libraries/1/book': { body: detail },
    'PATCH /admin/libraries/1/book': { body: detail },
    ...over,
  });
}

const patches = (calls: MockRequest[]) =>
  calls.filter((c) => c.method === 'PATCH' && c.path === '/admin/libraries/1/book');

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('community chapters on the book page', () => {
  it('offers detailed chapters that fit, and says what this copy lacks', async () => {
    const detail = bookDetail({
      community_chapters: check({
        detail: { ...check({}).detail!, omitted: ['Preview: Chapter 1 from Odyssey'] },
      }),
    });
    const calls = mockFetch(routes(detail));
    renderApp(URL);
    expect(
      await screen.findByText(
        'The community has 172 detailed chapters that fit this copy (the file has 2).',
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText('Not in this copy: Preview: Chapter 1 from Odyssey.'),
    ).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Use detailed chapters' }));
    await waitFor(() => expect(patches(calls)).toHaveLength(1));
    expect(patches(calls)[0].body).toEqual({ chapter_source: 'community' });
  });

  it('switches back to the file’s chapters while the community’s are used', async () => {
    const detail = bookDetail({
      chapter_source: 'community',
      chapter_choice: 'community',
      community_chapters: check({
        detail: { ...check({}).detail!, approximate: 3 },
      }),
    });
    const calls = mockFetch(routes(detail));
    renderApp(URL);
    expect(
      await screen.findByText(
        'Using the community’s 172 detailed chapters in place of the file’s 2.',
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        '3 chapter starts couldn’t be matched to a pause and may be a few seconds off.',
      ),
    ).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Use the file’s chapters' }));
    await waitFor(() => expect(patches(calls)).toHaveLength(1));
    expect(patches(calls)[0].body).toEqual({ chapter_source: 'files' });
  });

  it('reviews the titles the community has for the same chapters', async () => {
    const detail = bookDetail({
      community_chapters: check({
        status: 'titles',
        detail: {
          ...check({}).detail!,
          title_diffs: [
            { index: 0, current: 'Prelude to the Stormlight Archive', community: 'Prelude' },
            { index: 1, current: 'Chapter 2', community: 'Prologue: To Kill' },
          ],
        },
      }),
    });
    const calls = mockFetch(routes(detail));
    renderApp(URL);
    // Chapter 1 already carries the community's title (an earlier edit): one left.
    expect(
      await screen.findByText('The community titles 1 chapter differently.'),
    ).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Review titles' }));
    await user.click(screen.getByRole('button', { name: 'Use the title “Prelude”' }));
    await waitFor(() => expect(patches(calls)).toHaveLength(1));
    expect(patches(calls)[0].body).toEqual({ chapters: { set: { 0: 'Prelude' } } });
  });

  it('says why chapters that cross files cannot be used', async () => {
    const detail = bookDetail({
      community_chapters: check({
        status: 'crosses_files',
        detail: {
          ...check({}).detail!,
          straddle: {
            title: 'The Second Order',
            at: 3000,
            from: `${BOOK_PATH}/01.mp3`,
            to: `${BOOK_PATH}/02.mp3`,
          },
        },
      }),
    });
    mockFetch(routes(detail));
    renderApp(URL);
    expect(
      await screen.findByText(
        'The community’s chapter “The Second Order” runs from 01.mp3 into 02.mp3, so it can’t be played as one chapter of a file.',
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        'Merging the files into one (an M4B, say) would let the community’s chapters fit.',
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^Use/ })).not.toBeInTheDocument();
  });

  it('checks on demand and follows the check to the end', async () => {
    const before = bookDetail();
    const checking = { ...before, community_checking: true };
    const done = bookDetail({
      chapter_source: 'community',
      community_chapters: check({
        status: 'fill',
        detail: { ...check({}).detail!, community_chapters: 4 },
      }),
    });
    let gets = 0;
    const calls = mockFetch(
      routes(before, {
        // The page as it loads, then while the check runs, then once it is recorded.
        'GET /admin/libraries/1/book': () => ({ body: [before, checking][gets++] ?? done }),
        'POST /admin/libraries/1/book/community-chapters': { body: checking },
      }),
    );
    renderApp(URL);
    expect(
      await screen.findByText('Not checked against the community’s chapter lists yet.'),
    ).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Check now' }));
    expect(
      await screen.findByText('Checking the community’s chapters for this copy…'),
    ).toBeInTheDocument();
    expect(
      await screen.findByText(
        'Using the community’s 4 chapters: the files have none of their own.',
        undefined,
        { timeout: 8000 },
      ),
    ).toBeInTheDocument();
    expect(
      calls.some(
        (c) => c.method === 'POST' && c.path === '/admin/libraries/1/book/community-chapters',
      ),
    ).toBe(true);
  }, 12000);
});

describe('communityState', () => {
  const duration = (s: number) => `${Math.round(s / 60)}m`;

  it('says nothing for a book that is never checked', () => {
    const unmatched = bookDetail({ book: adminBook({ asin: '', isbn: '' }) });
    expect(communityState(unmatched, duration)).toBeNull();
    expect(communityState(bookDetail(), duration)?.line.key).toBe('book.community.unchecked');
  });

  it('gives an edition mismatch both lengths', () => {
    const d = bookDetail({ community_chapters: check({ status: 'length_mismatch' }) });
    expect(communityState(d, duration)?.line).toEqual({
      key: 'book.community.why.length_mismatch',
      values: { local: '926m', community: '936m' },
    });
  });

  it('offers nothing from a check of a book that has changed since', () => {
    const d = bookDetail({ community_chapters: check({ status: 'refine', stale: true }) });
    const state = communityState(d, duration);
    expect(state?.line.key).toBe('book.community.stale');
    expect(state?.action).toBeUndefined();
  });

  it('says when the last check failed', () => {
    const d = bookDetail({ community_chapters: check({}), community_check_failed: true });
    expect(communityState(d, duration)?.notes[0]).toEqual({ key: 'book.community.lastFailed' });
  });

  it('offers a fill the admin turned off back as automatic', () => {
    const d = bookDetail({
      chapter_choice: 'files',
      community_chapters: check({ status: 'fill' }),
    });
    expect(communityState(d, duration)?.action).toEqual({
      source: 'auto',
      label: 'book.community.use',
    });
  });
});
