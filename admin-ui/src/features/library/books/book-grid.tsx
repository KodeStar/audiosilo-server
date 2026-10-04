import { useMemo, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import type { AdminBook } from '@/api/types';
import { refKey } from '@/lib/book-route';
import { chunk } from '@/lib/utils';
import { BookTile } from '../book-tile';
import { gridLayout } from './books-model';
import { useIsPhone, useWidth, useWindowRows } from './use-layout';
import type { Selection } from './use-selection';

/** What the grid and the table views take (BooksPage renders one or the other). */
export interface BookViewProps {
  books: AdminBook[];
  selection: Selection;
  metadataOn: boolean;
  /** When the page opened: "added" times are relative to it. */
  now: number;
  hasMore: boolean;
  loadingMore: boolean;
  onLoadMore: () => void;
}

/**
 * The cover grid, virtualized by rows against the window scroll. The column
 * count follows the container's width with the same rule as .shelf-grid, and
 * each row's height is the cover plus its text, so nothing has to be measured.
 * Nearing the end loads the next page.
 */
export function BookGrid({
  books,
  selection,
  metadataOn,
  hasMore,
  loadingMore,
  onLoadMore,
}: BookViewProps) {
  const { t } = useTranslation();
  const ref = useRef<HTMLDivElement>(null);
  const width = useWidth(ref);
  const phone = useIsPhone();
  const layout = gridLayout(width, phone);
  const rows = useMemo(() => chunk(books, layout.columns), [books, layout.columns]);
  const { virtualizer, items, margin } = useWindowRows({
    ref,
    count: rows.length,
    rowHeight: layout.rowHeight,
    gap: layout.rowGap,
    overscan: 3,
    hasMore,
    loadingMore,
    onLoadMore,
  });

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
              transform: `translateY(${item.start - margin}px)`,
              height: item.size,
              gridTemplateColumns: `repeat(${layout.columns}, minmax(0, 1fr))`,
              columnGap: layout.columnGap,
            }}
          >
            {rows[item.index]?.map((b) => (
              <div key={refKey(b)} role="listitem" className="min-w-0">
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
