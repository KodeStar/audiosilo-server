import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { RefreshCw } from 'lucide-react';
import { useIssues, useLibraries, scanActive } from '@/api/hooks';
import type { IssueCount, IssueKind, OfflineLibrary } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Button, buttonVariants } from '@/components/ui/button';
import { OfflineNotice } from '@/features/libraries/offline-notice';
import { rescanAll, rescanLibrary } from '@/features/libraries/rescan';
import { counted, formatNumber, formatRelative } from '@/lib/format';
import { cn } from '@/lib/utils';
import { Duplicates } from './duplicates';
import { IssueBooks } from './issue-books';
import { CATEGORY_LOOK, attentionTotal, isBookKind, pickCategory } from './issues-model';

/**
 * Health > Issues (STYLEGUIDE.md "Health triage"): libraries whose folder is
 * offline (a safety stop, not a failure), one card per kind of issue with its
 * count and a few covers, and the open category's queue with Ignore and a fix.
 * The category and the ignored view live in the URL (?issue=, ?ignored=1).
 */
export function IssuesPage() {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const summary = useIssues();
  const libraries = useLibraries();
  const search = useSearch({ strict: false }) as { issue?: IssueKind; ignored?: true };
  const navigate = useNavigate();
  const go = (next: { issue?: IssueKind; ignored?: true }) =>
    void navigate({ to: '.', search: next, replace: true });

  const checking = (libraries.data ?? []).some(scanActive);
  const checkAgain = (
    <div className="flex flex-wrap items-center gap-3">
      {summary.data?.checked_at ? (
        <span className="text-[12.5px] text-muted-foreground">
          {t('health.checked', { when: formatRelative(summary.data.checked_at, lang) })}
        </span>
      ) : null}
      <Button
        variant="outline"
        disabled={checking || !libraries.data?.length}
        onClick={() => rescanAll(qc, libraries.data ?? [])}
      >
        <RefreshCw className={cn(checking && 'animate-spin')} aria-hidden="true" />
        {checking ? t('health.checking') : t('health.checkAgain')}
      </Button>
    </div>
  );

  if (summary.isError) {
    return (
      <Page>
        <PageHead title={t('health.title')} />
        <QueryError
          title={t('health.error')}
          error={summary.error}
          onRetry={() => void summary.refetch()}
        />
      </Page>
    );
  }

  const data = summary.data;
  const total = data ? attentionTotal(data) : 0;
  const cat = data ? pickCategory(data.categories, search.issue) : undefined;
  const ignored = !!search.ignored;

  return (
    <Page>
      <PageHead
        title={t('health.title')}
        description={
          !data ? (
            <span className="skel inline-block h-4 w-72 align-middle" />
          ) : total ? (
            t('health.summary', counted(total, lang))
          ) : (
            t('health.summaryClear')
          )
        }
        action={checkAgain}
      />

      {data?.offline.length ? (
        <div className="mb-6 flex flex-col gap-3">
          {data.offline.map((l) => (
            <OfflineCard key={l.library_id} library={l} />
          ))}
        </div>
      ) : null}

      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-4">
        {!data
          ? Array.from({ length: 4 }, (_, i) => (
              <div key={i} className="skel h-[118px] rounded-xl" />
            ))
          : data.categories.map((c) => (
              <CategoryCard
                key={c.kind}
                category={c}
                active={c.kind === cat?.kind}
                lang={lang}
                onSelect={() => go({ issue: c.kind })}
              />
            ))}
      </div>

      {cat ? (
        <section className="mt-9" aria-labelledby="issue-heading">
          <div className="mb-3.5 flex flex-wrap items-end justify-between gap-3">
            <div className="flex min-w-0 flex-col gap-0.5">
              <h2 id="issue-heading" className="h2">
                {ignored
                  ? t('health.ignoredHeading', { kind: t(`health.kind.${cat.kind}`) })
                  : t(`health.kind.${cat.kind}`)}
              </h2>
              <span className="text-[13px] text-muted-foreground">
                {t(`health.about.${cat.kind}`)}
              </span>
            </div>
            {cat.ignored || ignored ? (
              <Button
                variant="ghost"
                size="sm"
                aria-pressed={ignored}
                onClick={() =>
                  go(ignored ? { issue: cat.kind } : { issue: cat.kind, ignored: true })
                }
              >
                {ignored
                  ? t('health.showOpen')
                  : t('health.showIgnored', counted(cat.ignored, lang))}
              </Button>
            ) : null}
          </div>
          {isBookKind(cat.kind) ? (
            <IssueBooks key={`${cat.kind}:${ignored}`} kind={cat.kind} ignored={ignored} />
          ) : (
            <Duplicates ignored={ignored} />
          )}
        </section>
      ) : null}
    </Page>
  );
}

/** One kind of issue: icon, count, label, and up to three of its books fanned (STYLEGUIDE.md). */
function CategoryCard({
  category: c,
  active,
  lang,
  onSelect,
}: {
  category: IssueCount;
  active: boolean;
  lang: string;
  onSelect: () => void;
}) {
  const { t } = useTranslation();
  const { icon: Icon, tile } = CATEGORY_LOOK[c.kind];
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onSelect}
      className={cn(
        'flex min-w-0 flex-col items-start gap-1 rounded-xl border bg-card p-3.5 text-left transition-colors duration-(--dur-1) hover:bg-accent/40 md:p-4',
        active && 'border-brand shadow-[inset_0_0_0_1px_var(--brand)]',
      )}
    >
      <span className="flex w-full items-start justify-between gap-2">
        <span
          className={cn('grid size-8 place-items-center rounded-[9px]', tile)}
          aria-hidden="true"
        >
          <Icon className="size-4" />
        </span>
        <span className="flex max-md:hidden" aria-hidden="true">
          {c.samples.map((s, i) => (
            <span
              key={`${s.library_id}:${s.path}`}
              className="w-[30px]"
              style={{ marginLeft: i ? -12 : 0, transform: `rotate(${(i - 1) * 6}deg)` }}
            >
              <BookCover libraryId={s.library_id} path={s.path} title={s.title} size={160} />
            </span>
          ))}
        </span>
      </span>
      <span className="stat-value mt-1 max-md:text-2xl">
        {formatNumber(c.count, lang)}
        {c.kind === 'duplicate' ? (
          <span className="ml-1 font-sans text-[13px] font-semibold tracking-normal text-muted-foreground">
            {t('health.groups', { count: c.count })}
          </span>
        ) : null}
      </span>
      <span className="text-[13px] font-[550] text-muted-foreground">
        {t(`health.kind.${c.kind}`)}
      </span>
      {c.ignored ? (
        <span className="text-[11.5px] text-subtle-foreground">
          {t('health.ignoredCount', counted(c.ignored, lang))}
        </span>
      ) : null}
    </button>
  );
}

/**
 * A library whose folder can't be read: OfflineNotice (a safety stop when books
 * were kept), naming the library, with a retry.
 */
function OfflineCard({ library: o }: { library: OfflineLibrary }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const library = useLibraries().data?.find((l) => l.id === o.library_id);
  if (!library) return null;
  return (
    <OfflineNotice
      library={library}
      title={o.books ? t('health.offline.title', { name: o.name }) : undefined}
      listeners={o.listeners}
      actions={
        <>
          <Link
            to="/library/{-$section}"
            params={{ section: 'libraries' }}
            className={buttonVariants({ variant: 'outline', size: 'sm' })}
          >
            {t('health.offline.view')}
          </Link>
          <Button size="sm" onClick={() => rescanLibrary(qc, library)}>
            <RefreshCw aria-hidden="true" />
            {t('libraries.retry')}
          </Button>
        </>
      }
    />
  );
}
