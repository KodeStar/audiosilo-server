import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { BookOpen, LoaderCircle, Pencil, Plus, SearchX, Share2 } from 'lucide-react';
import { BULK_LIMIT } from '@/api/client';
import { useAdminBooks, useBookFacets, useBookLibraries, useServerInfo } from '@/api/hooks';
import type { AdminBook, AdminLibrary } from '@/api/types';
import { EmptyState } from '@/components/empty-state';
import { Page } from '@/components/page';
import { QueryError } from '@/components/query-error';
import { Button, buttonVariants } from '@/components/ui/button';
import { toastError } from '@/lib/errors';
import { counted, formatNumber } from '@/lib/format';
import { cn } from '@/lib/utils';
import { BulkAction, BulkBar } from '@/components/bulk-bar';
import { AddToShareDialog } from '../add-to-share-dialog';
import { LibraryFilter } from '../library-filter';
import { useLibrarySearch, useUpdateSearch } from '../library-param';
import { BookGrid, GridSkeleton } from './book-grid';
import { bookFilter, isBrowsing, listParams, withoutFilters } from './books-model';
import { ActiveChips, BooksToolbar } from './books-toolbar';
import { BulkEditDialog } from './bulk-edit-dialog';
import { FilterSheet } from './filter-sheet';
import { Shelves } from './shelves';
import { useKeepListInView } from './use-keep-list-in-view';
import { useSelection, type Selection } from './use-selection';

const NO_BOOKS: AdminBook[] = [];
const NO_LIBRARIES: AdminLibrary[] = [];

// The cover grid is the default view: the table (and TanStack Table) loads on first use.
const BooksTable = lazy(() => import('./books-table').then((m) => ({ default: m.BooksTable })));

/**
 * Library > Books: every book by its cover. Browsing (no search or filter)
 * shows two shelves above the full list; the list filters by library, text and
 * facets (all in the URL, so every view deep-links), sorts, switches between
 * the cover grid and a table, and selects books for bulk edits and shares.
 * A change to the list made while scrolled past its toolbar brings the
 * toolbar back to the top (useKeepListInView). Podcast libraries aren't here:
 * their episodes aren't books.
 */
export function BooksPage() {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const search = useLibrarySearch();
  const { anchor, update } = useKeepListInView(useUpdateSearch(), search);

  // "Added in the last 7 days" counts from when the page opened, so the query key holds still.
  const [now] = useState(() => Date.now());
  const metadataOn = !!useServerInfo().data?.capabilities.metadata;
  const libs = useBookLibraries() ?? NO_LIBRARIES;
  const params = useMemo(() => listParams(search, now), [search, now]);
  const filter = useMemo(() => bookFilter(search, now), [search, now]);
  const list = useAdminBooks(params);
  const facets = useBookFacets(filter);
  const browsing = isBrowsing(search);

  const books = useMemo(
    () => list.data?.pages.flatMap((p) => p.books ?? []) ?? NO_BOOKS,
    [list.data],
  );
  // The list as shown, for Shift-click ranges.
  const selection = useSelection(books);
  const [sheetOpen, setSheetOpen] = useState(false);
  const [dialog, setDialog] = useState<'edit' | 'share' | null>(null);
  const { clear } = selection;
  // Esc clears the selection (a dialog or the sheet takes Esc for itself first).
  useEffect(() => {
    if (!selection.selecting || dialog || sheetOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.defaultPrevented) clear();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selection.selecting, dialog, sheetOpen, clear]);

  const { fetchNextPage } = list;
  const loadMore = useCallback(() => void fetchNextPage(), [fetchNextPage]);
  const empty = browsing && !!list.data && !list.isPlaceholderData && books.length === 0;
  const total = facets.data?.total;
  const heading = browsing
    ? t('books.all')
    : total !== undefined
      ? t('books.count', counted(total, lang))
      : t('books.all');

  let content: React.ReactNode;
  if (list.isError) {
    content = (
      <QueryError title={t('books.error')} error={list.error} onRetry={() => void list.refetch()} />
    );
  } else if (!list.data) {
    content = <GridSkeleton />;
  } else if (books.length === 0) {
    content = (
      <EmptyState
        icon={SearchX}
        title={t('books.noMatch.title')}
        body={t('books.noMatch.body')}
        action={
          <Button variant="outline" onClick={() => update((prev) => withoutFilters(prev))}>
            {t('books.noMatch.clear')}
          </Button>
        }
      />
    );
  } else {
    const View = search.view === 'table' ? BooksTable : BookGrid;
    content = (
      <Suspense fallback={<GridSkeleton />}>
        <View
          books={books}
          selection={selection}
          metadataOn={metadataOn}
          now={now}
          hasMore={!!list.hasNextPage}
          loadingMore={list.isFetchingNextPage}
          onLoadMore={loadMore}
          series={search.series}
        />
      </Suspense>
    );
  }

  return (
    <Page>
      <h1 className="sr-only">{t('books.heading')}</h1>
      {empty ? (
        <EmptyLibrary hasLibraries={libs.length > 0} />
      ) : (
        <div className={cn(selection.selecting && 'selecting')}>
          <LibraryFilter counts={facets.data?.libraries} className="mb-6" />
          {browsing ? <Shelves selection={selection} metadataOn={metadataOn} now={now} /> : null}
          {/* Where the list starts: a change to it while scrolled past brings this back up. */}
          <div
            ref={anchor}
            aria-hidden="true"
            className="scroll-mt-[calc(var(--topbar-h)+var(--subbar-h)+2px)]"
          />
          <BooksToolbar
            heading={heading}
            search={search}
            update={update}
            onOpenFilters={() => setSheetOpen(true)}
            selecting={selection.selecting}
            onToggleSelect={() => (selection.selecting ? clear() : selection.setMode(true))}
          />
          <ActiveChips search={search} update={update} libraries={libs} />
          {selection.selecting && books.length > 0 ? (
            <SelectAllRow
              books={books}
              total={total}
              selection={selection}
              hasMore={!!list.hasNextPage}
              fetchNextPage={list.fetchNextPage}
              listKey={JSON.stringify(params)}
            />
          ) : null}
          {content}
        </div>
      )}
      <BulkBar count={selection.size} label={t('books.bulk.label')} onClear={clear}>
        <BulkAction icon={Pencil} label={t('books.bulk.edit')} onClick={() => setDialog('edit')} />
        <BulkAction
          icon={Share2}
          label={t('books.bulk.share')}
          onClick={() => setDialog('share')}
        />
      </BulkBar>
      <FilterSheet
        open={sheetOpen}
        onOpenChange={setSheetOpen}
        search={search}
        update={update}
        facets={facets.data}
        libraries={libs}
        metadataOn={metadataOn}
      />
      <BulkEditDialog
        open={dialog === 'edit'}
        onOpenChange={(o) => setDialog(o ? 'edit' : null)}
        books={selection.books}
        onDone={clear}
      />
      <AddToShareDialog
        open={dialog === 'share'}
        onOpenChange={(o) => setDialog(o ? 'share' : null)}
        books={selection.books}
        onDone={clear}
      />
    </Page>
  );
}

/**
 * While selecting: select every book shown (the grid has no header checkbox),
 * and, when the filter matches more than are loaded, every matching book, the
 * rest of the list fetched page by page here, up to BULK_LIMIT (what one
 * bulk action takes). A selection past that says so.
 *
 * `listKey` names the list (its filter and sort): a fetch still running when
 * it changes, or when the row goes (the selection ended), selects nothing, as
 * fetchNextPage follows the list as it is now and its pages would be another
 * filter's books.
 */
function SelectAllRow({
  books,
  total,
  selection,
  hasMore,
  fetchNextPage,
  listKey,
}: {
  books: AdminBook[];
  total: number | undefined;
  selection: Selection;
  hasMore: boolean;
  fetchNextPage: ReturnType<typeof useAdminBooks>['fetchNextPage'];
  listKey: string;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [loading, setLoading] = useState(false);
  const allShown = books.every(selection.isSelected);
  const matching = total ?? books.length;
  const capped = matching > BULK_LIMIT;
  // Which run may still select: a new list or the row going moves it on.
  const run = useRef(0);
  useEffect(
    () => () => {
      run.current += 1;
    },
    [listKey],
  );

  const selectMatching = async () => {
    const mine = run.current;
    setLoading(true);
    try {
      let all = books;
      let more = hasMore;
      while (more && all.length < BULK_LIMIT) {
        const r = await fetchNextPage();
        if (run.current !== mine) return;
        if (r.isError) throw r.error;
        all = r.data?.pages.flatMap((p) => p.books ?? []) ?? all;
        more = !!r.hasNextPage;
      }
      selection.setMany(all.slice(0, BULK_LIMIT), true);
    } catch (err) {
      if (run.current === mine) toastError(t('books.select.failed'), err);
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="mb-4 flex flex-wrap items-center gap-x-3 gap-y-1.5 text-[13px]">
      <Button
        variant="outline"
        size="sm"
        disabled={allShown}
        onClick={() => selection.setMany(books.slice(0, BULK_LIMIT), true)}
      >
        {hasMore
          ? t('books.select.shown', counted(Math.min(books.length, BULK_LIMIT), lang))
          : t('books.select.all', counted(Math.min(books.length, BULK_LIMIT), lang))}
      </Button>
      {hasMore ? (
        <Button
          variant="outline"
          size="sm"
          disabled={loading}
          onClick={() => void selectMatching()}
        >
          {loading ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
          {capped
            ? t('books.select.first', counted(BULK_LIMIT, lang))
            : t('books.select.matching', counted(matching, lang))}
        </Button>
      ) : null}
      {capped || selection.size > BULK_LIMIT ? (
        <span className="text-muted-foreground">
          {t('books.select.cap', { max: formatNumber(BULK_LIMIT, lang) })}
        </span>
      ) : (
        <span className="text-muted-foreground max-md:hidden">{t('books.select.shiftHint')}</span>
      )}
    </div>
  );
}

/** No books at all: point to Library > Libraries. */
function EmptyLibrary({ hasLibraries }: { hasLibraries: boolean }) {
  const { t } = useTranslation();
  return (
    <EmptyState
      icon={BookOpen}
      title={t('books.empty.title')}
      body={t(hasLibraries ? 'books.empty.bodyScan' : 'books.empty.body')}
      action={
        <Link
          to="/library/{-$section}"
          params={{ section: 'libraries' }}
          search={hasLibraries ? {} : { add: true }}
          className={buttonVariants()}
        >
          {hasLibraries ? null : <Plus aria-hidden="true" />}
          {t(hasLibraries ? 'books.empty.open' : 'books.empty.add')}
        </Link>
      }
    />
  );
}
