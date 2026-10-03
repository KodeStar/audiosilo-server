import { cn } from '@/lib/utils';

/**
 * The thin brand progress bar. With `label` it is a progressbar for assistive
 * tech (0-100); without, it's decorative and the percentage is said in text
 * next to it.
 */
export function ProgressBar({
  fraction,
  label,
  className,
}: {
  /** 0..1, or undefined while the total isn't known (an indeterminate pulse). */
  fraction: number | undefined;
  label?: string;
  className?: string;
}) {
  const pct =
    fraction === undefined ? undefined : Math.round(Math.min(1, Math.max(0, fraction)) * 100);
  return (
    <div
      className={cn('progress-track', className)}
      {...(label
        ? {
            role: 'progressbar',
            'aria-label': label,
            'aria-valuemin': 0,
            'aria-valuemax': 100,
            'aria-valuenow': pct,
          }
        : { 'aria-hidden': true })}
    >
      <i
        className={cn(pct === undefined && 'w-1/4 animate-pulse')}
        style={pct === undefined ? undefined : { width: `${pct}%` }}
      />
    </div>
  );
}
