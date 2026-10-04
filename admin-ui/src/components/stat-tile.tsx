import { useTranslation } from 'react-i18next';
import { ArrowDownRight, ArrowUpRight, type LucideIcon } from 'lucide-react';
import { formatPercent } from '@/lib/format';
import { cn } from '@/lib/utils';

/**
 * A stat tile (STYLEGUIDE.md "Stat tile with sparkline"): an optional icon and the
 * label, the value (a skeleton while undefined), an optional sparkline, the change
 * against the previous period and a muted line of context. Text stays ink; only
 * the line takes a chart colour.
 */
export function StatTile({
  icon: Icon,
  label,
  value,
  change,
  foot,
  spark,
}: {
  icon?: LucideIcon;
  label: string;
  value: React.ReactNode | undefined;
  /** The fraction against the previous period (null: nothing to compare with). */
  change?: number | null;
  foot?: React.ReactNode;
  spark?: number[];
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  // A change under half a percent reads as none: no arrow, no colour.
  const shown = change != null && Number.isFinite(change) && Math.abs(change) >= 0.005;
  const up = (change ?? 0) >= 0;
  const Arrow = up ? ArrowUpRight : ArrowDownRight;
  return (
    <div className="flex min-w-0 flex-col gap-1.5 rounded-xl border bg-card p-3.5 md:px-5 md:py-[18px]">
      <span className="flex items-center gap-1.5 text-[12.5px] font-[550] text-muted-foreground">
        {Icon ? <Icon className="size-[15px]" aria-hidden="true" /> : null}
        {label}
      </span>
      {value === undefined ? (
        <span className="skel h-[30px] w-20" />
      ) : (
        <div className="flex items-end justify-between gap-2">
          <span className="stat-value max-md:text-2xl">{value}</span>
          {spark ? <Sparkline values={spark} /> : null}
        </div>
      )}
      {shown || foot ? (
        <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5 text-[12px]">
          {shown ? (
            <span
              className={cn(
                'inline-flex items-center gap-0.5 font-semibold tabular-nums',
                up ? 'text-success' : 'text-destructive',
              )}
            >
              <Arrow className="size-3.5" aria-hidden="true" />
              <span className="sr-only">
                {up ? t('activity.tile.up') : t('activity.tile.down')}
              </span>
              {formatPercent(Math.abs(change!), lang)}
            </span>
          ) : null}
          {foot ? <span className="min-w-0 text-muted-foreground">{foot}</span> : null}
        </div>
      ) : null}
    </div>
  );
}

/** The tile's sparkline (96 x 34: a 2px line, a 10% area, an end dot). Decorative. */
function Sparkline({ values }: { values: number[] }) {
  const w = 96;
  const h = 34;
  if (values.length < 2) return null;
  const max = Math.max(...values) || 1;
  const pts = values.map(
    (v, i) => [(i / (values.length - 1)) * (w - 4) + 2, h - 3 - (v / max) * (h - 6)] as const,
  );
  const line = pts.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)} ${y.toFixed(1)}`).join(' ');
  const [ex, ey] = pts[pts.length - 1];
  return (
    <svg width={w} height={h} viewBox={`0 0 ${w} ${h}`} aria-hidden="true" className="shrink-0">
      <path d={`${line} L${ex} ${h} L2 ${h}Z`} fill="var(--chart-1)" opacity={0.1} />
      <path d={line} fill="none" stroke="var(--chart-1)" strokeWidth={2} strokeLinejoin="round" />
      <circle cx={ex} cy={ey} r={2.5} fill="var(--chart-1)" />
    </svg>
  );
}
