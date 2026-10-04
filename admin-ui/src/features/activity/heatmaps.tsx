import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { ActivityDay } from '@/api/types';
import { formatDay, formatHours } from '@/lib/format';
import { cn } from '@/lib/utils';
import {
  busiestSlot,
  calendarGrid,
  seqLevel,
  weekdayName,
  type CalendarCell,
} from './activity-model';

// Hand-built heatmaps on the one-hue sequential scale (STYLEGUIDE.md "Charts":
// magnitude uses --seq-0..5, never a rainbow). Each says its numbers in text
// too: the readout under the calendar, the busiest hour under the week grid.

/** Literal classes, so Tailwind sees every shade. */
const SEQ = ['bg-seq-0', 'bg-seq-1', 'bg-seq-2', 'bg-seq-3', 'bg-seq-4', 'bg-seq-5'];

function Scale() {
  const { t } = useTranslation();
  return (
    <span className="inline-flex items-center gap-1 text-subtle-foreground" aria-hidden="true">
      {t('activity.heat.less')}
      {SEQ.map((c) => (
        <i key={c} className={cn('size-[11px] rounded-[3px]', c)} />
      ))}
      {t('activity.heat.more')}
    </span>
  );
}

/** The period day by day, a column a week (Monday on top), with a readout of the hovered day. */
export function YearCalendar({
  days,
  value,
  label,
}: {
  days: ActivityDay[];
  /** Seconds to plot for a day (everyone's by default). */
  value?: (d: ActivityDay) => number;
  label: string;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const { weeks, months } = calendarGrid(days, value);
  const [hover, setHover] = useState<CalendarCell | null>(null);
  return (
    <div className="flex flex-col gap-2.5">
      <div className="hscroll">
        <div
          className="grid min-w-[640px] grid-flow-col grid-rows-[14px_repeat(7,1fr)] gap-[3px]"
          style={{ gridTemplateColumns: `repeat(${weeks.length}, minmax(0, 1fr))` }}
          role="img"
          aria-label={label}
          onMouseLeave={() => setHover(null)}
        >
          {weeks.map((week, w) => (
            <div key={w} className="contents">
              <span className="overflow-visible text-[11px] leading-[14px] whitespace-nowrap text-muted-foreground">
                {months.find((m) => m.week === w)
                  ? formatDay(months.find((m) => m.week === w)!.date, lang, { month: 'short' })
                  : ''}
              </span>
              {Array.from({ length: 7 }, (_, d) => {
                const c = week[d];
                return c ? (
                  <i
                    key={d}
                    className={cn('aspect-square rounded-[3px]', SEQ[c.level])}
                    onMouseEnter={() => setHover(c)}
                  />
                ) : (
                  <i key={d} />
                );
              })}
            </div>
          ))}
        </div>
      </div>
      <div className="flex flex-wrap items-center justify-between gap-2 text-[12px]">
        <span className="text-muted-foreground" aria-live="polite">
          {hover
            ? t('activity.heat.day', {
                hours: formatHours(hover.listened, lang),
                day: formatDay(hover.date, lang, {
                  weekday: 'long',
                  day: 'numeric',
                  month: 'long',
                }),
              })
            : t('activity.heat.hint')}
        </span>
        <Scale />
      </div>
    </div>
  );
}

/** Listening by hour of day and weekday, with the busiest hour said in words. */
export function HourWeekdayHeat({ grid }: { grid: number[][] }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const max = Math.max(0, ...grid.flat());
  const busiest = busiestSlot(grid);
  return (
    <div className="flex flex-col gap-2.5">
      <div className="hscroll">
        <div
          className="grid min-w-[520px] grid-cols-[36px_repeat(24,minmax(0,1fr))] items-center gap-[3px]"
          role="img"
          aria-label={t('activity.when.aria')}
        >
          {grid.map((row, d) => (
            <div key={d} className="contents">
              <span className="text-[11px] text-muted-foreground">{weekdayName(d, lang)}</span>
              {row.map((v, h) => (
                <i
                  key={h}
                  className={cn('aspect-[1.3] rounded-[3px]', SEQ[seqLevel(v, max)])}
                  title={`${weekdayName(d, lang)} ${String(h).padStart(2, '0')}:00 · ${formatHours(v, lang)}`}
                />
              ))}
            </div>
          ))}
          <span />
          {Array.from({ length: 24 }, (_, h) => (
            <span key={h} className="text-center text-[10px] text-subtle-foreground tabular-nums">
              {h % 6 === 0 ? String(h).padStart(2, '0') : ''}
            </span>
          ))}
        </div>
      </div>
      <div className="flex flex-wrap items-center justify-between gap-2 text-[12px]">
        <span className="text-muted-foreground">
          {busiest
            ? t('activity.when.busiest', {
                slot: `${weekdayName(busiest.weekday, lang, 'long')} ${String(busiest.hour).padStart(2, '0')}:00`,
              })
            : t('activity.when.none')}
        </span>
        <Scale />
      </div>
    </div>
  );
}
