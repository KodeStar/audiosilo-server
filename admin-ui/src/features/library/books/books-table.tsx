import { memo, useCallback, useMemo, useRef } from 'react';
import { Link, useNavigate } from '@tanstack/react-router';
import {
  FlexRender,
  createColumnHelper,
  tableFeatures,
  useTable,
  type Row,
} from '@tanstack/react-table';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import type { AdminBook } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { BooksLink } from '@/components/books-link';
import { PlaybackStatus } from '@/components/playback-status';
import { ProvenanceMarker } from '@/components/provenance';
import { Badge } from '@/components/ui/badge';
import { Checkbox } from '@/components/ui/checkbox';
import { bookRoute, isBooksField, refKey } from '@/lib/book-route';
import { formatDuration, formatRelative, seriesIndexLabel } from '@/lib/format';
import { cn } from '@/lib/utils';
import { positionIn } from '../series/series-model';
import { LoadingMore, type BookViewProps } from './book-grid';
import { useIsPhone, useWindowRows } from './use-layout';

const features = tableFeatures({});
const column = createColumnHelper<typeof features, AdminBook>();

/** Row heights: a 36px cover plus padding; the phone's stacked row adds a subtitle. */
const ROW_HEIGHT = { desktop: 57, phone: 69 };

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

function bookColumns(
  t: TFunction,
  lang: string,
  metadataOn: boolean,
  now: number,
  filtered?: string,
) {
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
      // Filtered to one series: the book's place in that one.
      cell: ({ row: { original: b } }) => {
        const name = filtered ?? b.series;
        const position = filtered ? positionIn(b, filtered) : b.series_index;
        return name ? (
          <>
            {name}
            {position > 0 ? (
              <span className="tabular-nums"> {seriesIndexLabel(position, lang, t)}</span>
            ) : null}
          </>
        ) : null;
      },
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
      cell: (c) => <PlaybackStatus direct={c.getValue()} />,
    }),
    ...(metadataOn
      ? [
          column.display({
            id: 'metadata',
            header: () => t('books.col.metadata'),
            cell: ({ row: { original: b } }) =>
              b.matched ? (
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

type Columns = ReturnType<typeof bookColumns>;

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
  now,
  hasMore,
  loadingMore,
  onLoadMore,
  series,
}: BookViewProps) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const navigate = useNavigate();
  const ref = useRef<HTMLDivElement>(null);
  const phone = useIsPhone();
  const rowHeight = phone ? ROW_HEIGHT.phone : ROW_HEIGHT.desktop;
  const columns = useMemo(
    () => bookColumns(t, lang, metadataOn, now, series),
    [t, lang, metadataOn, now, series],
  );
  const table = useTable({ features, columns, data: books, getRowId: refKey });
  const rows = table.getRowModel().rows;
  const { virtualizer, items, margin } = useWindowRows({
    ref,
    count: rows.length,
    rowHeight,
    overscan: 8,
    ahead: 10,
    hasMore,
    loadingMore,
    onLoadMore,
  });

  const padTop = items.length ? items[0].start - margin : 0;
  const padBottom = items.length ? virtualizer.getTotalSize() - (items.at(-1)!.end - margin) : 0;
  const colCount = columns.length;

  const selecting = selection.size > 0;
  const allOn = books.length > 0 && books.every(selection.isSelected);
  const someOn = !allOn && books.some(selection.isSelected);
  const open = useCallback(
    (b: AdminBook) => void navigate(bookRoute(b.library_id, b.path)),
    [navigate],
  );

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
                      <FlexRender header={header} />
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
              return row ? (
                <BookRow
                  key={row.id}
                  row={row}
                  columns={columns}
                  series={series}
                  height={rowHeight}
                  selected={selection.isSelected(row.original)}
                  selecting={selecting}
                  onToggle={selection.toggle}
                  onOpen={open}
                />
              ) : null;
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

/**
 * One book's row. Memoized (its handlers are stable), so selecting a book or
 * scrolling re-renders only the rows that changed; `columns` is a prop so a new
 * language or "now" still re-renders every row.
 */
const BookRow = memo(function BookRow({
  row,
  height,
  selected,
  selecting,
  onToggle,
  onOpen,
  series,
}: {
  row: Row<typeof features, AdminBook>;
  columns: Columns;
  height: number;
  selected: boolean;
  selecting: boolean;
  onToggle: (b: AdminBook) => void;
  onOpen: (b: AdminBook) => void;
  series?: string;
}) {
  const { t } = useTranslation();
  const b = row.original;
  return (
    <tr
      data-selected={selected || undefined}
      onClick={() => (selecting ? onToggle(b) : onOpen(b))}
      className="cursor-pointer border-b transition-colors duration-(--dur-1) last:border-b-0 hover:bg-muted/70 data-selected:bg-[color-mix(in_oklab,var(--brand)_7%,transparent)] max-md:grid max-md:grid-cols-[auto_minmax(0,1fr)_auto] max-md:items-center max-md:gap-x-3 max-md:px-3.5"
      style={{ height }}
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
                checked={selected}
                onCheckedChange={() => onToggle(b)}
                aria-label={t('books.tile.select', { title: b.title })}
              />
            ) : id === 'title' ? (
              <TitleCell book={b} selecting={selecting} />
            ) : (
              <FilterCell book={b} column={id} selecting={selecting} filtered={series}>
                <FlexRender cell={cell} />
              </FilterCell>
            )}
          </td>
        );
      })}
    </tr>
  );
});

/**
 * A link in a row follows itself, not the row's click; while a selection is
 * active it doesn't, and the click goes on to the row, which toggles the book.
 */
function rowLinkClick(e: React.MouseEvent, selecting: boolean) {
  if (selecting) e.preventDefault();
  else e.stopPropagation();
}

/**
 * A cell's content, as a link to the books it names in the author, narrator
 * and series columns (none when blank).
 */
function FilterCell({
  book: b,
  column,
  selecting,
  filtered,
  children,
}: {
  book: AdminBook;
  column: string;
  selecting: boolean;
  filtered?: string;
  children: React.ReactNode;
}) {
  if (!isBooksField(column)) return children;
  // A series filter's column shows (and so links) the filtered series, also for
  // a book in it beyond its main series (or with none).
  const value = column === 'series' && filtered ? filtered : b[column];
  if (!value) return children;
  return (
    <BooksLink
      field={column}
      value={value}
      onClick={(e) => rowLinkClick(e, selecting)}
      className="hover:text-foreground hover:underline hover:underline-offset-3"
    >
      {children}
    </BooksLink>
  );
}

/** The cover and the title (a link, for the keyboard); on a phone, author and length under it. */
function TitleCell({ book: b, selecting }: { book: AdminBook; selecting: boolean }) {
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
          onClick={(e) => rowLinkClick(e, selecting)}
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
