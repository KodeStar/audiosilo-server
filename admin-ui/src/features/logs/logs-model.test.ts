import type { LogEntry, LogPage } from '@/api/types';
import { appendPage, EMPTY_TAIL, MAX_LINES } from './logs-model';

const line = (seq: number): LogEntry => ({
  seq,
  time: '2026-10-04T12:00:00Z',
  level: 'info',
  message: `line ${seq}`,
  attrs: [],
});
const page = (seqs: number[], last: number, truncated = false): LogPage => ({
  entries: seqs.map(line),
  last_seq: last,
  truncated,
});

describe('appendPage', () => {
  it('starts from the first page and appends only newer lines', () => {
    let tail = appendPage(EMPTY_TAIL, page([1, 2, 3], 3), true);
    expect(tail.lastSeq).toBe(3);
    // A poll that overlaps (a retry) never shows a line twice.
    tail = appendPage(tail, page([3, 4], 4), false);
    expect(tail.lines.map((l) => l.seq)).toEqual([1, 2, 3, 4]);
    // A filtered poll with no match still moves the cursor.
    tail = appendPage(tail, page([], 9), false);
    expect(tail).toMatchObject({ lastSeq: 9, gap: false });
  });

  it('keeps the newest lines and remembers a gap', () => {
    const many = Array.from({ length: MAX_LINES + 5 }, (_, i) => i + 1);
    const tail = appendPage(EMPTY_TAIL, page(many, many.length, true), true);
    expect(tail.lines).toHaveLength(MAX_LINES);
    expect(tail.lines[0].seq).toBe(6);
    expect(tail.gap).toBe(true);
    expect(appendPage(tail, page([], many.length), false).gap).toBe(true);
  });
});
