import { useEffect, useMemo } from 'react';
import { Link } from '@tanstack/react-router';
import { Trans, useTranslation } from 'react-i18next';
import { Layers } from 'lucide-react';
import { useAdminBooks, useSeries, useServerInfo } from '@/api/hooks';
import { EmptyState } from '@/components/empty-state';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { Pager } from '@/components/pager';
import { QueryError } from '@/components/query-error';
import { SearchBox } from '@/components/search-box';
import { buttonVariants } from '@/components/ui/button';
import type { SeriesCount } from '@/api/types';
import { counted } from '@/lib/format';
import { pageOf } from '@/lib/pager';
import { LibraryFilter } from '../library-filter';
import { useLibraryBookCount, useLibraryParam, useSearchPage } from '../library-param';
import { SeriesCard } from './series-card';
import { SERIES_PAGE, filterSeries } from './series-model';
import { useInView } from './use-in-view';

/**
 * Library > Series: one card per series with its books as spines, a page of
 * cards at a time, found by name or author. The search and the page are in the
 * URL, so Back from a book returns to them (and the router to the scroll
 * position). Each card loads its own books once it nears the viewport; with
 * community metadata on, it marks the entries the server is missing.
 */
export function SeriesPage() {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [library] = useLibraryParam();
  const { q, page: wanted, search, goTo } = useSearchPage();
  const server = useServerInfo();
  const metadata = server.data?.capabilities.metadata ?? false;
  const series = useSeries(library);
  const bookCount = useLibraryBookCount(library);

  const list = useMemo(() => series.data ?? [], [series.data]);
  const matching = useMemo(() => filterSeries(list, q), [list, q]);
  const { page, pages, shown } = pageOf(matching, wanted, SERIES_PAGE);

  return (
    <Page>
      <PageHead
        title={t('series.title')}
        description={list.length ? t('series.description', counted(list.length, lang)) : undefined}
        action={<LibraryFilter />}
      />
      {series.isError ? (
        <QueryError
          title={t('series.error')}
          error={series.error}
          onRetry={() => void series.refetch()}
        />
      ) : !series.data ? (
        <div className="flex flex-col gap-4" role="status" aria-label={t('common.loading')}>
          {[0, 1, 2].map((i) => (
            <div key={i} className="skel h-[310px] rounded-xl" />
          ))}
        </div>
      ) : list.length === 0 ? (
        <EmptyState
          icon={Layers}
          title={t('series.empty.title')}
          body={bookCount === 0 ? t('series.empty.noBooks') : t('series.empty.body')}
          action={
            bookCount === 0 ? (
              <Link
                to="/library/{-$section}"
                params={{ section: 'libraries' }}
                className={buttonVariants({ variant: 'outline' })}
              >
                {t('credits.goToLibraries')}
              </Link>
            ) : undefined
          }
        />
      ) : (
        <div className="flex flex-col gap-4">
          {server.data ? (
            <p className="max-w-[720px] text-muted-foreground">
              {metadata ? (
                t('series.intro')
              ) : (
                <Trans
                  i18nKey="series.introOff"
                  components={{
                    // Not "link": the parser treats <link> as a void HTML element.
                    a: (
                      <Link
                        to="/server/{-$section}"
                        params={{ section: undefined }}
                        className="font-semibold text-brand-ink underline-offset-2 hover:underline"
                      />
                    ),
                  }}
                />
              )}
            </p>
          ) : null}
          <SearchBox
            value={q}
            onChange={search}
            label={t('series.filter')}
            placeholder={t('series.filter')}
            clearLabel={t('books.search.clear')}
            className="w-full max-w-[320px]"
          />
          {shown.length === 0 ? (
            <p className="py-10 text-center text-muted-foreground">
              {t('credits.noMatch', { q: q.trim() })}
            </p>
          ) : (
            shown.map((s) => (
              <SeriesShelf key={s.name} series={s} library={library} metadata={metadata} />
            ))
          )}
          <Pager page={page} count={pages} onPage={goTo} className="mt-4" />
        </div>
      )}
    </Page>
  );
}

/**
 * One series' card with its books: every book the series= filter finds (each
 * spelling of the series, books in it beyond their main series too, in their
 * order in it), asked for once the card nears the viewport and paged in full.
 */
function SeriesShelf({
  series: s,
  library,
  metadata,
}: {
  series: SeriesCount;
  library: number | undefined;
  metadata: boolean;
}) {
  const { t } = useTranslation();
  // Asked for once the card nears the viewport, like the cards' community rails.
  const [ref, seen] = useInView<HTMLDivElement>();
  const query = useAdminBooks(
    { series: s.name, sort: 'series', limit: 200, library_id: library },
    seen,
  );
  const { hasNextPage, isFetchingNextPage, isError, fetchNextPage } = query;
  useEffect(() => {
    if (hasNextPage && !isFetchingNextPage && !isError) void fetchNextPage();
  }, [hasNextPage, isFetchingNextPage, isError, fetchNextPage]);
  const loaded = !!query.data && !query.isPlaceholderData;
  // In order already: the server sorts a series filter by position in it.
  const pages = loaded ? query.data!.pages : undefined;
  const books = useMemo(() => (pages ?? []).flatMap((p) => p.books), [pages]);
  return (
    <div ref={ref}>
      {isError && !loaded ? (
        <QueryError
          title={t('series.booksError')}
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      ) : (
        <SeriesCard
          series={s}
          books={books}
          complete={loaded && (!hasNextPage || isError)}
          metadata={metadata}
        />
      )}
    </div>
  );
}
