import { ResponsiveContainer } from 'recharts';
import { cn } from '@/lib/utils';

// shadcn's chart, minus ChartStyle: shadcn injects each chart's colours in a
// <style>, which the CSP blocks. Here series colours are CSS variables passed
// straight to Recharts (fill="var(--chart-1)"), and the chart's look (dashed
// recessive grid, muted tabular axis text, the hover cursor) lives in
// globals.css under .chart. Recharts itself only writes styles through the
// CSSOM (the style prop, element.style), which the CSP allows.

/**
 * A chart that fills its container's width at a fixed height. `label` names it for
 * assistive technology; the numbers it shows are also said in text nearby (a
 * tooltip, a legend, a total).
 */
export function ChartContainer({
  label,
  height,
  className,
  children,
}: {
  label: string;
  height: number;
  className?: string;
  children: React.ReactElement;
}) {
  return (
    <div className={cn('chart w-full min-w-0', className)} role="img" aria-label={label}>
      {/* initialDimension renders before the first measure (and in tests, which can't measure). */}
      <ResponsiveContainer width="100%" height={height} initialDimension={{ width: 600, height }}>
        {children}
      </ResponsiveContainer>
    </div>
  );
}

/** A chart tooltip (STYLEGUIDE.md "Charts": a --primary pill): a title and one row per series. */
export function ChartTip({
  title,
  rows,
}: {
  title: React.ReactNode;
  rows: { key: string; color?: string; label?: React.ReactNode; value: React.ReactNode }[];
}) {
  return (
    <div className="chart-tip">
      <div className="font-semibold">{title}</div>
      {rows.map((r) => (
        <div key={r.key} className="flex items-center gap-2">
          {r.color ? (
            <i className="size-2 shrink-0 rounded-[3px]" style={{ background: r.color }} />
          ) : null}
          {r.label ? <span className="min-w-0 flex-1 truncate opacity-80">{r.label}</span> : null}
          <b className="tabular-nums">{r.value}</b>
        </div>
      ))}
    </div>
  );
}

/** A legend under a chart: a colour mark beside each series' name (text never takes the colour). */
export function ChartLegend({
  items,
  className,
}: {
  items: { key: string; color: string; label: React.ReactNode }[];
  className?: string;
}) {
  return (
    <ul
      className={cn(
        'flex flex-wrap gap-x-4 gap-y-1.5 text-[12.5px] text-muted-foreground',
        className,
      )}
    >
      {items.map((i) => (
        <li key={i.key} className="inline-flex min-w-0 items-center gap-1.5">
          <i className="size-2.5 shrink-0 rounded-[3px]" style={{ background: i.color }} />
          <span className="truncate">{i.label}</span>
        </li>
      ))}
    </ul>
  );
}
