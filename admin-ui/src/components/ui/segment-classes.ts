import { cn } from '@/lib/utils';

/**
 * The style guide's segmented control (muted track, raised active segment). The
 * sub bar renders its sections as links with these classes; in-page filters
 * use <SegmentedControl>.
 */
export const segmentTrack = 'inline-flex gap-0.5 rounded-[11px] border bg-muted p-[3px]';
export function segmentClass(active: boolean) {
  return cn(
    'inline-flex h-[30px] items-center rounded-sm px-3 text-[13px] font-[550] whitespace-nowrap text-muted-foreground tabular-nums transition-colors duration-(--dur-1) hover:text-foreground',
    active &&
      'bg-card text-foreground shadow-[0_1px_2px_rgb(18_28_54/0.08),0_0_0_1px_var(--border)]',
  );
}
