import type { BookMatchProposal, MatchCandidate, MatchRecording } from '@/api/types';
import { bookDetail } from '@/test/library-fixtures';
import {
  acceptRequest,
  communityCover,
  compareRows,
  defaultRecording,
  lengthComparison,
  parseMatchQuery,
  scoreTone,
} from './match-model';

const recording = (over: Partial<MatchRecording> = {}): MatchRecording => ({
  id: 'r1',
  narrators: [
    { id: 'n1', name: 'Michael Kramer' },
    { id: 'n2', name: 'Kate Reading' },
  ],
  runtime_min: 2730,
  publisher: 'Macmillan Audio',
  asins: ['B003P2WO5E'],
  isbns: [],
  ...over,
});

const candidate = (over: Partial<MatchCandidate> = {}): MatchCandidate => ({
  work_id: 'the-way-of-kings',
  title: 'The Way of Kings',
  authors: [{ id: 'a1', name: 'Brandon Sanderson' }],
  first_published: '2010-08-31',
  description: 'Roshar is a world of stone and storms.',
  series: [{ name: 'The Stormlight Archive', position: '1' }],
  web_url: 'https://meta.audiosilo.app/works/the-way-of-kings',
  recordings: [recording()],
  score: 96,
  ...over,
});

describe('parseMatchQuery', () => {
  it('looks ASINs and ISBNs up exactly, and searches words otherwise', () => {
    expect(parseMatchQuery(' b003p2wo5e ')).toEqual({ asin: 'B003P2WO5E' });
    expect(parseMatchQuery('978-0-7653-2635-5')).toEqual({ isbn: '9780765326355' });
    expect(parseMatchQuery('076532635X')).toEqual({ isbn: '076532635X' });
    expect(parseMatchQuery('The Way of Kings Sanderson')).toEqual({
      q: 'The Way of Kings Sanderson',
    });
    // Ten letters that aren't an ASIN are just a word.
    expect(parseMatchQuery('Stormlight')).toEqual({ q: 'Stormlight' });
    expect(parseMatchQuery('  ')).toEqual({});
  });
});

describe('defaultRecording', () => {
  const short = recording({ id: 'abridged', runtime_min: 600 });
  const full = recording({ id: 'full', runtime_min: 2730 });

  it("takes the server's pick, else an identifier's, else the first", () => {
    expect(
      defaultRecording(candidate({ recordings: [short, full], default_recording_id: 'full' }))?.id,
    ).toBe('full');
    const c = candidate({ recordings: [full, short], recording_id: 'abridged' });
    expect(defaultRecording(c)?.id).toBe('abridged');
    expect(defaultRecording(candidate({ recordings: [short, full] }))?.id).toBe('abridged');
    expect(defaultRecording(candidate({ recordings: [] }))).toBeUndefined();
  });

  it('picks nothing for a work holding only a dramatization the book is not', () => {
    const c = { recordings: [short, full], recording_id: 'full', dramatization_only: true };
    expect(defaultRecording(candidate(c))).toBeUndefined();
    expect(defaultRecording(candidate({ ...c, dramatization_only: false }))?.id).toBe('full');
  });
});

it('displays the proposed values and submits selected writes, including a revert and unticked identifiers', () => {
  const detail = bookDetail();
  const proposal: BookMatchProposal = {
    work_id: 'work',
    recording_id: '',
    cover_url: '',
    cover: false,
    match: true,
    values: { author: 'Community author', asin: 'B0OFFER000' },
    set: { author: 'Community author', asin: 'B0OFFER000' },
    revert: ['more_series'],
  };
  const rows = compareRows(detail.fields, proposal);
  expect(
    rows
      .filter((r) => r.planned)
      .map((r) => r.field)
      .sort(),
  ).toEqual(['asin', 'author', 'more_series']);
  expect(acceptRequest(rows, new Set(['more_series', 'author']), proposal)).toEqual({
    set: { author: 'Community author' },
    revert: ['more_series'],
    source: 'community',
    match: { work_id: 'work', recording_id: '', asin: 'B0OFFER000', isbn: '' },
  });
});

describe('lengthComparison', () => {
  it('matches within 2%, is close within 5%, else says how far off', () => {
    expect(lengthComparison(2730, 163800)).toEqual({ kind: 'match' });
    // 2% is the most a match run applies unasked; up to 5% is close (server#331).
    expect(lengthComparison(2784, 163800)).toEqual({ kind: 'match' });
    expect(lengthComparison(2860, 163800)).toEqual({ kind: 'close', seconds: 7800 });
    expect(lengthComparison(2600, 163800)).toEqual({ kind: 'close', seconds: 7800 });
    expect(lengthComparison(3000, 163800)).toEqual({ kind: 'longer', seconds: 16200 });
    expect(lengthComparison(2000, 163800)).toEqual({ kind: 'shorter', seconds: 43800 });
    expect(lengthComparison(undefined, 163800)).toBeUndefined();
  });

  it('tones the score', () => {
    expect(scoreTone(90)).toBe('success');
    expect(scoreTone(60)).toBe('warning');
    expect(scoreTone(59)).toBe('outline');
  });
});

describe('community cover', () => {
  it("is the recording's cover, else the work's", () => {
    const c = candidate({ cover_url: 'https://c/work.jpg' });
    expect(communityCover(c, recording({ cover_url: 'https://c/rec.jpg' }))).toBe(
      'https://c/rec.jpg',
    );
    expect(communityCover(c, recording())).toBe('https://c/work.jpg');
    expect(communityCover(c, undefined)).toBe('https://c/work.jpg');
    expect(communityCover(candidate({ cover_url: undefined }), recording())).toBe('');
  });
});
