import { useMemo, useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { BookUser, Mic, Search } from 'lucide-react';
import { usePeople } from '@/api/hooks';
import type { PersonCount, PersonField } from '@/api/types';
import { BooksLink } from '@/components/books-link';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Button, buttonVariants } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { counted, formatDuration } from '@/lib/format';
import { LibraryFilter } from '../library-filter';
import { useLibraryBookCount, useLibraryParam } from '../library-param';
import { MergeSuggestions } from './merge-suggestions';
import { PAGE_STEP, filterPeople, sortByBooks, sortByDuration } from './people-model';

/**
 * Library > Authors and Library > Narrators: one person per whole field value,
 * spellings that look alike offered for a merge first, then a tile per person
 * (authors by books, narrators by hours) that opens the Books list filtered to
 * them. Long lists render a page of tiles at a time.
 */
export function PeopleScreen({ field }: { field: PersonField }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const ns = field === 'author' ? 'authors' : 'narrators';
  const [library] = useLibraryParam();
  const query = usePeople(field, library);
  const bookCount = useLibraryBookCount(library);
  const [q, setQ] = useState('');
  const [limit, setLimit] = useState(PAGE_STEP);

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
  const shown = matching.slice(0, limit);
  const rest = matching.length - shown.length;

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
            people={data.people}
            libraryId={library}
          />
          <div className="mb-5 flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
            <div className="relative w-full max-w-[320px]">
              <Search
                className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-subtle-foreground"
                aria-hidden="true"
              />
              <Input
                type="search"
                value={q}
                onChange={(e) => {
                  setQ(e.target.value);
                  setLimit(PAGE_STEP);
                }}
                placeholder={t(`${ns}.filter`)}
                aria-label={t(`${ns}.filter`)}
                className="pl-9"
              />
            </div>
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
          {rest > 0 ? (
            <div className="mt-8 flex justify-center">
              <Button variant="outline" onClick={() => setLimit((n) => n + PAGE_STEP)}>
                {t('credits.showMore', counted(Math.min(rest, PAGE_STEP), lang))}
              </Button>
            </div>
          ) : null}
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
