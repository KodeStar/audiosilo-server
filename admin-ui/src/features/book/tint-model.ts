import type { CoverPalette } from '@/lib/cover-model';

// The book hero's tint (STYLEGUIDE.md "Cover": covers glow): two colours from
// the cover for the hero's radial washes (--tint1/--tint2) and the cover's
// glow (--glow). Real art is sampled from a small canvas (the component does
// the drawing); a generated cover's palette is used as is. Pure, so tested.

export interface HeroTint {
  tint1: string;
  tint2: string;
  glow: string;
}

const hex = (r: number, g: number, b: number) =>
  `#${[r, g, b]
    .map((v) =>
      Math.round(Math.min(255, Math.max(0, v)))
        .toString(16)
        .padStart(2, '0'),
    )
    .join('')}`;

/**
 * From RGBA pixel data: the most vivid colour (saturation times brightness,
 * ignoring near-black) and the average. A greyscale cover has no vivid colour,
 * so both are the average. Undefined when no pixel is opaque.
 */
export function tintFromPixels(data: ArrayLike<number>): HeroTint | undefined {
  let n = 0;
  let sr = 0;
  let sg = 0;
  let sb = 0;
  let best: [number, number, number] | undefined;
  let bestScore = 0;
  for (let i = 0; i + 3 < data.length; i += 4) {
    if (data[i + 3] < 128) continue;
    const r = data[i];
    const g = data[i + 1];
    const b = data[i + 2];
    n++;
    sr += r;
    sg += g;
    sb += b;
    const max = Math.max(r, g, b);
    const min = Math.min(r, g, b);
    const v = max / 255;
    if (v < 0.15) continue;
    const score = ((max - min) / max) * v;
    if (score > bestScore) {
      bestScore = score;
      best = [r, g, b];
    }
  }
  if (!n) return undefined;
  const average = hex(sr / n, sg / n, sb / n);
  const vivid = best && bestScore >= 0.12 ? hex(...best) : average;
  return { tint1: vivid, tint2: average, glow: `${vivid}66` };
}

/** Relative luminance of a #rrggbb colour (0 black .. 1 white), roughly. */
function lightness(color: string): number {
  const m = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(color);
  if (!m) return 0;
  const [r, g, b] = m.slice(1).map((x) => parseInt(x, 16) / 255);
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/**
 * A generated cover's tint: its accent and its background, or its highlight
 * when the background is near white (a white wash reads as no tint).
 */
export function tintFromPalette(palette: CoverPalette): HeroTint {
  const [bg, accent, highlight] = palette;
  return { tint1: accent, tint2: lightness(bg) > 0.92 ? highlight : bg, glow: `${accent}66` };
}
