import type { AdminChapter, AdminFile } from '@/api/types';
import { bookDetail } from '@/test/library-fixtures';
import {
  chapterProblem,
  checkField,
  commitDraft,
  coverFileProblem,
  detectionKey,
  diffRows,
  draftErrors,
  fileName,
  fileStrip,
  ribbonSegments,
  saveRequest,
  undoRevertRequest,
} from './book-model';

describe('checkField', () => {
  it('trims, and refuses an empty title', () => {
    expect(checkField('title', '  Dune  ')).toEqual({ value: 'Dune' });
    expect(checkField('title', '   ').error).toBe('book.invalid.title');
    expect(checkField('author', '')).toEqual({ value: '' });
  });

  it('refuses control characters, except line breaks and tabs in a description', () => {
    expect(checkField('series', 'a\nb').error).toBe('book.invalid.control');
    expect(checkField('description', 'one\r\ntwo\tthree')).toEqual({ value: 'one\ntwo\tthree' });
    expect(checkField('description', 'bell\u0007').error).toBe('book.invalid.control');
  });

  it('caps lengths in characters, not bytes', () => {
    expect(checkField('title', 'é'.repeat(500)).error).toBeUndefined();
    expect(checkField('title', 'é'.repeat(501)).error).toBe('book.invalid.tooLong');
    expect(checkField('description', 'x'.repeat(20001)).error).toBe('book.invalid.tooLong');
  });

  it('takes a series number from 0 to 100000, 0 meaning none', () => {
    expect(checkField('series_index', '2.50')).toEqual({ value: '2.5' });
    expect(checkField('series_index', '0')).toEqual({ value: '' });
    expect(checkField('series_index', '')).toEqual({ value: '' });
    for (const bad of ['-1', '100001', 'one', '0x10', '1,5']) {
      expect(checkField('series_index', bad).error).toBe('book.invalid.seriesIndex');
    }
  });

  it('takes real YYYY, YYYY-MM and YYYY-MM-DD dates', () => {
    for (const ok of ['2010', '2010-08', '2012-02-29', '0001']) {
      expect(checkField('published', ok).error).toBeUndefined();
    }
    for (const bad of ['10', '2010-13', '2011-02-29', '2010/08', 'August 2010']) {
      expect(checkField('published', bad).error).toBe('book.invalid.published');
    }
  });

  it('normalizes ASINs and ISBNs the way the server stores them', () => {
    expect(checkField('asin', 'b003p2wo5e')).toEqual({ value: 'B003P2WO5E' });
    expect(checkField('asin', 'B003').error).toBe('book.invalid.asin');
    expect(checkField('isbn', '978-0-7653-2635-5')).toEqual({ value: '9780765326355' });
    expect(checkField('isbn', '076532635x')).toEqual({ value: '076532635X' });
    expect(checkField('isbn', '12345').error).toBe('book.invalid.isbn');
  });
});

describe('drafts', () => {
  it('drops a draft that comes back to the saved value, as the server would store it', () => {
    let d = commitDraft({}, 'title', 'Dune', 'The Way of Kings');
    expect(d).toEqual({ title: 'Dune' });
    d = commitDraft(d, 'title', ' The Way of Kings ', 'The Way of Kings');
    expect(d).toEqual({});
    expect(commitDraft({}, 'asin', 'b003p2wo5e', 'B003P2WO5E')).toEqual({});
  });

  it('keeps an invalid draft so its message shows', () => {
    const d = commitDraft({}, 'published', 'soon', '2010');
    expect(draftErrors(d)).toEqual({ published: 'book.invalid.published' });
  });

  it('diffs against the saved values and saves normalized values (empty clears)', () => {
    const detail = bookDetail();
    const drafts = { series_index: '2.0', narrator: '', asin: 'b0000000aa' };
    expect(diffRows(drafts, detail.fields)).toEqual([
      { field: 'narrator', before: 'Michael Kramer & Kate Reading', after: '', source: 'tag' },
      { field: 'series_index', before: '1', after: '2', source: 'path' },
      { field: 'asin', before: 'B003P2WO5E', after: 'B0000000AA', source: 'community' },
    ]);
    expect(saveRequest(drafts)).toEqual({
      set: { narrator: '', series_index: '2', asin: 'B0000000AA' },
    });
  });

  it('undoes a revert with the value and source the field had', () => {
    const before = { value: 'Dune', source: 'community' as const, scanned: 'x', locked: true };
    expect(undoRevertRequest('title', before)).toEqual({
      set: { title: 'Dune' },
      source: 'community',
    });
    expect(undoRevertRequest('title', { ...before, source: 'edited' }).source).toBe('edited');
  });
});

const chapter = (index: number, file: string, start: number, end: number): AdminChapter => ({
  index,
  title: `Chapter ${index + 1}`,
  scanned_title: `Chapter ${index + 1}`,
  edited: false,
  file_path: file,
  start,
  end,
  book_offset: start,
});
const file = (path: string, seq: number, duration: number): AdminFile => ({
  path,
  seq,
  duration,
  format: 'mp3',
  codec: 'mp3',
  size: 1,
  bitrate: 0,
});

describe('chapter ribbon', () => {
  const files = [file('Book/02.mp3', 2, 600), file('Book/01.mp3', 1, 300)];
  const chapters = [
    chapter(0, 'Book/01.mp3', 0, 300),
    chapter(1, 'Book/02.mp3', 0, 200),
    chapter(2, 'Book/02.mp3', 200, 600),
  ];

  it('sizes segments by length and colours them by file in play order, alternating', () => {
    const segs = ribbonSegments(chapters, files);
    expect(segs.map((s) => [s.flex, s.file])).toEqual([
      [300, 0],
      [200, 1],
      [400, 1],
    ]);
    expect(segs[0].color).toBe('color-mix(in oklab, var(--chart-1) 86%, var(--card))');
    expect(segs[1].color).toBe('color-mix(in oklab, var(--chart-2) 62%, var(--card))');
  });

  it('strips files in play order, named under the book folder', () => {
    expect(fileStrip(files, 'Book').map((f) => [f.name, f.flex, f.color])).toEqual([
      ['01.mp3', 300, 'var(--chart-1)'],
      ['02.mp3', 600, 'var(--chart-2)'],
    ]);
    expect(fileName('Book.m4b', 'Book.m4b')).toBe('Book.m4b');
  });

  it('says when chapters are missing or one chapter is the whole long book', () => {
    expect(chapterProblem([], 100)).toBe('none');
    expect(chapterProblem([chapters[0]], 3 * 3600)).toBe('single');
    expect(chapterProblem([chapters[0]], 3600)).toBeUndefined();
    expect(chapterProblem(chapters, 3 * 3600)).toBeUndefined();
  });
});

describe('the rest of the page', () => {
  it('names folder detection', () => {
    expect(detectionKey('')).toBe('book.disk.automatic');
    expect(detectionKey('book')).toBe('book.disk.pinnedBook');
    expect(detectionKey('collection')).toBe('book.disk.pinnedSplit');
  });

  it('checks a cover before it is uploaded', () => {
    expect(coverFileProblem({ type: 'image/png', size: 1000 })).toBeUndefined();
    expect(coverFileProblem({ type: 'image/gif', size: 1000 })).toBe('book.cover.unsupported');
    expect(coverFileProblem({ type: 'image/jpeg', size: 6 * 1024 * 1024 })).toBe(
      'book.cover.tooLarge',
    );
  });
});
