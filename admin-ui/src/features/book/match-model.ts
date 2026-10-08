import {
  OVERRIDE_FIELDS,
  type BookEditRequest,
  type FieldSource,
  type FieldValue,
  type MatchCandidate,
  type MatchRecording,
  type OverrideField,
} from '@/api/types';
import type { MatchBy } from '@/api/client';
import { seriesKey } from '@/features/library/series/series-model';
import { ISBN, checkField } from './book-model';

// The match dialog's logic (STYLEGUIDE.md "Match with community"): what the
// search box asks for, which recording of a work is meant, what each field
// would become, which fields are ticked by default (never the admin's own
// edits), and how a recording's length compares with the files.

/** An ASIN (10 alphanumerics from B0) or an ISBN-10/13 is looked up exactly; anything else is words. */
export function parseMatchQuery(input: string): MatchBy {
  const text = input.trim();
  if (!text) return {};
  const compact = text.replace(/[- ]/g, '').toUpperCase();
  if (/^B0[A-Z0-9]{8}$/.test(compact)) return { asin: compact };
  if (ISBN.test(compact)) return { isbn: compact };
  return { q: text };
}

/**
 * The recording a candidate most likely is: the server's pick (the one an
 * identifier hit, else the closest runtime, the preferred marketplace breaking a
 * tie: meta.DefaultRecording), else the first.
 */
export function defaultRecording(c: MatchCandidate): MatchRecording | undefined {
  const recs = c.recordings ?? [];
  const id = c.default_recording_id || c.recording_id;
  return recs.find((r) => r.id === id) ?? recs[0];
}

/** A community value as the server would store it ("" when absent or one it would refuse). */
function clean(field: OverrideField, raw: string | undefined): string {
  if (!raw) return '';
  const { value, error } = checkField(field, raw);
  return error ? '' : value;
}

/** A candidate's authors, as one line. */
export function candidateAuthors(c: MatchCandidate): string {
  return (c.authors ?? []).map((a) => a.name).join(', ');
}

/**
 * What the community says for each field, from the work and the chosen recording.
 * A work in several series offers one as the main series - the one the book is
 * filed under (`mainSeries`) when the work is in it, else the work's first - and
 * the rest as the book's other series (as matchrun.planSeries does).
 */
export function communityValues(
  c: MatchCandidate,
  rec: MatchRecording | undefined,
  mainSeries = '',
): Record<OverrideField, string> {
  const all = c.series ?? [];
  const key = seriesKey(mainSeries);
  const series = (key && all.find((s) => seriesKey(s.name) === key)) || all[0];
  // The others: a work listed again in its main series isn't another series.
  const others = series ? all.filter((s) => seriesKey(s.name) !== seriesKey(series.name)) : [];
  const year = (c.first_published || rec?.release_date || '').match(/^\d{4}/)?.[0];
  return {
    title: clean('title', c.title),
    author: clean('author', candidateAuthors(c)),
    narrator: clean('narrator', (rec?.narrators ?? []).map((n) => n.name).join(', ')),
    series: clean('series', series?.name),
    series_index: clean('series_index', series?.position),
    more_series: clean('more_series', otherSeries(others)),
    published: clean('published', year),
    description: clean('description', c.description),
    asin: clean('asin', rec?.asins?.[0]),
    isbn: clean('isbn', rec?.isbns?.[0]),
  };
}

/** Community series as a more_series value (a position that is no plain number is none). */
function otherSeries(series: { name: string; position: string }[]): string {
  return JSON.stringify(
    series.map((s) => ({ name: s.name, position: Number(clean('series_index', s.position)) || 0 })),
  );
}

/** One row of the compare table. */
export interface CompareRow {
  field: OverrideField;
  mine: string;
  source: FieldSource;
  theirs: string;
  /** The community agrees with what the server has. */
  same: boolean;
  /** The community has a different, non-empty value: a checkbox. */
  offered: boolean;
}

export function compareRows(
  fields: Record<OverrideField, FieldValue>,
  c: MatchCandidate,
  rec: MatchRecording | undefined,
): CompareRow[] {
  const theirs = communityValues(c, rec, fields.series.value);
  return OVERRIDE_FIELDS.map((f) => {
    const mine = fields[f].value;
    const same = !!theirs[f] && clean(f, mine) === theirs[f];
    return {
      field: f,
      mine,
      source: fields[f].source,
      theirs: theirs[f],
      same,
      offered: !!theirs[f] && !same,
    };
  });
}

/** The cover the community has for a candidate: the recording's, else the work's ("" = none). */
export function communityCover(c: MatchCandidate, rec: MatchRecording | undefined): string {
  return rec?.cover_url || c.cover_url || '';
}

/**
 * Whether the community cover starts ticked: only for a book with no art of its
 * own. Art in the book's files, or one the admin uploaded, is kept unless ticked.
 * Nor one the server already couldn't fetch (`preview` null; undefined = not
 * answered yet): taking it would only fail.
 */
export function defaultCoverTick(
  hasCover: boolean,
  coverUrl: string,
  preview?: string | null,
): boolean {
  return !!coverUrl && !hasCover && preview !== null;
}

/** Ticked by default: every offered field the admin hasn't edited themselves. */
export function defaultTicks(rows: CompareRow[]): Set<OverrideField> {
  return new Set(rows.filter((r) => r.offered && r.source !== 'edited').map((r) => r.field));
}

/**
 * The PATCH that accepts the ticked fields from the community. Keeping the book's
 * own series over the one offered (Series unticked) keeps the series together as
 * matchrun.planSeries does: the offered position, which numbers the offered
 * series, isn't taken. The offered series isn't added as another either: a
 * series the community doesn't list is most likely the offered one spelled the
 * book's own way. Unticking only a respelling of the book's own series changes
 * nothing.
 */
export function acceptRequest(rows: CompareRow[], ticks: Set<OverrideField>): BookEditRequest {
  const set: Partial<Record<OverrideField, string>> = {};
  for (const r of rows) if (r.offered && ticks.has(r.field)) set[r.field] = r.theirs;
  const row = (f: OverrideField) => rows.find((r) => r.field === f);
  const series = row('series');
  if (
    series?.offered &&
    !ticks.has('series') &&
    seriesKey(series.theirs) !== seriesKey(series.mine)
  ) {
    delete set.series_index;
  }
  return { set, source: 'community' };
}

export type LengthCompare =
  { kind: 'match' } | { kind: 'longer' | 'shorter'; seconds: number } | undefined;

/** A recording's runtime against the files: within 5% "matches", else how far off. */
export function lengthComparison(
  runtimeMin: number | undefined,
  bookSeconds: number,
): LengthCompare {
  if (!runtimeMin || bookSeconds <= 0) return undefined;
  const diff = runtimeMin * 60 - bookSeconds;
  if (Math.abs(diff) <= bookSeconds * 0.05) return { kind: 'match' };
  return { kind: diff > 0 ? 'longer' : 'shorter', seconds: Math.abs(diff) };
}

/** The score badge's variant. */
export function scoreTone(score: number): 'success' | 'warning' | 'outline' {
  return score >= 90 ? 'success' : score >= 60 ? 'warning' : 'outline';
}
