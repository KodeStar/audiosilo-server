import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Eye, EyeOff } from 'lucide-react';
import { useAdminBooks, useLibraryRoots } from '@/api/hooks';
import { BookCover } from '@/components/book-cover';
import { BulkAction, BulkBar } from '@/components/bulk-bar';
import { QueryError } from '@/components/query-error';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import { useSelection } from '@/features/library/books/use-selection';
import { bookRoute, refKey } from '@/lib/book-route';
import { counted } from '@/lib/format';
import { joinLibraryPath } from '@/lib/paths';
import { AllClear } from './all-clear';
import {
  FIXES,
  FIX_LOOK,
  issueReason,
  say,
  type BookIssueKind,
  type IssueFix,
} from './issues-model';
import { useIssueActions } from './use-issue-actions';

/** How many books a page of an issue list loads. */
const PAGE = 50;

/**
 * One category's books (STYLEGUIDE.md "Health triage"): cover, title, why, where,
 * Ignore and the category's one fix; select several for the floating bar. With
 * `ignored`, the books an admin ignored, each with Show again.
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
  const roots = useLibraryRoots();
  const books = list.data?.pages.flatMap((p) => p.books ?? []) ?? [];
  const fix = ignored ? null : FIXES[kind];

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
  if (books.length === 0) return <AllClear ignored={ignored} />;

  const allSelected = books.every(selection.isSelected);
  // What the bar acts on: the selected books still listed (one ignored or fixed on
  // its own row has left the list but stays selected).
  const listed = new Set(books.map(refKey));
  const selectedListed = selection.books.filter((b) => listed.has(refKey(b)));
  const chosenBooks = () => {
    selection.clear();
    return selectedListed;
  };
  /** Ignores (or shows again) the selection, and clears it. */
  const toggleIgnore = () => {
    const chosen = chosenBooks();
    void (ignored ? actions.unignore(chosen) : actions.ignore(chosen));
  };

  return (
    <>
      <div className="mb-2.5 flex justify-end">
        <Button variant="ghost" size="sm" onClick={() => selection.setMany(books, !allSelected)}>
          {allSelected ? t('health.selectNone') : t('health.selectAll')}
        </Button>
      </div>
      <ul className="divide-y rounded-xl border bg-card" aria-label={t(`health.kind.${kind}`)}>
        {books.map((b) => (
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
                {say(t, issueReason(kind, b), lang)}
              </span>
              <span className="truncate font-mono text-[11.5px] text-subtle-foreground max-md:hidden">
                {joinLibraryPath(roots[b.library_id], b.path)}
              </span>
            </div>
            <div className="flex shrink-0 items-center gap-1.5">
              {ignored ? (
                <Button variant="outline" size="sm" onClick={() => void actions.unignore([b])}>
                  <Eye aria-hidden="true" />
                  <span className="max-md:sr-only">{t('health.unignore')}</span>
                </Button>
              ) : (
                <Button variant="ghost" size="sm" onClick={() => void actions.ignore([b])}>
                  <EyeOff aria-hidden="true" className="md:hidden" />
                  <span className="max-md:sr-only">{t('health.ignore')}</span>
                </Button>
              )}
              {fix ? (
                <FixButton
                  fix={fix}
                  disabled={actions.fixing(b)}
                  onClick={() => void actions.fix(b)}
                />
              ) : null}
            </div>
          </li>
        ))}
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

      <BulkBar
        count={selectedListed.length}
        label={t('health.bulk.label')}
        onClear={selection.clear}
      >
        {fix === 'rescan' ? (
          <BulkAction
            icon={FIX_LOOK.rescan.icon}
            label={t(FIX_LOOK.rescan.label)}
            onClick={() => void actions.rescanMany(chosenBooks())}
          />
        ) : null}
        <BulkAction
          icon={ignored ? Eye : EyeOff}
          label={ignored ? t('health.unignore') : t('health.ignore')}
          onClick={toggleIgnore}
        />
      </BulkBar>
    </>
  );
}

/** A category's fix as an outline button (the label hides on phones; the icon names it). */
function FixButton({
  fix,
  disabled,
  onClick,
}: {
  fix: NonNullable<IssueFix>;
  disabled: boolean;
  onClick: () => void;
}) {
  const { t } = useTranslation();
  const { icon: Icon, label } = FIX_LOOK[fix];
  return (
    <Button variant="outline" size="sm" onClick={onClick} disabled={disabled} aria-label={t(label)}>
      <Icon aria-hidden="true" />
      <span className="max-md:hidden">{t(label)}</span>
    </Button>
  );
}
