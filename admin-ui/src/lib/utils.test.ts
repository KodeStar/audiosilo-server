import { chunk, compact, fold } from './utils';

describe('utils', () => {
  it('chunks into runs of at most the size', () => {
    expect(chunk([1, 2, 3, 4, 5], 2)).toEqual([[1, 2], [3, 4], [5]]);
    expect(chunk([], 3)).toEqual([]);
  });

  it('compacts away unset values only', () => {
    expect(
      compact({ q: '', library: undefined, format: [], cover: 'no', n: 0, on: false }),
    ).toEqual({ cover: 'no', n: 0, on: false });
  });

  it('folds case and accents', () => {
    expect(fold('Brontë')).toBe('bronte');
    expect(fold('ÉMILE Zola')).toBe('emile zola');
  });
});
