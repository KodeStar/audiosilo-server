import type {
  AdminBook,
  AdminBookDetail,
  AuthorsResponse,
  BookFacets,
  FieldValue,
  NarratorsResponse,
  OverrideField,
  SeriesCount,
} from '@/api/types';

// Admin catalog fixtures for the Library and Book screens' tests. Library 1 is
// "Fiction" and 2 "Kids", as in fixtures.ts.

export function adminBook(over: Partial<AdminBook> = {}): AdminBook {
  const book = {
    library_id: 1,
    library_name: 'Fiction',
    path: 'Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings',
    is_folder: true,
    title: 'The Way of Kings',
    author: 'Brandon Sanderson',
    narrator: 'Michael Kramer & Kate Reading',
    series: 'The Stormlight Archive',
    series_index: 1,
    published: '2010',
    duration: 163800,
    format: 'm4b',
    codec: 'aac',
    direct_playable: true,
    size: 1_310_000_000,
    added_at: '2026-09-28T10:00:00Z',
    has_cover: true,
    custom_cover: false,
    chapter_count: 88,
    file_count: 4,
    asin: 'B003P2WO5E',
    isbn: '',
    edited: false,
    ...over,
  };
  // The server's rule (an ASIN or ISBN set), unless a test says otherwise.
  return { ...book, matched: over.matched ?? !!(book.asin || book.isbn) };
}

/** A small library: a series run, a book without cover or match, an opus book. */
export const books: AdminBook[] = [
  adminBook(),
  adminBook({
    path: 'Brandon Sanderson/The Stormlight Archive/02 - Words of Radiance',
    title: 'Words of Radiance',
    series_index: 2,
    added_at: '2026-09-29T10:00:00Z',
  }),
  adminBook({
    path: 'Martha Wells/All Systems Red',
    title: 'All Systems Red',
    author: 'Martha Wells',
    narrator: 'Kevin R. Free',
    series: 'The Murderbot Diaries',
    series_index: 1,
    has_cover: false,
    asin: '',
    chapter_count: 1,
    file_count: 1,
    is_folder: false,
    duration: 11880,
  }),
  adminBook({
    library_id: 2,
    library_name: 'Kids',
    path: 'Adrian Tchaikovsky/Children of Time',
    title: 'Children of Time',
    author: 'Adrian Tchaikovsky',
    narrator: 'Mel Hudson',
    series: '',
    series_index: 0,
    format: 'opus',
    codec: 'opus',
    direct_playable: false,
    edited: true,
  }),
];

export function facets(over: Partial<BookFacets> = {}): BookFacets {
  return {
    total: books.length,
    libraries: [
      { library_id: 1, count: 3 },
      { library_id: 2, count: 1 },
    ],
    formats: [
      { value: 'm4b', count: 3 },
      { value: 'opus', count: 1 },
    ],
    codecs: [
      { value: 'aac', count: 3 },
      { value: 'opus', count: 1 },
    ],
    direct_playable: { yes: 3, no: 1 },
    has_cover: { yes: 3, no: 1 },
    has_chapters: { yes: 3, no: 1 },
    matched: { yes: 3, no: 1 },
    edited: { yes: 1, no: 3 },
    ...over,
  };
}

export const authors: AuthorsResponse = {
  authors: [
    { name: 'Adrian Tchaikovsky', books: 1, duration: 60000 },
    { name: 'Brandon Sanderson', books: 2, duration: 327600 },
    { name: 'Martha Wells', books: 1, duration: 11880 },
    { name: 'Sanderson, Brandon', books: 1, duration: 90000 },
  ],
  merge_suggestions: [
    {
      names: ['Brandon Sanderson', 'Sanderson, Brandon'],
      suggested: 'Brandon Sanderson',
      books: 3,
    },
  ],
  unknown: 0,
};

export const narrators: NarratorsResponse = {
  narrators: [
    { name: 'Kevin R. Free', books: 1, duration: 11880 },
    { name: 'Mel Hudson', books: 1, duration: 60000 },
    { name: 'Michael Kramer & Kate Reading', books: 2, duration: 327600 },
  ],
  merge_suggestions: [],
  unknown: 1,
};

export const series: SeriesCount[] = [
  {
    name: 'The Murderbot Diaries',
    author: 'Martha Wells',
    books: 1,
    duration: 11880,
    positions: [1],
  },
  {
    name: 'The Stormlight Archive',
    author: 'Brandon Sanderson',
    books: 2,
    duration: 327600,
    positions: [1, 2],
  },
];

const field = (value: string, over: Partial<FieldValue> = {}): FieldValue => ({
  value,
  source: value ? 'tag' : '',
  scanned: value,
  locked: false,
  ...over,
});

export function bookDetail(over: Partial<AdminBookDetail> = {}): AdminBookDetail {
  const book = over.book ?? books[0];
  const fields: Record<OverrideField, FieldValue> = {
    title: field(book.title),
    author: field(book.author),
    narrator: field(book.narrator),
    series: field(book.series, { source: 'path' }),
    series_index: field(String(book.series_index), { source: 'path' }),
    published: field(''),
    description: field(''),
    asin: field(book.asin, { source: 'community' }),
    isbn: field(''),
  };
  return {
    book,
    description: '',
    fields,
    chapters: [
      {
        index: 0,
        title: 'Prelude to the Stormlight Archive',
        scanned_title: 'Prelude to the Stormlight Archive',
        edited: false,
        file_path: `${book.path}/01.m4b`,
        start: 0,
        end: 1200,
        book_offset: 0,
      },
      {
        index: 1,
        title: 'Prologue: To Kill',
        scanned_title: 'Chapter 2',
        edited: true,
        file_path: `${book.path}/01.m4b`,
        start: 1200,
        end: 3000,
        book_offset: 1200,
      },
    ],
    files: [
      {
        path: `${book.path}/01.m4b`,
        seq: 1,
        duration: 3000,
        format: 'm4b',
        codec: 'aac',
        size: 48_000_000,
        bitrate: 128000,
      },
    ],
    listeners: [
      {
        user_id: 2,
        username: 'sam',
        position: 1500,
        duration: book.duration,
        finished: false,
        updated_at: '2026-10-03T09:00:00Z',
      },
    ],
    shares: [{ share_id: 7, name: 'Cosy mysteries', path: 'Brandon Sanderson' }],
    folder: { path: book.path, override: '' },
    indexed_at: '2026-10-01T10:00:00Z',
    ...over,
  };
}
