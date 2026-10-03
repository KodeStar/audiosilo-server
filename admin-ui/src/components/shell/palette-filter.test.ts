import { paletteFilter } from './palette-filter';

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
