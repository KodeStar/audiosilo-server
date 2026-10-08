import type { AdminBook, MetaSeries } from '@/api/types';
import { refKey } from '@/lib/book-route';
import { coverModel } from '@/lib/cover-model';
import { adminBook } from '@/test/library-fixtures';
import {
  formatPositions,
  groupSeriesPages,
  metaCandidate,
  pickRail,
  placeBooks,
  positionIn,
  railEntries,
  seriesKey,
  seriesStatus,
  spineColors,
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

/** Work ids for some owned books, as the works endpoint answers them. */
const resolved = (pairs: [AdminBook, string][]) => new Map(pairs.map(([b, id]) => [refKey(b), id]));

/** A spine row as text: "#n Title" for books, "gap n" for missing entries. */
const shelf = (row: ReturnType<typeof spineRow>) =>
  row.map((s) =>
    s.kind === 'gap'
      ? `gap ${s.entry.position}`
      : s.position > 0
        ? `#${s.position} ${s.book.title}`
        : s.book.title,
  );

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
    const st = seriesStatus(stormlight, placeBooks(owned, stormlight));
    expect(st.total).toBe(5);
    expect(st.have).toEqual([1, 2, 4]);
    expect(st.missing).toEqual([
      { position: 3, title: 'Oathbringer' },
      { position: 5, title: 'Wind and Truth' },
    ]);
  });

  it("doesn't count an unnumbered book as holding a position", () => {
    const owned = [book(0, 'Edgedancer')];
    expect(seriesStatus(stormlight, placeBooks(owned, stormlight)).have).toEqual([]);
  });

  it('is complete when every position is held', () => {
    const owned = [1, 2, 3, 4, 5].map((i) => book(i, `B${i}`));
    expect(seriesStatus(stormlight, placeBooks(owned, stormlight)).missing).toEqual([]);
  });
});

describe('placeBooks', () => {
  // All the Skills: book 1 is tagged "1", books 2-6 carry the series but no number.
  const skills = rail(
    'All the Skills',
    [1, 2, 3, 4, 5, 6].map((i) => ({ ...work(String(i), `All the Skills ${i}`), id: `ats-${i}` })),
  );
  const skillBooks = [1, 2, 3, 4, 5, 6].map((i) =>
    adminBook({
      series: 'All the Skills',
      series_index: i === 1 ? 1 : 0,
      title: `Skills ${i}`,
      path: `ats/${i}`,
    }),
  );

  it('places books by their community work, whatever their series index', () => {
    const works = resolved(skillBooks.map((b, i) => [b, `ats-${i + 1}`]));
    const placed = placeBooks(skillBooks, skills, works);
    const st = seriesStatus(skills, placed);
    expect(st).toEqual({ total: 6, have: [1, 2, 3, 4, 5, 6], missing: [] });
    expect(shelf(spineRow(placed, st.missing))).toEqual(
      [1, 2, 3, 4, 5, 6].map((i) => `#${i} Skills ${i}`),
    );
    // By series index alone (no works yet) only book 1 holds an entry.
    const byIndex = seriesStatus(skills, placeBooks(skillBooks, skills));
    expect(byIndex.have).toEqual([1]);
    expect(shelf(spineRow(placeBooks(skillBooks, skills), byIndex.missing))).toEqual([
      '#1 Skills 1',
      'gap 2',
      'gap 3',
      'gap 4',
      'gap 5',
      'gap 6',
      'Skills 2',
      'Skills 3',
      'Skills 4',
      'Skills 5',
      'Skills 6',
    ]);
  });

  it('holds an entry for an unnumbered book that is that work (Alice)', () => {
    const alice = rail('Alice', [
      { ...work('1', "Alice's Adventures in Wonderland"), id: 'alice-1' },
      { ...work('2', 'Through the Looking-Glass'), id: 'alice-2' },
      { ...work('3', 'The Hunting of the Snark'), id: 'alice-3' },
    ]);
    const owned = [
      adminBook({ series: 'Alice', series_index: 0, title: 'Alice in Wonderland', path: 'alice' }),
    ];
    const placed = placeBooks(owned, alice, resolved([[owned[0], 'alice-1']]));
    const st = seriesStatus(alice, placed);
    expect(st.have).toEqual([1]);
    expect(st.missing.map((e) => e.position)).toEqual([2, 3]);
    expect(shelf(spineRow(placed, st.missing))).toEqual([
      '#1 Alice in Wonderland',
      'gap 2',
      'gap 3',
    ]);
  });

  it('mixes identity, fallback and the end of the shelf', () => {
    const owned = [
      book(0, 'The Way of Kings'), // resolves to entry 1
      book(2, 'Words of Radiance'), // doesn't resolve: its index
      book(7, 'Oathbringer'), // numbered differently locally: the rail wins
      book(2.5, 'Edgedancer'), // known to be a work the rail doesn't list
      book(0, 'Dawnshard'), // unnumbered, unresolved
    ];
    const works = resolved([
      [owned[0], 'the-way-of-kings'],
      [owned[2], 'oathbringer'],
      [owned[3], 'edgedancer'],
    ]);
    const placed = placeBooks(owned, stormlight, works);
    const st = seriesStatus(stormlight, placed);
    expect(st.have).toEqual([1, 2, 3]);
    expect(st.missing.map((e) => e.position)).toEqual([4, 5]);
    expect(shelf(spineRow(placed, st.missing))).toEqual([
      '#1 The Way of Kings',
      '#2 Words of Radiance',
      '#3 Oathbringer',
      'gap 4',
      'gap 5',
      '#2.5 Edgedancer',
      'Dawnshard',
    ]);
  });

  it('counts a position once and a book once', () => {
    const twice = rail('Twice', [
      { ...work('1', 'Omnibus'), id: 'omnibus' },
      { ...work('2', 'Two'), id: 'two' },
      { ...work('3', 'Omnibus again'), id: 'omnibus' },
    ]);
    const copies = [
      adminBook({ series_index: 0, title: 'Two (m4b)', path: 'two-a' }),
      adminBook({ series_index: 0, title: 'Two (mp3)', path: 'two-b' }),
      adminBook({ series_index: 0, title: 'Omnibus', path: 'omni' }),
    ];
    const placed = placeBooks(
      copies,
      twice,
      resolved([
        [copies[0], 'two'],
        [copies[1], 'two'],
        [copies[2], 'omnibus'],
      ]),
    );
    const st = seriesStatus(twice, placed);
    // Both copies of book 2 sit at 2; the omnibus holds its first entry only.
    expect(st.have).toEqual([1, 2]);
    expect(st.missing.map((e) => e.position)).toEqual([3]);
    expect(shelf(spineRow(placed, st.missing))).toEqual([
      '#1 Omnibus',
      '#2 Two (m4b)',
      '#2 Two (mp3)',
      'gap 3',
    ]);
  });

  it('ignores work ids without a rail', () => {
    const owned = [book(0, 'The Way of Kings')];
    expect(placeBooks(owned, undefined, resolved([[owned[0], 'the-way-of-kings']]))).toEqual([
      { book: owned[0], slot: 'end', position: 0 },
    ]);
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
      placeBooks([
        book(2, 'Words of Radiance'),
        book(0, 'Edgedancer'),
        book(1, 'The Way of Kings'),
      ]),
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

describe('a book in several series', () => {
  const guards = adminBook({
    title: 'Guards! Guards!',
    path: 'gg',
    series: 'Discworld',
    series_index: 8,
    series_list: [
      { name: 'Discworld', position: 8 },
      { name: 'City Watch', position: 1 },
    ],
  });
  const arms = adminBook({
    title: 'Men at Arms',
    path: 'maa',
    series: 'City Watch',
    series_index: 2,
  });

  it('has its own position in each', () => {
    expect(positionIn(guards)).toBe(8);
    expect(positionIn(guards, 'Discworld')).toBe(8);
    expect(positionIn(guards, 'City Watch')).toBe(1);
    expect(positionIn(guards, 'Mort')).toBe(0);
  });

  it("sits on a series' shelf at its place there", () => {
    expect(
      placeBooks([arms, guards], undefined, undefined, 'City Watch').map((p) => p.position),
    ).toEqual([2, 1]);
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

  it('sizes a spine by the book length, on a log scale within the shelf', () => {
    const hours = (h: number) => spineHeight(h * 3600, 'x');
    expect(hours(1)).toBe(72);
    expect(hours(3)).toBe(72);
    expect(hours(30)).toBe(98);
    expect(hours(55)).toBe(98);
    expect(hours(7)).toBeLessThan(hours(12));
    expect(hours(12)).toBeLessThan(hours(45));
  });

  it('varies a spine without a length, stably', () => {
    const h = spineHeight(0, 'Words of Radiance');
    expect(h).toBe(spineHeight(0, 'Words of Radiance'));
    expect(h).toBeGreaterThanOrEqual(80);
    expect(h).toBeLessThanOrEqual(95);
  });
});

describe('spineColors', () => {
  const b = { title: 'Mort', author: 'Terry Pratchett' };

  it('takes the cover colour, white type on a dark body, the accent as bands', () => {
    expect(spineColors({ ...b, cover_color: { bg: '#1e2a50', accent: '#f0a020' } })).toEqual({
      body: '#1e2a50',
      band: '#f0a020',
      ink: '#ffffff',
    });
  });

  it('sets ink type on a light body, and bands in the type colour without an accent', () => {
    expect(spineColors({ ...b, cover_color: { bg: '#f4e8c8' } })).toEqual({
      body: '#f4e8c8',
      band: '#121c36',
      ink: '#121c36',
    });
  });

  it('falls back to the procedural cover palette without a cover colour', () => {
    const [body, band, , ink] = coverModel(b.title, b.author).palette;
    expect(spineColors(b)).toEqual({ body, band, ink });
  });
});
