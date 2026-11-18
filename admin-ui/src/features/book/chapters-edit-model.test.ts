import { ApiError } from '@/api/client';
import type { AdminChapter, AdminFile } from '@/api/types';
import {
  addAfter,
  addAt,
  draftChanged,
  draftFromChapters,
  type DraftRow,
  editRequest,
  fileSpans,
  formatStamp,
  parseTime,
  parseTimestamps,
  removeRow,
  rowsFromPaste,
  saveProblem,
  setStart,
  shiftAll,
  spanAt,
  titleOk,
  toBookTime,
  toFileTime,
  timelineEnd,
  validateDraft,
} from './chapters-edit-model';

const BOOK = 'Author/Book';

function file(name: string, seq: number, duration: number): AdminFile {
  return {
    path: `${BOOK}/${name}`,
    seq,
    duration,
    format: 'mp3',
    codec: 'mp3',
    size: 1000,
    bitrate: 0,
    direct_playable: true,
  };
}

function chapter(index: number, name: string, start: number, end: number, offset: number) {
  return {
    index,
    title: `Chapter ${index + 1}`,
    scanned_title: `Chapter ${index + 1}`,
    edited: false,
    file_path: `${BOOK}/${name}`,
    start,
    end,
    book_offset: offset,
  } satisfies AdminChapter;
}

// Two files, listed out of play order: 02.mp3 plays second.
const FILES = [file('02.mp3', 2, 50.0004), file('01.mp3', 1, 100.0004)];
const BASIS = {
  files: [
    { path: '01.mp3', size: 1000, duration_ms: 100_000 },
    { path: '02.mp3', size: 1000, duration_ms: 50_000 },
  ],
};
const SPANS = fileSpans(FILES, BOOK, BASIS);

const rows = (...r: [number, string, boolean?][]): DraftRow[] =>
  r.map(([start, title, fixed], i) => ({ key: `k${i}`, start, title, fixed: !!fixed }));

describe('the whole-book timeline', () => {
  it('lays the files end to end in play order, by the basis lengths', () => {
    expect(SPANS).toEqual([
      { path: `${BOOK}/01.mp3`, name: '01.mp3', start: 0, end: 100, playable: true },
      { path: `${BOOK}/02.mp3`, name: '02.mp3', start: 100, end: 150, playable: true },
    ]);
  });

  it('falls back to the file’s own length without a basis entry', () => {
    expect(fileSpans([file('01.mp3', 1, 12.3456)], BOOK, { files: [] })).toEqual([
      { path: `${BOOK}/01.mp3`, name: '01.mp3', start: 0, end: 12.346, playable: true },
    ]);
  });

  it('says which files a browser can’t play as stored', () => {
    const mka = { ...file('01.mka', 1, 10), format: 'mka', codec: 'opus', direct_playable: false };
    expect(fileSpans([mka], BOOK, { files: [] })[0].playable).toBe(false);
  });

  it('maps a whole-book time to a file and back', () => {
    expect(toFileTime(SPANS, 120.5)).toEqual({ span: SPANS[1], offset: 20.5 });
    expect(toFileTime(SPANS, 100)?.span).toBe(SPANS[1]);
    expect(toFileTime(SPANS, 99.999)?.span).toBe(SPANS[0]);
    expect(toBookTime(SPANS[1], 20.5)).toBe(120.5);
    // The book's very end is the last file's; before 0 or past the end is no file.
    expect(spanAt(SPANS, 150)).toBe(SPANS[1]);
    expect(spanAt(SPANS, 150.001)).toBeUndefined();
    expect(spanAt(SPANS, -1)).toBeUndefined();
  });
});

describe('draftFromChapters', () => {
  it('fixes each file’s first chapter at the file’s exact start', () => {
    const draft = draftFromChapters(
      [
        chapter(2, '02.mp3', 0, 50, 100.0004),
        chapter(0, '01.mp3', 0, 60, 0),
        chapter(1, '01.mp3', 60, 100, 60),
      ],
      SPANS,
    );
    expect(draft.map((r) => [r.start, r.title, r.fixed])).toEqual([
      [0, 'Chapter 1', true],
      [60, 'Chapter 2', false],
      [100, 'Chapter 3', true],
    ]);
  });

  it('places a chapter by its file and its start in it, not a book offset that drifted', () => {
    // A book of many files: the book offsets add up the files' lengths unrounded,
    // 80 ms past where the basis' whole milliseconds put 02.mp3.
    const draft = draftFromChapters(
      [
        chapter(0, '01.mp3', 0, 100, 0),
        chapter(1, '02.mp3', 0, 20, 100.08),
        chapter(2, '02.mp3', 20, 50, 120.08),
      ],
      SPANS,
    );
    expect(draft.map((r) => [r.start, r.title, r.fixed])).toEqual([
      [0, 'Chapter 1', true],
      [100, 'Chapter 2', true],
      [120, 'Chapter 3', false],
    ]);
  });

  it('fixes the chapter nearest a file’s start when two are close to it', () => {
    // A 30 ms tail ends 01.mp3: 02.mp3's own first chapter starts it, not the tail.
    const draft = draftFromChapters(
      [
        chapter(0, '01.mp3', 0, 99.97, 0),
        chapter(1, '01.mp3', 99.97, 100, 99.97),
        chapter(2, '02.mp3', 0, 50, 100),
      ],
      SPANS,
    );
    expect(draft.map((r) => [r.start, r.title, r.fixed])).toEqual([
      [0, 'Chapter 1', true],
      [99.97, 'Chapter 2', false],
      [100, 'Chapter 3', true],
    ]);
  });

  it('adds a fixed row for a file no chapter starts, named after the chapter running into it', () => {
    const draft = draftFromChapters([chapter(0, '01.mp3', 0, 100, 0)], SPANS);
    expect(draft.map((r) => [r.start, r.title, r.fixed])).toEqual([
      [0, 'Chapter 1', true],
      [100, 'Chapter 1', true],
    ]);
    // A book with no chapters at all: one row per file, named after it.
    expect(draftFromChapters([], SPANS).map((r) => r.title)).toEqual(['01', '02']);
  });

  it('knows when the draft changed', () => {
    const draft = draftFromChapters([chapter(0, '01.mp3', 0, 100, 0)], SPANS);
    expect(draftChanged(draft, draft)).toBe(false);
    expect(draftChanged([{ ...draft[0], title: 'x' }, draft[1]], draft)).toBe(true);
  });
});

describe('operations on the draft', () => {
  const draft = rows([0, 'One', true], [40, 'Two'], [100, 'Three', true], [130, 'Four']);

  it('adds at a time, a second or more from the others, in order', () => {
    const r = addAt(draft, SPANS, 70.1234, 'New');
    expect('rows' in r && r.rows.map((x) => x.start)).toEqual([0, 40, 70.123, 100, 130]);
    expect(addAt(draft, SPANS, 40.5, 'New')).toEqual({ refused: 'too_close' });
    expect(addAt(draft, SPANS, 150, 'New')).toEqual({ refused: 'out_of_range' });
  });

  it('adds halfway to the next chapter, or the book’s end', () => {
    const mid = addAfter(draft, SPANS, 'k1', 'New');
    expect('rows' in mid && mid.rows.find((x) => x.key === mid.key)?.start).toBe(70);
    const last = addAfter(draft, SPANS, 'k3', 'New');
    expect('rows' in last && last.rows.at(-1)?.start).toBe(140);
    expect(addAfter(rows([0, 'a', true], [1.5, 'b']), SPANS, 'k0', 'New')).toEqual({
      refused: 'no_room',
    });
  });

  it('never deletes or moves a file’s start', () => {
    expect(removeRow(draft, 'k2')).toEqual({ refused: 'fixed', key: 'k2' });
    expect(setStart(draft, SPANS, 'k0', 5)).toEqual({ refused: 'fixed', key: 'k0' });
    const gone = removeRow(draft, 'k1');
    expect('rows' in gone && gone.rows.map((x) => x.key)).toEqual(['k0', 'k2', 'k3']);
  });

  it('moves a row and keeps the list in time order', () => {
    const moved = setStart(draft, SPANS, 'k3', 20);
    expect('rows' in moved && moved.rows.map((x) => x.key)).toEqual(['k0', 'k3', 'k1', 'k2']);
    expect(setStart(draft, SPANS, 'k1', 151)).toEqual({ refused: 'out_of_range', key: 'k1' });
  });

  it('shifts every chapter but the files’ starts', () => {
    const r = shiftAll(draft, SPANS, 2.5);
    expect('rows' in r && r.rows.map((x) => x.start)).toEqual([0, 42.5, 100, 132.5]);
  });

  it('refuses a shift that takes a chapter to or past its file’s start or end', () => {
    // Two's file starts at 0: -40 lands on it.
    expect(shiftAll(draft, SPANS, -40)).toEqual({ refused: 'crosses_file', key: 'k1' });
    // Two would leave 01.mp3 (ends at 100).
    expect(shiftAll(draft, SPANS, 60)).toEqual({ refused: 'crosses_file', key: 'k1' });
    // Four would leave 02.mp3 (ends at 150).
    expect(shiftAll(draft, SPANS, 20)).toEqual({ refused: 'crosses_file', key: 'k3' });
  });
});

describe('validateDraft', () => {
  it('passes a list that keeps the server’s rules', () => {
    const v = validateDraft(rows([0, 'One', true], [40, 'Two'], [100, 'Three', true]), SPANS);
    expect(v.first).toBeUndefined();
    expect(v.rows.size).toBe(0);
  });

  it('marks chapters closer than a second, outside the book, or badly titled', () => {
    const v = validateDraft(
      rows([0, 'One', true], [0.5, 'Close'], [100, ' ', true], [149, 'a\u0007b'], [150, 'Out']),
      SPANS,
    );
    expect(Object.fromEntries(v.rows)).toEqual({
      k1: 'order',
      k2: 'title',
      k3: 'title',
      k4: 'out_of_range',
    });
    expect(v.first).toBe('out_of_range');
  });

  it('says what the whole list breaks', () => {
    expect(validateDraft([], SPANS).first).toBe('empty');
    const many = Array.from({ length: 2001 }, (_, i): DraftRow => ({
      key: `${i}`,
      start: i,
      title: 't',
      fixed: false,
    }));
    expect(validateDraft(many, SPANS).first).toBe('too_many');
    // The second file's start is missing.
    expect(validateDraft(rows([0, 'One', true], [40, 'Two']), SPANS).first).toBe('file_start');
  });

  it('lets two files’ starts be under a second apart, never a chapter of the admin’s own', () => {
    // A 400 ms file between two others (server#410).
    const spans = fileSpans(
      [file('01.mp3', 1, 100), file('02.mp3', 2, 0.4), file('03.mp3', 3, 50)],
      BOOK,
      { files: [] },
    );
    const fixed = rows([0, 'One', true], [100, 'Sting', true], [100.4, 'Two', true]);
    expect(validateDraft(fixed, spans).first).toBeUndefined();
    const own = rows([0, 'One', true], [100, 'Sting', true], [100.4, 'Two', true], [100.9, 'Mine']);
    expect(Object.fromEntries(validateDraft(own, spans).rows)).toEqual({ k3: 'order' });
  });

  it('gives files that start together one row, and refuses two at the same moment', () => {
    // The scan read no length for 02.mp3: it starts where 03.mp3 does.
    const spans = fileSpans(
      [file('01.mp3', 1, 100), file('02.mp3', 2, 0), file('03.mp3', 3, 50)],
      BOOK,
      { files: [] },
    );
    const draft = draftFromChapters([chapter(0, '01.mp3', 0, 100, 0)], spans);
    expect(draft.map((r) => r.start)).toEqual([0, 100]);
    expect(validateDraft(draft, spans).first).toBeUndefined();
    const pasted = parseTimestamps('0:00 One\n1:40 Two', timelineEnd(spans));
    expect(rowsFromPaste(pasted.chapters, spans, draft).map((r) => r.start)).toEqual([0, 100]);
    // The server refuses two chapters at one moment, files' starts or not.
    const twice = rows([0, 'One', true], [100, 'Two', true], [100, 'Three', true]);
    expect(Object.fromEntries(validateDraft(twice, spans).rows)).toEqual({ k2: 'order' });
  });

  it('gives a file shorter than the snap its own row, not the row of the file before it', () => {
    // 01.mp3 is one MP3 frame long: the chapter at 0 is its start, never 02.mp3's too.
    const spans = fileSpans([file('01.mp3', 1, 0.026), file('02.mp3', 2, 100)], BOOK, {
      files: [],
    });
    const draft = draftFromChapters(
      [chapter(0, '01.mp3', 0, 0.026, 0), chapter(1, '02.mp3', 5, 100, 5.026)],
      spans,
    );
    expect(draft.map((r) => [r.start, r.fixed])).toEqual([
      [0, true],
      [0.026, true],
      [5.026, false],
    ]);
    expect(validateDraft(draft, spans).first).toBeUndefined();
  });

  it('asks no chapter of a last file of no length', () => {
    // The scan read no length for 03.mp3: it starts at the book's end.
    const spans = fileSpans(
      [file('01.mp3', 1, 100), file('02.mp3', 2, 50), file('03.mp3', 3, 0)],
      BOOK,
      { files: [] },
    );
    const draft = draftFromChapters([chapter(0, '01.mp3', 0, 100, 0)], spans);
    expect(draft.map((r) => r.start)).toEqual([0, 100]);
    expect(validateDraft(draft, spans).first).toBeUndefined();
    const pasted = parseTimestamps('0:00 One\n1:40 Two', timelineEnd(spans));
    expect(rowsFromPaste(pasted.chapters, spans, draft).map((r) => r.start)).toEqual([0, 100]);
  });

  it('counts a title’s bytes, not its characters', () => {
    expect(titleOk('a'.repeat(200))).toBe(true);
    expect(titleOk('a'.repeat(201))).toBe(false);
    expect(titleOk('é'.repeat(100))).toBe(true);
    expect(titleOk('é'.repeat(101))).toBe(false);
    expect(titleOk('  ')).toBe(false);
  });
});

describe('editRequest', () => {
  it('sends the basis and the rows in order, trimmed, to the millisecond', () => {
    expect(editRequest(rows([40.00049, ' Two '], [0, 'One', true]), BASIS)).toEqual({
      basis: BASIS,
      chapters: [
        { start: 0, title: 'One' },
        { start: 40, title: 'Two' },
      ],
    });
  });
});

describe('saveProblem', () => {
  it('words each chapters_invalid detail, and an unknown one generically', () => {
    const invalid = (detail?: string) =>
      new ApiError(400, 'bad', 'chapters_invalid', undefined, { detail });
    expect(saveProblem(invalid('order'))).toBe('book.chapters.edit.invalid.order');
    expect(saveProblem(invalid('file_start'))).toBe('book.chapters.edit.invalid.file_start');
    expect(saveProblem(invalid('something_new'))).toBe('book.chapters.edit.invalid.other');
  });

  it('says the files changed on a 409, and leaves other failures to the generic wording', () => {
    expect(saveProblem(new ApiError(409, 'changed', 'chapters_basis'))).toBe(
      'book.chapters.edit.basis',
    );
    // Another 409 (a podcast library) keeps the server's own words.
    expect(saveProblem(new ApiError(409, 'not a book', 'not_book_library'))).toBeUndefined();
    expect(saveProblem(new ApiError(403, 'admin only'))).toBeUndefined();
    expect(saveProblem(new Error('offline'))).toBeUndefined();
  });
});

describe('times', () => {
  it('reads h:mm:ss, m:ss and seconds, with a fraction', () => {
    expect(parseTime('1:02:03')).toBe(3723);
    expect(parseTime('1:02:03.5')).toBe(3723.5);
    expect(parseTime('12:34')).toBe(754);
    expect(parseTime('75:00')).toBe(4500);
    expect(parseTime('12:34,250')).toBe(754.25);
    expect(parseTime(' 42.125 ')).toBe(42.125);
    // A longer fraction is kept to the millisecond.
    expect(parseTime('5:10.500000')).toBe(310.5);
    expect(parseTime('42.1256')).toBe(42.126);
    expect(parseTime('1:60')).toBeUndefined();
    expect(parseTime('1:61:00')).toBeUndefined();
    expect(parseTime('abc')).toBeUndefined();
    expect(parseTime('')).toBeUndefined();
  });

  it('writes a time with milliseconds only when it has them', () => {
    expect(formatStamp(754)).toBe('12:34');
    expect(formatStamp(754.25)).toBe('12:34.250');
    expect(formatStamp(3723.5)).toBe('1:02:03.500');
    expect(formatStamp(5, true)).toBe('0:00:05');
  });
});

describe('pasted timestamps', () => {
  it('reads a time first or last, in time order, and says which lines it left out', () => {
    const text = [
      '0:00 Opening credits',
      '[01:02.5] - Chapter 1',
      'Chapter 2 (1:10)',
      '1. 1:20 Chapter 3',
      'Chapter 4 | 1:30:00',
      '',
      'no time here',
      '2:00',
      'Epilogue 3:00',
      'Too long 1234:56',
      '00:01:25.500000 Chapter 5',
      '1:02:03:04 Frames',
      '1:234 Not a time',
      'Frames 1:02:03:04',
      'Part 1: 1:15',
    ].join('\n');
    expect(parseTimestamps(text, 170)).toEqual({
      chapters: [
        { line: 1, start: 0, title: 'Opening credits' },
        { line: 2, start: 62.5, title: 'Chapter 1' },
        { line: 3, start: 70, title: 'Chapter 2' },
        { line: 15, start: 75, title: 'Part 1' },
        { line: 4, start: 80, title: 'Chapter 3' },
        { line: 11, start: 85.5, title: 'Chapter 5' },
      ],
      unreadable: [7, 8, 10, 12, 13, 14],
      outside: [5, 9],
    });
  });

  it('keeps each file’s start fixed, named by a chapter there or the one running into it', () => {
    const current = rows([0, 'Old one', true], [100, 'Old three', true]);
    const pasted = parseTimestamps('0:00.4 Intro\n0:40 Middle\n1:30 Long one\n2:05 End', 150);
    const out = rowsFromPaste(pasted.chapters, SPANS, current);
    expect(out.map((r) => [r.start, r.title, r.fixed])).toEqual([
      [0, 'Intro', true],
      [40, 'Middle', false],
      [90, 'Long one', false],
      // No pasted chapter starts 02.mp3: "Long one" runs into it.
      [100, 'Long one', true],
      [125, 'End', false],
    ]);
    // The fixed rows keep their keys.
    expect(out[0].key).toBe('k0');
    expect(out[3].key).toBe('k1');
  });

  it('names a file’s start after the nearest pasted chapter within a second of it', () => {
    const pasted = parseTimestamps('0:00 Intro\n1:39.2 Tail\n1:40 Part two', 150);
    const out = rowsFromPaste(pasted.chapters, SPANS, rows([0, 'Old one', true]));
    expect(out.map((r) => [r.start, r.title, r.fixed])).toEqual([
      [0, 'Intro', true],
      [99.2, 'Tail', false],
      [100, 'Part two', true],
    ]);
  });
});
