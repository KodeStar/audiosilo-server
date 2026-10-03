import { useTranslation } from 'react-i18next';
import { useAdminBookPage, useBookFacets } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { formatNumber } from '@/lib/format';
import { BookTile } from '../book-tile';
import { SHELF_SIZE, curatingShelf, daysAgo, selectionKey } from './books-model';
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
  const curateLoading = noCover.isPending || (metadataOn && unmatched.isPending);
  const thisWeek = week.data?.total ?? 0;

  const tile = (b: AdminBook, note?: string) => (
    <li key={selectionKey(b)} className="min-w-0">
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
      {recent.isPending || recent.data?.books?.length ? (
        <section aria-labelledby="shelf-recent" className="mb-8">
          <div className="mb-3.5 flex flex-wrap items-baseline justify-between gap-x-3 gap-y-2">
            <h2 id="shelf-recent" className="h2">
              {t('books.recent.title')}
            </h2>
            {thisWeek ? (
              <span className="text-[13px] text-muted-foreground tabular-nums">
                {t('books.recent.thisWeek', {
                  count: thisWeek,
                  formatted: formatNumber(thisWeek, lang),
                })}
              </span>
            ) : null}
          </div>
          {recent.isPending ? (
            <ShelfSkeleton />
          ) : (
            <ul className="shelf-row">{recent.data?.books?.map((b) => tile(b))}</ul>
          )}
        </section>
      ) : null}
      {curateLoading || curate.length ? (
        <section aria-labelledby="shelf-curate" className="mb-8">
          <div className="mb-3.5 flex flex-col gap-0.5">
            <h2 id="shelf-curate" className="h2">
              {t('books.curate.title')}
            </h2>
            <span className="text-[13px] text-muted-foreground">{t('books.curate.body')}</span>
          </div>
          {curateLoading ? (
            <ShelfSkeleton />
          ) : (
            <ul className="shelf-row">
              {curate.map(({ book, issue }) => tile(book, t(`books.issue.${issue}`)))}
            </ul>
          )}
        </section>
      ) : null}
    </>
  );
}

function ShelfSkeleton() {
  const { t } = useTranslation();
  return (
    <div className="shelf-row" role="status" aria-label={t('common.loading')}>
      {Array.from({ length: 6 }, (_, i) => (
        <span key={i} className="cover skel block" aria-hidden="true" />
      ))}
    </div>
  );
}
