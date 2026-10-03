import {
  OVERRIDE_FIELDS,
  type AdminChapter,
  type AdminFile,
  type BookEditRequest,
  type FieldSource,
  type FieldValue,
  type FolderMode,
  type OverrideField,
} from '@/api/types';

// The book page's logic, kept out of the components so it is unit-tested: field
// validation (the server's normalizeOverride rules, so a value is refused here
// with a message under the field before a save is tried), the unsaved edits and
// their diff, time and size formatting, and the chapter ribbon's segment math.

/** Unsaved edits: what the admin typed per field, before normalizing. */
export type Drafts = Partial<Record<OverrideField, string>>;

/** A field's value as the server would store it, or the i18n key of why it can't. */
export interface FieldCheck {
  value: string;
  error?: string;
}

const MAX_SHORT = 500;
const MAX_DESCRIPTION = 20000;
const MAX_SERIES_INDEX = 100000;
// unicode.IsControl: C0, DEL and C1.
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;
// A description keeps line breaks and tabs.
// eslint-disable-next-line no-control-regex
const CONTROL_IN_TEXT = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/;
const NUMBER = /^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/;
const PUBLISHED = /^\d{4}(-\d{2}(-\d{2})?)?$/;
const ASIN = /^[A-Z0-9]{10}$/;
const ISBN = /^(\d{9}[\dX]|\d{13})$/;

const runes = (s: string) => [...s].length;

/** Whether a YYYY[-MM[-DD]] string names a real date. */
function realDate(v: string): boolean {
  const [y, m = 1, d = 1] = v.split('-').map(Number);
  if (m < 1 || m > 12) return false;
  const date = new Date(0);
  date.setUTCFullYear(y, m - 1, d);
  return date.getUTCMonth() === m - 1 && date.getUTCDate() === d;
}

/** Mirrors catalog.normalizeOverride: trims, normalizes, and refuses what the server would. */
export function checkField(field: OverrideField, raw: string): FieldCheck {
  let v = raw.trim();
  switch (field) {
    case 'title':
    case 'author':
    case 'narrator':
    case 'series':
      if (runes(v) > MAX_SHORT) return { value: v, error: 'book.invalid.tooLong' };
      if (CONTROL.test(v)) return { value: v, error: 'book.invalid.control' };
      if (field === 'title' && !v) return { value: v, error: 'book.invalid.title' };
      return { value: v };
    case 'description':
      v = v.replaceAll('\r', '');
      if (runes(v) > MAX_DESCRIPTION) return { value: v, error: 'book.invalid.tooLong' };
      if (CONTROL_IN_TEXT.test(v)) return { value: v, error: 'book.invalid.control' };
      return { value: v };
    case 'series_index': {
      if (!v) return { value: '' };
      const n = Number(v);
      if (!NUMBER.test(v) || n < 0 || n > MAX_SERIES_INDEX) {
        return { value: v, error: 'book.invalid.seriesIndex' };
      }
      // formatSeriesPosition: 0 is no position.
      return { value: n === 0 ? '' : String(n) };
    }
    case 'published':
      if (!v) return { value: '' };
      if (!PUBLISHED.test(v) || !realDate(v)) return { value: v, error: 'book.invalid.published' };
      return { value: v };
    case 'asin':
      v = v.toUpperCase();
      if (v && !ASIN.test(v)) return { value: v, error: 'book.invalid.asin' };
      return { value: v };
    case 'isbn':
      v = v.toUpperCase().replace(/[- ]/g, '');
      if (v && !ISBN.test(v)) return { value: v, error: 'book.invalid.isbn' };
      return { value: v };
  }
}

/**
 * The drafts after a field is committed: typing the saved value back (as the
 * server would store it) drops the draft, so the save bar counts real changes.
 */
export function commitDraft(
  drafts: Drafts,
  field: OverrideField,
  raw: string,
  current: string,
): Drafts {
  const next = { ...drafts };
  const check = checkField(field, raw);
  if (raw === current || (!check.error && check.value === current)) delete next[field];
  else next[field] = raw;
  return next;
}

/** The fields whose draft the server would refuse, with the message key for each. */
export function draftErrors(drafts: Drafts): Partial<Record<OverrideField, string>> {
  const out: Partial<Record<OverrideField, string>> = {};
  for (const f of OVERRIDE_FIELDS) {
    const raw = drafts[f];
    if (raw === undefined) continue;
    const { error } = checkField(f, raw);
    if (error) out[f] = error;
  }
  return out;
}

/** One line of the save dialog's diff. */
export interface DiffRow {
  field: OverrideField;
  before: string;
  after: string;
  source: FieldSource;
}

/** The unsaved edits as a diff against the saved values, in display order. */
export function diffRows(drafts: Drafts, fields: Record<OverrideField, FieldValue>): DiffRow[] {
  return OVERRIDE_FIELDS.filter((f) => drafts[f] !== undefined).map((f) => ({
    field: f,
    before: fields[f].value,
    after: checkField(f, drafts[f]!).value,
    source: fields[f].source,
  }));
}

/** The PATCH that saves the drafts (an emptied optional field sets ""). */
export function saveRequest(drafts: Drafts): BookEditRequest {
  const set: Partial<Record<OverrideField, string>> = {};
  for (const f of OVERRIDE_FIELDS) {
    const raw = drafts[f];
    if (raw !== undefined) set[f] = checkField(f, raw).value;
  }
  return { set };
}

/** The PATCH that undoes a revert: the value it had, from the source it had. */
export function undoRevertRequest(field: OverrideField, before: FieldValue): BookEditRequest {
  return {
    set: { [field]: before.value },
    source: before.source === 'community' ? 'community' : 'edited',
  };
}

// ---- time and size ----

/** "1:02:05" (or "2:05" under an hour unless `withHours`). */
export function formatClock(seconds: number, withHours = seconds >= 3600): string {
  const s = Math.max(0, Math.round(seconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = String(s % 60).padStart(2, '0');
  return withHours || h > 0 ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`;
}

/** A translate function (i18next's `t`, or a stub in tests). */
type Translate = (key: string, opts: Record<string, unknown>) => string;

/** "45h 30m", "2h", "38m", "40s". */
export function formatLength(seconds: number, t: Translate): string {
  const s = Math.max(0, Math.round(seconds));
  if (s < 60) return t('book.len.s', { s });
  const total = Math.round(s / 60);
  const h = Math.floor(total / 60);
  const m = total % 60;
  if (h && m) return t('book.len.hm', { h, m });
  return h ? t('book.len.h', { h }) : t('book.len.m', { m });
}

const SIZE_UNITS = ['byte', 'kilobyte', 'megabyte', 'gigabyte', 'terabyte'] as const;

/** "1.3 GB" (decimal units, one decimal under 10). */
export function formatSize(bytes: number, lang: string): string {
  let v = Math.max(0, bytes);
  let i = 0;
  while (v >= 1000 && i < SIZE_UNITS.length - 1) {
    v /= 1000;
    i++;
  }
  return new Intl.NumberFormat(lang, {
    style: 'unit',
    unit: SIZE_UNITS[i],
    unitDisplay: 'short',
    maximumFractionDigits: v < 10 && i > 0 ? 1 : 0,
  }).format(v);
}

// ---- files and chapters ----

/** A file's name as shown under its book: the part after the book's folder. */
export function fileName(filePath: string, bookPath: string): string {
  if (filePath.startsWith(`${bookPath}/`)) return filePath.slice(bookPath.length + 1);
  return filePath.slice(filePath.lastIndexOf('/') + 1);
}

/** Files in play order. */
export const sortFiles = (files: AdminFile[]) => [...files].sort((a, b) => a.seq - b.seq);

/** The chart colour of the n-th file (five, then they repeat). */
export const fileColor = (n: number) => `var(--chart-${(n % 5) + 1})`;

export interface RibbonSegment {
  index: number;
  /** flex-grow: the chapter's length. */
  flex: number;
  /** Position of the chapter's file in play order. */
  file: number;
  /** --ch: the file's colour, alternate chapters lighter. */
  color: string;
}

/** One ribbon segment per chapter, sized by length and coloured by file. */
export function ribbonSegments(chapters: AdminChapter[], files: AdminFile[]): RibbonSegment[] {
  const order = new Map(sortFiles(files).map((f, i) => [f.path, i]));
  return chapters.map((c, i) => {
    const file = order.get(c.file_path) ?? 0;
    return {
      index: c.index,
      flex: Math.max(c.end - c.start, 0.001),
      file,
      color: `color-mix(in oklab, ${fileColor(file)} ${i % 2 ? 62 : 86}%, var(--card))`,
    };
  });
}

/** The strip under the ribbon for a multi-file book: each file by its length. */
export function fileStrip(files: AdminFile[], bookPath: string) {
  return sortFiles(files).map((f, i) => ({
    path: f.path,
    name: fileName(f.path, bookPath),
    flex: Math.max(f.duration, 0.001),
    color: fileColor(i),
  }));
}

/** Chapters worth a plain warning: none, or one that runs over two hours. */
export function chapterProblem(
  chapters: AdminChapter[],
  duration: number,
): 'none' | 'single' | undefined {
  if (chapters.length === 0) return 'none';
  if (chapters.length === 1 && duration > 2 * 3600) return 'single';
  return undefined;
}

// ---- the rest of the page ----

/** What the hero's eyebrow says: the series and position, if any. */
export function seriesLine(series: string, index: number): { series: string; n?: string } | null {
  if (!series) return null;
  return index > 0 ? { series, n: String(index) } : { series };
}

/** The book's full path on the server, for Copy path. */
export function fullPath(root: string | undefined, path: string): string {
  return root ? `${root.replace(/\/+$/, '')}/${path}` : path;
}

/** The i18n key for a folder's detection. */
export function detectionKey(override: FolderMode | ''): string {
  return override === 'book'
    ? 'book.disk.pinnedBook'
    : override === 'collection'
      ? 'book.disk.pinnedSplit'
      : 'book.disk.automatic';
}

export const COVER_TYPES = ['image/jpeg', 'image/png', 'image/webp'];
/** catalog.SetCover's ceiling. */
export const MAX_COVER_BYTES = 5 * 1024 * 1024;

/** Why an image can't be a cover (an i18n key), or undefined when it can. */
export function coverFileProblem(file: { size: number; type: string }): string | undefined {
  if (!COVER_TYPES.includes(file.type)) return 'book.cover.unsupported';
  if (file.size > MAX_COVER_BYTES) return 'book.cover.tooLarge';
  return undefined;
}
