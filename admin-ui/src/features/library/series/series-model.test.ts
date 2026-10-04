import type { MetaSeries } from '@/api/types';
import { adminBook } from '@/test/library-fixtures';
import {
  formatPositions,
  groupSeriesPages,
  metaCandidate,
  pickRail,
  railEntries,
  seriesKey,
  seriesStatus,
  spineHeight,
  spineRow,
} from './series-model';

const work = (position: string, title: string) => ({
  id: title.toLowerCase().replace(/\W+/g, '-'),
  title,
  position,
  authors: [],
  web_url: '',
});

const rail = (name: string, works: ReturnType<typeof work>[]): MetaSeries => ({
  id: name,
  name,
  position: '1',
  works,
});

const stormlight = rail('The Stormlight Archive', [
  work('1', 'The Way of Kings'),
  work('2', 'Words of Radiance'),
  work('3', 'Oathbringer'),
  work('4', 'Rhythm of War'),
  work('5', 'Wind and Truth'),
]);

const book = (series_index: number, title: string, over = {}) =>
  adminBook({ series_index, title, path: `Stormlight/${title}`, ...over });

describe('rails', () => {
  it('matches the series name without case, accents or punctuation', () => {
    expect(seriesKey('The Stormlight Archive')).toBe(seriesKey('the stormlight-archive'));
    expect(seriesKey('Les Rougon-Macquart')).toBe(seriesKey('les rougon macquart'));
    expect(seriesKey('Émile')).toBe('emile');
  });

  it("picks the rail named like the series, else the book's first", () => {
    const cosmere = rail('Cosmere', []);
    expect(pickRail([cosmere, stormlight], 'The Stormlight Archive.')).toBe(stormlight);
    expect(pickRail([cosmere, stormlight], 'Mistborn')).toBe(cosmere);
    expect(pickRail([], 'x')).toBeUndefined();
    expect(pickRail(undefined, 'x')).toBeUndefined();
  });

  it('keeps one numbered entry per position, in order', () => {
    const r = rail('X', [
      work('2', 'Two'),
      work('1', 'One'),
      work('', 'Unnumbered'),
      work('1.5', 'Novella'),
      work('2', 'Two again'),
      work('omnibus', 'Box set'),
    ]);
    expect(railEntries(r)).toEqual([
      { position: 1, title: 'One' },
      { position: 1.5, title: 'Novella' },
      { position: 2, title: 'Two' },
    ]);
  });
});

describe('seriesStatus', () => {
  it('compares rail positions with the series index', () => {
    const owned = [book(1, 'The Way of Kings'), book(2, 'Words of Radiance'), book(4, 'RoW')];
    const st = seriesStatus(stormlight, owned);
    expect(st.total).toBe(5);
    expect(st.have).toEqual([1, 2, 4]);
    expect(st.missing).toEqual([
      { position: 3, title: 'Oathbringer' },
      { position: 5, title: 'Wind and Truth' },
    ]);
  });

  it("doesn't count an unnumbered book as holding a position", () => {
    expect(seriesStatus(stormlight, [book(0, 'Edgedancer')]).have).toEqual([]);
  });

  it('is complete when every position is held', () => {
    const owned = [1, 2, 3, 4, 5].map((i) => book(i, `B${i}`));
    expect(seriesStatus(stormlight, owned).missing).toEqual([]);
  });
});

describe('formatPositions', () => {
  it('collapses runs of three or more whole numbers', () => {
    expect(formatPositions([1, 2, 4])).toBe('1, 2, 4');
    expect(formatPositions([3, 5])).toBe('3, 5');
    expect(formatPositions([3, 4, 5])).toBe('3-5');
    expect(formatPositions([1, 2, 3, 4, 6, 7.5, 8, 9, 10])).toBe('1-4, 6, 7.5, 8-10');
    expect(formatPositions([])).toBe('');
  });

  it('formats each number with the given formatter', () => {
    expect(formatPositions([1.5, 2], (n) => String(n).replace('.', ','))).toBe('1,5, 2');
  });
});

describe('spineRow', () => {
  it('interleaves owned books and gaps by position, unnumbered books last', () => {
    const row = spineRow(
      [book(2, 'Words of Radiance'), book(0, 'Edgedancer'), book(1, 'The Way of Kings')],
      [
        { position: 3, title: 'Oathbringer' },
        { position: 1.5, title: 'Novella' },
      ],
    );
    expect(row.map((s) => (s.kind === 'book' ? s.book.title : `gap ${s.entry.title}`))).toEqual([
      'The Way of Kings',
      'gap Novella',
      'Words of Radiance',
      'gap Oathbringer',
      'Edgedancer',
    ]);
  });
});

describe('groupSeriesPages', () => {
  it('groups by series in index order and stops at the first book without one', () => {
    const pages = [
      {
        books: [
          book(2, 'Words of Radiance'),
          book(1, 'The Way of Kings'),
          adminBook({ series: 'The Murderbot Diaries', path: 'm1' }),
        ],
        next_cursor: 'c1',
      },
    ];
    const first = groupSeriesPages(pages, true);
    expect(first.done).toBe(false);
    expect(first.bySeries.get('The Stormlight Archive')?.map((b) => b.title)).toEqual([
      'The Way of Kings',
      'Words of Radiance',
    ]);
    expect(first.bySeries.get('The Murderbot Diaries')).toHaveLength(1);

    const second = groupSeriesPages(
      [...pages, { books: [adminBook({ series: '', path: 'loose' })], next_cursor: 'c2' }],
      true,
    );
    expect(second.done).toBe(true);
    expect([...second.bySeries.keys()]).not.toContain('');
    // No next page: everything has loaded.
    expect(groupSeriesPages(pages, false).done).toBe(true);
  });
});

describe('metaCandidate and spineHeight', () => {
  it('picks the first matched book', () => {
    const a = book(1, 'A', { asin: '', isbn: '' });
    const b = book(2, 'B', { asin: '', isbn: '9780765326355' });
    expect(metaCandidate([a, b])).toBe(b);
    expect(metaCandidate([a])).toBeUndefined();
  });

  it('varies heights within the shelf, stably', () => {
    const h = spineHeight('Words of Radiance');
    expect(h).toBe(spineHeight('Words of Radiance'));
    expect(h).toBeGreaterThanOrEqual(80);
    expect(h).toBeLessThanOrEqual(95);
  });
});
