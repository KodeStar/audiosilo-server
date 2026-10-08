import { useId, useMemo } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { Trans, useTranslation } from 'react-i18next';
import { useBookMeta, useBookWorks } from '@/api/hooks';
import type { AdminBook, SeriesCount } from '@/api/types';
import { BooksLink } from '@/components/books-link';
import { BookCover } from '@/components/book-cover';
import { ProvenanceMarker } from '@/components/provenance';
import { Badge } from '@/components/ui/badge';
import { bookRoute, refKey } from '@/lib/book-route';
import { coverModel } from '@/lib/cover-model';
import { counted, formatNumber } from '@/lib/format';
import { useMediaQuery } from '@/lib/use-media-query';
import {
  formatPositions,
  metaCandidate,
  pickRail,
  placeBooks,
  seriesStatus,
  spineHeight,
  spineRow,
  type Placed,
  type SeriesStatus,
} from './series-model';
import { useInView } from './use-in-view';

/**
 * One series: its name (a link to its books), author and what the server
 * holds, then the books as spines on a shelf (missing entries as dashed ghosts,
 * from the community rail of one of its books, fetched once the card nears the
 * viewport). The matched books are then resolved to their community works, so
 * each lands on its own entry whatever its series index says.
 */
export function SeriesCard({
  series: s,
  books,
  complete,
  metadata,
}: {
  series: SeriesCount;
  /** The series' books loaded so far, in series order. */
  books: AdminBook[];
  /** Every book of the series has loaded. */
  complete: boolean;
  /** Community metadata is on. */
  metadata: boolean;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const headingId = useId();
  const [ref, seen] = useInView<HTMLElement>();
  const candidate = complete ? metaCandidate(books) : undefined;
  const meta = useBookMeta(
    candidate?.library_id ?? 0,
    candidate?.path ?? '',
    metadata && seen && !!candidate,
  );
  const rail = meta.data?.matched ? pickRail(meta.data.series, s.name) : undefined;
  // Every matched book is asked about (the rail's own book too), batched with
  // the other cards' books.
  const matched = useMemo(() => (rail ? books.filter((b) => b.matched) : []), [books, rail]);
  const works = useBookWorks(matched);
  // Until the works answer, no rail: wrong gaps would flash, then go. A failed
  // lookup answers too (unresolved: the book falls back to its series index).
  const placedOn = rail && !works.pending ? rail : undefined;
  const placed = useMemo(
    () => placeBooks(books, placedOn, works.ids),
    [books, placedOn, works.ids],
  );
  const status = useMemo(() => placedOn && seriesStatus(placedOn, placed), [placedOn, placed]);
  const known = status && status.total > 0 ? status : undefined;
  const unmatched = metadata && complete && (!candidate || meta.data?.matched === false);
  const fmt = (n: number) => formatNumber(n, lang);

  return (
    <section
      ref={ref}
      aria-labelledby={headingId}
      className="min-w-0 rounded-xl border bg-card p-4 md:p-5"
    >
      <div className="mb-[18px] flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 flex-col gap-[3px]">
          <h2 id={headingId} className="h2 [overflow-wrap:anywhere]">
            <BooksLink field="series" value={s.name} className="underline-offset-3 hover:underline">
              {s.name}
            </BooksLink>
          </h2>
          <span className="text-muted-foreground tabular-nums">
            {s.author ? `${s.author} · ` : null}
            {known ? (
              <Summary status={known} fmt={fmt} />
            ) : (
              t('series.books', counted(s.books, lang))
            )}
          </span>
          {unmatched ? (
            <span className="text-[12.5px] text-subtle-foreground">{t('series.noMatch')}</span>
          ) : null}
        </div>
        {known ? (
          <div className="flex items-center gap-2">
            <ProvenanceMarker source="community" />
            <Badge
              variant={known.missing.length ? 'warning' : 'success'}
              className="tabular-nums"
              aria-label={t('series.badge', {
                have: fmt(known.have.length),
                total: fmt(known.total),
              })}
            >
              {known.have.length}/{known.total}
            </Badge>
          </div>
        ) : null}
      </div>
      {books.length === 0 && !complete ? (
        <div className="skel h-[200px] rounded-lg" role="status" aria-label={t('common.loading')} />
      ) : (
        <Shelf name={s.name} placed={placed} status={known} fmt={fmt} fan={seen} />
      )}
    </section>
  );
}

/** "You have 1, 2, 4 of 5; missing 3, 5", or that the series is complete. */
function Summary({ status, fmt }: { status: SeriesStatus; fmt: (n: number) => string }) {
  const { t } = useTranslation();
  if (status.missing.length === 0) {
    return t('series.complete', { count: status.total, formatted: fmt(status.total) });
  }
  const missing = formatPositions(
    status.missing.map((e) => e.position),
    fmt,
  );
  return (
    <Trans
      i18nKey={status.have.length ? 'series.have' : 'series.haveNone'}
      values={{ have: formatPositions(status.have, fmt), total: fmt(status.total), missing }}
      components={{ b: <b className="font-semibold text-foreground" /> }}
    />
  );
}

function Shelf({
  name,
  placed,
  status,
  fmt,
  fan,
}: {
  name: string;
  /**
   * The series' books in their local order (by series index; placeBooks keeps it),
   * each with its place on the rail. spineRow sorts the spines by place; the fan
   * of covers keeps the local order.
   */
  placed: Placed[];
  status?: SeriesStatus;
  fmt: (n: number) => string;
  /** The card has come into view: its fan of covers may load. */
  fan: boolean;
}) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  // The fan only fits beside the spines on wide screens; elsewhere its covers aren't fetched.
  const wide = useMediaQuery('(min-width: 1024px)');
  const spines = spineRow(placed, status?.missing ?? []);
  return (
    // Room above for the hover lift and below for the shelf's shadow, inside the scroller.
    <div className="hscroll -mx-0.5 -mt-3 -mb-[18px] px-0.5 pt-3 pb-[18px]">
      <ul className="spines min-w-max" aria-label={t('series.spinesLabel', { name })}>
        {spines.map((sp) => {
          if (sp.kind === 'gap') {
            const label = t('series.gap', { index: fmt(sp.entry.position), title: sp.entry.title });
            return (
              <li key={`gap-${sp.entry.position}`} className="flex h-full items-end">
                <span
                  className="spine"
                  data-gap=""
                  role="img"
                  aria-label={label}
                  title={label}
                  style={{ height: `${spineHeight(sp.entry.title)}%` }}
                >
                  <span className="spine-idx">{fmt(sp.entry.position)}</span>
                  {sp.entry.title}
                </span>
              </li>
            );
          }
          const b = sp.book;
          const [c1, c3, , ink] = coverModel(b.title, b.author).palette;
          const label =
            sp.position > 0
              ? t('series.spine', { index: fmt(sp.position), title: b.title })
              : b.title;
          return (
            <li key={refKey(b)} className="flex h-full items-end">
              <button
                type="button"
                className="spine"
                aria-label={label}
                title={label}
                onClick={() => void navigate(bookRoute(b.library_id, b.path))}
                style={
                  {
                    '--c1': c1,
                    '--c3': c3,
                    '--ink': ink,
                    height: `${spineHeight(b.path)}%`,
                  } as React.CSSProperties
                }
              >
                {sp.position > 0 ? <span className="spine-idx">{fmt(sp.position)}</span> : null}
                {b.title}
              </button>
            </li>
          );
        })}
        {fan && wide ? (
          <li aria-hidden="true" className="ml-auto flex self-center pr-3 pl-10">
            {placed.slice(0, 4).map(({ book: b }, i) => (
              <div
                key={refKey(b)}
                className="w-24"
                style={{
                  marginLeft: i ? -40 : 0,
                  transform: `rotate(${(i - 1.5) * 5}deg) translateY(${Math.abs(i - 1.5) * 4}px)`,
                }}
              >
                <BookCover
                  libraryId={b.library_id}
                  path={b.path}
                  title={b.title}
                  author={b.author}
                  size={160}
                />
              </div>
            ))}
          </li>
        ) : null}
      </ul>
    </div>
  );
}
