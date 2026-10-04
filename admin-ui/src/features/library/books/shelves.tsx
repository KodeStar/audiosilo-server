import { useTranslation } from 'react-i18next';
import { useAdminBookPage, useBookFacets } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { refKey } from '@/lib/book-route';
import { counted } from '@/lib/format';
import { cn } from '@/lib/utils';
import { BookTile } from '../book-tile';
import { SHELF_SIZE, curatingShelf, daysAgo } from './books-model';
import type { Selection } from './use-selection';

/**
 * The browsing state's two shelves, above the full list: the newest books, and
 * books worth a minute of attention (no cover, then not matched while community
 * metadata is on), each tile naming its first issue. A shelf with nothing to
 * show is left out.
 */
export function Shelves({
  selection,
  metadataOn,
  now,
}: {
  selection: Selection;
  metadataOn: boolean;
  now: number;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const recent = useAdminBookPage({ sort: 'added', order: 'desc', limit: SHELF_SIZE });
  const week = useBookFacets({ added_after: daysAgo(7, now) });
  const noCover = useAdminBookPage({
    has_cover: false,
    sort: 'added',
    order: 'desc',
    limit: SHELF_SIZE,
  });
  const unmatched = useAdminBookPage(
    { matched: false, sort: 'added', order: 'desc', limit: SHELF_SIZE },
    metadataOn,
  );
  const curate = curatingShelf(
    noCover.data?.books ?? [],
    metadataOn ? (unmatched.data?.books ?? []) : [],
  );
  const thisWeek = week.data?.total ?? 0;

  const tile = (b: AdminBook, note?: string) => (
    <li key={refKey(b)} className="min-w-0">
      <BookTile
        book={b}
        metadataOn={metadataOn}
        selected={selection.isSelected(b)}
        selecting={selection.size > 0}
        onToggle={selection.toggle}
        note={note}
      />
    </li>
  );

  return (
    <>
      <Shelf
        id="shelf-recent"
        title={t('books.recent.title')}
        aside={thisWeek ? t('books.recent.thisWeek', counted(thisWeek, lang)) : undefined}
        loading={recent.isPending}
      >
        {recent.data?.books?.map((b) => tile(b))}
      </Shelf>
      <Shelf
        id="shelf-curate"
        title={t('books.curate.title')}
        description={t('books.curate.body')}
        loading={noCover.isPending || (metadataOn && unmatched.isPending)}
      >
        {curate.map(({ book, issue }) => tile(book, t(`books.issue.${issue}`)))}
      </Shelf>
    </>
  );
}

/**
 * One shelf: a heading (with a muted line under it, or a count beside it) over a
 * row of tiles, a skeleton row while `loading`, and nothing at all once loaded
 * empty.
 */
function Shelf({
  id,
  title,
  description,
  aside,
  loading,
  children,
}: {
  id: string;
  title: string;
  description?: string;
  aside?: string;
  loading: boolean;
  children: React.ReactNode[] | undefined;
}) {
  const { t } = useTranslation();
  if (!loading && !children?.length) return null;
  return (
    <section aria-labelledby={id} className="mb-8">
      <div
        className={cn(
          'mb-3.5 flex',
          description
            ? 'flex-col gap-0.5'
            : 'flex-wrap items-baseline justify-between gap-x-3 gap-y-2',
        )}
      >
        <h2 id={id} className="h2">
          {title}
        </h2>
        {description ? (
          <span className="text-[13px] text-muted-foreground">{description}</span>
        ) : null}
        {aside ? (
          <span className="text-[13px] text-muted-foreground tabular-nums">{aside}</span>
        ) : null}
      </div>
      {loading ? (
        <div className="shelf-row" role="status" aria-label={t('common.loading')}>
          {Array.from({ length: 6 }, (_, i) => (
            <span key={i} className="cover skel block" aria-hidden="true" />
          ))}
        </div>
      ) : (
        <ul className="shelf-row">{children}</ul>
      )}
    </section>
  );
}
