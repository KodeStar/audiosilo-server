import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { ArrowRight, Headphones } from 'lucide-react';
import { useActivity, useSessions, useUserProgress } from '@/api/hooks';
import type { User, UserProgress } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { ProgressBar } from '@/components/progress-bar';
import { QueryError } from '@/components/query-error';
import { buttonVariants } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import {
  finishedIn,
  listenedOn,
  longestStreak,
  monthTotals,
} from '@/features/activity/activity-model';
import { MonthBars } from '@/features/activity/charts';
import { SessionTable } from '@/features/activity/sessions-page';
import { bookRoute } from '@/lib/book-route';
import {
  formatDate,
  formatHours,
  formatNumber,
  formatPercent,
  formatRelative,
  progressFraction,
} from '@/lib/format';
import { ProgressMenu } from './progress-actions';
import type { ProgressTarget } from './use-edit-progress';

/** How many recent sessions the person page shows before "All sessions". */
const RECENT_SESSIONS = 8;

/** A person's listening: their year, what they're in the middle of, what they finished, recent sessions. */
export function ListeningTab({ user }: { user: User }) {
  const { t } = useTranslation();
  const progress = useUserProgress(user.id);
  const rows = progress.data ?? [];
  const inProgress = rows.filter((r) => !r.finished);
  const finished = rows
    .filter((r) => r.finished)
    .sort((a, b) => Date.parse(b.finished_at ?? '') - Date.parse(a.finished_at ?? ''));

  return (
    <div className="flex flex-col gap-5">
      <ListeningYear user={user} progress={rows} />
      {progress.isError ? (
        <QueryError
          title={t('user.listening.error')}
          error={progress.error}
          onRetry={() => void progress.refetch()}
        />
      ) : (
        <div className="grid items-start gap-4 xl:grid-cols-2">
          <ProgressCard
            title={t('user.listening.inProgress')}
            empty={t('user.listening.noneInProgress')}
            rows={progress.data ? inProgress : undefined}
            user={user}
          />
          <ProgressCard
            title={t('user.listening.finished')}
            empty={t('user.listening.noneFinished')}
            rows={progress.data ? finished : undefined}
            user={user}
          />
        </div>
      )}
      <RecentSessions user={user} />
    </div>
  );
}

function ListeningYear({ user, progress }: { user: User; progress: UserProgress[] }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const year = new Date().getFullYear();
  const activity = useActivity(String(year));
  const days = activity.data?.range === String(year) ? activity.data.days : undefined;
  const listened = days?.reduce((s, d) => s + listenedOn(d, user.id), 0) ?? 0;
  const facts = days
    ? [
        [formatHours(listened, lang), t('user.year.hours')],
        [
          formatNumber(finishedIn(progress, year).length, lang),
          t('user.year.finished', { count: finishedIn(progress, year).length }),
        ],
        [
          formatNumber(longestStreak(days, user.id), lang),
          t('user.year.streak', { count: longestStreak(days, user.id) }),
        ],
      ]
    : undefined;

  return (
    <section className="year-hero rounded-xl border p-6" aria-labelledby="user-year-title">
      <h2 id="user-year-title" className="eyebrow">
        {t('user.year.title', { name: user.username, year })}
      </h2>
      {activity.isError ? (
        <p className="mt-3 text-muted-foreground">{t('activity.error')}</p>
      ) : !facts || !days ? (
        <div className="skel mt-4 h-20" role="status" aria-label={t('common.loading')} />
      ) : (
        <div className="mt-4 flex flex-wrap items-end gap-x-10 gap-y-5">
          <dl className="flex flex-wrap items-end gap-x-10 gap-y-4">
            {facts.map(([value, label], i) => (
              <div key={label} className="flex flex-col">
                <dt className="order-2 text-muted-foreground">{label}</dt>
                <dd
                  className={
                    i === 0
                      ? 'font-display text-[52px] leading-[0.95] font-[750] tracking-[-0.045em] tabular-nums'
                      : 'font-display text-[36px] leading-[0.95] font-bold tracking-[-0.04em] tabular-nums'
                  }
                >
                  {value}
                </dd>
              </div>
            ))}
          </dl>
          <div className="min-w-[220px] flex-1">
            {listened > 0 ? (
              <MonthBars months={monthTotals(days, user.id)} year={year} />
            ) : (
              <p className="text-muted-foreground">
                {t('user.year.none', { name: user.username })}
              </p>
            )}
          </div>
        </div>
      )}
    </section>
  );
}

function ProgressCard({
  title,
  empty,
  rows,
  user,
}: {
  title: string;
  empty: string;
  rows: UserProgress[] | undefined;
  user: User;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  return (
    <Card aria-label={title}>
      <CardHeader
        title={title}
        action={
          rows ? (
            <span className="text-muted-foreground tabular-nums">
              {formatNumber(rows.length, lang)}
            </span>
          ) : null
        }
      />
      {!rows ? (
        <div className="flex flex-col gap-3 p-4">
          {[0, 1].map((i) => (
            <div key={i} className="skel h-12" />
          ))}
        </div>
      ) : rows.length === 0 ? (
        <p className="px-5 py-6 text-center text-muted-foreground">{empty}</p>
      ) : (
        <ul className="max-h-[520px] divide-y overflow-y-auto">
          {rows.map((r) => {
            const bookTitle = r.title || r.path;
            const frac = r.finished ? 1 : progressFraction(r.position, r.duration);
            const target: ProgressTarget = {
              ...r,
              userId: user.id,
              username: user.username,
              title: bookTitle,
            };
            const dates = [
              r.started_at ? t('progress.startedOn', { date: formatDate(r.started_at, lang) }) : '',
              r.finished_at
                ? t('progress.finishedOn', { date: formatDate(r.finished_at, lang) })
                : '',
            ].filter(Boolean);
            return (
              <li
                key={`${r.library_id}:${r.path}`}
                className="flex items-center gap-3.5 px-[18px] py-3"
              >
                <Link
                  {...bookRoute(r.library_id, r.path)}
                  className="w-11 shrink-0"
                  aria-label={bookTitle}
                >
                  <BookCover libraryId={r.library_id} path={r.path} title={bookTitle} size={160} />
                </Link>
                <div className="flex min-w-0 flex-1 flex-col gap-1">
                  <Link
                    {...bookRoute(r.library_id, r.path)}
                    className="truncate font-semibold hover:underline"
                  >
                    {bookTitle}
                  </Link>
                  {r.finished ? null : <ProgressBar fraction={frac} className="max-w-[360px]" />}
                  <span className="text-[12px] text-muted-foreground tabular-nums">
                    {(r.finished
                      ? dates
                      : [
                          formatPercent(frac, lang),
                          t('progress.saved', { time: formatRelative(r.updated_at, lang) }),
                        ]
                    ).join(' · ')}
                  </span>
                </div>
                <ProgressMenu target={target} />
              </li>
            );
          })}
        </ul>
      )}
    </Card>
  );
}

function RecentSessions({ user }: { user: User }) {
  const { t } = useTranslation();
  const sessions = useSessions({ user_id: user.id });
  const rows = (sessions.data?.pages[0]?.sessions ?? []).slice(0, RECENT_SESSIONS);
  return (
    <section aria-labelledby="user-sessions-title" className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 id="user-sessions-title" className="h2">
          {t('user.listening.sessions')}
        </h2>
        <Link
          to="/activity/{-$section}"
          params={{ section: 'sessions' }}
          search={{ person: user.id }}
          className={buttonVariants({ variant: 'ghost', size: 'sm' })}
        >
          {t('user.listening.allSessions')}
          <ArrowRight aria-hidden="true" />
        </Link>
      </div>
      {sessions.isError ? (
        <QueryError
          title={t('sessions.error')}
          error={sessions.error}
          onRetry={() => void sessions.refetch()}
        />
      ) : !sessions.data ? (
        <div className="skel h-24 rounded-xl" />
      ) : rows.length === 0 ? (
        <p className="flex items-center justify-center gap-2 rounded-xl border bg-card px-5 py-6 text-muted-foreground">
          <Headphones className="size-4" aria-hidden="true" />
          {t('user.listening.noSessions', { name: user.username })}
        </p>
      ) : (
        <SessionTable sessions={rows} showPerson={false} />
      )}
    </section>
  );
}
