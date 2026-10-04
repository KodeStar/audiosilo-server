import type { PlaybackPart } from './activity-model';

/** Series colours in the style guide's fixed order; everyone else is a recessive grey. */
export const SERIES = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)'];
export const OTHERS = 'var(--border-strong)';

/** The colour of each part of the playback donut: direct play, then each transcoded codec. */
export function playbackColor(part: PlaybackPart, index: number): string {
  if (!part.transcoded) return 'var(--chart-3)';
  if (part.key === 'other') return OTHERS;
  return ['var(--chart-4)', 'var(--chart-1)', 'var(--chart-5)'][(index - 1) % 3] ?? OTHERS;
}
