import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Sparkles } from 'lucide-react';
import { useActivity, useServerInfo } from '@/api/hooks';
import type { Activity } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Card, CardHeader } from '@/components/ui/card';
import { NativeSelect } from '@/components/ui/native-select';
import { bookRoute } from '@/lib/book-route';
import { counted, formatHours, formatNumber } from '@/lib/format';
import { busiestSlot, longestStreak, recentYears, weekdayName } from './activity-model';
import { YearCalendar } from './heatmaps';

/** Activity > Year in listening: a calendar year told as a story, then day by day. */
export function YearPage() {
  const { t } = useTranslation();
  const search = useSearch({ strict: false }) as { year?: number };
  const navigate = useNavigate();
  const now = new Date();
  const year = search.year ?? now.getFullYear();
  const years = recentYears(now);
  if (!years.includes(year)) years.push(year);
  const activity = useActivity(String(year));

  return (
    <Page>
      <PageHead
        title={t('year.title')}
        description={t('year.description')}
        action={
          <NativeSelect
            className="w-[140px]"
            aria-label={t('year.pick')}
            value={year}
            onChange={(e) => {
              const y = Number(e.target.value);
              void navigate({
                to: '.',
                search: y === now.getFullYear() ? {} : { year: y },
                replace: true,
              });
            }}
          >
            {years.map((y) => (
              <option key={y} value={y}>
                {y}
              </option>
            ))}
          </NativeSelect>
        }
      />
      {activity.isError ? (
        <QueryError
          title={t('activity.error')}
          error={activity.error}
          onRetry={() => void activity.refetch()}
        />
      ) : !activity.data || activity.data.range !== String(year) ? (
        <div className="flex flex-col gap-4" role="status" aria-label={t('common.loading')}>
          <div className="skel h-[340px] rounded-xl" />
          <div className="skel h-[180px] rounded-xl" />
        </div>
      ) : activity.data.totals.listened <= 0 ? (
        <EmptyState
          icon={Sparkles}
          title={t('year.empty.title', { year })}
          body={t('year.empty.body')}
        />
      ) : (
        <YearStory a={activity.data} year={year} current={year === now.getFullYear()} />
      )}
    </Page>
  );
}

function YearStory({ a, year, current }: { a: Activity; year: number; current: boolean }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const server = useServerInfo();
  const top = a.top_books[0];
  const narrator = a.top_narrators[0];
  const slot = busiestSlot(a.hour_weekday);
  const hours = a.totals.listened / 3600;
  const facts = [
    [formatNumber(a.totals.finished, lang), t('year.fact.finished', { count: a.totals.finished })],
    [formatNumber(a.totals.books, lang), t('year.fact.books', { count: a.totals.books })],
    [
      formatNumber(longestStreak(a.days), lang),
      t('year.fact.streak', { count: longestStreak(a.days) }),
    ],
    ...(slot
      ? [
          [
            `${weekdayName(slot.weekday, lang)} ${String(slot.hour).padStart(2, '0')}:00`,
            t('year.fact.busiest'),
          ],
        ]
      : []),
  ];
  const name = server.data?.name || t('activity.when.server');

  return (
    <div className="flex flex-col gap-5">
      <section
        className="year-hero overflow-hidden rounded-xl border bg-card"
        aria-labelledby="year-headline"
      >
        <div className="px-6 py-8 md:px-10 md:py-10">
          <div className="eyebrow">
            {current ? t('year.eyebrowSoFar', { name, year }) : t('year.eyebrow', { name, year })}
          </div>
          <h2
            id="year-headline"
            className="mt-3 mb-7 max-w-[900px] font-display text-[clamp(30px,5vw,56px)] leading-[1.02] font-bold tracking-[-0.035em]"
          >
            {t('year.headline', {
              count: a.totals.listeners,
              people: formatNumber(a.totals.listeners, lang),
              hours: formatHours(a.totals.listened, lang),
            })}{' '}
            {hours >= 48 ? (
              <span className="text-brand-ink">
                {t('year.days', { days: formatNumber(Math.round(hours / 24), lang) })}
              </span>
            ) : null}
          </h2>
          <div className="flex flex-wrap items-end gap-x-9 gap-y-6">
            {top ? (
              <Link {...bookRoute(top.library_id, top.path)} className="w-[180px] shrink-0">
                <BookCover
                  libraryId={top.library_id}
                  path={top.path}
                  title={top.title || top.path}
                  className="shadow-cover"
                />
              </Link>
            ) : null}
            {top ? (
              <div className="flex max-w-[420px] min-w-[220px] flex-1 flex-col gap-2.5">
                <span className="eyebrow">{t('year.bookOfYear')}</span>
                <Link
                  {...bookRoute(top.library_id, top.path)}
                  className="font-display text-[28px] leading-none font-bold tracking-[-0.03em] hover:underline"
                >
                  {top.title || top.path}
                </Link>
                <span className="text-muted-foreground">
                  {t('year.bookOfYearBody', {
                    ...counted(top.listeners, lang),
                    hours: formatHours(top.listened, lang),
                  })}
                  {narrator ? ` ${t('year.voice', { name: narrator.name })}` : ''}
                </span>
                <div className="flex -space-x-2">
                  {a.top_users.slice(0, 5).map((u) => (
                    <Monogram
                      key={u.user_id}
                      name={u.username}
                      size={32}
                      className="ring-2 ring-card"
                    />
                  ))}
                </div>
              </div>
            ) : null}
            <dl className="grid min-w-[260px] flex-1 grid-cols-2 gap-5">
              {facts.map(([value, label]) => (
                <div key={label} className="flex flex-col">
                  <dt className="order-2 text-muted-foreground">{label}</dt>
                  <dd className="font-display text-[32px] leading-tight font-bold tracking-[-0.03em] tabular-nums">
                    {value}
                  </dd>
                </div>
              ))}
            </dl>
          </div>
        </div>
      </section>

      <Card aria-labelledby="year-days-title">
        <CardHeader
          titleId="year-days-title"
          title={t('year.daysTitle', { year })}
          description={t('activity.year.description')}
        />
        <div className="p-5">
          <YearCalendar days={a.days} label={t('year.daysTitle', { year })} />
        </div>
      </Card>

      <div className="grid items-start gap-4 xl:grid-cols-2">
        <Card aria-labelledby="year-books-title">
          <CardHeader titleId="year-books-title" title={t('year.mostPlayed')} />
          <ul className="grid grid-cols-[repeat(auto-fill,minmax(96px,1fr))] gap-4 p-5">
            {a.top_books.slice(0, 8).map((b) => (
              <li key={`${b.library_id}:${b.path}`} className="flex min-w-0 flex-col gap-1.5">
                <Link {...bookRoute(b.library_id, b.path)} aria-label={b.title || b.path}>
                  <BookCover
                    libraryId={b.library_id}
                    path={b.path}
                    title={b.title || b.path}
                    size={160}
                  />
                </Link>
                <span className="truncate text-[12.5px] font-semibold">{b.title || b.path}</span>
                <span className="text-[12px] text-muted-foreground tabular-nums">
                  {formatHours(b.listened, lang)}
                </span>
              </li>
            ))}
          </ul>
        </Card>
        <Card aria-labelledby="year-people-title">
          <CardHeader titleId="year-people-title" title={t('year.people')} />
          <ol className="divide-y">
            {a.top_users.map((u) => (
              <li key={u.user_id} className="flex items-center gap-3 px-[18px] py-2.5">
                <Monogram name={u.username} size={30} />
                <Link
                  to="/people/user/$userId"
                  params={{ userId: String(u.user_id) }}
                  className="min-w-0 flex-1 truncate font-semibold hover:underline"
                >
                  {u.username}
                </Link>
                <span className="text-[12.5px] text-muted-foreground">
                  {t('year.finishedCount', counted(u.finished, lang))}
                </span>
                <b className="w-14 text-right tabular-nums">{formatHours(u.listened, lang)}</b>
              </li>
            ))}
          </ol>
        </Card>
      </div>
    </div>
  );
}
