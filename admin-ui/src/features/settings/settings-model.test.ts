import { outOfRange } from './settings-model';

describe('outOfRange', () => {
  it('refuses what is not a whole number inside the range, and blank', () => {
    for (const bad of ['0', '51', '-3', '2.5', 'six', '1e3', '', '  ']) {
      expect(outOfRange(bad, 1, 50)).toBe(true);
    }
  });

  it('accepts whole numbers inside the range', () => {
    for (const ok of ['1', '50', ' 6 ']) {
      expect(outOfRange(ok, 1, 50)).toBe(false);
    }
  });
});
