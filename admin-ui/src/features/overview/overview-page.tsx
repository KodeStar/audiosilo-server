import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import {
  ArrowRight,
  BookOpen,
  CheckCircle2,
  ChevronRight,
  Headphones,
  LibraryBig,
  Plus,
  RotateCw,
  TriangleAlert,
  Users,
} from 'lucide-react';
import {
  useIssues,
  useLiveSessions,
  useOfflineLibraries,
  useServerInfo,
  useSettings,
  useStats,
  useUpdateStatus,
} from '@/api/hooks';
import type {
  AdminSettings,
  LibraryStat,
  ListeningRow,
  ListeningSession,
  ServerInfo,
} from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { ProgressBar } from '@/components/progress-bar';
import { SessionState } from '@/components/session-state';
import { StatTile } from '@/components/stat-tile';
import { FactList } from '@/components/fact-list';
import { StatusText, type StatusTone } from '@/components/status-text';
import { Monogram } from '@/components/monogram';
import { CATEGORY_LOOK } from '@/features/health/issues-model';
import { OfflineNotice } from '@/features/libraries/offline-notice';
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
 * Home (the mark): a greeting, who is listening right now (the live sessions),
 * catalog totals, recent listening, books per library and a server card. Built on
 * GET /admin/stats, /admin/sessions/live, /admin/settings, /server and
 * /admin/issues (the "needs attention" card).
 */
export function OverviewPage() {
  const { t, i18n } = useTranslation();
  const user = useCurrentUser();
  const stats = useStats();
  const live = useLiveSessions();
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

  // A failed live list reads as nobody live rather than holding the page back.
  const count = (n: number | undefined) => (n === undefined ? undefined : formatNumber(n, lang));
  const split =
    stats.data && (live.data || live.isError)
      ? splitListening(stats.data.listening, live.data ?? [])
      : null;

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

      <OfflineLibraries />

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
            {split.live.map((s) => (
              <LiveCard key={s.id} session={s} lang={lang} />
            ))}
          </div>
        )}
      </section>

      <div className="mt-4 grid grid-cols-2 gap-2.5 md:gap-4 xl:grid-cols-4">
        <StatTile
          icon={BookOpen}
          label={t('home.stat.books')}
          value={count(stats.data?.total_books)}
        />
        <StatTile
          icon={LibraryBig}
          label={t('home.stat.libraries')}
          value={count(stats.data?.total_libraries)}
        />
        <StatTile
          icon={Users}
          label={t('home.stat.people')}
          value={count(stats.data?.total_users)}
        />
        <StatTile
          icon={Headphones}
          label={t('home.stat.inProgress')}
          value={count(split?.inProgress)}
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
          <NeedsAttention lang={lang} />
          <LibrariesCard libraries={stats.data?.libraries} lang={lang} />
          <ServerCard />
        </aside>
      </div>
    </Page>
  );
}

/** A safety notice per library whose folder can't be read (STYLEGUIDE.md "Safety stops"). */
function OfflineLibraries() {
  const { t } = useTranslation();
  const offline = useOfflineLibraries();
  if (offline.length === 0) return null;
  return (
    <div className="mb-8 flex flex-col gap-3">
      {offline.map((l) => (
        <OfflineNotice
          key={l.id}
          library={l}
          // With no books there was nothing to keep: the notice's own title fits.
          title={l.book_count ? t('home.offline.title', { name: l.name }) : undefined}
          actions={
            <Link
              to="/library/{-$section}"
              params={{ section: 'libraries' }}
              className={buttonVariants({ variant: 'outline', size: 'sm' })}
            >
              {t('home.offline.action')}
            </Link>
          }
        />
      ))}
    </div>
  );
}

function LiveCard({ session: s, lang }: { session: ListeningSession; lang: string }) {
  const { t } = useTranslation();
  const frac = progressFraction(s.position, s.duration);
  const title = s.title || s.path;
  return (
    <Link
      to="/activity/{-$section}"
      params={{ section: 'live' }}
      className="flex items-center gap-3.5 rounded-xl border bg-card p-3.5 transition-colors duration-(--dur-1) hover:border-border-strong"
    >
      <BookCover libraryId={s.library_id} path={s.path} title={title} className="w-16 shrink-0" />
      <div className="flex min-w-0 flex-1 flex-col gap-1.5">
        <div className="flex min-w-0 items-center gap-2">
          <Monogram name={s.username} size={24} />
          <b className="truncate">{s.username}</b>
        </div>
        <div className="truncate font-semibold">{title}</div>
        <div className="truncate text-[12.5px] text-muted-foreground">
          {s.chapter || s.author || s.device_name}
        </div>
        <ProgressBar fraction={frac} label={t('home.progressAria', { title })} />
        <div className="flex justify-between gap-2 text-[11.5px] text-subtle-foreground tabular-nums">
          <span>{formatPercent(frac, lang)}</span>
          <SessionState state={s.state} />
        </div>
      </div>
    </Link>
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

/** The Health categories that need attention, in the Health page's order, each opening its queue. */
function NeedsAttention({ lang }: { lang: string }) {
  const { t } = useTranslation();
  const issues = useIssues();
  const open = (issues.data?.categories ?? []).filter((c) => c.count > 0).slice(0, 6);
  return (
    <section className="rounded-xl border bg-card" aria-labelledby="attention-heading">
      <div className="flex items-center justify-between gap-3 border-b px-5 py-3.5">
        <h3 id="attention-heading" className="text-[14.5px]">
          {t('home.attention.title')}
        </h3>
        <Link
          to="/health/{-$section}"
          params={{ section: undefined }}
          className={buttonVariants({ variant: 'ghost', size: 'sm' })}
        >
          {t('home.attention.triage')}
          <ArrowRight aria-hidden="true" />
        </Link>
      </div>
      {issues.isError ? (
        <p className="px-5 py-4 text-[13px] text-muted-foreground">{t('home.attention.error')}</p>
      ) : !issues.data ? (
        <div className="flex flex-col gap-2 p-4">
          {[0, 1, 2].map((i) => (
            <div key={i} className="skel h-8" />
          ))}
        </div>
      ) : open.length === 0 ? (
        <p className="flex items-center gap-2 px-5 py-4 text-[13px] text-muted-foreground">
          <CheckCircle2 className="size-4 text-success" aria-hidden="true" />
          {t('home.attention.clear')}
        </p>
      ) : (
        <ul>
          {open.map((c) => {
            const { icon: Icon, tile } = CATEGORY_LOOK[c.kind];
            return (
              <li key={c.kind}>
                <Link
                  to="/health/{-$section}"
                  params={{ section: undefined }}
                  search={{ issue: c.kind }}
                  className="flex items-center gap-3 px-5 py-2.5 hover:bg-accent/50"
                >
                  <span
                    className={cn('grid size-7 place-items-center rounded-[8px]', tile)}
                    aria-hidden="true"
                  >
                    <Icon className="size-[15px]" />
                  </span>
                  <span className="min-w-0 flex-1 truncate">{t(`health.kind.${c.kind}`)}</span>
                  <b className="tabular-nums">{formatNumber(c.count, lang)}</b>
                  <ChevronRight className="size-[15px] text-subtle-foreground" aria-hidden="true" />
                </Link>
              </li>
            );
          })}
        </ul>
      )}
    </section>
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
  const max = (libraries ?? []).reduce((m, l) => Math.max(m, l.book_count), 1);
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

type Status = { tone?: StatusTone; key: string };

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
  const update = useUpdateStatus();
  const latest = update.data?.update_available ? update.data.latest : null;
  const rows: [string, React.ReactNode][] = [
    [
      t('home.server.version'),
      server.data && (
        <span className="inline-flex flex-wrap items-center gap-x-2">
          {formatVersion(server.data.version)}
          {latest ? (
            <Link
              to="/server/{-$section}"
              params={{ section: 'about' }}
              className="font-semibold text-brand-ink hover:underline"
            >
              {t('home.server.update', { version: formatVersion(latest.version) })}
            </Link>
          ) : null}
        </span>
      ),
    ],
    [
      t('home.server.address'),
      <span className="font-mono text-[12.5px]">{window.location.host}</span>,
    ],
    [
      t('home.server.transcoding'),
      <CapabilityStatus status={capability(server.data, 'transcode')} />,
    ],
    [
      t('home.server.webPlayer'),
      <CapabilityStatus status={capability(server.data, 'web_player')} />,
    ],
    [t('home.server.metadata'), <CapabilityStatus status={metadataStatus(settings.data)} />],
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
      <FactList rows={rows} />
    </section>
  );
}

function CapabilityStatus({ status }: { status: Status | null }) {
  const { t } = useTranslation();
  if (!status) return <span className="skel inline-block h-4 w-16 align-middle" />;
  return <StatusText tone={status.tone}>{t(status.key)}</StatusText>;
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
          <Link
            to="/library/{-$section}"
            params={{ section: 'libraries' }}
            search={{ add: true }}
            className={buttonVariants({ size: 'lg' })}
          >
            <Plus aria-hidden="true" />
            {t('home.firstRun.cta')}
          </Link>
        </div>
      </div>
    </Page>
  );
}
