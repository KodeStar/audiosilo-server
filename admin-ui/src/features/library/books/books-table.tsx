import { useEffect, useMemo, useRef } from 'react';
import { Link, useNavigate } from '@tanstack/react-router';
import { createColumnHelper, tableFeatures, useTable } from '@tanstack/react-table';
import { useWindowVirtualizer } from '@tanstack/react-virtual';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import type { AdminBook } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { ProvenanceMarker } from '@/components/provenance';
import { Badge } from '@/components/ui/badge';
import { Checkbox } from '@/components/ui/checkbox';
import { bookRoute } from '@/lib/book-route';
import { formatNumber, formatRelative } from '@/lib/format';
import { cn } from '@/lib/utils';
import { LoadingMore } from './book-grid';
import { formatDuration, isMatched, selectionKey, shouldLoadMore } from './books-model';
import { useDocumentTop, useIsPhone } from './use-layout';
import type { Selection } from './use-selection';

const features = tableFeatures({});
const column = createColumnHelper<typeof features, AdminBook>();

/** Row heights: a 36px cover plus padding; the phone's stacked row adds a subtitle. */
const ROW_HEIGHT = { desktop: 57, phone: 69 };
const INITIAL_RECT = { width: 1024, height: 900 };

/**
 * Per-column cell classes. On a phone the table stacks (STYLEGUIDE.md "Table"):
 * the checkbox, the title (with author and length under it) and the metadata
 * status stay; the rest are hidden.
 */
const CELL: Record<string, string> = {
  select: 'w-11 pr-0',
  title: 'min-w-[240px] max-md:min-w-0',
  author: 'max-md:hidden',
  narrator: 'max-w-[200px] truncate text-muted-foreground max-md:hidden',
  series: 'text-muted-foreground max-md:hidden',
  length: 'text-right tabular-nums max-md:hidden',
  format: 'max-md:hidden',
  playback: 'max-md:hidden',
  metadata: '',
  added: 'text-right text-muted-foreground tabular-nums max-md:hidden',
};

function bookColumns(t: TFunction, lang: string, metadataOn: boolean, now: number) {
  return column.columns([
    column.display({ id: 'select', header: () => null, cell: () => null }),
    column.accessor('title', {
      header: () => t('books.col.title'),
      cell: () => null,
    }),
    column.accessor('author', { header: () => t('books.col.author'), cell: (c) => c.getValue() }),
    column.accessor('narrator', {
      header: () => t('books.col.narrator'),
      cell: (c) => c.getValue(),
    }),
    column.accessor('series', {
      header: () => t('books.col.series'),
      cell: ({ row: { original: b } }) =>
        b.series ? (
          <>
            {b.series}
            {b.series_index > 0 ? (
              <span className="tabular-nums">
                {' '}
                {t('books.tile.seriesIndex', { index: formatNumber(b.series_index, lang) })}
              </span>
            ) : null}
          </>
        ) : null,
    }),
    column.accessor('duration', {
      id: 'length',
      header: () => t('books.col.length'),
      cell: (c) => formatDuration(c.getValue(), lang),
    }),
    column.accessor('format', {
      header: () => t('books.col.format'),
      cell: ({ row: { original: b } }) => (
        <span className="font-mono text-[12.5px]">
          {b.file_count > 1
            ? t('books.table.formatFiles', { format: b.format, files: b.file_count })
            : b.format}
        </span>
      ),
    }),
    column.accessor('direct_playable', {
      id: 'playback',
      header: () => t('books.col.playback'),
      cell: (c) =>
        c.getValue() ? (
          <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
            <span className="dot" aria-hidden="true" />
            {t('books.table.direct')}
          </span>
        ) : (
          <span className="inline-flex items-center gap-1.5 whitespace-nowrap text-warning">
            <span className="dot" data-tone="warn" aria-hidden="true" />
            {t('books.table.transcode')}
          </span>
        ),
    }),
    ...(metadataOn
      ? [
          column.display({
            id: 'metadata',
            header: () => t('books.col.metadata'),
            cell: ({ row: { original: b } }) =>
              isMatched(b) ? (
                <ProvenanceMarker source="community" short />
              ) : (
                <Badge variant="outline">{t('books.table.unmatched')}</Badge>
              ),
          }),
        ]
      : []),
    column.accessor('added_at', {
      id: 'added',
      header: () => t('books.col.added'),
      cell: (c) => formatRelative(c.getValue(), lang, now),
    }),
  ]);
}

/**
 * The table view: TanStack Table for the columns, rows virtualized against the
 * window scroll (spacer rows above and below keep it a real <table>). The header
 * checkbox selects the loaded rows. A row opens its book, or toggles it while a
 * selection is active.
 */
export function BooksTable({
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
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const navigate = useNavigate();
  const ref = useRef<HTMLDivElement>(null);
  const top = useDocumentTop(ref);
  const phone = useIsPhone();
  const rowHeight = phone ? ROW_HEIGHT.phone : ROW_HEIGHT.desktop;
  const now = useMemo(() => Date.now(), []);
  const columns = useMemo(() => bookColumns(t, lang, metadataOn, now), [t, lang, metadataOn, now]);
  const table = useTable({ features, columns, data: books, getRowId: (b) => selectionKey(b) });
  const rows = table.getRowModel().rows;

  const virtualizer = useWindowVirtualizer({
    count: rows.length,
    estimateSize: () => rowHeight,
    overscan: 8,
    scrollMargin: top,
    initialRect: INITIAL_RECT,
    useFlushSync: false,
  });
  useEffect(() => virtualizer.measure(), [virtualizer, rowHeight]);
  const items = virtualizer.getVirtualItems();
  const last = items.at(-1)?.index;
  useEffect(() => {
    if (shouldLoadMore(last, rows.length, hasMore, loadingMore, 10)) onLoadMore();
  }, [last, rows.length, hasMore, loadingMore, onLoadMore]);

  const margin = virtualizer.options.scrollMargin;
  const padTop = items.length ? items[0].start - margin : 0;
  const padBottom = items.length ? virtualizer.getTotalSize() - (items.at(-1)!.end - margin) : 0;
  const colCount = columns.length;

  const selecting = selection.size > 0;
  const allOn = books.length > 0 && books.every(selection.isSelected);
  const someOn = !allOn && books.some(selection.isSelected);
  const open = (b: AdminBook) =>
    selecting ? selection.toggle(b) : void navigate(bookRoute(b.library_id, b.path));

  return (
    <>
      <div ref={ref} className="overflow-x-auto rounded-xl border bg-card">
        <table
          className="w-full min-w-[980px] text-[13.5px] max-md:min-w-0"
          aria-label={t('books.all')}
        >
          <thead className="max-md:hidden">
            {table.getHeaderGroups().map((group) => (
              <tr key={group.id} className="border-b">
                {group.headers.map((header) => (
                  <th
                    key={header.id}
                    scope="col"
                    className={cn(
                      'h-10 px-3 text-left text-[12px] font-semibold whitespace-nowrap text-muted-foreground',
                      CELL[header.column.id],
                    )}
                  >
                    {header.column.id === 'select' ? (
                      <Checkbox
                        checked={allOn}
                        indeterminate={someOn}
                        onCheckedChange={() => selection.setMany(books, !allOn)}
                        aria-label={t('books.table.selectAll')}
                      />
                    ) : (
                      <table.FlexRender header={header} />
                    )}
                  </th>
                ))}
              </tr>
            ))}
          </thead>
          <tbody>
            {padTop > 0 ? (
              <tr aria-hidden="true">
                <td colSpan={colCount} style={{ height: padTop }} />
              </tr>
            ) : null}
            {items.map((item) => {
              const row = rows[item.index];
              if (!row) return null;
              const b = row.original;
              const on = selection.isSelected(b);
              return (
                <tr
                  key={row.id}
                  data-selected={on || undefined}
                  onClick={() => open(b)}
                  className="cursor-pointer border-b transition-colors duration-(--dur-1) last:border-b-0 hover:bg-muted/70 data-selected:bg-[color-mix(in_oklab,var(--brand)_7%,transparent)] max-md:grid max-md:grid-cols-[auto_minmax(0,1fr)_auto] max-md:items-center max-md:gap-x-3 max-md:px-3.5"
                  style={{ height: rowHeight }}
                >
                  {row.getAllCells().map((cell) => {
                    const id = cell.column.id;
                    return (
                      <td
                        key={cell.id}
                        className={cn('px-3 align-middle max-md:p-0', CELL[id])}
                        onClick={id === 'select' ? (e) => e.stopPropagation() : undefined}
                      >
                        {id === 'select' ? (
                          <Checkbox
                            checked={on}
                            onCheckedChange={() => selection.toggle(b)}
                            aria-label={t('books.tile.select', { title: b.title })}
                          />
                        ) : id === 'title' ? (
                          <TitleCell book={b} selecting={selecting} onToggle={selection.toggle} />
                        ) : (
                          <table.FlexRender cell={cell} />
                        )}
                      </td>
                    );
                  })}
                </tr>
              );
            })}
            {padBottom > 0 ? (
              <tr aria-hidden="true">
                <td colSpan={colCount} style={{ height: padBottom }} />
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>
      {loadingMore ? <LoadingMore /> : null}
    </>
  );
}

/** The cover and the title (a link, for the keyboard); on a phone, author and length under it. */
function TitleCell({
  book: b,
  selecting,
  onToggle,
}: {
  book: AdminBook;
  selecting: boolean;
  onToggle: (b: AdminBook) => void;
}) {
  const { i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const length = formatDuration(b.duration, lang);
  return (
    <div className="flex min-w-0 items-center gap-3">
      <span className="w-9 shrink-0">
        <BookCover
          libraryId={b.library_id}
          path={b.path}
          title={b.title}
          author={b.author}
          size={160}
        />
      </span>
      <span className="flex min-w-0 flex-col">
        <Link
          {...bookRoute(b.library_id, b.path)}
          onClick={(e) => {
            e.stopPropagation();
            if (!selecting) return;
            e.preventDefault();
            onToggle(b);
          }}
          className="max-w-[320px] truncate font-semibold hover:underline hover:underline-offset-3"
        >
          {b.title}
        </Link>
        <span className="truncate text-[12px] text-muted-foreground md:hidden">
          {[b.author, length].filter(Boolean).join(' · ')}
        </span>
      </span>
    </div>
  );
}
