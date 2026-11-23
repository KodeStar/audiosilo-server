import {
  OVERRIDE_FIELDS,
  isLabelField,
  type BookEditRequest,
  type BookMatchProposal,
  type SeriesPlan,
  type FieldSource,
  type FieldValue,
  type MatchCandidate,
  type MatchRecording,
  type OverrideField,
} from '@/api/types';
import type { MatchBy } from '@/api/client';
import { ISBN, checkField } from './book-model';

// Display the server's proposal and collect the admin's choices.

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
 * identifier hit, else of the recordings the book's narrators read that fit its
 * length the closest runtime, the preferred marketplace breaking a tie:
 * meta.DefaultRecording), else the first. None for a work that holds only a
 * dramatization a plain book isn't: one click must not take it.
 */
export function defaultRecording(c: MatchCandidate): MatchRecording | undefined {
  if (c.dramatization_only) return undefined;
  const recs = c.recordings ?? [];
  const id = c.default_recording_id || c.recording_id;
  return recs.find((r) => r.id === id) ?? recs[0];
}

/** A community value as the server would store it ("" when absent or one it would refuse). */
export function clean(field: OverrideField, raw: string | undefined): string {
  if (!raw) return '';
  const { value, error } = checkField(field, raw);
  return error ? '' : value;
}

/** A candidate's authors, as one line. */
export function candidateAuthors(c: MatchCandidate): string {
  return (c.authors ?? []).map((a) => a.name).join(', ');
}

/** One row of the compare table. */
export interface CompareRow {
  field: OverrideField;
  mine: string;
  source: FieldSource;
  theirs: string;
  /** The community agrees with what the server has. */
  same: boolean;
  /** A checkbox: the server plans a write, or the community has a different, non-empty value. */
  offered: boolean;
  /** The planned write puts the scanned value back (a stale community value). */
  revert: boolean;
  /** The server's plan writes this field: the checkbox's default. */
  planned: boolean;
}

/**
 * The compare table's rows, in field order. The label fields are left out: the
 * community offers neither, so their rows would only ever say so.
 */
export function compareRows(
  fields: Record<OverrideField, FieldValue>,
  proposal: SeriesPlan,
  takeSeries?: boolean,
): CompareRow[] {
  const seriesDefault = !!proposal.set.series || !!proposal.revert?.includes('series');
  const alternate =
    takeSeries !== undefined && takeSeries !== seriesDefault
      ? proposal.alternate_series
      : undefined;
  return OVERRIDE_FIELDS.filter((f) => !isLabelField(f)).map((field) => {
    const plan =
      alternate && (field === 'more_series' || field === 'series_index') ? alternate : proposal;
    const { value: mine, source, scanned } = fields[field];
    const revert = plan.revert?.includes(field) ?? false;
    const planned = revert || plan.set[field] !== undefined;
    const theirs = revert ? scanned : (plan.values[field] ?? '');
    const same = !planned && !!theirs && clean(field, mine) === theirs;
    return {
      field,
      mine,
      source,
      theirs,
      same,
      planned,
      revert,
      offered: planned || (!!theirs && !same),
    };
  });
}

/** The cover the community has for a candidate: the recording's, else the work's ("" = none). */
export function communityCover(c: MatchCandidate, rec: MatchRecording | undefined): string {
  return rec?.cover_url || c.cover_url || '';
}

/** Accept the selected writes and record the identifiers offered, even when unticked. */
export function acceptRequest(
  rows: CompareRow[],
  ticks: Set<OverrideField>,
  proposal: BookMatchProposal,
): BookEditRequest {
  const set: Partial<Record<OverrideField, string>> = {};
  const revert: OverrideField[] = [];
  for (const r of rows) {
    if (!r.offered || !ticks.has(r.field)) continue;
    if (r.revert) revert.push(r.field);
    else set[r.field] = r.theirs;
  }
  return {
    set,
    ...(revert.length ? { revert } : {}),
    source: 'community',
    match: {
      work_id: proposal.work_id,
      recording_id: proposal.recording_id,
      asin: proposal.values.asin ?? '',
      isbn: proposal.values.isbn ?? '',
    },
  };
}

export type LengthCompare =
  { kind: 'match' } | { kind: 'close' | 'longer' | 'shorter'; seconds: number } | undefined;

/**
 * How far a recording of runtimeMin minutes is from a book of bookSeconds, as a
 * fraction of the book's length (meta.RuntimeOff); undefined when either is unknown.
 */
export function runtimeOff(
  runtimeMin: number | undefined,
  bookSeconds: number,
): number | undefined {
  if (!runtimeMin || runtimeMin <= 0 || bookSeconds <= 0) return undefined;
  return Math.abs(runtimeMin * 60 - bookSeconds) / bookSeconds;
}

/**
 * How far a recording may be off the files and still "match": the 2% a match
 * run's automatic rule allows (matchrun.AnchorRuntimeOff), so the dialog never
 * says a length matches that a run holds for review.
 */
const LENGTH_MATCH_OFF = 0.02;
/** How far off it still reads "close" rather than longer or shorter. */
const LENGTH_CLOSE_OFF = 0.05;

/** A recording's runtime against the files: within 2% "matches", within 5% "close", else how far off. */
export function lengthComparison(
  runtimeMin: number | undefined,
  bookSeconds: number,
): LengthCompare {
  const off = runtimeOff(runtimeMin, bookSeconds);
  if (off === undefined) return undefined;
  if (off <= LENGTH_MATCH_OFF) return { kind: 'match' };
  const diff = (runtimeMin ?? 0) * 60 - bookSeconds;
  const seconds = Math.abs(diff);
  return off <= LENGTH_CLOSE_OFF
    ? { kind: 'close', seconds }
    : { kind: diff > 0 ? 'longer' : 'shorter', seconds };
}

/**
 * A score under it is a weak fit, almost always another book: the server's
 * matchrun.ReviewFloor, under which a bulk match records nothing.
 */
export const WEAK_SCORE = 60;

/** The score badge's variant. */
export function scoreTone(score: number): 'success' | 'warning' | 'outline' {
  return score >= 90 ? 'success' : score >= WEAK_SCORE ? 'warning' : 'outline';
}

/**
 * Whether every candidate is a weak fit (WEAK_SCORE), so Add it is offered
 * beside them (server#392): a search also sends the folder's author, whose
 * surname alone can bring back other people's books.
 */
export function onlyWeak(candidates: readonly Pick<MatchCandidate, 'score'>[]): boolean {
  return candidates.length > 0 && candidates.every((c) => c.score < WEAK_SCORE);
}
