import { useTranslation } from 'react-i18next';
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Pie,
  PieChart,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import type { ActivityDay, GrowthPoint } from '@/api/types';
import { ChartContainer, ChartLegend, ChartTip } from '@/components/ui/chart';
import { formatDay, formatHours, formatNumber, formatPercent } from '@/lib/format';
import { hoursBars, niceTicks, topListeners, type PlaybackPart } from './activity-model';
import { OTHERS, SERIES, playbackColor } from './chart-colors';

// The Activity charts (Recharts through ChartContainer; STYLEGUIDE.md "Charts":
// categorical --chart-1..5 in order, one y-axis, bars at most 18px with 4px
// rounded ends, a --primary tooltip). Heatmaps are hand-built (heatmaps.tsx).

const axis = { axisLine: false, tickLine: false } as const;
const hoursTick = (lang: string) => (h: number) => formatHours(h * 3600, lang);

/** Listening hours per day (a week per bar over a year), stacked by the biggest listeners. */
export function HoursChart({
  days,
  names,
  height = 240,
}: {
  days: ActivityDay[];
  /** Display names by user id. */
  names: Map<number, string>;
  height?: number;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const listeners = topListeners(days);
  const bars = hoursBars(days, listeners);
  const weekly = bars.length < days.length;
  const short = days.length <= 31;
  const tick = (from: string) =>
    formatDay(
      from,
      lang,
      weekly ? { month: 'short' } : short ? { day: 'numeric' } : { month: 'short', day: 'numeric' },
    );
  const series = [
    ...listeners.map((id, i) => ({
      key: `u${id}`,
      color: SERIES[i],
      label: names.get(id) ?? t('activity.someone'),
    })),
    { key: 'others', color: OTHERS, label: t('activity.chart.others') },
  ];
  const ticks = niceTicks(Math.max(0, ...bars.map((b) => b.total)));
  const shown = series.filter((_, i) => bars.some((b) => b.series[i] > 0));
  return (
    <div className="flex flex-col gap-3">
      <ChartContainer label={t('activity.hours.aria')} height={height}>
        <BarChart
          data={bars}
          margin={{ top: 8, right: 4, bottom: 0, left: 0 }}
          barCategoryGap="22%"
        >
          <CartesianGrid vertical={false} />
          <XAxis
            dataKey="from"
            tickFormatter={tick}
            interval="preserveStartEnd"
            minTickGap={14}
            {...axis}
          />
          <YAxis
            width={44}
            tickFormatter={hoursTick(lang)}
            ticks={ticks}
            domain={[0, ticks[ticks.length - 1]]}
            {...axis}
          />
          <Tooltip
            cursor
            content={({ active, payload }) => {
              const bar = active ? (payload?.[0]?.payload as (typeof bars)[number]) : undefined;
              if (!bar) return null;
              const title =
                bar.from === bar.to
                  ? formatDay(bar.from, lang, { weekday: 'short', day: 'numeric', month: 'short' })
                  : t('activity.hours.week', {
                      from: formatDay(bar.from, lang, { day: 'numeric', month: 'short' }),
                      to: formatDay(bar.to, lang, { day: 'numeric', month: 'short' }),
                    });
              return (
                <ChartTip
                  title={`${title} · ${formatHours(bar.total * 3600, lang)}`}
                  rows={series
                    .map((s, i) => ({ ...s, value: bar.series[i] }))
                    .filter((s) => s.value > 0)
                    .map((s) => ({ ...s, value: formatHours(s.value * 3600, lang) }))}
                />
              );
            }}
          />
          {series.map((s, i) => (
            <Bar
              key={s.key}
              dataKey={(b: (typeof bars)[number]) => b.series[i]}
              name={String(s.label)}
              stackId="hours"
              fill={s.color}
              maxBarSize={18}
              radius={[3, 3, 0, 0]}
              isAnimationActive={false}
            />
          ))}
        </BarChart>
      </ChartContainer>
      {shown.length > 1 ? <ChartLegend items={shown} /> : null}
    </div>
  );
}

/** Books in the collection over the period. */
export function GrowthChart({ points, height = 180 }: { points: GrowthPoint[]; height?: number }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const span =
    points.length > 1 ? Date.parse(points[points.length - 1].date) - Date.parse(points[0].date) : 0;
  const long = span > 120 * 86_400_000;
  const fmt = (d: string) =>
    formatDay(d, lang, long ? { month: 'short' } : { month: 'short', day: 'numeric' });
  return (
    <ChartContainer label={t('activity.growth.aria')} height={height}>
      <AreaChart data={points} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
        <CartesianGrid vertical={false} />
        <XAxis dataKey="date" tickFormatter={fmt} minTickGap={18} {...axis} />
        <YAxis
          width={48}
          tickFormatter={(n: number) => formatNumber(n, lang)}
          domain={['auto', 'auto']}
          allowDecimals={false}
          tickCount={4}
          {...axis}
        />
        <Tooltip
          content={({ active, payload }) => {
            const p = active ? (payload?.[0]?.payload as GrowthPoint | undefined) : undefined;
            return p ? (
              <ChartTip
                title={formatDay(p.date, lang, { day: 'numeric', month: 'short', year: 'numeric' })}
                rows={[
                  {
                    key: 'books',
                    value: t('activity.growth.books', {
                      count: p.books,
                      formatted: formatNumber(p.books, lang),
                    }),
                  },
                ]}
              />
            ) : null;
          }}
        />
        <Area
          type="monotone"
          dataKey="books"
          stroke="var(--chart-2)"
          strokeWidth={2}
          fill="var(--chart-2)"
          fillOpacity={0.1}
          isAnimationActive={false}
          dot={false}
          activeDot={{ r: 4.5, stroke: 'var(--card)', strokeWidth: 2 }}
        />
      </AreaChart>
    </ChartContainer>
  );
}

/** Listening direct vs through the transcoder, as a donut. */
export function PlaybackDonut({
  parts,
  label,
  size = 124,
}: {
  parts: PlaybackPart[];
  label: (p: PlaybackPart) => string;
  size?: number;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  return (
    <div className="shrink-0" style={{ width: size }}>
      <ChartContainer label={t('activity.playback.aria')} height={size}>
        <PieChart>
          <Pie
            data={parts}
            dataKey="listened"
            nameKey="key"
            innerRadius="68%"
            outerRadius="100%"
            paddingAngle={parts.length > 1 ? 2 : 0}
            stroke="none"
            isAnimationActive={false}
          >
            {parts.map((p, i) => (
              <Cell key={p.key} fill={playbackColor(p, i)} />
            ))}
          </Pie>
          <Tooltip
            content={({ active, payload }) => {
              const p = active ? (payload?.[0]?.payload as PlaybackPart | undefined) : undefined;
              return p ? (
                <ChartTip
                  title={label(p)}
                  rows={[{ key: 'share', value: formatPercent(p.share, lang) }]}
                />
              ) : null;
            }}
          />
        </PieChart>
      </ChartContainer>
    </div>
  );
}

/** A person's listening per month of a year. */
export function MonthBars({ months, year }: { months: number[]; year: number }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const data = months.map((s, m) => ({
    month: `${year}-${String(m + 1).padStart(2, '0')}-01`,
    hours: s / 3600,
  }));
  return (
    <ChartContainer label={t('user.year.monthsAria')} height={96}>
      <BarChart data={data} margin={{ top: 4, right: 0, bottom: 0, left: 0 }}>
        <XAxis
          dataKey="month"
          tickFormatter={(d: string) => formatDay(d, lang, { month: 'narrow' })}
          interval={0}
          {...axis}
        />
        <Tooltip
          cursor={false}
          content={({ active, payload }) => {
            const p = active ? (payload?.[0]?.payload as (typeof data)[number]) : undefined;
            return p ? (
              <ChartTip
                title={formatDay(p.month, lang, { month: 'long' })}
                rows={[{ key: 'hours', value: formatHours(p.hours * 3600, lang) }]}
              />
            ) : null;
          }}
        />
        <Bar
          dataKey="hours"
          fill="var(--brand)"
          maxBarSize={18}
          radius={[4, 4, 0, 0]}
          minPointSize={(v) => ((v ?? 0) > 0 ? 2 : 0)}
          isAnimationActive={false}
        />
      </BarChart>
    </ChartContainer>
  );
}

/** A stat tile's sparkline (96 x 34: a 2px line, a 10% area, an end dot). Decorative. */
export function Sparkline({ values, color }: { values: number[]; color: string }) {
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
      <path d={`${line} L${ex} ${h} L2 ${h}Z`} fill={color} opacity={0.1} />
      <path d={line} fill="none" stroke={color} strokeWidth={2} strokeLinejoin="round" />
      <circle cx={ex} cy={ey} r={2.5} fill={color} />
    </svg>
  );
}
