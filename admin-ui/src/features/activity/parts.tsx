import { cn } from '@/lib/utils';

/** A horizontal bar for ranked lists (top books, people, apps): its share of the largest. */
export function ShareBar({
  fraction,
  color = 'var(--chart-2)',
  className,
}: {
  fraction: number;
  color?: string;
  className?: string;
}) {
  return (
    <div className={cn('h-2 overflow-hidden rounded-full bg-muted', className)} aria-hidden="true">
      <i
        className="block h-full rounded-full"
        style={{
          width: fraction > 0 ? `${Math.max(2, Math.min(1, fraction) * 100)}%` : 0,
          background: color,
        }}
      />
    </div>
  );
}
