import type { MatchCandidate, MatchRecording } from '@/api/types';
import { bookDetail } from '@/test/library-fixtures';
import {
  acceptRequest,
  communityValues,
  compareRows,
  defaultRecording,
  defaultTicks,
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

  it('prefers the recording an identifier hit', () => {
    const c = candidate({ recordings: [full, short], recording_id: 'abridged' });
    expect(defaultRecording(c, 163800)?.id).toBe('abridged');
  });

  it('else takes the runtime closest to the files', () => {
    expect(defaultRecording(candidate({ recordings: [short, full] }), 163800)?.id).toBe('full');
    expect(defaultRecording(candidate({ recordings: [] }), 100)).toBeUndefined();
  });
});

describe('compare', () => {
  it('maps a candidate onto the fields', () => {
    expect(communityValues(candidate(), recording())).toEqual({
      title: 'The Way of Kings',
      author: 'Brandon Sanderson',
      narrator: 'Michael Kramer, Kate Reading',
      series: 'The Stormlight Archive',
      series_index: '1',
      published: '2010',
      description: 'Roshar is a world of stone and storms.',
      asin: 'B003P2WO5E',
      isbn: '',
    });
  });

  it('drops values the server would refuse (a range as a series position)', () => {
    const c = candidate({ series: [{ name: 'Mistborn', position: '1-3' }] });
    expect(communityValues(c, undefined).series_index).toBe('');
    expect(communityValues(c, undefined).narrator).toBe('');
  });

  it('offers what differs, and leaves the admin’s own edits unticked', () => {
    const detail = bookDetail();
    detail.fields.narrator = { ...detail.fields.narrator, source: 'edited', locked: true };
    const rows = compareRows(detail.fields, candidate(), recording());
    const offered = rows.filter((r) => r.offered).map((r) => r.field);
    expect(offered).toEqual(['narrator', 'published', 'description']);
    expect(rows.find((r) => r.field === 'title')?.same).toBe(true);
    expect(rows.find((r) => r.field === 'isbn')).toMatchObject({ same: false, offered: false });
    const ticks = defaultTicks(rows);
    expect([...ticks]).toEqual(['published', 'description']);
    expect(acceptRequest(rows, ticks)).toEqual({
      set: { published: '2010', description: 'Roshar is a world of stone and storms.' },
      source: 'community',
    });
  });
});

describe('lengthComparison', () => {
  it('matches within 5%, else says how far off', () => {
    expect(lengthComparison(2730, 163800)).toEqual({ kind: 'match' });
    expect(lengthComparison(2860, 163800)).toEqual({ kind: 'match' });
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
