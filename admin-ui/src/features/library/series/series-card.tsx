import { useId, useMemo, useState } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { Trans, useTranslation } from 'react-i18next';
import { Sparkles } from 'lucide-react';
import { useBookMeta, useBookWorks } from '@/api/hooks';
import type { AdminBook, SeriesCount } from '@/api/types';
import { BooksLink } from '@/components/books-link';
import { BookCover } from '@/components/book-cover';
import { ProvenanceMarker } from '@/components/provenance';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { SeriesGenerateDialog } from '@/features/bridge/series-generate-dialog';
import { bookRoute, refKey } from '@/lib/book-route';
import { counted, formatNumber } from '@/lib/format';
import { useMediaQuery } from '@/lib/use-media-query';
import {
  formatPositions,
  formatSpan,
  metaCandidate,
  pickRail,
  placeBooks,
  seriesStatus,
  spineColors,
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
  generate = false,
}: {
  series: SeriesCount;
  /** The series' books loaded so far, in series order. */
  books: AdminBook[];
  /** Every book of the series has loaded. */
  complete: boolean;
  /** Community metadata is on. */
  metadata: boolean;
  /** An online Bridge offers characters and recaps: Generate for the series. */
  generate?: boolean;
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
  const rails = meta.data?.matched ? meta.data.series : undefined;
  // Every matched book is asked about (the rail's own book too), batched with
  // the other cards' books: their works place them, and pick the rail when no
  // rail is named like the series.
  const matched = useMemo(
    () => (rails?.length ? books.filter((b) => b.matched) : []),
    [books, rails],
  );
  const works = useBookWorks(matched);
  // Until the works answer, no rail: wrong gaps would flash, then go. A failed
  // lookup answers too (unresolved: the book falls back to its series index).
  const placedOn = useMemo(
    () => (works.pending ? undefined : pickRail(rails, s.name, books, works.ids)),
    [works.pending, works.ids, rails, s.name, books],
  );
  const placed = useMemo(
    () => placeBooks(books, placedOn, works.ids, s.name),
    [books, placedOn, works.ids, s.name],
  );
  const status = useMemo(() => placedOn && seriesStatus(placedOn, placed), [placedOn, placed]);
  const known = status && status.total > 0 ? status : undefined;
  const unmatched = metadata && complete && (!candidate || meta.data?.matched === false);
  const fmt = (n: number) => formatNumber(n, lang);
  const [generating, setGenerating] = useState(false);
  const canGenerate = generate && complete && books.length > 0;

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
        {known || canGenerate ? (
          <div className="flex flex-wrap items-center gap-2">
            {canGenerate ? (
              <Button variant="ghost" size="sm" onClick={() => setGenerating(true)}>
                <Sparkles aria-hidden="true" />
                {t('bridge.series.action')}
              </Button>
            ) : null}
            {known ? (
              <>
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
              </>
            ) : null}
          </div>
        ) : null}
      </div>
      {canGenerate ? (
        <SeriesGenerateDialog
          open={generating}
          onOpenChange={setGenerating}
          series={s.name}
          books={books}
        />
      ) : null}
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
  const missing = formatPositions(status.missing, fmt);
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
            const index = formatSpan(sp.entry, fmt);
            const label = t('series.gap', { index, title: sp.entry.title });
            return (
              <li key={`gap-${sp.entry.position}`} className="flex h-full items-end">
                <span
                  className="spine"
                  data-gap=""
                  role="img"
                  aria-label={label}
                  title={label}
                  style={{ height: `${spineHeight(0, sp.entry.title)}%` }}
                >
                  <span className="spine-idx">{index}</span>
                  {sp.entry.title}
                </span>
              </li>
            );
          }
          const b = sp.book;
          const { body, band, ink } = spineColors(b);
          const index = formatSpan(sp, fmt);
          const label = sp.position > 0 ? t('series.spine', { index, title: b.title }) : b.title;
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
                    '--c1': body,
                    '--c3': band,
                    '--ink': ink,
                    height: `${spineHeight(b.duration, b.path)}%`,
                  } as React.CSSProperties
                }
              >
                {sp.position > 0 ? <span className="spine-idx">{index}</span> : null}
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
