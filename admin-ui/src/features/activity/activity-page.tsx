import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { BarChart3, Clock, FileWarning } from 'lucide-react';
import { useActivity, useListeningDays, useServerInfo } from '@/api/hooks';
import { ACTIVITY_RANGES, type Activity, type ActivityRange } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Notice } from '@/components/notice';
import { Ring } from '@/components/ring';
import { StatTile } from '@/components/stat-tile';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Card, CardHeader } from '@/components/ui/card';
import { ChartLegend } from '@/components/ui/chart';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { bookRoute } from '@/lib/book-route';
import {
  counted,
  formatBytes,
  formatDateTime,
  formatDuration,
  formatHours,
  formatNumber,
  formatPercent,
  formatRelative,
} from '@/lib/format';
import {
  DAILY_BARS_MAX,
  change,
  clientRows,
  finishRate,
  playbackParts,
  type PlaybackPart,
} from './activity-model';
import { OTHERS, SERIES, playbackColor } from './chart-colors';
import { GrowthChart, HoursChart, PlaybackDonut } from './charts';
import { useClientName } from './use-client-name';
import { HourWeekdayHeat, YearCalendar } from './heatmaps';
import { ShareBar } from './parts';

/** Activity > Overview: listening over a period, completion, playback, apps and the collection. */
export function ActivityPage() {
  const { t } = useTranslation();
  const search = useSearch({ strict: false }) as { range?: ActivityRange };
  const navigate = useNavigate();
  const range: ActivityRange = search.range ?? '30d';
  // The previous period stays on screen while the next one loads.
  const activity = useActivity(range, true);
  const setRange = (r: ActivityRange) =>
    void navigate({ to: '.', search: r === '30d' ? {} : { range: r }, replace: true });

  return (
    <Page>
      <PageHead title={t('activity.title')} description={t('activity.description')} />
      <div className="mb-5 flex flex-wrap items-center justify-between gap-3">
        <SegmentedControl
          label={t('activity.range.label')}
          value={range}
          onChange={setRange}
          options={ACTIVITY_RANGES.map((r) => ({ value: r, label: t(`activity.range.${r}`) }))}
        />
        {activity.data ? (
          <span className="text-[13px] text-muted-foreground">
            {t('activity.scope', { zone: activity.data.timezone })}
          </span>
        ) : null}
      </div>
      {activity.isError ? (
        <QueryError
          title={t('activity.error')}
          error={activity.error}
          onRetry={() => void activity.refetch()}
        />
      ) : !activity.data ? (
        <div className="flex flex-col gap-4" role="status" aria-label={t('common.loading')}>
          <div className="grid grid-cols-2 gap-2.5 md:gap-4 xl:grid-cols-4">
            {[0, 1, 2, 3].map((i) => (
              <div key={i} className="skel h-[112px] rounded-xl" />
            ))}
          </div>
          <div className="skel h-[300px] rounded-xl" />
        </div>
      ) : (
        <ActivityView a={activity.data} />
      )}
    </Page>
  );
}

function ActivityView({ a }: { a: Activity }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  // The chart stacks the top four listeners, who are always among the top people.
  const names = new Map(a.top_users.map((u) => [u.user_id, u.username]));
  const listened = a.totals.listened > 0 || a.totals.sessions > 0;
  const rate = finishRate(a.funnel);
  const spark = a.days.slice(-30).map((d) => d.listened);

  return (
    <div className="flex flex-col gap-5">
      <div className="grid grid-cols-2 gap-2.5 md:gap-4 xl:grid-cols-4">
        <StatTile
          label={t('activity.tile.hours')}
          value={formatHours(a.totals.listened, lang)}
          change={change(a.totals.listened, a.previous.listened)}
          spark={spark}
          foot={t('activity.tile.listeners', counted(a.totals.listeners, lang))}
        />
        <StatTile
          label={t('activity.tile.sessions')}
          value={formatNumber(a.totals.sessions, lang)}
          change={change(a.totals.sessions, a.previous.sessions)}
          foot={
            a.totals.sessions
              ? t('activity.tile.average', {
                  time: formatDuration(a.totals.listened / a.totals.sessions, lang),
                })
              : undefined
          }
        />
        <StatTile
          label={t('activity.tile.peak')}
          value={formatNumber(a.peak_concurrent.streams, lang)}
          foot={a.peak_concurrent.at ? formatDateTime(a.peak_concurrent.at, lang) : undefined}
        />
        <StatTile
          label={t('activity.tile.finished')}
          value={formatNumber(a.totals.finished, lang)}
          change={change(a.totals.finished, a.previous.finished)}
          foot={
            rate === null
              ? undefined
              : t('activity.tile.finishRate', {
                  rate: formatPercent(rate, lang),
                  finished: formatNumber(a.funnel.finished, lang),
                  started: formatNumber(a.funnel.started, lang),
                })
          }
        />
      </div>

      {listened ? (
        <>
          <Card aria-labelledby="hours-title">
            <CardHeader
              titleId="hours-title"
              title={
                a.days.length > DAILY_BARS_MAX
                  ? t('activity.hours.titleWeek')
                  : t('activity.hours.titleDay')
              }
              action={
                <span className="text-muted-foreground tabular-nums">
                  {t('activity.hours.total', { hours: formatHours(a.totals.listened, lang) })}
                </span>
              }
            />
            <div className="p-5">
              <HoursChart days={a.days} names={names} />
            </div>
          </Card>
          <div className="grid gap-4 xl:grid-cols-2">
            <YearCard current={a} />
            <WhenCard a={a} />
          </div>
          <div className="grid items-start gap-4 xl:grid-cols-2">
            <TopBooks a={a} />
            <div className="flex min-w-0 flex-col gap-4">
              <TopPeople a={a} />
              <MostHeard a={a} />
              <Inactive a={a} />
            </div>
          </div>
          <div className="grid items-start gap-4 lg:grid-cols-2 xl:grid-cols-3">
            <FunnelCard a={a} />
            <PlaybackCard a={a} />
            <AppsCard a={a} />
          </div>
        </>
      ) : (
        <>
          <EmptyState
            icon={BarChart3}
            title={t('activity.empty.title')}
            body={t('activity.empty.body')}
          />
          <Inactive a={a} />
        </>
      )}
      <div className="grid items-start gap-4 xl:grid-cols-2">
        <Card aria-labelledby="growth-title">
          <CardHeader
            titleId="growth-title"
            title={t('activity.growth.title')}
            description={t('activity.growth.description')}
          />
          <div className="p-5">
            {a.growth.length > 1 ? (
              <GrowthChart points={a.growth} />
            ) : (
              <p className="text-center text-muted-foreground">{t('activity.growth.none')}</p>
            )}
          </div>
        </Card>
        <StorageCard a={a} />
      </div>
    </div>
  );
}

/** The last year day by day: the 1y period's own days, else the slim days-only query. */
function YearCard({ current }: { current: Activity }) {
  const { t } = useTranslation();
  const own = current.range === '1y' ? current.days : undefined;
  const year = useListeningDays('1y', 0, !own);
  const days = own ?? year.data;
  return (
    <Card aria-labelledby="year-title">
      <CardHeader
        titleId="year-title"
        title={t('activity.year.title')}
        description={t('activity.year.description')}
      />
      <div className="p-5">
        {days ? (
          <YearCalendar days={days} label={t('activity.year.aria')} />
        ) : year.isError ? (
          <p className="text-muted-foreground">{t('activity.error')}</p>
        ) : (
          <div className="skel h-[130px]" />
        )}
      </div>
    </Card>
  );
}

function WhenCard({ a }: { a: Activity }) {
  const { t } = useTranslation();
  const server = useServerInfo();
  return (
    <Card aria-labelledby="when-title">
      <CardHeader
        titleId="when-title"
        title={t('activity.when.title', { name: server.data?.name || t('activity.when.server') })}
        description={t('activity.when.description')}
      />
      <div className="p-5">
        <HourWeekdayHeat grid={a.hour_weekday} />
      </div>
    </Card>
  );
}

function TopBooks({ a }: { a: Activity }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const max = a.top_books[0]?.listened || 1;
  return (
    <Card aria-labelledby="top-books-title">
      <CardHeader titleId="top-books-title" title={t('activity.topBooks.title')} />
      <ol className="divide-y">
        {a.top_books.map((b, i) => (
          <li key={`${b.library_id}:${b.path}`}>
            <Link
              {...bookRoute(b.library_id, b.path)}
              className="flex items-center gap-3.5 px-[18px] py-2.5 hover:bg-accent/50"
            >
              <span className="w-4 text-right font-bold text-subtle-foreground tabular-nums">
                {i + 1}
              </span>
              <BookCover
                libraryId={b.library_id}
                path={b.path}
                title={b.title || b.path}
                size={160}
                className="w-11 shrink-0"
              />
              <span className="flex min-w-0 flex-1 flex-col gap-1.5">
                <span className="truncate font-semibold">{b.title || b.path}</span>
                <span className="truncate text-[12px] text-muted-foreground">
                  {t('activity.topBooks.listeners', counted(b.listeners, lang))}
                </span>
                <ShareBar fraction={b.listened / max} color="var(--chart-1)" />
              </span>
              <b className="w-14 text-right tabular-nums">{formatHours(b.listened, lang)}</b>
            </Link>
          </li>
        ))}
      </ol>
    </Card>
  );
}

function TopPeople({ a }: { a: Activity }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const max = a.top_users[0]?.listened || 1;
  return (
    <Card aria-labelledby="top-people-title">
      <CardHeader titleId="top-people-title" title={t('activity.topPeople.title')} />
      <ol className="divide-y">
        {a.top_users.map((u) => (
          <li key={u.user_id}>
            <Link
              to="/people/user/$userId"
              params={{ userId: String(u.user_id) }}
              className="flex items-center gap-3 px-[18px] py-2.5 hover:bg-accent/50"
            >
              <Monogram name={u.username} size={30} />
              <span className="flex min-w-0 flex-1 flex-col">
                <b className="truncate font-semibold">{u.username}</b>
                <span className="truncate text-[12px] text-muted-foreground">
                  {t('activity.topPeople.detail', {
                    books: formatNumber(u.books, lang),
                    finished: formatNumber(u.finished, lang),
                  })}
                </span>
              </span>
              <ShareBar fraction={u.listened / max} className="hidden w-[120px] sm:block" />
              <b className="w-14 text-right tabular-nums">{formatHours(u.listened, lang)}</b>
            </Link>
          </li>
        ))}
      </ol>
    </Card>
  );
}

function MostHeard({ a }: { a: Activity }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const lists = [
    { key: 'authors', title: t('activity.heard.authors'), rows: a.top_authors.slice(0, 5) },
    { key: 'narrators', title: t('activity.heard.narrators'), rows: a.top_narrators.slice(0, 5) },
  ].filter((l) => l.rows.length);
  if (!lists.length) return null;
  return (
    <Card aria-labelledby="heard-title">
      <CardHeader titleId="heard-title" title={t('activity.heard.title')} />
      <div className="grid gap-5 p-5 sm:grid-cols-2">
        {lists.map((l) => (
          <div key={l.key} className="flex min-w-0 flex-col gap-2">
            <span className="eyebrow">{l.title}</span>
            <ol className="flex flex-col gap-1.5 text-[13px]">
              {l.rows.map((p) => (
                <li key={p.name} className="flex min-w-0 items-baseline gap-2">
                  <span className="min-w-0 flex-1 truncate">{p.name}</span>
                  <b className="tabular-nums">{formatHours(p.listened, lang)}</b>
                </li>
              ))}
            </ol>
          </div>
        ))}
      </div>
    </Card>
  );
}

/** People with no activity for 60 days: a quiet nudge, each a link to their page. */
function Inactive({ a }: { a: Activity }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const list = a.inactive_users;
  if (!list.length) return null;
  return (
    <Notice
      tone="warn"
      icon={Clock}
      title={t('activity.inactive.title', counted(list.length, lang))}
    >
      <ul className="mt-1 flex flex-wrap gap-x-3 gap-y-1">
        {list.slice(0, 8).map((u) => (
          <li key={u.user_id}>
            <Link
              to="/people/user/$userId"
              params={{ userId: String(u.user_id) }}
              className="font-semibold text-foreground hover:underline"
            >
              {u.username}
            </Link>{' '}
            <span className="text-[12.5px]">
              {u.last_seen_at
                ? t('activity.inactive.seen', { time: formatRelative(u.last_seen_at, lang) })
                : t('activity.inactive.never')}
            </span>
          </li>
        ))}
      </ul>
    </Notice>
  );
}

function FunnelCard({ a }: { a: Activity }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const f = a.funnel;
  const steps = [
    ['started', f.started],
    ['reached25', f.reached_25],
    ['reached50', f.reached_50],
    ['reached75', f.reached_75],
    ['finished', f.finished],
  ] as const;
  return (
    <Card aria-labelledby="funnel-title">
      <CardHeader titleId="funnel-title" title={t('activity.funnel.title')} />
      <div className="flex flex-col gap-2.5 p-5">
        {steps.map(([key, n], i) => (
          <div key={key} className="flex flex-col gap-1">
            <div className="flex justify-between gap-2 text-[12.5px]">
              <span className="text-muted-foreground">{t(`activity.funnel.${key}`)}</span>
              <b className="tabular-nums">{formatNumber(n, lang)}</b>
            </div>
            <ShareBar
              fraction={f.started ? n / f.started : 0}
              color={i === steps.length - 1 ? 'var(--chart-3)' : 'var(--chart-2)'}
              className="h-2.5"
            />
          </div>
        ))}
        {a.drop_offs.length ? (
          <div className="mt-2 flex flex-col gap-3 border-t pt-3.5">
            <span className="text-[12.5px] font-semibold">{t('activity.dropOff.title')}</span>
            {a.drop_offs.slice(0, 3).map((d) => (
              <div key={`${d.library_id}:${d.path}:${d.chapter_index}`} className="flex gap-2.5">
                <BookCover
                  libraryId={d.library_id}
                  path={d.path}
                  title={d.title || d.path}
                  size={160}
                  className="w-9 shrink-0"
                />
                <p className="min-w-0 text-[12.5px] text-muted-foreground">
                  <Link
                    {...bookRoute(d.library_id, d.path)}
                    className="font-semibold text-foreground hover:underline"
                  >
                    {d.title || d.path}
                  </Link>
                  {': '}
                  {t('activity.dropOff.body', {
                    ...counted(d.listeners, lang),
                    chapter: d.chapter || t('activity.dropOff.chapter', { n: d.chapter_index + 1 }),
                  })}
                  {d.scan_error ? (
                    <>
                      {' '}
                      <Link
                        to="/health/{-$section}"
                        params={{ section: undefined }}
                        search={{ issue: 'scan_error' }}
                        className="inline-flex items-center gap-1 font-semibold text-brand-ink hover:underline"
                      >
                        <FileWarning className="size-3.5" aria-hidden="true" />
                        {t('activity.dropOff.problem')}
                      </Link>
                    </>
                  ) : null}
                </p>
              </div>
            ))}
          </div>
        ) : null}
      </div>
    </Card>
  );
}

function PlaybackCard({ a }: { a: Activity }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const parts = playbackParts(a.playback);
  const label = (p: PlaybackPart) =>
    !p.transcoded
      ? t('activity.playback.direct')
      : p.key === 'other'
        ? t('activity.playback.other')
        : t('activity.playback.transcoded', {
            codec: p.key ? p.key.toUpperCase() : t('activity.playback.unknownCodec'),
          });
  const transcoded = parts.filter((p) => p.transcoded).reduce((s, p) => s + p.share, 0);
  return (
    <Card aria-labelledby="playback-title">
      <CardHeader titleId="playback-title" title={t('activity.playback.title')} />
      {parts.length === 0 ? (
        <p className="px-5 py-6 text-center text-muted-foreground">{t('activity.playback.none')}</p>
      ) : (
        <div className="flex flex-wrap items-center gap-5 p-5">
          <PlaybackDonut parts={parts} label={label} />
          <div className="flex min-w-[160px] flex-1 flex-col gap-2 text-[13px]">
            {parts.map((p, i) => (
              <span key={p.key} className="flex items-center gap-2">
                <i
                  className="size-2.5 shrink-0 rounded-[3px]"
                  style={{ background: playbackColor(p, i) }}
                />
                <span className="min-w-0 flex-1 truncate">{label(p)}</span>
                <b className="tabular-nums">{formatPercent(p.share, lang)}</b>
              </span>
            ))}
            <span className="text-[12px] text-muted-foreground">
              {transcoded > 0
                ? t('activity.playback.note', { share: formatPercent(transcoded, lang) })
                : t('activity.playback.allDirect')}
            </span>
          </div>
        </div>
      )}
    </Card>
  );
}

function AppsCard({ a }: { a: Activity }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const name = useClientName();
  const rows = clientRows(a.clients);
  const max = rows[0]?.devices || 1;
  const outdated = rows.filter((r) => r.outdated).reduce((s, r) => s + r.devices, 0);
  const unknown = rows.filter((r) => !r.app).reduce((s, r) => s + r.devices, 0);
  return (
    <Card aria-labelledby="apps-title">
      <CardHeader
        titleId="apps-title"
        title={t('activity.apps.title')}
        description={t('activity.apps.description')}
      />
      {rows.length === 0 ? (
        <p className="px-5 py-6 text-center text-muted-foreground">{t('activity.apps.none')}</p>
      ) : (
        <div className="flex flex-col gap-2.5 p-5">
          {rows.map((r) => (
            <div
              key={`${r.app}|${r.version}|${r.platform}`}
              className="flex items-center gap-2.5 text-[13px]"
            >
              <span className="w-[42%] min-w-0 truncate" title={name(r)}>
                {name(r)}
              </span>
              <ShareBar
                fraction={r.devices / max}
                color={r.outdated || !r.app ? 'var(--chart-4)' : 'var(--chart-2)'}
                className="flex-1"
              />
              <b className="w-7 text-right tabular-nums">{formatNumber(r.devices, lang)}</b>
            </div>
          ))}
          {outdated || unknown ? (
            <span className="text-[12px] text-muted-foreground">
              {[
                outdated ? t('activity.apps.outdated', counted(outdated, lang)) : '',
                unknown ? t('activity.apps.unknown', counted(unknown, lang)) : '',
              ]
                .filter(Boolean)
                .join(' ')}
            </span>
          ) : null}
        </div>
      )}
    </Card>
  );
}

/** A library's colour in the storage bar: the series in order, then grey. */
const LIBRARY_COLORS = [...SERIES, 'var(--chart-5)'];

function StorageCard({ a }: { a: Activity }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const s = a.storage;
  const libs = s.by_library.filter((l) => l.bytes > 0);
  const color = (i: number) => LIBRARY_COLORS[i] ?? OTHERS;
  const c = a.coverage;
  const rings = [
    ['identified', c.identified],
    ['chapters', c.with_chapters],
    ['cover', c.with_cover],
  ] as const;
  return (
    <Card aria-labelledby="storage-title">
      <CardHeader
        titleId="storage-title"
        title={t('activity.storage.title')}
        action={<b className="tabular-nums">{formatBytes(s.bytes, lang)}</b>}
      />
      <div className="flex flex-col gap-5 p-5">
        {libs.length ? (
          <div className="flex flex-col gap-2.5">
            <div className="flex h-3.5 gap-0.5 overflow-hidden rounded-full" aria-hidden="true">
              {libs.map((l, i) => (
                <i key={l.library_id} style={{ flex: l.bytes, background: color(i) }} />
              ))}
            </div>
            <ChartLegend
              items={libs.map((l, i) => ({
                key: String(l.library_id),
                color: color(i),
                label: `${l.name} · ${formatBytes(l.bytes, lang)}`,
              }))}
            />
          </div>
        ) : null}
        {s.by_format.length ? (
          <p className="text-[12.5px] text-muted-foreground">
            {t('activity.storage.formats', {
              list: s.by_format
                .slice(0, 4)
                .map(
                  (g) =>
                    `${g.key ? g.key.toUpperCase() : t('activity.storage.unknown')} ${formatBytes(g.bytes, lang)}`,
                )
                .join(' · '),
            })}
          </p>
        ) : null}
        <div className="grid grid-cols-3 gap-2.5">
          {rings.map(([key, n]) => {
            const f = c.books ? n / c.books : 0;
            return (
              <div key={key} className="flex flex-col items-center gap-1.5 text-center">
                <Ring fraction={f} size={64} stroke={6} color="var(--chart-5)" />
                <b className="tabular-nums">{formatPercent(f, lang)}</b>
                <span className="text-[12px] text-muted-foreground">
                  {t(`activity.coverage.${key}`)}
                </span>
              </div>
            );
          })}
        </div>
      </div>
    </Card>
  );
}
