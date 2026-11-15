import { ApiError } from '@/api/client';
import type {
  AdminBookDetail,
  AdminChapter,
  AdminFile,
  ChapterBasis,
  ChapterEditRequest,
} from '@/api/types';
import { CONTROL } from './field-rules';
import { fileName, sortFiles } from './book-model';

// The chapter editor's pure logic (server#240): the book's files laid end to end
// as one timeline, the draft rows, the operations on them, the server's rules
// (catalog's chapter edit validation) and the timestamps paste. Every time here is
// whole-book seconds at millisecond precision, as PUT .../book/chapters takes it.

/** The server's rules on an edited list (the 400 chapters_invalid details). */
export const MAX_CHAPTERS = 2000;
/** Consecutive chapters start at least this far apart (seconds). */
export const MIN_GAP = 1;
/** A title is 1 to this many bytes of UTF-8. */
export const MAX_TITLE_BYTES = 200;

/** The 400 chapters_invalid details the server sends (anything else is worded generically). */
export const INVALID_DETAILS = [
  'empty',
  'too_many',
  'order',
  'out_of_range',
  'title',
  'file_start',
] as const;
export type InvalidDetail = (typeof INVALID_DETAILS)[number];

/** One audio file of the book on the whole-book timeline. */
export interface FileSpan {
  /** Library-relative, as the stream route takes it. */
  path: string;
  /** Its name under the book's folder. */
  name: string;
  /** Whole-book seconds where it starts and ends. */
  start: number;
  end: number;
}

/** One row of the editor. A file-start row is fixed: its title is editable, its time is not. */
export interface DraftRow {
  /** Stable across edits (React's key). */
  key: string;
  start: number;
  title: string;
  fixed: boolean;
}

/** Seconds rounded to the millisecond (the wire's precision). */
export const roundMs = (s: number) => Math.round(s * 1000) / 1000;

/**
 * The book's files end to end, in play order. A file's length is its basis
 * entry's (duration_ms, what the server checks a PUT against), matched by its
 * book-relative path, else the file's own; a file's start is the sum of those
 * before it, so a file-start row sends exactly the time the server expects.
 */
export function fileSpans(
  files: AdminFile[],
  bookPath: string,
  basis: ChapterBasis | undefined,
): FileSpan[] {
  const ms = new Map((basis?.files ?? []).map((f) => [f.path, f.duration_ms]));
  const lengthMs = (f: AdminFile) =>
    ms.get(fileName(f.path, bookPath)) ?? ms.get(f.path) ?? Math.round(f.duration * 1000);
  let at = 0;
  return sortFiles(files).map((f) => {
    const start = at;
    at += lengthMs(f);
    return { path: f.path, name: fileName(f.path, bookPath), start: start / 1000, end: at / 1000 };
  });
}

/** The book's length on the timeline (the last file's end). */
export const timelineEnd = (spans: FileSpan[]) => spans.at(-1)?.end ?? 0;

/** The file a whole-book time falls in (the last file holds its own end), or undefined outside. */
export function spanAt(spans: FileSpan[], t: number): FileSpan | undefined {
  if (t < 0) return undefined;
  const last = spans.at(-1);
  return (
    spans.find((s) => t >= s.start && t < s.end) ?? (last && t === last.end ? last : undefined)
  );
}

/** A whole-book time as a file and the seconds into it (what the scrubber seeks to). */
export function toFileTime(
  spans: FileSpan[],
  t: number,
): { span: FileSpan; offset: number } | undefined {
  const span = spanAt(spans, t);
  return span ? { span, offset: roundMs(t - span.start) } : undefined;
}

/** A position in a file as whole-book seconds. */
export const toBookTime = (span: FileSpan, offset: number) => roundMs(span.start + offset);

let keySeq = 0;
const newKey = () => `n${++keySeq}`;

/** How close (seconds) a chapter's offset must be to a file's start to be that file's first chapter. */
const FILE_START_SLACK = 0.05;

const byStart = (rows: DraftRow[]) => [...rows].sort((a, b) => a.start - b.start);

/**
 * The editor's first rows: the book's chapters as they play now, each file's
 * start a fixed row (the chapter that starts it, snapped to the exact file start;
 * one titled after the chapter running into it, or the file, when none does).
 */
export function draftFromChapters(chapters: AdminChapter[], spans: FileSpan[]): DraftRow[] {
  const rows: DraftRow[] = [...chapters]
    .sort((a, b) => a.book_offset - b.book_offset)
    .map((c) => ({
      key: `c${c.index}`,
      start: roundMs(c.book_offset),
      title: c.title,
      fixed: false,
    }));
  const own = [...rows];
  for (const span of spans) {
    const at = own.find((r) => Math.abs(r.start - span.start) < FILE_START_SLACK);
    if (at) {
      at.start = roundMs(span.start);
      at.fixed = true;
      continue;
    }
    const before = own.filter((r) => r.start < span.start).at(-1);
    rows.push({
      key: newKey(),
      start: roundMs(span.start),
      title: before?.title ?? span.name.replace(/\.[^./]+$/, ''),
      fixed: true,
    });
  }
  return byStart(rows);
}

/** Whether the draft differs from where it started. */
export function draftChanged(rows: DraftRow[], initial: DraftRow[]): boolean {
  return (
    rows.length !== initial.length ||
    rows.some((r, i) => r.start !== initial[i].start || r.title !== initial[i].title)
  );
}

/** Why an operation on the draft was refused (book.chapters.edit.refused.*). */
export type Refusal = 'too_close' | 'no_room' | 'crosses_file' | 'out_of_range' | 'fixed';

export type Edit = { rows: DraftRow[]; key?: string } | { refused: Refusal; key?: string };

const tooClose = (rows: DraftRow[], t: number, except?: string) =>
  rows.some((r) => r.key !== except && Math.abs(r.start - t) < MIN_GAP);

/** A new chapter at whole-book time t, a second or more from every other. */
export function addAt(rows: DraftRow[], spans: FileSpan[], t: number, title: string): Edit {
  const at = roundMs(t);
  if (at < 0 || at >= timelineEnd(spans)) return { refused: 'out_of_range' };
  if (tooClose(rows, at)) return { refused: 'too_close' };
  const key = newKey();
  return { rows: byStart([...rows, { key, start: at, title, fixed: false }]), key };
}

/** A new chapter halfway between a row and the next (or the book's end). */
export function addAfter(rows: DraftRow[], spans: FileSpan[], key: string, title: string): Edit {
  const sorted = byStart(rows);
  const i = sorted.findIndex((r) => r.key === key);
  if (i < 0) return { refused: 'out_of_range' };
  const from = sorted[i].start;
  const to = sorted[i + 1]?.start ?? timelineEnd(spans);
  if (to - from < 2 * MIN_GAP) return { refused: 'no_room' };
  return addAt(rows, spans, from + (to - from) / 2, title);
}

/** Deletes a row; a file's start stays. */
export function removeRow(rows: DraftRow[], key: string): Edit {
  const row = rows.find((r) => r.key === key);
  if (!row) return { rows };
  if (row.fixed) return { refused: 'fixed', key };
  return { rows: rows.filter((r) => r.key !== key) };
}

/** Moves a row to whole-book time t (a file's start never moves); the list stays in time order. */
export function setStart(rows: DraftRow[], spans: FileSpan[], key: string, t: number): Edit {
  const row = rows.find((r) => r.key === key);
  if (!row) return { rows };
  if (row.fixed) return { refused: 'fixed', key };
  const at = roundMs(t);
  if (at < 0 || at >= timelineEnd(spans)) return { refused: 'out_of_range', key };
  return { rows: byStart(rows.map((r) => (r.key === key ? { ...r, start: at } : r))), key };
}

export function setTitle(rows: DraftRow[], key: string, title: string): DraftRow[] {
  return rows.map((r) => (r.key === key ? { ...r, title } : r));
}

/**
 * Moves every chapter but the files' starts by delta seconds. Refused (naming the
 * first row that would) when a chapter would reach its file's start or leave its
 * file: no chapter crosses a file.
 */
export function shiftAll(rows: DraftRow[], spans: FileSpan[], delta: number): Edit {
  const d = roundMs(delta);
  const out: DraftRow[] = [];
  for (const r of byStart(rows)) {
    if (r.fixed || d === 0) {
      out.push(r);
      continue;
    }
    const span = spanAt(spans, r.start);
    const at = roundMs(r.start + d);
    if (!span || at <= span.start || at >= span.end) return { refused: 'crosses_file', key: r.key };
    out.push({ ...r, start: at });
  }
  return { rows: byStart(out) };
}

/** What a row breaks of the server's rules, and the list's first broken rule. */
export interface Validation {
  /** By row key: the rule it breaks. */
  rows: Map<string, InvalidDetail>;
  /** The first rule broken, list-wide ones first (undefined: the list can be saved). */
  first?: InvalidDetail;
}

const utf8 = new TextEncoder();

/** Whether a title passes the server's rule (1-200 bytes, no control characters), trimmed. */
export function titleOk(title: string): boolean {
  const v = title.trim();
  const bytes = utf8.encode(v).length;
  return bytes > 0 && bytes <= MAX_TITLE_BYTES && !CONTROL.test(v);
}

/** The server's rules (catalog's chapter edit validation), checked before a save. */
export function validateDraft(rows: DraftRow[], spans: FileSpan[]): Validation {
  const out = new Map<string, InvalidDetail>();
  if (!rows.length) return { rows: out, first: 'empty' };
  if (rows.length > MAX_CHAPTERS) return { rows: out, first: 'too_many' };
  const end = timelineEnd(spans);
  const sorted = byStart(rows);
  sorted.forEach((r, i) => {
    if (r.start < 0 || r.start >= end) out.set(r.key, 'out_of_range');
    else if (i > 0 && roundMs(r.start - sorted[i - 1].start) < MIN_GAP) out.set(r.key, 'order');
    else if (!titleOk(r.title)) out.set(r.key, 'title');
  });
  const starts = new Set(sorted.map((r) => r.start));
  const fileStart = spans.some((s) => !starts.has(roundMs(s.start)));
  const order: InvalidDetail[] = ['out_of_range', 'order', 'title'];
  const first = fileStart ? 'file_start' : order.find((d) => [...out.values()].includes(d));
  return { rows: out, first };
}

/** The PUT body: the basis the editor opened on, and the rows in time order. */
export function editRequest(rows: DraftRow[], basis: ChapterBasis): ChapterEditRequest {
  return {
    basis,
    chapters: byStart(rows).map((r) => ({ start: roundMs(r.start), title: r.title.trim() })),
  };
}

/** What a refused save means, as an i18n key (undefined: not one of the editor's own answers). */
export function saveProblem(err: unknown): string | undefined {
  if (!(err instanceof ApiError)) return undefined;
  // The code rides in "code" (respond.go writeErrorCode); a bare {"error": code} reads the same.
  const code = err.code ?? err.message;
  if (err.status === 409 && (code === 'chapters_basis' || !err.code)) {
    return 'book.chapters.edit.basis';
  }
  if (err.status === 400 && code === 'chapters_invalid') {
    const d = INVALID_DETAILS.find((x) => x === err.detail);
    return d ? `book.chapters.edit.invalid.${d}` : 'book.chapters.edit.invalid.other';
  }
  return undefined;
}

// ---- times as typed ----

const TIME = String.raw`\d{1,3}(?::\d{1,2}){1,2}(?:[.,]\d{1,3})?`;

/**
 * A time as typed: h:mm:ss, m:ss (minutes past 59 allowed), each with an optional
 * fraction (.5, ,250), or plain seconds. Undefined when it isn't one.
 */
export function parseTime(raw: string): number | undefined {
  const v = raw.trim().replace(',', '.');
  if (/^\d+(\.\d{1,3})?$/.test(v)) return roundMs(Number(v));
  const m = /^(\d{1,3})(?::(\d{1,2}))?:(\d{1,2})(\.\d{1,3})?$/.exec(v);
  if (!m) return undefined;
  const [, a, b, c, frac] = m;
  const sec = Number(c);
  if (sec > 59) return undefined;
  const total =
    b === undefined
      ? Number(a) * 60 + sec
      : Number(b) > 59
        ? undefined
        : Number(a) * 3600 + Number(b) * 60 + sec;
  return total === undefined ? undefined : roundMs(total + (frac ? Number(frac) : 0));
}

/** A whole-book time as the editor shows it: "1:02:03", with ".450" when not on a second. */
export function formatStamp(seconds: number, withHours = seconds >= 3600): string {
  const ms = Math.max(0, Math.round(seconds * 1000));
  const whole = Math.floor(ms / 1000);
  const h = Math.floor(whole / 3600);
  const m = Math.floor((whole % 3600) / 60);
  const s = String(whole % 60).padStart(2, '0');
  const frac = ms % 1000 ? `.${String(ms % 1000).padStart(3, '0')}` : '';
  return withHours || h > 0 ? `${h}:${String(m).padStart(2, '0')}:${s}${frac}` : `${m}:${s}${frac}`;
}

// ---- pasted timestamps ----

/** One read line of a pasted list. */
export interface PastedChapter {
  /** 1-based line number in the paste. */
  line: number;
  start: number;
  title: string;
}

export interface PastedList {
  chapters: PastedChapter[];
  /** Lines with no time or no title. */
  unreadable: number[];
  /** Lines whose time is past the book's end. */
  outside: number[];
}

// "0:00 Title", "[01:02:03.5] - Title", "1. 0:00 Title" are time first; "Title 1:02:03",
// "Title (12:34)", "Title - 12:34" time last. Separators between are dropped: a
// hyphen, an en or em dash, a colon, a pipe.
const SEP = String.raw`[\s\-–—:|]*`;
const TIME_FIRST = new RegExp(String.raw`^(?:\d+[.)]\s+)?[[(]?(${TIME})[\])]?${SEP}(.*)$`);
const TIME_LAST = new RegExp(String.raw`^(.*?)${SEP}[[(]?(${TIME})[\])]?$`);

/** Reads a pasted chapter list (whole-book times), in time order. */
export function parseTimestamps(text: string, end: number): PastedList {
  const out: PastedList = { chapters: [], unreadable: [], outside: [] };
  text.split(/\r?\n/).forEach((raw, i) => {
    const line = raw.trim();
    if (!line) return;
    const first = TIME_FIRST.exec(line);
    const last = first ? undefined : TIME_LAST.exec(line);
    const [time, title] = first ? [first[1], first[2]] : last ? [last[2], last[1]] : [];
    const start = time === undefined ? undefined : parseTime(time);
    const name = title?.trim() ?? '';
    if (start === undefined || !name) {
      out.unreadable.push(i + 1);
      return;
    }
    if (start >= end) {
      out.outside.push(i + 1);
      return;
    }
    out.chapters.push({ line: i + 1, start, title: name });
  });
  out.chapters.sort((a, b) => a.start - b.start);
  return out;
}

/**
 * The rows a pasted list makes: its chapters, and each file's start fixed. A
 * pasted chapter within a second of a file's start names that file's row (the
 * file starts it); a file no chapter starts is titled after the pasted chapter
 * running into it, else keeps its row's title from `current`.
 */
export function rowsFromPaste(
  pasted: PastedChapter[],
  spans: FileSpan[],
  current: DraftRow[],
): DraftRow[] {
  const used = new Set<PastedChapter>();
  const rows: DraftRow[] = spans.map((span) => {
    const at = pasted.find((p) => !used.has(p) && Math.abs(p.start - span.start) < MIN_GAP);
    if (at) used.add(at);
    const before = pasted.filter((p) => p.start < span.start).at(-1);
    const own = current.find((r) => r.fixed && r.start === roundMs(span.start));
    return {
      key: own?.key ?? newKey(),
      start: roundMs(span.start),
      title: at?.title ?? before?.title ?? own?.title ?? span.name,
      fixed: true,
    };
  });
  for (const p of pasted) {
    if (!used.has(p)) rows.push({ key: newKey(), start: p.start, title: p.title, fixed: false });
  }
  return byStart(rows);
}

/** The spans of the book a page shows, from its detail. */
export const detailSpans = (d: AdminBookDetail) =>
  fileSpans(d.files ?? [], d.book.path, d.chapter_basis);
