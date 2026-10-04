import { coverModel, seededRandom } from './cover-model';

describe('coverModel', () => {
  it('is deterministic for a title and author', () => {
    const a = coverModel('The Way of Kings', 'Brandon Sanderson');
    const b = coverModel('The Way of Kings', 'Brandon Sanderson');
    expect(a.layout).toBe(b.layout);
    expect(a.palette).toBe(b.palette);
    expect([a.rand(), a.rand()]).toEqual([b.rand(), b.rand()]);
  });

  it('spreads books over every layout', () => {
    const layouts = new Set(
      Array.from({ length: 60 }, (_, i) => coverModel(`Book ${i}`, 'Someone').layout),
    );
    expect(layouts).toEqual(new Set(['field', 'band', 'grotesk', 'sigil']));
  });
});

describe('seededRandom', () => {
  it('stays within 0..1 and repeats for a seed', () => {
    const r = seededRandom(42);
    const xs = Array.from({ length: 100 }, r);
    expect(xs.every((x) => x >= 0 && x < 1)).toBe(true);
    expect(seededRandom(42)()).toBe(xs[0]);
  });
});
