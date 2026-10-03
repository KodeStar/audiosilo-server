import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { CLASSIC_CONSOLE_URL } from '@/components/shell/destinations';
import {
  ArrowRight,
  BookOpen,
  Headphones,
  LibraryBig,
  Plus,
  RotateCw,
  TriangleAlert,
  Users,
} from 'lucide-react';
import { useServerInfo, useSettings, useStats } from '@/api/hooks';
import type { AdminSettings, LibraryStat, ListeningRow, ServerInfo } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { Button, buttonVariants } from '@/components/ui/button';
import {
  formatVersion,
  formatLongDate,
  formatNumber,
  formatPercent,
  formatRelative,
  progressFraction,
} from '@/lib/format';
import { useCurrentUser } from '@/lib/session';
import { cn } from '@/lib/utils';
import { greetingFor, splitListening } from './overview-model';

/**
 * Home (the mark): a greeting, who is listening right now, catalog totals,
 * recent listening, books per library and a server card. Built on today's
 * GET /admin/stats, /admin/settings and /server; the "what happened" and
 * "needs attention" cards arrive with Phases 3 and 4.
 */
export function OverviewPage() {
  const { t, i18n } = useTranslation();
  const user = useCurrentUser();
  const stats = useStats();
  const lang = i18n.resolvedLanguage ?? 'en';
  const now = new Date();

  if (stats.isError) {
    return (
      <Page>
        <div className="flex flex-wrap items-start gap-3.5 rounded-xl border border-[color-mix(in_oklab,var(--destructive)_30%,var(--border))] bg-card px-[18px] py-4">
          <span className="grid size-9 shrink-0 place-items-center rounded-[11px] bg-destructive-soft text-destructive">
            <TriangleAlert className="size-[18px]" aria-hidden="true" />
          </span>
          <div className="flex min-w-0 flex-1 flex-col gap-0.5">
            <b>{t('home.error.title')}</b>
            <span className="text-muted-foreground">{stats.error.message}</span>
          </div>
          <Button variant="outline" size="sm" onClick={() => void stats.refetch()}>
            <RotateCw aria-hidden="true" />
            {t('common.tryAgain')}
          </Button>
        </div>
      </Page>
    );
  }

  if (stats.data && stats.data.total_libraries === 0) return <FirstRun />;

  const split = stats.data ? splitListening(stats.data.listening, now.getTime()) : null;

  return (
    <Page>
      <div className="mb-7 flex flex-wrap items-end justify-between gap-4">
        <div className="flex flex-col gap-2">
          <div className="eyebrow">{formatLongDate(now, lang)}</div>
          <h1 className="display">
            {t(`home.greeting.${greetingFor(now.getHours())}`, { name: user.username })}
          </h1>
          <p className="text-[15px] text-muted-foreground">
            {split ? (
              t('home.summary', { count: split.listeners })
            ) : (
              <span className="skel inline-block h-4 w-64 align-middle" />
            )}
          </p>
        </div>
      </div>

      <section aria-labelledby="live-heading">
        <div className="mb-3.5 flex flex-wrap items-baseline justify-between gap-2">
          <h2 id="live-heading" className="h2 flex items-center gap-2.5">
            <span
              className="dot"
              data-tone={split?.live.length ? 'live' : 'off'}
              aria-hidden="true"
            />
            {t('home.live.title')}
          </h2>
        </div>
        {!split ? (
          <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
            {[0, 1].map((i) => (
              <div key={i} className="skel h-[92px] rounded-xl" />
            ))}
          </div>
        ) : split.live.length === 0 ? (
          <div className="rounded-xl border bg-card px-5 py-6 text-center text-muted-foreground">
            {t('home.live.empty')}
          </div>
        ) : (
          <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
            {split.live.map((r) => (
              <LiveCard key={`${r.user_id}:${r.library_id}:${r.path}`} row={r} lang={lang} />
            ))}
          </div>
        )}
      </section>

      <div className="mt-4 grid grid-cols-2 gap-2.5 md:gap-4 xl:grid-cols-4">
        <StatTile
          icon={BookOpen}
          label={t('home.stat.books')}
          value={stats.data?.total_books}
          lang={lang}
        />
        <StatTile
          icon={LibraryBig}
          label={t('home.stat.libraries')}
          value={stats.data?.total_libraries}
          lang={lang}
        />
        <StatTile
          icon={Users}
          label={t('home.stat.people')}
          value={stats.data?.total_users}
          lang={lang}
        />
        <StatTile
          icon={Headphones}
          label={t('home.stat.inProgress')}
          value={split?.inProgress}
          lang={lang}
        />
      </div>

      <div className="mt-10 grid items-start gap-6 xl:grid-cols-[minmax(0,1fr)_360px]">
        <section aria-labelledby="recent-heading" className="min-w-0">
          <h2 id="recent-heading" className="h2 mb-3.5">
            {t('home.recent.title')}
          </h2>
          <div className="rounded-xl border bg-card">
            {!split ? (
              <div className="flex flex-col gap-3 p-4">
                {[0, 1, 2].map((i) => (
                  <div key={i} className="skel h-11" />
                ))}
              </div>
            ) : split.recent.length === 0 ? (
              <p className="px-5 py-6 text-center text-muted-foreground">
                {t('home.recent.empty')}
              </p>
            ) : (
              <ul className="divide-y">
                {split.recent.map((r) => (
                  <RecentRow key={`${r.user_id}:${r.library_id}:${r.path}`} row={r} lang={lang} />
                ))}
              </ul>
            )}
          </div>
        </section>
        <aside className="flex flex-col gap-4">
          <LibrariesCard libraries={stats.data?.libraries} lang={lang} />
          <ServerCard />
        </aside>
      </div>
    </Page>
  );
}

function StatTile({
  icon: Icon,
  label,
  value,
  lang,
}: {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  value: number | undefined;
  lang: string;
}) {
  return (
    <div className="flex flex-col gap-1.5 rounded-xl border bg-card p-3.5 md:px-5 md:py-[18px]">
      <div className="flex items-center gap-1.5 text-[12.5px] font-[550] text-muted-foreground">
        <Icon className="size-[15px]" aria-hidden="true" />
        {label}
      </div>
      {value === undefined ? (
        <span className="skel h-[30px] w-20" />
      ) : (
        <div className="stat-value max-md:text-2xl">{formatNumber(value, lang)}</div>
      )}
    </div>
  );
}

function LiveCard({ row, lang }: { row: ListeningRow; lang: string }) {
  const { t } = useTranslation();
  const frac = progressFraction(row.position, row.duration);
  return (
    <article className="flex items-center gap-3.5 rounded-xl border bg-card p-3.5">
      <BookCover
        libraryId={row.library_id}
        path={row.path}
        title={row.title}
        className="w-16 shrink-0"
      />
      <div className="flex min-w-0 flex-1 flex-col gap-1.5">
        <div className="flex min-w-0 items-center gap-2">
          <Monogram name={row.username} size={24} />
          <b className="truncate">{row.username}</b>
        </div>
        <div className="truncate font-semibold">{row.title || row.path}</div>
        {row.author ? (
          <div className="truncate text-[12.5px] text-muted-foreground">{row.author}</div>
        ) : null}
        <div
          className="progress-track"
          role="progressbar"
          aria-label={t('home.progressAria', { title: row.title || row.path })}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(frac * 100)}
        >
          <i style={{ width: `${frac * 100}%` }} />
        </div>
        <div className="flex justify-between gap-2 text-[11.5px] text-subtle-foreground tabular-nums">
          <span>{formatPercent(frac, lang)}</span>
          <span>{formatRelative(row.updated_at, lang)}</span>
        </div>
      </div>
    </article>
  );
}

function RecentRow({ row, lang }: { row: ListeningRow; lang: string }) {
  const { t } = useTranslation();
  const frac = progressFraction(row.position, row.duration);
  return (
    <li className="flex items-center gap-3.5 px-[18px] py-3">
      <BookCover
        libraryId={row.library_id}
        path={row.path}
        title={row.title}
        className="w-9 shrink-0 rounded-[3px]"
      />
      <div className="flex min-w-0 flex-1 flex-col">
        <span className="truncate font-semibold">{row.title || row.path}</span>
        <span className="truncate text-[12.5px] text-muted-foreground">
          {row.finished
            ? t('home.recent.finished', { name: row.username })
            : t('home.recent.at', { name: row.username, percent: formatPercent(frac, lang) })}
        </span>
      </div>
      <span className="text-xs whitespace-nowrap text-subtle-foreground">
        {formatRelative(row.updated_at, lang)}
      </span>
    </li>
  );
}

function LibrariesCard({
  libraries,
  lang,
}: {
  libraries: LibraryStat[] | undefined;
  lang: string;
}) {
  const { t } = useTranslation();
  const max = Math.max(1, ...(libraries ?? []).map((l) => l.book_count));
  return (
    <section className="rounded-xl border bg-card" aria-labelledby="libraries-heading">
      <div className="flex items-center justify-between gap-3 border-b px-5 py-4">
        <h3 id="libraries-heading" className="text-[14.5px]">
          {t('overview.booksPerLibrary')}
        </h3>
      </div>
      <div className="flex flex-col gap-3.5 p-5">
        {!libraries
          ? [0, 1].map((i) => <div key={i} className="skel h-8" />)
          : libraries.map((l) => (
              <div key={l.id} className="flex flex-col gap-1.5">
                <div className="flex justify-between gap-3 text-[13px]">
                  <span className="truncate font-[550]">{l.name}</span>
                  <span className="text-muted-foreground tabular-nums">
                    {formatNumber(l.book_count, lang)}
                  </span>
                </div>
                <div className="h-2 overflow-hidden rounded-full bg-muted" aria-hidden="true">
                  <i
                    className="block h-full rounded-full bg-chart-2"
                    style={{ width: `${(l.book_count / max) * 100}%` }}
                  />
                </div>
              </div>
            ))}
      </div>
    </section>
  );
}

type Status = { tone?: 'off' | 'warn'; key: string };

function metadataStatus(s: AdminSettings | undefined): Status | null {
  if (!s) return null;
  if (!s.metadata.available) return { tone: 'off', key: 'home.server.metaUnavailable' };
  return s.metadata.enabled ? { key: 'home.server.on' } : { tone: 'off', key: 'home.server.off' };
}

function capability(
  info: ServerInfo | undefined,
  cap: keyof ServerInfo['capabilities'],
): Status | null {
  if (!info) return null;
  return info.capabilities[cap]
    ? { key: 'home.server.on' }
    : { tone: 'off', key: 'home.server.off' };
}

function ServerCard() {
  const { t } = useTranslation();
  const server = useServerInfo();
  const settings = useSettings();
  const rows: [string, React.ReactNode][] = [
    [t('home.server.version'), server.data && formatVersion(server.data.version)],
    [
      t('home.server.address'),
      <span className="font-mono text-[12.5px]">{window.location.host}</span>,
    ],
    [t('home.server.transcoding'), <StatusText status={capability(server.data, 'transcode')} />],
    [t('home.server.webPlayer'), <StatusText status={capability(server.data, 'web_player')} />],
    [t('home.server.metadata'), <StatusText status={metadataStatus(settings.data)} />],
  ];
  return (
    <section
      className="flex flex-col gap-3 rounded-xl border bg-card p-5"
      aria-labelledby="server-heading"
    >
      <div className="flex items-center justify-between gap-3">
        <h3 id="server-heading" className="h3">
          {t('home.server.title')}
        </h3>
        <Link
          to="/server/{-$section}"
          params={{ section: undefined }}
          className={cn(buttonVariants({ variant: 'ghost', size: 'sm' }))}
        >
          {t('home.server.open')}
          <ArrowRight aria-hidden="true" />
        </Link>
      </div>
      <dl className="grid grid-cols-[minmax(110px,auto)_1fr] gap-x-[18px] gap-y-2 text-[13px]">
        {rows.map(([label, value]) => (
          <div key={label} className="contents">
            <dt className="text-muted-foreground">{label}</dt>
            <dd className="min-w-0 font-[550] [overflow-wrap:anywhere]">
              {value ?? <span className="skel inline-block h-4 w-16 align-middle" />}
            </dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

function StatusText({ status }: { status: Status | null }) {
  const { t } = useTranslation();
  if (!status) return <span className="skel inline-block h-4 w-16 align-middle" />;
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="dot" data-tone={status.tone} aria-hidden="true" />
      {t(status.key)}
    </span>
  );
}

/** No libraries yet: the welcome card (STYLEGUIDE.md, first-run). */
function FirstRun() {
  const { t } = useTranslation();
  const steps = ['add', 'scan', 'invite'] as const;
  return (
    <Page>
      <div className="overflow-hidden rounded-xl border bg-card">
        <div className="px-6 py-10 md:px-11 md:py-12">
          <div className="eyebrow">{t('home.firstRun.eyebrow')}</div>
          <h1 className="display mt-2.5 mb-3">{t('home.firstRun.title')}</h1>
          <p className="max-w-[460px] text-[15px] text-muted-foreground">
            {t('home.firstRun.body')}
          </p>
          <ol className="my-7 flex flex-col gap-3.5">
            {steps.map((s, i) => (
              <li key={s} className="flex items-start gap-3.5">
                <span
                  className={cn(
                    'grid size-7 shrink-0 place-items-center rounded-full text-[13px] font-bold',
                    i === 0
                      ? 'bg-primary text-primary-foreground'
                      : 'text-muted-foreground shadow-[inset_0_0_0_1.5px_var(--border-strong)]',
                  )}
                >
                  {i + 1}
                </span>
                <span className="flex flex-col">
                  <b>{t(`home.firstRun.${s}.title`)}</b>
                  <span className="text-muted-foreground">{t(`home.firstRun.${s}.body`)}</span>
                </span>
              </li>
            ))}
          </ol>
          {/* Adding a library moves into this console in Phase 1b. */}
          <a href={CLASSIC_CONSOLE_URL} className={buttonVariants({ size: 'lg' })}>
            <Plus aria-hidden="true" />
            {t('home.firstRun.cta')}
          </a>
        </div>
      </div>
    </Page>
  );
}
