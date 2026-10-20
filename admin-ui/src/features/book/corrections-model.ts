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
import { runes } from './book-model';
import { CONTROL, CONTROL_IN_TEXT } from './field-rules';
import { clean, compareRows, defaultRecording, type CompareRow } from './match-model';

// Suggest corrections (BRIDGE-PLAN.md Phase 2b, decision 18; CORRECTIONS-SPIKE
// sections 4.1 and 5): the admin's own edits to a matched book, set against
// what the community says, sent to the Bridge as one `meta.correct` job that
// files a correction-batch issue on audiosilo-meta. Pure: no React, no i18n.

/** The longest value a change carries, either side. */
export const MAX_VALUE = 500;
/** The longest evidence. */
export const MAX_EVIDENCE = 2000;

/**
 * The bidirectional embedding, override and isolate controls (U+202A-U+202E,
 * U+2066-U+2069): invisible, they reorder how text shows, and the server
 * refuses a value or evidence holding one, as the community does
 * (bridge.isBidiControl). The joiners and marks right-to-left scripts need are
 * not among them.
 */
const BIDI = /[\u202A-\u202E\u2066-\u2069]/;

/** The community issue form's blank field, which reads as none (bridge.noResponse). */
const NO_RESPONSE = '_No response_';

/** Go's unicode.IsSpace, which strings.TrimSpace trims and asRead reads a reference to as a space. */
const IS_GO_SPACE = /^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]$/;

/** v trimmed as the server trims it (Go's strings.TrimSpace, not String.trim: U+0085 yes, U+FEFF no). */
export function goTrim(v: string): string {
  let i = 0;
  let j = v.length;
  while (i < j && IS_GO_SPACE.test(v[i])) i++;
  while (j > i && IS_GO_SPACE.test(v[j - 1])) j--;
  return v.slice(i, j);
}

/** An HTML character reference ending in ';', as the server finds them (bridge.charRef). */
const CHAR_REF = /&(?:#[0-9]+|#[xX][0-9a-fA-F]+|[A-Za-z][A-Za-z0-9]*);/g;

/** What HTML reads code points 0x80-0x9F in a numeric reference as (Windows-1252, as Go's html does). */
const WINDOWS_1252 =
  '\u20ac\u0081\u201a\u0192\u201e\u2026\u2020\u2021\u02c6\u2030\u0160\u2039\u0152\u008d\u017d\u008f' +
  '\u0090\u2018\u2019\u201c\u201d\u2022\u2013\u2014\u02dc\u2122\u0161\u203a\u0153\u009d\u017e\u0178';

/**
 * A numeric reference decoded as Go's html.UnescapeString decodes it: the
 * number accumulated in an int32 (wrapping as Go's does), 0x80-0x9F read as
 * Windows-1252, and zero, a surrogate or anything out of range read as U+FFFD.
 */
function numericRef(ref: string): string {
  const hex = ref[2] === 'x' || ref[2] === 'X';
  let x = 0;
  for (const c of ref.slice(hex ? 3 : 2, -1)) {
    x = (Math.imul(x, hex ? 16 : 10) + parseInt(c, 16)) | 0;
  }
  if (x >= 0x80 && x <= 0x9f) return WINDOWS_1252[x - 0x80];
  if (x <= 0 || (x >= 0xd800 && x <= 0xdfff) || x > 0x10ffff) return '\ufffd';
  return String.fromCodePoint(x);
}

/** Named references already decoded, at most MAX_NAMED_REFS (the names are typed, so unbounded). */
const namedRefs = new Map<string, string>();
const MAX_NAMED_REFS = 256;
let refParser: DOMParser | undefined;

/**
 * A named reference decoded by the browser's own HTML parser, which reads the
 * full HTML table as Go's html does (both follow the HTML standard, legacy
 * prefixes like "&copy" in "&copyright;" included). Without a DOM it stays as
 * written.
 */
function namedRef(ref: string): string {
  let r = namedRefs.get(ref);
  if (r === undefined) {
    if (typeof DOMParser === 'undefined') return ref;
    refParser ??= new DOMParser();
    // In the body: before it, the parser drops a reference decoding to whitespace.
    r =
      refParser.parseFromString(`<!doctype html><body>${ref}`, 'text/html').body.textContent ?? ref;
    if (namedRefs.size >= MAX_NAMED_REFS) namedRefs.clear();
    namedRefs.set(ref, r);
  }
  return r;
}

/**
 * v with each character reference decoded as the community reads it
 * (bridge.decodeRefs): one decoding to a single whitespace character read as
 * a space, one decoding to more than two characters left as written (it isn't
 * one). Untrimmed: a reference is ASCII, so every character written stays.
 */
function decodeRefs(v: string): string {
  if (!v.includes('&')) return v;
  return v.replace(CHAR_REF, (ref) => {
    const r = ref[1] === '#' ? numericRef(ref) : namedRef(ref);
    if (runes(r) > 2) return ref;
    return IS_GO_SPACE.test(r) ? ' ' : r;
  });
}

/** v as the community reads it, which is how the server checks it (bridge.asRead): decoded, then trimmed as Go trims. */
export function asRead(v: string): string {
  return goTrim(decodeRefs(v));
}

/**
 * Evidence's length in characters as the community's issue carries it, which
 * the server bounds (bridge.issueLength): a line starting with # (after its
 * indent) is escaped, its indent dropped and a backslash added.
 */
export function issueLength(evidence: string): number {
  let n = runes(evidence);
  for (const line of evidence.split('\n')) {
    const t = line.replace(/^[ \t]+/, '');
    if (t.startsWith('#')) n += 1 - (line.length - t.length);
  }
  return n;
}

/**
 * The fields meta applies by itself once checked (work title, recording release
 * date and ISBN, decision D5); every other field waits for a maintainer.
 */
const AUTO_FIELDS: ReadonlySet<CorrectionField> = new Set(['title', 'published', 'isbn']);

/** The fields meta keeps on the recording (its narrators, release date and ISBNs). */
const RECORDING_FIELDS: ReadonlySet<CorrectionField> = new Set(['narrator', 'published', 'isbn']);

/** Who applies a field's correction on the community's side. */
export type Handling = 'auto' | 'maintainer';

/**
 * Who applies a field's correction. A recording's field applies by itself only
 * when the suggestion names the recording (`recordingKnown`): meta hands one
 * addressed to the work to a maintainer, who finds the recording.
 */
export function handling(field: CorrectionField, recordingKnown = true): Handling {
  if (!AUTO_FIELDS.has(field)) return 'maintainer';
  return recordingKnown || !RECORDING_FIELDS.has(field) ? 'auto' : 'maintainer';
}

/** Whether a field is one a correction may carry. */
export function isCorrectionField(f: string): f is CorrectionField {
  return (CORRECTION_FIELDS as readonly string[]).includes(f);
}

/**
 * The correction fields the admin edited on this book: with a value (clearing
 * one is no correction), and the ISBN only beside an ASIN (without one, the
 * ISBN is how the community finds the book, not a correction: CORRECTIONS-SPIKE
 * section 5).
 */
export function editedCorrectionFields(
  fields: Record<OverrideField, FieldValue>,
): CorrectionField[] {
  return CORRECTION_FIELDS.filter(
    (f) =>
      fields[f].source === 'edited' &&
      fields[f].value !== '' &&
      (f !== 'isbn' || fields.asin.value !== ''),
  );
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

/** The recording a suggestion is about, and whether the book's identifier names it. */
export interface CorrectionRecording {
  /** The recording whose values the review shows: the one named, else the likeliest. */
  rec: MatchRecording | undefined;
  /** The identifier names it, so the suggestion carries its id. */
  known: boolean;
}

/**
 * The recording the book is, as its identifier names it: the one the lookup
 * resolved, else the one recording of the work that lists the book's ASIN
 * (else its ISBN), as the Bridge picks one without an id (correct.recordingFor).
 * When none or several do, the likeliest (defaultRecording, a guess from the
 * runtime) is only shown: the suggestion names no recording, and meta hands
 * the recording's fields to a maintainer, so a guess never has one applied by
 * itself.
 */
export function correctionRecording(
  c: MatchCandidate,
  fields: Record<OverrideField, FieldValue>,
): CorrectionRecording {
  const recs = c.recordings ?? [];
  const resolved = c.recording_id ? recs.find((r) => r.id === c.recording_id) : undefined;
  if (resolved) return { rec: resolved, known: true };
  const asin = fields.asin.value;
  const isbn = fields.isbn.value;
  const named = recs.filter((r) =>
    asin
      ? (r.asins ?? []).includes(asin) || (r.asin_refs ?? []).some((a) => a.asin === asin)
      : isbn !== '' && (r.isbns ?? []).some((i) => clean('isbn', i) === isbn),
  );
  if (named.length === 1) return { rec: named[0], known: true };
  return { rec: defaultRecording(c), known: false };
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
  /** A value holds a control character, as written or as references decode it: it can't be sent. */
  control: boolean;
  /** A value, references decoded, holds a bidirectional control (BIDI): it can't be sent. */
  bidi: boolean;
  /**
   * The corrected value, read as the community reads it, is empty or the
   * community's value as written ("Tom &amp; Jerry" for "Tom & Jerry"): it
   * changes nothing and can't be sent.
   */
  noChange: boolean;
}

/** A row's refusal flags, in the order the review names them (each is book.corrections.<flag>). */
export const REFUSALS = ['tooLong', 'control', 'bidi', 'noChange'] as const;

/** Whether a row can be sent: the server takes it (none of its refusal flags is set). */
export function sendable(r: CorrectionRow): boolean {
  return !REFUSALS.some((k) => r[k]);
}

/**
 * The review's rows: each edited correction field whose value differs from the
 * community's (the community lacking one counts: the correction adds it), in
 * field order. The community's values are the match dialog's (communityValues,
 * for the recording the book is), as a correction reads them (asCorrection).
 */
export function correctionRows(
  fields: Record<OverrideField, FieldValue>,
  c: MatchCandidate,
  { rec, known }: CorrectionRecording,
): CorrectionRow[] {
  const edited = new Set<string>(editedCorrectionFields(fields));
  return compareRows(fields, c, rec)
    .filter((r) => edited.has(r.field) && isCorrectionField(r.field))
    .map((r) => asCorrection(r, c, rec))
    .filter((r) => !r.same)
    .map((r) => {
      const field = r.field as CorrectionField;
      // Each value as the server checks it (bridge.parseCorrect, checkProse):
      // trimmed, then its references decoded (which keeps what is written).
      const values = [goTrim(r.theirs), goTrim(r.mine)];
      const decoded = values.map(decodeRefs);
      const corrected = goTrim(decoded[1]);
      return {
        field,
        current: r.theirs,
        corrected: r.mine,
        handling: handling(field, known),
        tooLong: values.some((v) => runes(v) > MAX_VALUE),
        control: decoded.some((v) => CONTROL.test(v)),
        bidi: decoded.some((v) => BIDI.test(v)),
        noChange: corrected === '' || corrected === values[0],
      };
    });
}

/**
 * A compare row as a correction reads it, where that differs from the match
 * dialog: `published` is filed as the recording's release date, so that is the
 * community's value now, and the community already says the admin's date when
 * it is the release date or the work's first publication, or one of them more
 * precisely (a coarser date corrects nothing); an ISBN is one more of the
 * recording's (meta adds it), so one the recording lists already is no change.
 */
function asCorrection(
  r: CompareRow,
  c: MatchCandidate,
  rec: MatchRecording | undefined,
): CompareRow {
  switch (r.field) {
    case 'published': {
      const release = clean('published', rec?.release_date);
      const first = clean('published', c.first_published);
      return {
        ...r,
        theirs: release,
        same: saysNoMore(r.mine, release) || saysNoMore(r.mine, first),
      };
    }
    case 'isbn':
      return { ...r, same: r.same || (rec?.isbns ?? []).some((i) => clean('isbn', i) === r.mine) };
    default:
      return r;
  }
}

/** Whether a date says nothing another doesn't: the same date, or it less precisely ("2010" beside "2010-08-31"). */
function saysNoMore(date: string, other: string): boolean {
  return other !== '' && (date === other || other.startsWith(`${date}-`));
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
 * already (one row per field, so far under the server's 25-change cap).
 */
export function defaultCorrectionTicks(
  rows: readonly CorrectionRow[],
  suggested: ReadonlySet<CorrectionField> = new Set(),
): Set<CorrectionField> {
  return new Set(rows.filter((r) => sendable(r) && !suggested.has(r.field)).map((r) => r.field));
}

/** Why the suggestion can't be sent yet, or undefined when it can. */
export type CorrectionProblem =
  'none' | 'noEvidence' | 'evidenceTooLong' | 'evidenceControl' | 'evidenceBidi';

/**
 * Why the suggestion can't be sent: nothing ticked, or evidence the server
 * refuses, checked as it does (bridge.parseCorrect): CRLF read as LF and
 * trimmed as Go trims; the form's blank or reading as nothing is none; over
 * MAX_EVIDENCE as the issue carries it (issueLength); a control character but
 * a line break or a tab (CONTROL_IN_TEXT), or a bidirectional control, as
 * written or as references decode it.
 */
export function correctionProblem(ticked: number, evidence: string): CorrectionProblem | undefined {
  if (ticked === 0) return 'none';
  const e = goTrim(evidence.replaceAll('\r\n', '\n'));
  const decoded = decodeRefs(e);
  if (e === NO_RESPONSE || !goTrim(decoded)) return 'noEvidence';
  if (issueLength(e) > MAX_EVIDENCE) return 'evidenceTooLong';
  if (CONTROL_IN_TEXT.test(decoded)) return 'evidenceControl';
  if (BIDI.test(decoded)) return 'evidenceBidi';
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
 * The meta.correct job for the ticked rows: the work and the recording (only
 * one the identifier names, correctionRecording: a guess is never filed as
 * the record), how the book was found (its ASIN, else its ISBN: an edited ISBN
 * is a change, not the identity, when an ASIN found it), the changes in field
 * order, and the evidence. The server checks each change against the book's
 * edits and stamps the source.
 */
export function correctionRequest(
  detail: Pick<AdminBookDetail, 'book' | 'fields'>,
  c: MatchCandidate,
  { rec, known }: CorrectionRecording,
  rows: readonly CorrectionRow[],
  ticks: ReadonlySet<CorrectionField>,
  evidence: string,
): BridgeJobRequest {
  const asin = detail.fields.asin.value;
  const changes: MetaCorrectChange[] = rows
    .filter((r) => ticks.has(r.field) && sendable(r))
    .map((r) => ({ field: r.field, current: r.current, corrected: r.corrected }));
  const named = known ? rec : undefined;
  const params: MetaCorrectParams = {
    work_id: c.work_id,
    ...(named ? { recording_id: named.id } : {}),
    ...(asin ? { asin } : { isbn: detail.fields.isbn.value }),
    changes,
    evidence: goTrim(evidence),
  };
  const region = asinRegion(named, asin);
  if (region) params.region = region;
  return {
    kind: KIND_CORRECT,
    library_id: detail.book.library_id,
    path: detail.book.path,
    params: { ...params },
  };
}
