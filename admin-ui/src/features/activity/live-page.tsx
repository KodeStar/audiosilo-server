import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Headphones } from 'lucide-react';
import { useLiveSessions } from '@/api/hooks';
import type { ListeningSession } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { PlaybackStatus } from '@/components/playback-status';
import { ProgressBar } from '@/components/progress-bar';
import { QueryError } from '@/components/query-error';
import { bookRoute } from '@/lib/book-route';
import { formatClock, formatDuration, formatRelative, progressFraction } from '@/lib/format';
import { liveSummary, sortLive } from './activity-model';
import { useClientName } from './use-client-name';

/** Activity > Live now: every device playing or paused in the last ten minutes. */
export function LivePage() {
  const { t } = useTranslation();
  const live = useLiveSessions();
  const sessions = sortLive(live.data ?? []);
  const sum = liveSummary(sessions);

  return (
    <Page>
      <PageHead
        title={t('live.title')}
        description={
          live.data
            ? [
                sum.playing
                  ? t('live.description', {
                      count: sum.playing,
                      direct: sum.playing - sum.transcoding,
                      transcoding: sum.transcoding,
                    })
                  : '',
                sum.paused ? t('live.paused', { count: sum.paused }) : '',
              ]
                .filter(Boolean)
                .join(' · ') || t('live.quiet')
            : undefined
        }
      />
      {live.isError ? (
        <QueryError
          title={t('live.error')}
          error={live.error}
          onRetry={() => void live.refetch()}
        />
      ) : !live.data ? (
        <div className="flex flex-col gap-3.5" role="status" aria-label={t('common.loading')}>
          {[0, 1].map((i) => (
            <div key={i} className="skel h-[136px] rounded-xl" />
          ))}
        </div>
      ) : sessions.length === 0 ? (
        <EmptyState icon={Headphones} title={t('live.empty.title')} body={t('live.empty.body')} />
      ) : (
        <ul className="flex flex-col gap-3.5">
          {sessions.map((s) => (
            <li key={s.id}>
              <LiveSession s={s} />
            </li>
          ))}
        </ul>
      )}
      <p className="mt-4 text-[12.5px] text-subtle-foreground">{t('live.note')}</p>
    </Page>
  );
}

function LiveSession({ s }: { s: ListeningSession }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const clientName = useClientName();
  const frac = progressFraction(s.position, s.duration);
  const title = s.title || s.path;
  const rows: [string, React.ReactNode][] = [
    [t('live.device'), s.device_name || t('live.unnamed')],
    [t('live.app'), clientName(s.client)],
    [t('live.playback'), <PlaybackStatus direct={!s.transcoded} codec={s.codec} />],
    ...(s.ip
      ? [
          [t('live.address'), <span className="font-mono">{s.ip}</span>] as [
            string,
            React.ReactNode,
          ],
        ]
      : []),
    [t('live.started'), formatRelative(s.started_at, lang)],
  ];
  return (
    <article className="flex flex-wrap items-start gap-x-5 gap-y-4 rounded-xl border bg-card p-4">
      <Link {...bookRoute(s.library_id, s.path)} className="w-[88px] shrink-0" aria-label={title}>
        <BookCover libraryId={s.library_id} path={s.path} title={title} size={160} />
      </Link>
      <div className="flex min-w-[220px] flex-1 flex-col gap-1.5">
        <div className="flex flex-wrap items-center gap-2">
          <Monogram name={s.username} size={26} />
          <Link
            to="/people/user/$userId"
            params={{ userId: String(s.user_id) }}
            className="font-semibold hover:underline"
          >
            {s.username}
          </Link>
          <span className="inline-flex items-center gap-1.5 text-muted-foreground">
            <span
              className="dot"
              data-tone={s.state === 'playing' ? 'live' : 'off'}
              aria-hidden="true"
            />
            {s.state === 'playing' ? t('live.playing') : t('live.pausedState')}
          </span>
        </div>
        <Link
          {...bookRoute(s.library_id, s.path)}
          className="font-display text-[17px] font-[650] tracking-[-0.01em] hover:underline"
        >
          {title}
        </Link>
        {s.chapter ? <span className="text-muted-foreground">{s.chapter}</span> : null}
        <div className="flex max-w-[480px] items-center gap-2.5">
          <ProgressBar
            fraction={frac}
            label={t('home.progressAria', { title })}
            className="flex-1"
          />
          <span className="text-[12px] whitespace-nowrap text-muted-foreground tabular-nums">
            {s.duration > 0
              ? t('live.position', {
                  position: formatClock(s.position),
                  duration: formatDuration(s.duration, lang),
                })
              : formatClock(s.position)}
          </span>
        </div>
      </div>
      <dl className="grid min-w-[240px] grid-cols-[minmax(80px,auto)_1fr] gap-x-4 gap-y-1.5 text-[12.5px]">
        {rows.map(([label, value]) => (
          <div key={label} className="contents">
            <dt className="text-muted-foreground">{label}</dt>
            <dd className="min-w-0 font-[550] [overflow-wrap:anywhere]">{value}</dd>
          </div>
        ))}
      </dl>
    </article>
  );
}
