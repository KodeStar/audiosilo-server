import { paletteScore, rankEntries, topMatches } from './palette-filter';

describe('paletteScore', () => {
  const rescan = ['Look for new, changed and moved books now', 'scan', 'Fiction'];
  const narrators = ['Library › Narrators'];

  it('matches everything for an empty search', () => {
    expect(paletteScore('  ', 'Rescan Fiction', rescan)).toBe(1);
  });

  it('needs every word as a substring, not scattered letters', () => {
    expect(paletteScore('narrators', 'Rescan Fiction', rescan)).toBe(0);
    expect(paletteScore('narrators', 'Narrators', narrators)).toBeGreaterThan(0);
    expect(
      paletteScore('dark theme', 'Use the dark theme', ['Appearance', 'theme']),
    ).toBeGreaterThan(0);
  });

  it('ignores case and accents', () => {
    expect(paletteScore('BRONTE', 'Charlotte Brontë')).toBeGreaterThan(0);
  });

  it('ranks title prefixes above other matches', () => {
    expect(paletteScore('narr', 'Narrators', narrators)).toBeGreaterThan(
      paletteScore('scan', 'Rescan Fiction', rescan),
    );
  });
});

describe('rankEntries', () => {
  const entries = [
    { title: 'Rescan Fiction', subtitle: 'Look for new books', keywords: ['scan'] },
    { title: 'Scan settings', subtitle: 'Server' },
    { title: 'Narrators', subtitle: 'Library › Narrators' },
  ];

  it('keeps the matches, best first, ties in order', () => {
    expect(rankEntries(entries, 'scan').map((e) => e.title)).toEqual([
      'Scan settings',
      'Rescan Fiction',
    ]);
    expect(rankEntries(entries, 'library').map((e) => e.title)).toEqual(['Narrators']);
  });

  it('keeps everything, in order, for a blank search', () => {
    expect(rankEntries(entries, '')).toEqual(entries);
  });
});

describe('topMatches', () => {
  const names = ['Brandon Sanderson', 'Sanderson, Brandon', 'Andy Weir', 'Mel Hudson'];
  const id = (s: string) => s;

  it('keeps names holding every word, prefixes first', () => {
    expect(topMatches(names, id, 'sanderson', 5)).toEqual([
      'Sanderson, Brandon',
      'Brandon Sanderson',
    ]);
    expect(topMatches(names, id, 'brandon sand', 5)).toEqual([
      'Brandon Sanderson',
      'Sanderson, Brandon',
    ]);
  });

  it('caps the list and returns nothing for an empty search', () => {
    expect(topMatches(names, id, 'n', 2)).toHaveLength(2);
    expect(topMatches(names, id, '  ', 5)).toEqual([]);
  });
});
