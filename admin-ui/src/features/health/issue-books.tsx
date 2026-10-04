import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Eye, EyeOff, X } from 'lucide-react';
import { useAdminBooks, useLibraries } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { QueryError } from '@/components/query-error';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import { onInk } from '@/components/ui/on-ink';
import { useSelection } from '@/features/library/books/use-selection';
import { bookRoute, refKey } from '@/lib/book-route';
import { counted, formatNumber } from '@/lib/format';
import { joinLibraryPath } from '@/lib/paths';
import { FIXES, FIX_LOOK, issueReason, type BookIssueKind } from './issues-model';
import { AllClear } from './all-clear';
import { useIssueActions } from './use-issue-actions';

/** How many books a page of an issue list loads. */
const PAGE = 50;

/**
 * One category's books (STYLEGUIDE.md "Health triage"): cover, title, why, where,
 * Ignore and the category's one fix; select several for the floating bar. With
 * `ignored`, the books an admin ignored, each with Stop ignoring.
 */
export function IssueBooks({ kind, ignored }: { kind: BookIssueKind; ignored: boolean }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const list = useAdminBooks({
    issue: kind,
    issue_ignored: ignored || undefined,
    sort: 'added',
    order: 'desc',
    limit: PAGE,
  });
  const selection = useSelection();
  const actions = useIssueActions(kind);
  const roots = new Map((useLibraries().data ?? []).map((l) => [l.id, l.root]));
  const books = list.data?.pages.flatMap((p) => p.books ?? []) ?? [];
  const fix = FIXES[kind];
  const RescanIcon = FIX_LOOK.rescan.icon;

  if (list.isError) {
    return (
      <QueryError
        title={t('health.listError')}
        error={list.error}
        onRetry={() => void list.refetch()}
      />
    );
  }
  if (list.isPending) {
    return (
      <div
        className="flex flex-col gap-2 rounded-xl border bg-card p-4"
        role="status"
        aria-label={t('common.loading')}
      >
        {[0, 1, 2].map((i) => (
          <div key={i} className="skel h-14" />
        ))}
      </div>
    );
  }
  if (books.length === 0) {
    return <AllClear ignored={ignored} />;
  }

  const allSelected = books.every(selection.isSelected);
  const act = (chosen: AdminBook[]) => {
    selection.clear();
    return ignored ? actions.unignore(chosen) : actions.ignore(chosen);
  };

  return (
    <>
      <div className="mb-2.5 flex justify-end">
        <Button variant="ghost" size="sm" onClick={() => selection.setMany(books, !allSelected)}>
          {allSelected ? t('health.selectNone') : t('health.selectAll')}
        </Button>
      </div>
      <ul className="divide-y rounded-xl border bg-card" aria-label={t(`health.kind.${kind}`)}>
        {books.map((b) => {
          const reason = issueReason(kind, b);
          return (
            <li key={refKey(b)} className="flex items-center gap-3.5 px-3.5 py-3 md:px-[18px]">
              <Checkbox
                checked={selection.isSelected(b)}
                onCheckedChange={() => selection.toggle(b)}
                aria-label={t('health.select', { title: b.title || b.path })}
              />
              <Link
                {...bookRoute(b.library_id, b.path)}
                className="w-12 shrink-0"
                aria-label={t('health.open', { title: b.title || b.path })}
              >
                <BookCover
                  libraryId={b.library_id}
                  path={b.path}
                  title={b.title}
                  author={b.author}
                  size={160}
                />
              </Link>
              <div className="flex min-w-0 flex-1 flex-col">
                <Link
                  {...bookRoute(b.library_id, b.path)}
                  className="truncate font-semibold hover:underline"
                >
                  {b.title || b.path}
                </Link>
                <span className="text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">
                  {t(reason.key, reason.values)}
                </span>
                <span className="truncate font-mono text-[11.5px] text-subtle-foreground max-md:hidden">
                  {joinLibraryPath(roots.get(b.library_id), b.path)}
                </span>
              </div>
              <div className="flex shrink-0 items-center gap-1.5">
                {ignored ? (
                  <Button variant="outline" size="sm" onClick={() => void actions.unignore([b])}>
                    <Eye aria-hidden="true" />
                    <span className="max-md:sr-only">{t('health.unignore')}</span>
                  </Button>
                ) : (
                  <>
                    <Button variant="ghost" size="sm" onClick={() => void actions.ignore([b])}>
                      <EyeOff aria-hidden="true" className="md:hidden" />
                      <span className="max-md:sr-only">{t('health.ignore')}</span>
                    </Button>
                    {fix ? <FixButton kind={kind} onClick={() => void actions.fix(b)} /> : null}
                  </>
                )}
              </div>
            </li>
          );
        })}
      </ul>
      {list.hasNextPage ? (
        <div className="mt-3 flex justify-center">
          <Button
            variant="outline"
            onClick={() => void list.fetchNextPage()}
            disabled={list.isFetchingNextPage}
          >
            {list.isFetchingNextPage ? t('common.loading') : t('health.more')}
          </Button>
        </div>
      ) : null}
      <p className="mt-2.5 text-[12.5px] text-subtle-foreground">
        {t(ignored ? 'health.footIgnored' : 'health.foot', counted(books.length, lang))}
      </p>

      {selection.size ? (
        <div className="float-bar" role="toolbar" aria-label={t('health.bulk.label')}>
          <span className="mr-2 font-bold whitespace-nowrap tabular-nums" aria-live="polite">
            {t('books.bulk.selected', {
              count: selection.size,
              formatted: formatNumber(selection.size, lang),
            })}
          </span>
          {fix === 'rescan' && !ignored ? (
            <Button
              variant="ghost"
              size="sm"
              className={onInk}
              onClick={() => {
                const chosen = selection.books;
                selection.clear();
                for (const b of chosen) void actions.rescan(b);
              }}
            >
              <RescanIcon aria-hidden="true" />
              <span className="max-md:sr-only">{t(FIX_LOOK.rescan.label)}</span>
            </Button>
          ) : null}
          <Button
            variant="ghost"
            size="sm"
            className={onInk}
            onClick={() => void act(selection.books)}
          >
            {ignored ? <Eye aria-hidden="true" /> : <EyeOff aria-hidden="true" />}
            <span className="max-md:sr-only">
              {ignored ? t('health.unignore') : t('health.ignore')}
            </span>
          </Button>
          <span
            className="mx-1 h-[22px] w-px bg-[color-mix(in_oklab,var(--primary-foreground)_20%,transparent)]"
            aria-hidden="true"
          />
          <Button
            variant="ghost"
            size="icon-sm"
            className={onInk}
            aria-label={t('books.bulk.clear')}
            onClick={selection.clear}
          >
            <X aria-hidden="true" />
          </Button>
        </div>
      ) : null}
    </>
  );
}

/** A category's fix as an outline button (the label hides on phones; the icon names it). */
function FixButton({ kind, onClick }: { kind: BookIssueKind; onClick: () => void }) {
  const { t } = useTranslation();
  const fix = FIXES[kind];
  if (!fix) return null;
  const { icon: Icon, label } = FIX_LOOK[fix];
  return (
    <Button variant="outline" size="sm" onClick={onClick} aria-label={t(label)}>
      <Icon aria-hidden="true" />
      <span className="max-md:hidden">{t(label)}</span>
    </Button>
  );
}
