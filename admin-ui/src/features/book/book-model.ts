import {
  FITTED_STATUSES,
  OVERRIDE_FIELDS,
  type AdminBookDetail,
  type AdminChapter,
  type AdminFile,
  type BookEditRequest,
  type CommunityChaptersDetail,
  type FieldSource,
  type FieldValue,
  type FolderMode,
  type OverrideField,
} from '@/api/types';
import type { Phrase } from '@/lib/phrase';
import { CONTROL, MAX_SERIES_INDEX, MAX_SHORT, NUMBER } from './field-rules';
import { moreSeriesText, parseMoreSeries } from './more-series';

// The book page's logic, kept out of the components so it is unit-tested: field
// validation (the server's normalizeOverride rules, so a value is refused here
// with a message under the field before a save is tried), the unsaved edits and
// their diff, and the chapter ribbon's segment math.

/** Unsaved edits: what the admin typed per field, before normalizing. */
export type Drafts = Partial<Record<OverrideField, string>>;

/** A field's value as the server would store it, or the i18n key of why it can't. */
export interface FieldCheck {
  value: string;
  error?: string;
}

const MAX_DESCRIPTION = 20000;
// A description keeps line breaks and tabs.
// eslint-disable-next-line no-control-regex
const CONTROL_IN_TEXT = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/;
const PUBLISHED = /^\d{4}(-\d{2}(-\d{2})?)?$/;
const ASIN = /^[A-Z0-9]{10}$/;
/** An ISBN-10 or ISBN-13, upper case, without hyphens or spaces. */
export const ISBN = /^(\d{9}[\dX]|\d{13})$/;

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
    case 'more_series':
      return parseMoreSeries(v);
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

/** A stored value as it is shown and edited: more_series as its line, the rest as they are. */
export function displayValue(field: OverrideField, value: string): string {
  return field === 'more_series' ? moreSeriesText(value) : value;
}

/**
 * The drafts after a field is committed: leaving the value as it was shown, or
 * typing the saved value back (as the server would store it), drops the draft,
 * so the save bar counts real changes.
 */
export function commitDraft(
  drafts: Drafts,
  field: OverrideField,
  raw: string,
  current: string,
): Drafts {
  const next = { ...drafts };
  const check = checkField(field, raw);
  const unchanged =
    raw === current ||
    raw === displayValue(field, current) ||
    (!check.error && check.value === current);
  if (unchanged) delete next[field];
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

// ---- community chapters ----

/** What the Chapters card says about the community's chapters, and the one thing it offers. */
export interface CommunityState {
  /** The status line. */
  line: Phrase;
  /** The edit its button makes (a chapter_source), if any, and the button's label key. */
  action?: { source: NonNullable<BookEditRequest['chapter_source']>; label: string };
  /** The chapters the community titles differently (a titles status). */
  titles: NonNullable<CommunityChaptersDetail['title_diffs']>;
  /** Notes under the line: chapters this copy lacks, starts that may be off, a merge hint. */
  notes: Phrase[];
}

/**
 * The community chapters of a book page, in words (null when there is nothing to
 * say: a book with no ASIN or ISBN is never checked). Durations are formatted by
 * the caller's `duration`.
 */
export function communityState(
  d: AdminBookDetail,
  duration: (seconds: number) => string,
): CommunityState | null {
  const cc = d.community_chapters;
  if (!cc) {
    return d.book.matched
      ? { line: { key: 'book.community.unchecked' }, titles: [], notes: [] }
      : null;
  }
  const det = cc.detail;
  const count = det?.community_chapters ?? 0;
  const local = det?.local_chapters ?? 0;
  const out: CommunityState = {
    line: { key: `book.community.why.${cc.status}` },
    titles: [],
    notes: [],
  };
  const using = d.chapter_source === 'community';
  if (cc.stale) {
    out.line = { key: 'book.community.stale' };
    return out;
  }
  if (using) {
    const key =
      cc.status === 'fill'
        ? 'book.community.usingFill'
        : cc.status === 'refine'
          ? 'book.community.usingRefine'
          : 'book.community.using';
    out.line = { key, values: { count, local } };
    out.action = { source: 'files', label: 'book.community.useFiles' };
  } else {
    switch (cc.status) {
      case 'fill':
        // Only the admin's choice keeps a fill out: back to automatic uses it.
        out.line = { key: 'book.community.offFill', values: { count } };
        out.action = { source: 'auto', label: 'book.community.use' };
        break;
      case 'refine':
        out.line = { key: 'book.community.refine', values: { count, local } };
        out.action = { source: 'community', label: 'book.community.useDetailed' };
        break;
      case 'restructure':
        out.line = { key: 'book.community.restructure', values: { count, local } };
        out.action = { source: 'community', label: 'book.community.use' };
        break;
      case 'titles': {
        // Only the differences still standing: a title the admin took is done.
        const now = new Map(d.chapters.map((c) => [c.index, c.title]));
        out.titles = (det?.title_diffs ?? []).filter((t) => now.get(t.index) !== t.community);
        out.line = out.titles.length
          ? { key: 'book.community.titles', values: { count: out.titles.length } }
          : { key: 'book.community.same' };
        break;
      }
      case 'same':
        out.line = { key: 'book.community.same' };
        break;
      case 'length_mismatch':
        out.line.values = {
          local: duration(det?.local_duration ?? 0),
          community: duration(det?.community_duration ?? 0),
        };
        break;
      case 'crosses_files':
        out.line.values = {
          title: det?.straddle?.title ?? '',
          from: fileName(det?.straddle?.from ?? '', d.book.path),
          to: fileName(det?.straddle?.to ?? '', d.book.path),
        };
        out.notes.push({ key: 'book.community.mergeHint' });
        break;
    }
  }
  if (d.community_check_failed) out.notes.unshift({ key: 'book.community.lastFailed' });
  if (FITTED_STATUSES.includes(cc.status)) {
    if (det?.omitted?.length) {
      out.notes.push({ key: 'book.community.omitted', values: { titles: det.omitted.join(', ') } });
    }
    if (using && det?.approximate) {
      out.notes.push({ key: 'book.community.approximate', values: { count: det.approximate } });
    }
  }
  return out;
}

// ---- the rest of the page ----

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

/** "M4B · AAC · 4 files": a book's format, codec and (for several) file count. */
export function audioLine(
  b: { format: string; codec: string; file_count: number },
  t: (key: string, values: { count: number }) => string,
): string {
  return [
    b.format.toUpperCase(),
    b.codec.toUpperCase(),
    b.file_count > 1 ? t('book.hero.files', { count: b.file_count }) : '',
  ]
    .filter(Boolean)
    .join(' · ');
}
