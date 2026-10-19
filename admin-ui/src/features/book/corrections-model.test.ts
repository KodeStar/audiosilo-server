import { describe, expect, it } from 'vitest';
import type {
  AdminBookDetail,
  BridgeJob,
  FieldValue,
  MatchCandidate,
  MatchRecording,
  OverrideField,
  ServerInfo,
} from '@/api/types';
import { serverInfo } from '@/test/fixtures';
import { bookDetail } from '@/test/library-fixtures';
import {
  alreadySuggested,
  asinRegion,
  correctionCandidate,
  correctionProblem,
  correctionRecording,
  correctionRequest,
  correctionRows,
  correctionsOffered,
  defaultCorrectionTicks,
  editedCorrectionFields,
  handling,
  lookupBy,
  MAX_CHANGES,
  MAX_EVIDENCE,
  type CorrectionRow,
} from './corrections-model';

const recording = (over: Partial<MatchRecording> = {}): MatchRecording => ({
  id: 'r1',
  narrators: [{ id: 'n1', name: 'Michael Kramer' }],
  release_date: '2010-08-31',
  asins: ['B003P2WO5E'],
  asin_refs: [{ region: 'us', asin: 'B003P2WO5E' }],
  asin_region: 'us',
  isbns: [],
  ...over,
});

const candidate = (over: Partial<MatchCandidate> = {}): MatchCandidate => ({
  work_id: 'the-way-of-kings',
  title: 'The Way of Kings',
  authors: [{ id: 'a1', name: 'Brandon Sanderson' }],
  first_published: '2010-08-31',
  series: [{ name: 'The Stormlight Archive', position: '1' }],
  web_url: 'https://meta.audiosilo.app/works/the-way-of-kings',
  recordings: [recording()],
  recording_id: 'r1',
  score: 100,
  ...over,
});

const edited = (value: string): FieldValue => ({
  value,
  source: 'edited',
  scanned: '',
  locked: false,
});

/** The book with some fields edited by the admin. */
function detailWith(edits: Partial<Record<OverrideField, string>>): AdminBookDetail {
  const d = bookDetail();
  for (const [f, v] of Object.entries(edits)) d.fields[f as OverrideField] = edited(v);
  return d;
}

function server(over: { contributions?: boolean; features?: string[] } = {}): ServerInfo {
  const { contributions = true, features = ['bridge', 'sidecars', 'meta', 'contribute'] } = over;
  return {
    ...serverInfo,
    capabilities: { ...serverInfo.capabilities, bridge: true, contributions },
    bridge: { version: '0.3.0', features },
  };
}

describe('when corrections are offered', () => {
  it('needs the module, metadata, the meta family, a match with an id, and an edited field', () => {
    const d = detailWith({ title: 'The Way of Kings (Tenth Anniversary)' });
    expect(correctionsOffered(server(), d)).toBe(true);
    expect(correctionsOffered(server({ contributions: false }), d)).toBe(false);
    expect(correctionsOffered(server({ features: ['bridge', 'sidecars'] }), d)).toBe(false);
    const off = server();
    off.capabilities.metadata = false;
    expect(correctionsOffered(off, d)).toBe(false);
    expect(correctionsOffered(undefined, d)).toBe(false);
    expect(correctionsOffered(server(), { ...d, book: { ...d.book, matched: false } })).toBe(false);
    const noIds = detailWith({ title: 'X' });
    noIds.fields.asin = { ...noIds.fields.asin, value: '' };
    expect(correctionsOffered(server(), noIds)).toBe(false);
  });

  it('counts only the fields a correction carries, edited and not cleared', () => {
    expect(correctionsOffered(server(), bookDetail())).toBe(false);
    // Publisher and description travel no correction (CORRECTIONS-SPIKE section 5).
    expect(correctionsOffered(server(), detailWith({ publisher: 'Tor', description: 'x' }))).toBe(
      false,
    );
    expect(
      editedCorrectionFields(detailWith({ author: '', narrator: 'Kate Reading' }).fields),
    ).toEqual(['narrator']);
  });

  it('looks the book up by its ASIN, else its ISBN', () => {
    expect(lookupBy(bookDetail().fields)).toEqual({ asin: 'B003P2WO5E' });
    const d = bookDetail();
    d.fields.asin = { ...d.fields.asin, value: '' };
    d.fields.isbn = { ...d.fields.isbn, value: '9780765326355' };
    expect(lookupBy(d.fields)).toEqual({ isbn: '9780765326355' });
  });

  it('takes only an identifier hit as the work the book is', () => {
    const hit = candidate();
    const words = candidate({ work_id: 'other', recording_id: undefined });
    expect(correctionCandidate([words, hit])?.work_id).toBe('the-way-of-kings');
    expect(
      correctionCandidate([candidate({ recording_id: undefined, reasons: { identifier: 'isbn' } })])
        ?.work_id,
    ).toBe('the-way-of-kings');
    expect(correctionCandidate([words])).toBeUndefined();
    expect(correctionCandidate(undefined)).toBeUndefined();
  });
});

describe('the review rows', () => {
  it('lists edited fields that differ, with who applies each', () => {
    const d = detailWith({
      title: 'The Way of Kings',
      author: 'B. Sanderson',
      narrator: 'Kate Reading',
      isbn: '9780765326355',
      publisher: 'Tor',
    });
    const c = candidate();
    const rows = correctionRows(d.fields, c, correctionRecording(c, d.fields));
    // The title agrees with the community; the publisher is no correction field.
    expect(rows.map((r) => [r.field, r.current, r.corrected, r.handling])).toEqual([
      ['author', 'Brandon Sanderson', 'B. Sanderson', 'maintainer'],
      ['narrator', 'Michael Kramer', 'Kate Reading', 'maintainer'],
      // The community has none: the correction adds it.
      ['isbn', '', '9780765326355', 'auto'],
    ]);
    expect(handling('title')).toBe('auto');
    expect(handling('published')).toBe('auto');
    expect(handling('series')).toBe('maintainer');
  });

  it("marks a value over the limit as one that can't be sent, counting characters", () => {
    const d = detailWith({ series: 'S'.repeat(501) });
    const c = candidate();
    const [row] = correctionRows(d.fields, c, correctionRecording(c, d.fields));
    expect(row.tooLong).toBe(true);
    expect(defaultCorrectionTicks([row]).size).toBe(0);
    // Characters as the server counts them, not UTF-16 units: 300 emoji are 300.
    const wide = detailWith({ series: '📚'.repeat(300) });
    expect(correctionRows(wide.fields, c, correctionRecording(c, wide.fields))[0].tooLong).toBe(
      false,
    );
  });

  it("reads published as the recording's release date, never filing what it already says", () => {
    const c = candidate({
      first_published: '1965',
      recordings: [recording({ release_date: '2006-12-05' })],
    });
    const rowFor = (published: string) => {
      const d = detailWith({ published });
      return correctionRows(d.fields, c, correctionRecording(c, d.fields));
    };
    // The release date, or the work's first publication, at any precision: nothing to file.
    expect(rowFor('2006-12-05')).toEqual([]);
    expect(rowFor('2006')).toEqual([]);
    expect(rowFor('1965')).toEqual([]);
    // Another date corrects the release date, which is what the community says now.
    expect(rowFor('2006-12-06').map((r) => [r.current, r.corrected, r.handling])).toEqual([
      ['2006-12-05', '2006-12-06', 'auto'],
    ]);
  });

  it('takes an ISBN the recording lists as no change, and none without an ASIN', () => {
    const c = candidate({
      recordings: [recording({ isbns: ['978-0765326355', '9780765365279'] })],
    });
    const second = detailWith({ isbn: '9780765365279' });
    expect(correctionRows(second.fields, c, correctionRecording(c, second.fields))).toEqual([]);
    // Without an ASIN the ISBN is how the community finds the book: no correction.
    const noAsin = detailWith({ isbn: '9780000000002' });
    noAsin.fields.asin = { ...noAsin.fields.asin, value: '' };
    expect(editedCorrectionFields(noAsin.fields)).toEqual([]);
    expect(correctionsOffered(server(), noAsin)).toBe(false);
  });

  it('names only a recording the identifier names, never a guess', () => {
    const d = detailWith({ published: '2011', narrator: 'Kate Reading' });
    // The lookup's recording.
    expect(correctionRecording(candidate(), d.fields)).toMatchObject({ known: true });
    // No recording from the lookup: the one listing the book's ASIN.
    const r2 = recording({ id: 'r2', asins: ['B00OTHER00'], asin_refs: [] });
    const byAsin = candidate({ recording_id: undefined, recordings: [r2, recording()] });
    expect(correctionRecording(byAsin, d.fields)).toEqual({
      rec: byAsin.recordings[1],
      known: true,
    });
    // None lists it: the likeliest is shown, but recording fields go to a maintainer
    // and the request names no recording.
    const guess = candidate({
      recording_id: undefined,
      default_recording_id: 'r2',
      recordings: [r2, recording({ id: 'r3', asins: [], asin_refs: [] })],
    });
    const target = correctionRecording(guess, d.fields);
    expect(target).toEqual({ rec: r2, known: false });
    const rows = correctionRows(d.fields, guess, target);
    expect(rows.map((r) => [r.field, r.handling])).toEqual([
      ['narrator', 'maintainer'],
      ['published', 'maintainer'],
    ]);
    const req = correctionRequest(d, guess, target, rows, new Set(['published']), 'e');
    expect(req.params).not.toHaveProperty('recording_id');
    expect(req.params).not.toHaveProperty('region');
    expect(handling('published', false)).toBe('maintainer');
    expect(handling('title', false)).toBe('auto');
  });

  it('ticks what can be sent and was not suggested before, at most the cap', () => {
    const row = (
      field: CorrectionRow['field'],
      over: Partial<CorrectionRow> = {},
    ): CorrectionRow => ({
      field,
      current: 'a',
      corrected: 'b',
      handling: 'maintainer',
      tooLong: false,
      ...over,
    });
    const rows = [row('title'), row('author'), row('narrator', { tooLong: true })];
    expect([...defaultCorrectionTicks(rows, new Set(['author']))]).toEqual(['title']);
    const many = Array.from({ length: 30 }, () => row('title'));
    expect(defaultCorrectionTicks(many).size).toBeLessThanOrEqual(MAX_CHANGES);
  });

  it("remembers what the newest job suggested, while its value is still the book's", () => {
    const d = detailWith({ author: 'B. Sanderson', narrator: 'Kate Reading' });
    const job = (state: BridgeJob['state']) =>
      ({
        kind: 'meta.correct',
        state,
        params: {
          changes: [
            { field: 'author', current: 'Brandon Sanderson', corrected: 'B. Sanderson' },
            { field: 'narrator', current: 'Michael Kramer', corrected: 'Someone else' },
            { field: 'publisher', current: '', corrected: 'Tor' },
          ],
        },
      }) as unknown as BridgeJob;
    expect([...alreadySuggested(job('done'), d.fields)]).toEqual(['author']);
    expect([...alreadySuggested(job('running'), d.fields)]).toEqual(['author']);
    // A failed or cancelled one filed nothing.
    expect(alreadySuggested(job('failed'), d.fields).size).toBe(0);
    expect(alreadySuggested(undefined, d.fields).size).toBe(0);
    expect(
      alreadySuggested({ kind: 'meta.correct', state: 'done', params: {} } as BridgeJob, d.fields)
        .size,
    ).toBe(0);
  });
});

describe('sending', () => {
  it('needs something ticked, at most the cap, and evidence within its limit', () => {
    expect(correctionProblem(0, 'x')).toBe('none');
    expect(correctionProblem(MAX_CHANGES + 1, 'x')).toBe('tooMany');
    expect(correctionProblem(2, '   ')).toBe('noEvidence');
    expect(correctionProblem(2, 'x'.repeat(MAX_EVIDENCE + 1))).toBe('evidenceTooLong');
    expect(correctionProblem(2, '📚'.repeat(MAX_EVIDENCE))).toBeUndefined();
    expect(correctionProblem(MAX_CHANGES, 'A store page')).toBeUndefined();
  });

  it("finds the marketplace of the book's ASIN", () => {
    expect(asinRegion(recording({ asin_refs: [{ region: 'uk', asin: 'B0X' }] }), 'B0X')).toBe('uk');
    expect(
      asinRegion(recording({ asin_refs: undefined, asins: ['B0X'], asin_region: 'de' }), 'B0X'),
    ).toBe('de');
    expect(asinRegion(recording({ asin_refs: [] }), 'B0Y')).toBeUndefined();
    expect(asinRegion(undefined, 'B0X')).toBeUndefined();
    expect(asinRegion(recording(), '')).toBeUndefined();
  });

  it('queues one meta.correct job with the ticked changes, found by the ASIN', () => {
    const d = detailWith({ author: 'B. Sanderson', isbn: '9780765326355' });
    const c = candidate();
    const target = correctionRecording(c, d.fields);
    const rows = correctionRows(d.fields, c, target);
    expect(correctionRequest(d, c, target, rows, new Set(['isbn']), '  A store page  ')).toEqual({
      kind: 'meta.correct',
      library_id: d.book.library_id,
      path: d.book.path,
      params: {
        work_id: 'the-way-of-kings',
        recording_id: 'r1',
        // The edited ISBN is a change, not how the book was found.
        asin: 'B003P2WO5E',
        region: 'us',
        changes: [{ field: 'isbn', current: '', corrected: '9780765326355' }],
        evidence: 'A store page',
      },
    });
  });

  it('names the ISBN as the identity when the book has no ASIN', () => {
    const d = detailWith({ title: 'Kings' });
    d.fields.asin = { ...d.fields.asin, value: '' };
    d.fields.isbn = { ...d.fields.isbn, value: '9780765326355' };
    const c = candidate();
    const none = { rec: undefined, known: false };
    const rows = correctionRows(d.fields, c, none);
    const req = correctionRequest(d, c, none, rows, new Set(['title']), 'e');
    expect(req.params).toEqual({
      work_id: 'the-way-of-kings',
      isbn: '9780765326355',
      changes: [{ field: 'title', current: 'The Way of Kings', corrected: 'Kings' }],
      evidence: 'e',
    });
  });
});
