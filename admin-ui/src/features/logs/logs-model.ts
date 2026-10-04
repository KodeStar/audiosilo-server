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

/**
 * Adds a page to the tail. A first page replaces it; a later one appends what is
 * new (a line already held is never shown twice), keeping the newest MAX_LINES.
 */
export function appendPage(tail: Tail, page: LogPage, first: boolean): Tail {
  const known = first ? 0 : (tail.lines.at(-1)?.seq ?? 0);
  const fresh = page.entries.filter((e) => e.seq > known);
  const lines = first ? page.entries : [...tail.lines, ...fresh];
  return {
    lines: lines.length > MAX_LINES ? lines.slice(lines.length - MAX_LINES) : lines,
    lastSeq: first ? page.last_seq : Math.max(tail.lastSeq, page.last_seq),
    gap: first ? page.truncated : tail.gap || page.truncated,
  };
}

/** A line's time as HH:MM:SS in the browser's zone. */
export function lineTime(iso: string, lang: string): string {
  return new Date(iso).toLocaleTimeString(lang, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  });
}
