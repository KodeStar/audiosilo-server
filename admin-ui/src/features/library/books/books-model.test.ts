import { adminBook } from '@/test/library-fixtures';
import {
  activeFilters,
  bookFilter,
  bulkSet,
  commonValue,
  curatingShelf,
  daysAgo,
  defaultOrder,
  facetOptions,
  gridLayout,
  isBrowsing,
  listParams,
  sheetFilterCount,
  shouldLoadMore,
  tileFlags,
  toggleValue,
  withoutFilter,
  withoutFilters,
} from './books-model';

const NOW = Date.parse('2026-10-03T12:00:00Z');

describe('bookFilter', () => {
  it('is empty with no search params', () => {
    expect(bookFilter({}, NOW)).toEqual({});
  });

  it('maps every filter to the API', () => {
    expect(
      bookFilter(
        {
          q: '  kings ',
          library: 2,
          author: 'Brandon Sanderson',
          series: 'The Stormlight Archive',
          narrator: 'Kate Reading',
          format: ['m4b', 'mp3'],
          codec: ['aac'],
          playback: 'transcode',
          cover: 'no',
          chapters: 'yes',
          matched: 'no',
          edited: 'yes',
          length: 'medium',
          added: '30',
        },
        NOW,
      ),
    ).toEqual({
      q: 'kings',
      library_id: 2,
      author: 'Brandon Sanderson',
      series: 'The Stormlight Archive',
      narrator: 'Kate Reading',
      format: ['m4b', 'mp3'],
      codec: ['aac'],
      direct_playable: false,
      has_cover: false,
      has_chapters: true,
      matched: false,
      edited: true,
      min_duration: 5 * 3600,
      max_duration: 15 * 3600,
      added_after: '2026-09-03',
    });
  });

  it('maps the yes sides and the open-ended lengths', () => {
    expect(bookFilter({ playback: 'direct', cover: 'yes', length: 'short' }, NOW)).toEqual({
      direct_playable: true,
      has_cover: true,
      max_duration: 5 * 3600,
    });
    expect(bookFilter({ length: 'long' }, NOW)).toEqual({
      min_duration: 15 * 3600,
      max_duration: 30 * 3600,
    });
    expect(bookFilter({ length: 'epic' }, NOW)).toEqual({ min_duration: 30 * 3600 });
  });

  it('drops a blank search', () => {
    expect(bookFilter({ q: '   ' }, NOW)).toEqual({});
  });
});

describe('daysAgo', () => {
  it('is a UTC calendar date', () => {
    expect(daysAgo(7, NOW)).toBe('2026-09-26');
    expect(daysAgo(365, NOW)).toBe('2025-10-03');
    // Just after midnight UTC, still the UTC day.
    expect(daysAgo(0, Date.parse('2026-10-03T00:05:00Z'))).toBe('2026-10-03');
  });

  it('feeds the added filter', () => {
    expect(bookFilter({ added: '7' }, NOW)).toEqual({ added_after: '2026-09-26' });
  });
});

describe('listParams', () => {
  it('sorts by title, ascending, a page at a time by default', () => {
    expect(listParams({}, NOW)).toEqual({ sort: 'title', order: 'asc', limit: 60 });
  });

  it('reads newest, longest and largest first', () => {
    expect(defaultOrder('added')).toBe('desc');
    expect(defaultOrder('duration')).toBe('desc');
    expect(defaultOrder('size')).toBe('desc');
    expect(defaultOrder('author')).toBe('asc');
    expect(listParams({ sort: 'duration', q: 'x' }, NOW)).toEqual({
      q: 'x',
      sort: 'duration',
      order: 'desc',
      limit: 60,
    });
  });

  it('keeps an explicit order from the URL', () => {
    expect(listParams({ sort: 'added', order: 'asc' }, NOW)).toMatchObject({ order: 'asc' });
  });
});

describe('active filters', () => {
  const s = {
    q: 'way',
    library: 1,
    author: 'Brandon Sanderson',
    format: ['m4b', 'mp3'],
    cover: 'no' as const,
    sort: 'added' as const,
    view: 'table' as const,
  };

  it('lists one per value, in the chips order, without search, sort or view', () => {
    expect(activeFilters(s)).toEqual([
      { key: 'library', value: '1' },
      { key: 'author', value: 'Brandon Sanderson' },
      { key: 'format', value: 'm4b' },
      { key: 'format', value: 'mp3' },
      { key: 'cover', value: 'no' },
    ]);
  });

  it('counts the sheet filters (not the exact author, series, narrator)', () => {
    expect(sheetFilterCount(s)).toBe(4);
    expect(sheetFilterCount({ series: 'x' })).toBe(0);
  });

  it('is browsing only with no search and no filter', () => {
    expect(isBrowsing({})).toBe(true);
    expect(isBrowsing({ sort: 'added', view: 'table' })).toBe(true);
    expect(isBrowsing({ q: ' ' })).toBe(true);
    expect(isBrowsing({ q: 'x' })).toBe(false);
    expect(isBrowsing({ library: 1 })).toBe(false);
    expect(isBrowsing({ narrator: 'x' })).toBe(false);
  });

  it('removes one value of a repeatable filter, or the whole of a single one', () => {
    expect(withoutFilter(s, { key: 'format', value: 'm4b' }).format).toEqual(['mp3']);
    expect(withoutFilter({ format: ['m4b'] }, { key: 'format', value: 'm4b' }).format).toBe(
      undefined,
    );
    expect(withoutFilter(s, { key: 'library', value: '1' }).library).toBe(undefined);
  });

  it('clears every filter, keeping the search on request, and sort and view always', () => {
    expect(withoutFilters(s)).toEqual({ sort: 'added', view: 'table' });
    expect(withoutFilters(s, true)).toEqual({ q: 'way', sort: 'added', view: 'table' });
  });

  it('toggles repeatable values', () => {
    expect(toggleValue(undefined, 'm4b')).toEqual(['m4b']);
    expect(toggleValue(['m4b'], 'mp3')).toEqual(['m4b', 'mp3']);
    expect(toggleValue(['m4b'], 'm4b')).toBe(undefined);
  });
});

describe('facetOptions', () => {
  it('keeps a selected value the counts no longer hold', () => {
    expect(facetOptions([{ value: 'm4b', count: 3 }], ['flac'])).toEqual([
      { value: 'm4b', count: 3 },
      { value: 'flac', count: 0 },
    ]);
    expect(facetOptions(undefined)).toEqual([]);
  });
});

describe('selection and bulk edits', () => {
  it('finds a shared value, or how many differ', () => {
    const a = adminBook({ path: 'a' });
    const b = adminBook({ path: 'b', narrator: 'Someone else' });
    expect(commonValue([a, b], 'author')).toEqual({ same: true, value: 'Brandon Sanderson' });
    expect(commonValue([a, b], 'narrator')).toEqual({ same: false, distinct: 2 });
    expect(commonValue([], 'series')).toEqual({ same: true, value: '' });
  });

  it('sends only the filled fields, trimmed', () => {
    expect(bulkSet({ author: ' Ursula K. Le Guin ', narrator: '  ', series: '' })).toEqual({
      author: 'Ursula K. Le Guin',
    });
  });
});

describe('tileFlags', () => {
  it('flags the most actionable two', () => {
    const b = adminBook({
      has_cover: false,
      asin: '',
      isbn: '',
      direct_playable: false,
      edited: true,
    });
    expect(tileFlags(b, true)).toEqual(['cover', 'match']);
    expect(tileFlags(b, false)).toEqual(['cover', 'transcode']);
    expect(tileFlags(adminBook({ edited: true }), true)).toEqual(['edited']);
    expect(tileFlags(adminBook(), true)).toEqual([]);
  });

  it("goes by the server's matched flag", () => {
    expect(tileFlags(adminBook({ asin: '', matched: true }), true)).toEqual([]);
    expect(tileFlags(adminBook({ matched: false }), true)).toEqual(['match']);
  });
});

describe('curatingShelf', () => {
  it('lists missing covers first, then unmatched, each book once, up to the cap', () => {
    const a = adminBook({ path: 'a', has_cover: false });
    const b = adminBook({ path: 'b', has_cover: false, asin: '' });
    const c = adminBook({ path: 'c', asin: '' });
    expect(curatingShelf([a, b], [b, c])).toEqual([
      { book: a, issue: 'cover' },
      { book: b, issue: 'cover' },
      { book: c, issue: 'match' },
    ]);
    expect(curatingShelf([a, b], [c], 2)).toHaveLength(2);
  });
});

describe('gridLayout', () => {
  it('fits 158px columns with 22px gaps, like .shelf-grid', () => {
    expect(gridLayout(1000, false).columns).toBe(5);
    expect(gridLayout(158, false).columns).toBe(1);
    expect(gridLayout(338, false).columns).toBe(2);
    expect(gridLayout(337, false).columns).toBe(1);
  });

  it('has two columns on a phone, with tighter gaps', () => {
    expect(gridLayout(360, true)).toMatchObject({ columns: 2, columnGap: 14, rowGap: 22 });
  });

  it('makes a row the cover plus its text', () => {
    // 5 columns of (1000 - 4 * 22) / 5 = 182.4px.
    expect(gridLayout(1000, false).rowHeight).toBe(Math.round(182.4 + 66));
  });

  it('assumes a width before the grid is measured', () => {
    expect(gridLayout(0, false).columns).toBeGreaterThan(1);
    expect(gridLayout(0, true).columns).toBe(2);
  });
});

describe('shouldLoadMore', () => {
  it('loads near the end, once, when there is more', () => {
    expect(shouldLoadMore(8, 10, true, false)).toBe(true);
    expect(shouldLoadMore(6, 10, true, false)).toBe(false);
    expect(shouldLoadMore(9, 10, false, false)).toBe(false);
    expect(shouldLoadMore(9, 10, true, true)).toBe(false);
    expect(shouldLoadMore(undefined, 10, true, false)).toBe(false);
  });
});
