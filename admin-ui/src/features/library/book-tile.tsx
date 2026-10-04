import { memo } from 'react';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Globe, ImageOff, Lock, Repeat, type LucideIcon } from 'lucide-react';
import type { AdminBook } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { Checkbox } from '@/components/ui/checkbox';
import { bookRoute } from '@/lib/book-route';
import { seriesIndexLabel } from '@/lib/format';
import { cn } from '@/lib/utils';
import { tileFlags, type TileFlag } from './books/books-model';

const FLAG_ICONS: Record<TileFlag, LucideIcon> = {
  cover: ImageOff,
  match: Globe,
  transcode: Repeat,
  edited: Lock,
};

/**
 * A book as a cover tile (STYLEGUIDE.md "Cover tile"): the cover, a two-line
 * title and a muted "author · #index". Up to two flags sit top-right; with
 * `onToggle` a checkbox shows top-left on hover and focus (always, while an
 * ancestor has .selecting). The tile opens the book page, or, while a
 * selection is active (`selecting`), toggles the book in it. `note` replaces
 * the subtitle (and the flags) with one issue in the warning colour. Memoized:
 * a grid re-renders only the tiles whose props changed (pass a stable `onToggle`).
 */
export const BookTile = memo(function BookTile({
  book,
  metadataOn = false,
  selected = false,
  selecting = false,
  onToggle,
  note,
  className,
}: {
  book: AdminBook;
  metadataOn?: boolean;
  selected?: boolean;
  selecting?: boolean;
  onToggle?: (book: AdminBook) => void;
  note?: string;
  className?: string;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const flags = note ? [] : tileFlags(book, metadataOn);
  const index = book.series ? seriesIndexLabel(book.series_index, lang, t) : '';

  const flagLabel = (f: TileFlag) =>
    f === 'transcode'
      ? t('books.flag.transcode', { codec: book.codec || book.format })
      : t(`books.flag.${f}`);

  return (
    <div className={cn('tile', className)} data-selected={selected || undefined}>
      {onToggle ? (
        <span className="tile-check">
          <Checkbox
            checked={selected}
            onCheckedChange={() => onToggle(book)}
            aria-label={t('books.tile.select', { title: book.title })}
          />
        </span>
      ) : null}
      {flags.length ? (
        <span className="tile-flags">
          {flags.map((f) => {
            const Icon = FLAG_ICONS[f];
            const label = flagLabel(f);
            return (
              <span
                key={f}
                className="tile-flag"
                data-tone={f === 'transcode' ? 'warn' : undefined}
                role="img"
                aria-label={label}
                title={label}
              >
                <Icon className="size-[13px]" strokeWidth={2.4} aria-hidden="true" />
              </span>
            );
          })}
        </span>
      ) : null}
      <Link
        {...bookRoute(book.library_id, book.path)}
        aria-label={
          book.author
            ? t('books.tile.label', { title: book.title, author: book.author })
            : book.title
        }
        onClick={(e) => {
          if (!selecting || !onToggle) return;
          e.preventDefault();
          onToggle(book);
        }}
        className="flex min-w-0 flex-col gap-2.5 rounded-[6px]"
      >
        <span className="cover-wrap">
          <BookCover
            libraryId={book.library_id}
            path={book.path}
            title={book.title}
            author={book.author}
            size={320}
          />
        </span>
        <span className="flex min-w-0 flex-col gap-px">
          <span className="line-clamp-2 text-[13.5px] leading-[1.3] font-[650] [overflow-wrap:anywhere]">
            {book.title}
          </span>
          <span className="truncate text-[12.5px] text-muted-foreground">
            {note ? (
              <span className="font-semibold text-warning">{note}</span>
            ) : (
              <>
                {book.author}
                {book.author && index ? ' · ' : null}
                {index ? <span className="tabular-nums">{index}</span> : null}
              </>
            )}
          </span>
        </span>
      </Link>
    </div>
  );
});
