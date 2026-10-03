import { useEffect, useMemo, useState } from 'react';
import { Link } from '@tanstack/react-router';
import { Trans, useTranslation } from 'react-i18next';
import { Layers } from 'lucide-react';
import { useAdminBooks, useSeries, useServerInfo } from '@/api/hooks';
import { EmptyState } from '@/components/empty-state';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Button, buttonVariants } from '@/components/ui/button';
import { formatNumber } from '@/lib/format';
import { LibraryFilter } from '../people/library-filter';
import { useLibraryBookCount, useLibraryParam } from '../people/library-param';
import { SeriesCard } from './series-card';
import { CARD_STEP, groupSeriesPages } from './series-model';

/**
 * Library > Series: one card per series with its books as spines. The books
 * come from the series-sorted list, paged only as far as the cards on screen
 * need; with community metadata on, each card marks the entries the server is
 * missing.
 */
export function SeriesPage() {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [library] = useLibraryParam();
  const server = useServerInfo();
  const metadata = server.data?.capabilities.metadata ?? false;
  const series = useSeries(library);
  const books = useAdminBooks({ sort: 'series', limit: 200, library_id: library });
  const bookCount = useLibraryBookCount(library);
  const [limit, setLimit] = useState(CARD_STEP);

  // A previous library's pages stay on screen while a new filter loads: not ours.
  const loaded = !!books.data && !books.isPlaceholderData;
  const pages = loaded ? books.data?.pages : undefined;
  const { bySeries, done } = useMemo(
    () => groupSeriesPages(pages ?? [], !!books.hasNextPage),
    [pages, books.hasNextPage],
  );
  const list = series.data ?? [];
  const shown = list.slice(0, limit);
  const has = (name: string) => bySeries.get(name)?.length ?? 0;
  const needMore = loaded && !done && shown.some((s) => has(s.name) < s.books);

  const { fetchNextPage, isFetchingNextPage, isError: booksFailed } = books;
  useEffect(() => {
    if (needMore && !isFetchingNextPage && !booksFailed) void fetchNextPage();
  }, [needMore, isFetchingNextPage, booksFailed, fetchNextPage]);

  const rest = list.length - shown.length;
  const counted = (n: number) => ({ count: n, formatted: formatNumber(n, lang) });

  return (
    <Page>
      <PageHead
        title={t('series.title')}
        description={list.length ? t('series.description', counted(list.length)) : undefined}
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
          {books.isError ? (
            <QueryError
              title={t('series.booksError')}
              error={books.error}
              onRetry={() => void books.refetch()}
            />
          ) : null}
          {shown.map((s) => (
            <SeriesCard
              key={s.name}
              series={s}
              books={bySeries.get(s.name) ?? []}
              complete={loaded && (done || has(s.name) >= s.books)}
              metadata={metadata}
            />
          ))}
          {rest > 0 ? (
            <div className="mt-4 flex justify-center">
              <Button variant="outline" onClick={() => setLimit((n) => n + CARD_STEP)}>
                {t('series.showMore', counted(Math.min(rest, CARD_STEP)))}
              </Button>
            </div>
          ) : null}
        </div>
      )}
    </Page>
  );
}
