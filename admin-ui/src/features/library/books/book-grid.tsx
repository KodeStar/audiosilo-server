import { useEffect, useMemo, useRef } from 'react';
import { useWindowVirtualizer } from '@tanstack/react-virtual';
import { useTranslation } from 'react-i18next';
import type { AdminBook } from '@/api/types';
import { BookTile } from '../book-tile';
import { gridLayout, selectionKey, shouldLoadMore, toRows } from './books-model';
import { useDocumentTop, useIsPhone, useWidth } from './use-layout';
import type { Selection } from './use-selection';

/** Before the first measurement (and in jsdom): a desktop-sized window. */
const INITIAL_RECT = { width: 1024, height: 900 };

/**
 * The cover grid, virtualized by rows against the window scroll (the page
 * scrolls, not an inner box). The column count follows the container's width
 * with the same rule as .shelf-grid, and each row's height is the cover plus its
 * text, so nothing has to be measured. Nearing the end loads the next page.
 */
export function BookGrid({
  books,
  selection,
  metadataOn,
  hasMore,
  loadingMore,
  onLoadMore,
}: {
  books: AdminBook[];
  selection: Selection;
  metadataOn: boolean;
  hasMore: boolean;
  loadingMore: boolean;
  onLoadMore: () => void;
}) {
  const { t } = useTranslation();
  const ref = useRef<HTMLDivElement>(null);
  const width = useWidth(ref);
  const phone = useIsPhone();
  const top = useDocumentTop(ref);
  const layout = gridLayout(width, phone);
  const rows = useMemo(() => toRows(books, layout.columns), [books, layout.columns]);

  const virtualizer = useWindowVirtualizer({
    count: rows.length,
    estimateSize: () => layout.rowHeight,
    gap: layout.rowGap,
    overscan: 3,
    scrollMargin: top,
    initialRect: INITIAL_RECT,
    useFlushSync: false,
  });
  // Sizes come from the estimate; a new width (or phone layout) means new rows.
  useEffect(() => virtualizer.measure(), [virtualizer, layout.rowHeight]);

  const items = virtualizer.getVirtualItems();
  const last = items.at(-1)?.index;
  useEffect(() => {
    if (shouldLoadMore(last, rows.length, hasMore, loadingMore)) onLoadMore();
  }, [last, rows.length, hasMore, loadingMore, onLoadMore]);

  const selecting = selection.size > 0;
  return (
    <>
      <div
        ref={ref}
        role="list"
        aria-label={t('books.all')}
        className="relative"
        style={{ height: virtualizer.getTotalSize() }}
      >
        {items.map((item) => (
          <div
            key={item.key}
            role="presentation"
            className="absolute inset-x-0 top-0 grid"
            style={{
              transform: `translateY(${item.start - virtualizer.options.scrollMargin}px)`,
              height: item.size,
              gridTemplateColumns: `repeat(${layout.columns}, minmax(0, 1fr))`,
              columnGap: layout.columnGap,
            }}
          >
            {rows[item.index]?.map((b) => (
              <div key={selectionKey(b)} role="listitem" className="min-w-0">
                <BookTile
                  book={b}
                  metadataOn={metadataOn}
                  selected={selection.isSelected(b)}
                  selecting={selecting}
                  onToggle={selection.toggle}
                />
              </div>
            ))}
          </div>
        ))}
      </div>
      {loadingMore ? <LoadingMore /> : null}
    </>
  );
}

/** Square placeholders exactly in the grid's slots while the first page loads. */
export function GridSkeleton({ count = 12 }: { count?: number }) {
  const { t } = useTranslation();
  return (
    <div className="shelf-grid" role="status" aria-label={t('common.loading')}>
      {Array.from({ length: count }, (_, i) => (
        <div key={i} className="tile" aria-hidden="true">
          <span className="cover-wrap">
            <span className="cover skel block" />
          </span>
          <span className="flex flex-col gap-1.5">
            <span className="skel h-3.5 w-4/5" />
            <span className="skel h-3 w-1/2" />
          </span>
        </div>
      ))}
    </div>
  );
}

export function LoadingMore() {
  const { t } = useTranslation();
  return (
    <p role="status" className="mt-7 text-center text-[12.5px] text-muted-foreground">
      {t('books.loadingMore')}
    </p>
  );
}
