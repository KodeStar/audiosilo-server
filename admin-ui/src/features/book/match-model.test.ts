import type { MatchCandidate, MatchRecording } from '@/api/types';
import { bookDetail } from '@/test/library-fixtures';
import {
  acceptRequest,
  communityCover,
  communityValues,
  compareRows,
  defaultCoverTick,
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

  it("takes the server's pick, else an identifier's, else the first", () => {
    expect(
      defaultRecording(candidate({ recordings: [short, full], default_recording_id: 'full' }))?.id,
    ).toBe('full');
    const c = candidate({ recordings: [full, short], recording_id: 'abridged' });
    expect(defaultRecording(c)?.id).toBe('abridged');
    expect(defaultRecording(candidate({ recordings: [short, full] }))?.id).toBe('abridged');
    expect(defaultRecording(candidate({ recordings: [] }))).toBeUndefined();
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
      more_series: '',
      published: '2010',
      description: 'Roshar is a world of stone and storms.',
      asin: 'B003P2WO5E',
      isbn: '',
    });
  });

  it('offers a work in several series as one main series and the rest', () => {
    const watch = 'Discworld: Ankh-Morpork City Watch';
    const c = candidate({
      series: [
        { name: 'Discworld', position: '8' },
        { name: watch, position: '1' },
        { name: 'Omnibus', position: '1-3' },
      ],
    });
    // No main series yet: the work's first.
    expect(communityValues(c, undefined)).toMatchObject({
      series: 'Discworld',
      series_index: '8',
      more_series: `[{"name":"${watch}","position":1},{"name":"Omnibus","position":0}]`,
    });
    // Filed under City Watch: that one, numbered as City Watch numbers it.
    expect(communityValues(c, undefined, 'discworld: ankh-morpork city watch')).toMatchObject({
      series: watch,
      series_index: '1',
      more_series: '[{"name":"Discworld","position":8},{"name":"Omnibus","position":0}]',
    });
  });

  it('keeps the book series together when its own series is kept', () => {
    const detail = bookDetail();
    detail.fields.series = { ...detail.fields.series, value: 'Red Planet' };
    detail.fields.series_index = { ...detail.fields.series_index, value: '' };
    const c = candidate({
      series: [
        { name: 'Discworld', position: '8' },
        { name: 'City Watch', position: '1' },
      ],
    });
    const rows = compareRows(detail.fields, c, recording());
    const ticks = defaultTicks(rows);
    ticks.delete('series');
    // Discworld's #8 isn't put beside Red Planet, nor Discworld itself (Red Planet
    // may be it, spelled otherwise); the work's other series are still added.
    const { set } = acceptRequest(rows, ticks);
    expect(set?.series_index).toBeUndefined();
    expect(set?.more_series).toBe('[{"name":"City Watch","position":1}]');
  });

  it("doesn't add the offered series beside the book's own when it is kept", () => {
    // The book's "Stormlight Archive" is most likely the offered series spelled
    // its own way: nothing to add beside it.
    const detail = bookDetail();
    detail.fields.series = { ...detail.fields.series, value: 'Stormlight Archive' };
    const rows = compareRows(detail.fields, candidate(), recording());
    const ticks = defaultTicks(rows);
    ticks.delete('series');
    const { set } = acceptRequest(rows, ticks);
    expect(set?.series_index).toBeUndefined();
    expect(set?.more_series).toBeUndefined();
  });

  it('keeps the position when only a respelling of the own series is declined', () => {
    const detail = bookDetail();
    detail.fields.series = { ...detail.fields.series, value: 'the stormlight archive' };
    detail.fields.series_index = { ...detail.fields.series_index, value: '' };
    const rows = compareRows(detail.fields, candidate(), recording());
    const ticks = defaultTicks(rows);
    ticks.delete('series');
    const { set } = acceptRequest(rows, ticks);
    expect(set?.series_index).toBe('1');
    expect(set?.more_series).toBeUndefined();
  });

  it('lists a work once per series, at its first position', () => {
    const c = candidate({
      series: [
        { name: 'Discworld', position: '8' },
        { name: 'Discworld', position: '8.5' },
      ],
    });
    expect(communityValues(c, undefined)).toMatchObject({ series_index: '8', more_series: '' });
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

  it('starts ticked only for a book without art', () => {
    expect(defaultCoverTick(false, 'https://c/rec.jpg')).toBe(true);
    expect(defaultCoverTick(true, 'https://c/rec.jpg')).toBe(false);
    expect(defaultCoverTick(false, '')).toBe(false);
  });

  it("isn't ticked when the server couldn't fetch its preview", () => {
    expect(defaultCoverTick(false, 'https://c/rec.jpg', null)).toBe(false);
    expect(defaultCoverTick(false, 'https://c/rec.jpg', 'data:image/jpeg;base64,AA')).toBe(true);
  });
});
