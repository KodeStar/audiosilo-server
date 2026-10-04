import type { LogEntry, LogPage } from '@/api/types';

// Server > Logs: the live tail's bookkeeping, kept out of the component so it
// is tested on its own.

/** How many lines the page holds at once (older ones scroll out). */
export const MAX_LINES = 1000;

/** The level filter: everything the server keeps (info and up), warnings and up, errors. */
export const LOG_LEVELS = ['all', 'warn', 'error'] as const;
export type LogLevelFilter = (typeof LOG_LEVELS)[number];

export interface Tail {
  lines: LogEntry[];
  /** The newest seq the server had at the last poll: the next poll's `after`. */
  lastSeq: number;
  /** Lines were left out: before the first page, or dropped between polls. */
  gap: boolean;
}

export const EMPTY_TAIL: Tail = { lines: [], lastSeq: 0, gap: false };

const newest = (lines: LogEntry[]) =>
  lines.length > MAX_LINES ? lines.slice(lines.length - MAX_LINES) : lines;

/**
 * Adds a page to the tail. A first page replaces it; a later one appends what is
 * new (a line already held is never shown twice), keeping the newest MAX_LINES.
 * A poll that brought nothing new returns the same tail, so nothing re-renders.
 */
export function appendPage(tail: Tail, page: LogPage, first: boolean): Tail {
  // The server restarted (its seqs start over): what it sends is a fresh first page.
  if (first || page.last_seq < tail.lastSeq) {
    return { lines: newest(page.entries), lastSeq: page.last_seq, gap: page.truncated };
  }
  const known = tail.lines.at(-1)?.seq ?? 0;
  const fresh = page.entries.filter((e) => e.seq > known);
  const lastSeq = Math.max(tail.lastSeq, page.last_seq);
  if (fresh.length === 0 && lastSeq === tail.lastSeq && !page.truncated) return tail;
  return {
    lines: newest([...tail.lines, ...fresh]),
    lastSeq,
    gap: tail.gap || page.truncated,
  };
}
