import { paletteFilter, topMatches } from './palette-filter';

describe('paletteFilter', () => {
  const rescan = ['Rescan Fiction', 'Look for new, changed and moved books now', 'scan', 'Fiction'];
  const narrators = ['Narrators', 'Library › Narrators'];

  it('shows everything for an empty search', () => {
    expect(paletteFilter('x', '  ', rescan)).toBe(1);
  });

  it('needs every word as a substring, not scattered letters', () => {
    expect(paletteFilter('x', 'narrators', rescan)).toBe(0);
    expect(paletteFilter('x', 'narrators', narrators)).toBeGreaterThan(0);
    expect(
      paletteFilter('x', 'dark theme', ['Use the dark theme', 'Appearance', 'theme']),
    ).toBeGreaterThan(0);
  });

  it('ranks title prefixes above other matches', () => {
    expect(paletteFilter('x', 'narr', narrators)).toBeGreaterThan(
      paletteFilter('x', 'scan', rescan),
    );
  });
});

it('ranks a fallback entry below every real match', () => {
  const fallback = paletteFilter('fallback:books', 'dark', ['Search all books for “dark”']);
  expect(fallback).toBeGreaterThan(0);
  expect(fallback).toBeLessThan(paletteFilter('x', 'dark', ['Use the dark theme']));
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
