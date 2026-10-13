import { useMemo } from 'react';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { BookUser, Mic } from 'lucide-react';
import { usePeople } from '@/api/hooks';
import type { PersonCount, PersonField } from '@/api/types';
import { BooksLink } from '@/components/books-link';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { Pager } from '@/components/pager';
import { QueryError } from '@/components/query-error';
import { SearchBox } from '@/components/search-box';
import { buttonVariants } from '@/components/ui/button';
import { counted, formatDuration } from '@/lib/format';
import { pageOf } from '@/lib/pager';
import { LibraryFilter } from '../library-filter';
import { useLibraryBookCount, useLibraryParam, useSearchPage } from '../library-param';
import { MergeSuggestions } from './merge-suggestions';
import { PEOPLE_PAGE, filterPeople, sortByBooks, sortByDuration } from './people-model';

/**
 * Library > Authors and Library > Narrators: spellings that look alike offered
 * for a merge first (whole credits), then a tile per person a credit names (a
 * co-credit counts for each; authors by books, narrators by hours) that opens
 * the Books list filtered to them, a page of tiles at a time. The search and the
 * page are in the URL, so Back from the Books list returns to them.
 */
export function PeopleScreen({ field }: { field: PersonField }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const ns = field === 'author' ? 'authors' : 'narrators';
  const [library] = useLibraryParam();
  const query = usePeople(field, library);
  const bookCount = useLibraryBookCount(library);
  const { q, page: wanted, search, goTo } = useSearchPage();

  const raw = query.data;
  const data = useMemo(
    () =>
      raw && {
        ...raw,
        people: field === 'author' ? sortByBooks(raw.people) : sortByDuration(raw.people),
      },
    [field, raw],
  );

  const matching = data ? filterPeople(data.people, q) : [];
  const { page, pages, shown } = pageOf(matching, wanted, PEOPLE_PAGE);

  return (
    <Page>
      <PageHead
        title={t(`${ns}.title`)}
        description={
          data?.people.length
            ? t(`${ns}.description`, counted(data.people.length, lang))
            : undefined
        }
        action={<LibraryFilter />}
      />
      {query.isError ? (
        <QueryError
          title={t(`${ns}.error`)}
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      ) : !data ? (
        <PeopleSkeleton field={field} />
      ) : data.people.length === 0 ? (
        <EmptyState
          icon={field === 'author' ? BookUser : Mic}
          title={t(`${ns}.empty.title`)}
          body={bookCount === 0 ? t(`${ns}.empty.noBooks`) : t(`${ns}.empty.body`)}
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
        <>
          <MergeSuggestions
            field={field}
            suggestions={data.merge_suggestions}
            libraryId={library}
          />
          <div className="mb-5 flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
            <SearchBox
              value={q}
              onChange={search}
              label={t(`${ns}.filter`)}
              placeholder={t(`${ns}.filter`)}
              clearLabel={t('books.search.clear')}
              className="w-full max-w-[320px]"
            />
            {data.unknown > 0 ? (
              <p className="text-[12.5px] text-muted-foreground tabular-nums">
                {t(`${ns}.unknown`, counted(data.unknown, lang))}
              </p>
            ) : null}
          </div>
          {shown.length === 0 ? (
            <p className="py-10 text-center text-muted-foreground">
              {t('credits.noMatch', { q: q.trim() })}
            </p>
          ) : field === 'author' ? (
            <ul className="shelf-grid" aria-label={t(`${ns}.title`)}>
              {shown.map((p) => (
                <AuthorTile key={p.name} person={p} />
              ))}
            </ul>
          ) : (
            <ul
              className="grid grid-cols-1 gap-3.5 sm:grid-cols-2 lg:grid-cols-3"
              aria-label={t(`${ns}.title`)}
            >
              {shown.map((p) => (
                <NarratorCard key={p.name} person={p} />
              ))}
            </ul>
          )}
          <Pager page={page} count={pages} onPage={goTo} className="mt-8" />
        </>
      )}
    </Page>
  );
}

/** "12 books · 85h 10m" (narrators: "· 85h 10m narrated"). */
function useStats(p: PersonCount, narrated: boolean) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const books = t('credits.books', counted(p.books, lang));
  const time = formatDuration(p.duration, lang);
  if (!time) return books;
  return t(narrated ? 'credits.statsNarrated' : 'credits.stats', { books, time });
}

function AuthorTile({ person: p }: { person: PersonCount }) {
  const stats = useStats(p, false);
  return (
    <li className="min-w-0">
      <BooksLink field="author" value={p.name} className="tile items-center rounded-xl text-center">
        <span className="cover-wrap w-full max-w-[148px] rounded-full">
          <Monogram
            name={p.name}
            size={148}
            className="aspect-square h-auto! w-full! shadow-cover"
          />
        </span>
        <span className="flex w-full min-w-0 flex-col items-center gap-0.5">
          <span className="line-clamp-2 text-[13.5px] leading-[18px] font-semibold [overflow-wrap:anywhere]">
            {p.name}
          </span>
          <span className="text-[12.5px] text-muted-foreground tabular-nums">{stats}</span>
        </span>
      </BooksLink>
    </li>
  );
}

function NarratorCard({ person: p }: { person: PersonCount }) {
  const stats = useStats(p, true);
  return (
    <li className="min-w-0">
      <BooksLink
        field="narrator"
        value={p.name}
        className="flex h-full items-center gap-3.5 rounded-xl border bg-card p-4 text-left transition-colors duration-(--dur-1) hover:border-border-strong md:px-5"
      >
        <span
          className="grid size-[46px] shrink-0 place-items-center rounded-full bg-muted text-muted-foreground"
          aria-hidden="true"
        >
          <Mic className="size-5" />
        </span>
        <span className="flex min-w-0 flex-col gap-0.5">
          <b className="truncate font-semibold" title={p.name}>
            {p.name}
          </b>
          <span className="text-[12.5px] text-muted-foreground tabular-nums">{stats}</span>
        </span>
      </BooksLink>
    </li>
  );
}

function PeopleSkeleton({ field }: { field: PersonField }) {
  const { t } = useTranslation();
  return field === 'author' ? (
    <div className="shelf-grid" role="status" aria-label={t('common.loading')}>
      {Array.from({ length: 12 }, (_, i) => (
        <div key={i} className="flex flex-col items-center gap-2.5">
          <span className="skel aspect-square w-full max-w-[148px] rounded-full" />
          <span className="skel h-3.5 w-3/4" />
        </div>
      ))}
    </div>
  ) : (
    <div
      className="grid grid-cols-1 gap-3.5 sm:grid-cols-2 lg:grid-cols-3"
      role="status"
      aria-label={t('common.loading')}
    >
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className="skel h-[80px] rounded-xl" />
      ))}
    </div>
  );
}
