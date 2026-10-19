import {
  CORRECTION_FIELDS,
  type AdminBookDetail,
  type BridgeJob,
  type BridgeJobRequest,
  type CorrectionField,
  type FieldValue,
  type MatchCandidate,
  type MatchRecording,
  type MetaCorrectChange,
  type MetaCorrectParams,
  type OverrideField,
  type ServerInfo,
} from '@/api/types';
import type { MatchBy } from '@/api/client';
import { contributionsOn, FEATURE_META, KIND_CORRECT } from '@/features/bridge/generate-model';
import { compareRows } from './match-model';

// Suggest corrections (BRIDGE-PLAN.md Phase 2b, decision 18; CORRECTIONS-SPIKE
// sections 4.1 and 5): the admin's own edits to a matched book, set against
// what the community says, sent to the Bridge as one `meta.correct` job that
// files a correction-batch issue on audiosilo-meta. Pure: no React, no i18n.

/** The most changes one suggestion carries (meta's correction-batch cap). */
export const MAX_CHANGES = 25;
/** The longest value a change carries, either side. */
export const MAX_VALUE = 500;
/** The longest evidence. */
export const MAX_EVIDENCE = 2000;

/**
 * The fields meta applies by itself once checked (work title, recording release
 * date and ISBN, decision D5); every other field waits for a maintainer.
 */
const AUTO_FIELDS: ReadonlySet<CorrectionField> = new Set(['title', 'published', 'isbn']);

/** Who applies a field's correction on the community's side. */
export type Handling = 'auto' | 'maintainer';

export function handling(field: CorrectionField): Handling {
  return AUTO_FIELDS.has(field) ? 'auto' : 'maintainer';
}

/** Whether a field is one a correction may carry. */
export function isCorrectionField(f: string): f is CorrectionField {
  return (CORRECTION_FIELDS as readonly string[]).includes(f);
}

/** The correction fields the admin edited on this book (with a value: clearing one is no correction). */
export function editedCorrectionFields(
  fields: Record<OverrideField, FieldValue>,
): CorrectionField[] {
  return CORRECTION_FIELDS.filter((f) => fields[f].source === 'edited' && fields[f].value !== '');
}

/**
 * Whether the Community card offers "Suggest corrections": the contributions
 * module is on, community metadata is on, the online Bridge runs the meta
 * family (meta.correct), the book is matched and has an ASIN or ISBN (how the
 * community finds it), and the admin edited a field a correction carries.
 * Otherwise hidden, not disabled.
 */
export function correctionsOffered(
  server: ServerInfo | undefined,
  detail: Pick<AdminBookDetail, 'book' | 'fields'>,
): boolean {
  if (!contributionsOn(server) || !server?.capabilities.metadata) return false;
  if (!server.bridge?.features.includes(FEATURE_META)) return false;
  if (!detail.book.matched) return false;
  if (!detail.fields.asin.value && !detail.fields.isbn.value) return false;
  return editedCorrectionFields(detail.fields).length > 0;
}

/** How the community is asked for the book: by its ASIN, else its ISBN. */
export function lookupBy(fields: Record<OverrideField, FieldValue>): MatchBy {
  const asin = fields.asin.value;
  return asin ? { asin } : { isbn: fields.isbn.value };
}

/**
 * The community work the book is: the candidate an identifier lookup resolved
 * (a recording hit, or a match scored on the identifier). A lookup that found
 * nothing by the identifier offers nothing to correct.
 */
export function correctionCandidate(
  candidates: readonly MatchCandidate[] | undefined,
): MatchCandidate | undefined {
  return candidates?.find((c) => c.recording_id || c.reasons?.identifier);
}

/** One row of the review: the community's value now, and the admin's edit. */
export interface CorrectionRow {
  field: CorrectionField;
  /** The community's value now ("" when it has none). */
  current: string;
  /** The admin's edited value, as the server stores it. */
  corrected: string;
  handling: Handling;
  /** A value is over MAX_VALUE: it can't be sent. */
  tooLong: boolean;
}

/**
 * The review's rows: each edited correction field whose value differs from the
 * community's (the community lacking one counts: the correction adds it), in
 * field order. The community's values are the match dialog's (communityValues,
 * for the recording the book is).
 */
export function correctionRows(
  fields: Record<OverrideField, FieldValue>,
  c: MatchCandidate,
  rec: MatchRecording | undefined,
): CorrectionRow[] {
  const edited = new Set<string>(editedCorrectionFields(fields));
  return compareRows(fields, c, rec)
    .filter((r) => edited.has(r.field) && !r.same && isCorrectionField(r.field))
    .map((r) => {
      const field = r.field as CorrectionField;
      return {
        field,
        current: r.theirs,
        corrected: r.mine,
        handling: handling(field),
        tooLong: r.mine.length > MAX_VALUE || r.theirs.length > MAX_VALUE,
      };
    });
}

/**
 * The fields the book's newest corrections job (open, or finished and filed)
 * already suggested with the value the book has now: those rows start
 * unticked, so the same suggestion isn't filed twice by accident.
 */
export function alreadySuggested(
  job: Pick<BridgeJob, 'kind' | 'state' | 'params'> | undefined,
  fields: Record<OverrideField, FieldValue>,
): Set<CorrectionField> {
  const out = new Set<CorrectionField>();
  if (!job || job.kind !== KIND_CORRECT) return out;
  if (job.state === 'failed' || job.state === 'cancelled') return out;
  const changes = (job.params as { changes?: unknown }).changes;
  if (!Array.isArray(changes)) return out;
  for (const ch of changes as unknown[]) {
    if (!ch || typeof ch !== 'object') continue;
    const { field, corrected } = ch as Record<string, unknown>;
    if (typeof field !== 'string' || !isCorrectionField(field)) continue;
    if (corrected === fields[field].value) out.add(field);
  }
  return out;
}

/**
 * Ticked to begin with: every row that can be sent and wasn't suggested
 * already, at most MAX_CHANGES of them.
 */
export function defaultCorrectionTicks(
  rows: readonly CorrectionRow[],
  suggested: ReadonlySet<CorrectionField> = new Set(),
): Set<CorrectionField> {
  return new Set(
    rows
      .filter((r) => !r.tooLong && !suggested.has(r.field))
      .slice(0, MAX_CHANGES)
      .map((r) => r.field),
  );
}

/** Why the suggestion can't be sent yet, or undefined when it can. */
export type CorrectionProblem = 'none' | 'tooMany' | 'noEvidence' | 'evidenceTooLong';

export function correctionProblem(ticked: number, evidence: string): CorrectionProblem | undefined {
  if (ticked === 0) return 'none';
  if (ticked > MAX_CHANGES) return 'tooMany';
  const e = evidence.trim();
  if (!e) return 'noEvidence';
  if (e.length > MAX_EVIDENCE) return 'evidenceTooLong';
  return undefined;
}

/**
 * The marketplace the book's ASIN sells in, as the recording lists it, else
 * undefined (the Bridge then files without one).
 */
export function asinRegion(rec: MatchRecording | undefined, asin: string): string | undefined {
  if (!rec || !asin) return undefined;
  const ref = rec.asin_refs?.find((r) => r.asin === asin);
  if (ref) return ref.region;
  return rec.asins?.[0] === asin ? rec.asin_region || undefined : undefined;
}

/**
 * The meta.correct job for the ticked rows: the work and recording, how the
 * book was found (its ASIN, else its ISBN: an edited ISBN is a change, not the
 * identity, when an ASIN found it), the changes in field order, and the
 * evidence. The server checks each change against the book's edits and stamps
 * the source.
 */
export function correctionRequest(
  detail: Pick<AdminBookDetail, 'book' | 'fields'>,
  c: MatchCandidate,
  rec: MatchRecording | undefined,
  rows: readonly CorrectionRow[],
  ticks: ReadonlySet<CorrectionField>,
  evidence: string,
): BridgeJobRequest {
  const asin = detail.fields.asin.value;
  const changes: MetaCorrectChange[] = rows
    .filter((r) => ticks.has(r.field) && !r.tooLong)
    .map((r) => ({ field: r.field, current: r.current, corrected: r.corrected }));
  const params: MetaCorrectParams = {
    work_id: c.work_id,
    ...(rec ? { recording_id: rec.id } : {}),
    ...(asin ? { asin } : { isbn: detail.fields.isbn.value }),
    changes,
    evidence: evidence.trim(),
  };
  const region = asinRegion(rec, asin);
  if (region) params.region = region;
  return {
    kind: KIND_CORRECT,
    library_id: detail.book.library_id,
    path: detail.book.path,
    params: { ...params },
  };
}
