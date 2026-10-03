import { hashString } from './monogram';

// The procedural cover (STYLEGUIDE.md "Cover"): what a book without art looks
// like. Deterministic from `title|author`, so a book keeps its cover across
// reloads and every view agrees. There is no genre to pick a layout by, so the
// hash picks one of four (a colour field, a crime band, a grotesk rule, a framed
// sigil) and a palette from that layout's set. Pure, so it is unit-tested.

export type CoverLayout = 'field' | 'band' | 'grotesk' | 'sigil';

/** [background, accent, highlight, ink] */
export type CoverPalette = readonly [string, string, string, string];

const PALETTES: Record<CoverLayout, readonly CoverPalette[]> = {
  field: [
    ['#e3ebf5', '#1f3a8a', '#e14b4b', '#0f1a33'],
    ['#14532d', '#f5d0a8', '#f2a65a', '#f6f3ea'],
    ['#f7d6e0', '#7a1f3d', '#2b2b2b', '#3a0e1e'],
    ['#1e1b4b', '#f5b301', '#f0abfc', '#fdf6e3'],
    ['#d3ebe2', '#0f766e', '#f97316', '#0b2f2b'],
    ['#101820', '#8bd3dd', '#f582ae', '#fef6e4'],
  ],
  band: [
    ['#0b0b0c', '#f5c518', '#e11d48', '#f2f2f2'],
    ['#111827', '#ef4444', '#f9fafb', '#e5e7eb'],
    ['#f1f3f6', '#101828', '#dc2626', '#101828'],
    ['#0c1a14', '#a3e635', '#fafafa', '#e7efe9'],
  ],
  grotesk: [
    ['#fbfbfc', '#d99a06', '#111827', '#111827'],
    ['#ffd60a', '#0b0b0b', '#ffffff', '#0b0b0b'],
    ['#e4edff', '#1d4ed8', '#f43f5e', '#0f172a'],
    ['#111827', '#38bdf8', '#f472b6', '#f8fafc'],
    ['#ff4f5e', '#111111', '#ffffff', '#111111'],
  ],
  sigil: [
    ['#0e2a2b', '#c79a3b', '#e9c46a', '#f7efd8'],
    ['#2a0d14', '#d4a24c', '#e8c179', '#fbeedd'],
    ['#0f1938', '#9bb7ff', '#d9c38a', '#f1f4ff'],
    ['#16240f', '#c9a94b', '#e7d48e', '#f4f1de'],
    ['#24123d', '#e0a8ff', '#f1cf7a', '#fbf3ff'],
  ],
};

const LAYOUTS: readonly CoverLayout[] = ['field', 'band', 'grotesk', 'sigil'];

export interface CoverModel {
  layout: CoverLayout;
  palette: CoverPalette;
  /** Picks the art's variant and positions (0..1, deterministic). */
  rand: () => number;
}

/** A small seeded PRNG (mulberry32): the same seed, the same sequence. */
export function seededRandom(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function coverModel(title: string, author: string): CoverModel {
  const h = hashString(`${title}|${author}`);
  const layout = LAYOUTS[h % LAYOUTS.length];
  const set = PALETTES[layout];
  return { layout, palette: set[(h >>> 4) % set.length], rand: seededRandom(h) };
}

/** The colour a generated cover glows and tints the book hero with. */
export function coverTint(title: string, author: string): string {
  return coverModel(title, author).palette[1];
}
