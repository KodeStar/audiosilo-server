import { useTranslation } from 'react-i18next';
import { ArrowDownRight, ArrowUpRight } from 'lucide-react';
import { formatPercent } from '@/lib/format';
import { cn } from '@/lib/utils';
import { Sparkline } from './charts';

// Small pieces the Activity screens share: the stat tile, a ring, a ranked bar.

/**
 * A stat tile (STYLEGUIDE.md "Stat tile with sparkline"): label, value, an optional
 * sparkline, the change against the previous period and a muted line of context.
 * Text stays ink; only the line takes a chart colour.
 */
export function StatTile({
  label,
  value,
  change,
  foot,
  spark,
  color = 'var(--chart-1)',
}: {
  label: string;
  value: React.ReactNode;
  /** The fraction against the previous period (null: nothing to compare with). */
  change?: number | null;
  foot?: React.ReactNode;
  spark?: number[];
  color?: string;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const up = (change ?? 0) >= 0;
  // A change under half a percent reads as none: no arrow, no colour.
  const flat = Math.abs(change ?? 0) < 0.005;
  const Arrow = up ? ArrowUpRight : ArrowDownRight;
  return (
    <div className="flex min-w-0 flex-col gap-1.5 rounded-xl border bg-card p-3.5 md:px-5 md:py-[18px]">
      <span className="text-[12.5px] font-[550] text-muted-foreground">{label}</span>
      <div className="flex items-end justify-between gap-2">
        <span className="stat-value max-md:text-2xl">{value}</span>
        {spark ? <Sparkline values={spark} color={color} /> : null}
      </div>
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5 text-[12px]">
        {change !== undefined && change !== null && Number.isFinite(change) && !flat ? (
          <span
            className={cn(
              'inline-flex items-center gap-0.5 font-semibold tabular-nums',
              up ? 'text-success' : 'text-destructive',
            )}
          >
            <Arrow className="size-3.5" aria-hidden="true" />
            <span className="sr-only">{up ? t('activity.tile.up') : t('activity.tile.down')}</span>
            {formatPercent(Math.abs(change), lang)}
          </span>
        ) : null}
        {foot ? <span className="min-w-0 text-muted-foreground">{foot}</span> : null}
      </div>
    </div>
  );
}

/** A progress ring around a percentage (coverage). Decorative: the number sits beside it. */
export function Ring({
  fraction,
  size = 64,
  stroke = 6,
  color = 'var(--chart-5)',
}: {
  fraction: number;
  size?: number;
  stroke?: number;
  color?: string;
}) {
  const r = (size - stroke) / 2;
  const c = 2 * Math.PI * r;
  const f = Math.min(1, Math.max(0, fraction));
  return (
    <svg
      width={size}
      height={size}
      viewBox={`0 0 ${size} ${size}`}
      className="-rotate-90"
      aria-hidden="true"
    >
      <circle
        cx={size / 2}
        cy={size / 2}
        r={r}
        fill="none"
        stroke="var(--border)"
        strokeWidth={stroke}
      />
      {f > 0 ? (
        <circle
          cx={size / 2}
          cy={size / 2}
          r={r}
          fill="none"
          stroke={color}
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={`${c * f} ${c}`}
        />
      ) : null}
    </svg>
  );
}

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
