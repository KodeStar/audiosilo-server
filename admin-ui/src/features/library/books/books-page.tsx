import { Suspense, lazy, useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useSearch } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { BookOpen, Plus, SearchX } from 'lucide-react';
import { useAdminBooks, useBookFacets, useLibraries, useServerInfo } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { EmptyState } from '@/components/empty-state';
import { Page } from '@/components/page';
import { QueryError } from '@/components/query-error';
import { Button, buttonVariants } from '@/components/ui/button';
import { counted } from '@/lib/format';
import { cn } from '@/lib/utils';
import { AddToShareDialog } from '../add-to-share-dialog';
import { LibraryFilter } from '../library-filter';
import { useUpdateSearch } from '../library-param';
import type { LibrarySearch } from '../library-search';
import { BookGrid, GridSkeleton } from './book-grid';
import { bookFilter, isBrowsing, listParams, withoutFilters } from './books-model';
import { ActiveChips, BooksToolbar } from './books-toolbar';
import { BulkBar } from './bulk-bar';
import { BulkEditDialog } from './bulk-edit-dialog';
import { FilterSheet } from './filter-sheet';
import { Shelves } from './shelves';
import { useSelection } from './use-selection';

const NO_BOOKS: AdminBook[] = [];

// The cover grid is the default view: the table (and TanStack Table) loads on first use.
const BooksTable = lazy(() => import('./books-table').then((m) => ({ default: m.BooksTable })));

/**
 * Library > Books: every book by its cover. Browsing (no search or filter)
 * shows two shelves above the full list; the list filters by library, text and
 * facets (all in the URL, so every view deep-links), sorts, switches between
 * the cover grid and a table, and selects books for bulk edits and shares.
 */
export function BooksPage() {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const search = useSearch({ strict: false }) as LibrarySearch;
  const update = useUpdateSearch();

  // "Added in the last 7 days" counts from when the page opened, so the query key holds still.
  const [now] = useState(() => Date.now());
  const metadataOn = !!useServerInfo().data?.capabilities.metadata;
  const libraries = useLibraries();
  const params = useMemo(() => listParams(search, now), [search, now]);
  const filter = useMemo(() => bookFilter(search, now), [search, now]);
  const list = useAdminBooks(params);
  const facets = useBookFacets(filter);
  const browsing = isBrowsing(search);

  const selection = useSelection();
  const [sheetOpen, setSheetOpen] = useState(false);
  const [dialog, setDialog] = useState<'edit' | 'share' | null>(null);
  const { clear } = selection;
  // Esc clears the selection (a dialog or the sheet takes Esc for itself first).
  useEffect(() => {
    if (!selection.size || dialog || sheetOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.defaultPrevented) clear();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selection.size, dialog, sheetOpen, clear]);

  const books = useMemo(
    () => list.data?.pages.flatMap((p) => p.books ?? []) ?? NO_BOOKS,
    [list.data],
  );
  const { fetchNextPage } = list;
  const loadMore = useCallback(() => void fetchNextPage(), [fetchNextPage]);
  const libs = libraries.data ?? [];
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
        <div className={cn(selection.size > 0 && 'selecting')}>
          <LibraryFilter counts={facets.data?.libraries} className="mb-6" />
          {browsing ? <Shelves selection={selection} metadataOn={metadataOn} now={now} /> : null}
          <BooksToolbar
            heading={heading}
            search={search}
            update={update}
            onOpenFilters={() => setSheetOpen(true)}
          />
          <ActiveChips search={search} update={update} libraries={libs} />
          {content}
        </div>
      )}
      <BulkBar
        count={selection.size}
        onEdit={() => setDialog('edit')}
        onShare={() => setDialog('share')}
        onClear={clear}
      />
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
