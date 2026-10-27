import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { Link } from '@tanstack/react-router';
import {
  useQueryClient,
  type InfiniteData,
  type QueryClient,
  type QueryKey,
} from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ArrowRight, Check, ExternalLink, LoaderCircle, Sparkles } from 'lucide-react';
import { api } from '@/api/client';
import {
  invalidateIssues,
  invalidateMatches,
  keys,
  matchRunActive,
  useLibraryRoots,
  useMatchRunItems,
  useMatchRuns,
} from '@/api/hooks';
import {
  MATCH_SCOPES,
  type AdminBookDetail,
  type MatchOutcome,
  type MatchRun,
  type MatchRunItem,
  type MatchRunItemPage,
  type MatchRunMode,
  type MatchScope,
  type SeriesRef,
} from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { LibrarySelect } from '@/components/library-select';
import { ProgressBar } from '@/components/progress-bar';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import {
  Dialog,
  DialogBody,
  DialogClose,
  DialogContent,
  DialogFooter,
} from '@/components/ui/dialog';
import { RadioCards } from '@/components/ui/radio-cards';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { MatchDialog } from '@/features/book/match-dialog';
import { keepShiftClickFromSelectingText, shiftHeld } from '@/features/library/books/use-selection';
import { scoreTone } from '@/features/book/match-model';
import { moreSeriesRefs, seriesRef } from '@/features/book/more-series';
import { bookRoute } from '@/lib/book-route';
import { regionName, regionTag, storeOf } from '@/lib/regions';
import { toastError } from '@/lib/errors';
import {
  counted,
  formatDuration,
  formatList,
  formatNumber,
  formatRelative,
  seriesLabel,
} from '@/lib/format';
import { joinLibraryPath } from '@/lib/paths';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import {
  NO_PICKS,
  applyCount,
  applyRequest,
  bulkPhase,
  isChosen,
  isPickable,
  leaveOut,
  runFraction,
  runProgress,
  leaveOutStale,
  togglePick,
  togglePickRange,
  withId,
  withoutId,
  type Picks,
} from './bulk-match-model';

/**
 * Health > Not matched: match every unmatched book in one pass (STYLEGUIDE.md
 * "Health triage"). A run only looks; the admin reviews what it found, chooses
 * how much it may write, and applies it. A repick run switches books matched
 * earlier to the preferred marketplace's ASIN, and a refresh run looks the books
 * that already have an ASIN or ISBN up by it to fill in their details (an
 * identifier alone, from the manager or an ids-only apply, brought none). Runs
 * work on the server, so the
 * card follows the newest one by polling, whoever started it.
 */
export function BulkMatch() {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const runs = useMatchRuns();
  const run = runs.data?.runs[0];
  const region = runs.data?.region ?? '';
  const phase = bulkPhase(run);
  const [libraryId, setLibraryId] = useState(0);
  const [busy, setBusy] = useState(false);
  const [reviewing, setReviewing] = useState(false);

  // An apply that stops has changed books: they leave "Not matched", and their
  // pages, lists and covers are stale. Keyed on the run's state, not on having
  // seen it working (a small apply can finish between two polls); matching
  // alone changes no book.
  const active = matchRunActive(run);
  const stamp = run ? `${run.id}:${run.status}:${run.apply_done}` : '';
  const applied = (run?.apply_done ?? 0) > 0;
  const seen = useRef(stamp);
  useEffect(() => {
    if (seen.current && stamp !== seen.current && !active && applied) {
      invalidateMatches(qc);
    }
    seen.current = stamp;
  }, [stamp, active, applied, qc]);

  // Start over runs the same books again: the run's library, not the select's
  // (hidden while a run waits for review, and reset when the page reloads).
  const start = async (mode: MatchRunMode, library = libraryId) => {
    setBusy(true);
    try {
      await api.startMatchRun({ library_id: library || undefined, mode });
      await qc.invalidateQueries({ queryKey: keys.matchRuns });
    } catch (err) {
      toastError(t('health.bulkMatch.startFailed'), err);
    } finally {
      setBusy(false);
    }
  };
  const stop = async () => {
    if (!run) return;
    try {
      await api.cancelMatchRun(run.id);
    } catch (err) {
      toastError(t('health.bulkMatch.stopFailed'), err);
    }
    await qc.invalidateQueries({ queryKey: keys.matchRuns });
  };

  if (runs.isError) return null; // the list below still works; the card is extra
  return (
    <section
      aria-labelledby="bulk-match-title"
      className="mb-4 flex flex-col gap-3 rounded-xl border bg-card px-4 py-4 md:px-[18px]"
    >
      <div className="flex items-start gap-3">
        <span
          className="grid size-8 shrink-0 place-items-center rounded-[9px] bg-prov-community-soft text-prov-community"
          aria-hidden="true"
        >
          <Sparkles className="size-4" />
        </span>
        <div className="flex min-w-0 flex-col gap-0.5">
          <h3 id="bulk-match-title" className="font-semibold">
            {t('health.bulkMatch.title')}
          </h3>
          <p className="text-[13px] text-muted-foreground">
            {phase === 'idle' ? t('health.bulkMatch.body') : t('health.bulkMatch.bodyRunning')}
          </p>
        </div>
      </div>

      {!runs.data ? (
        <div className="skel h-9 w-48" />
      ) : phase === 'matching' || phase === 'applying' ? (
        run ? (
          <Working run={run} lang={lang} onStop={() => void stop()} />
        ) : null
      ) : phase === 'ready' && run ? (
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <Summary run={run} lang={lang} />
          <div className="ml-auto flex flex-wrap gap-2">
            <Button
              variant="ghost"
              disabled={busy}
              onClick={() => void start(run.mode, run.library_id ?? 0)}
            >
              {t('health.bulkMatch.startOver')}
            </Button>
            <Button variant="brand" onClick={() => setReviewing(true)}>
              {t('health.bulkMatch.review')}
              <ArrowRight aria-hidden="true" />
            </Button>
          </div>
        </div>
      ) : (
        <>
          {run ? <LastRun run={run} lang={lang} /> : null}
          <div className="flex flex-wrap items-center gap-2">
            <LibrarySelect
              value={libraryId}
              onChange={setLibraryId}
              label={t('health.bulkMatch.library')}
            />
            <Button disabled={busy} onClick={() => void start('match')}>
              {busy ? (
                <LoaderCircle className="animate-spin" aria-hidden="true" />
              ) : (
                <Sparkles aria-hidden="true" />
              )}
              {t('health.bulkMatch.start')}
            </Button>
            <Button variant="outline" disabled={busy} onClick={() => void start('refresh')}>
              {t('health.bulkMatch.refresh')}
            </Button>
            {region ? (
              <Button variant="outline" disabled={busy} onClick={() => void start('repick')}>
                {t('health.bulkMatch.repick', { country: regionName(region, lang) })}
              </Button>
            ) : null}
          </div>
          <p className="text-[12.5px] text-subtle-foreground">
            {t('health.bulkMatch.refreshHint')}
          </p>
          <p className="text-[12.5px] text-subtle-foreground">
            {t('health.bulkMatch.privacy')}{' '}
            {region ? (
              t('health.bulkMatch.regionSet', { store: storeOf(region) })
            ) : (
              <>
                {t('health.bulkMatch.regionNone')}{' '}
                <Link
                  to="/server/{-$section}"
                  params={{ section: undefined }}
                  search={{ topic: 'metadata' }}
                  className="font-semibold text-brand-ink hover:underline"
                >
                  {t('health.bulkMatch.regionLink')}
                </Link>
              </>
            )}
          </p>
        </>
      )}

      {run && phase === 'ready' ? (
        <ReviewDialog run={run} open={reviewing} onOpenChange={setReviewing} />
      ) : null}
    </section>
  );
}

/** A run at work: how far it has got, and Stop. */
function Working({ run, lang, onStop }: { run: MatchRun; lang: string; onStop: () => void }) {
  const { t } = useTranslation();
  const applying = run.status === 'applying';
  const { done, total } = runProgress(run);
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-[13px] font-[550]" role="status">
          {t(applying ? 'health.bulkMatch.applying' : 'health.bulkMatch.matching', {
            done: formatNumber(done, lang),
            total: formatNumber(total, lang),
          })}
        </span>
        <Button variant="ghost" size="sm" onClick={onStop}>
          {t('health.bulkMatch.stop')}
        </Button>
      </div>
      <ProgressBar fraction={runFraction(run)} label={t('health.bulkMatch.progress')} />
    </div>
  );
}

/** What a ready run found, as counts. */
function Summary({ run, lang }: { run: MatchRun; lang: string }) {
  const { t } = useTranslation();
  if (run.mode === 'repick') {
    return (
      <span className="text-[13px] font-[550]">
        {t('health.bulkMatch.repickSummary', {
          ...counted(run.counts.pending, lang),
          country: regionName(run.region, lang),
        })}
      </span>
    );
  }
  const parts: [MatchOutcome, number][] = [
    ['auto', run.counts.pending],
    ['review', run.counts.review],
    ['none', run.counts.none],
    ['error', run.counts.error],
  ];
  return (
    <span className="flex flex-wrap gap-x-3 gap-y-1 text-[13px]">
      {parts
        .filter(([kind, n]) => n > 0 || kind === 'auto')
        .map(([kind, n]) => (
          <span key={kind} className={cn(kind === 'auto' ? 'font-[550]' : 'text-muted-foreground')}>
            {t(`health.bulkMatch.count.${kind}`, counted(n, lang))}
          </span>
        ))}
    </span>
  );
}

/** How the newest finished run ended. */
function LastRun({ run, lang }: { run: MatchRun; lang: string }) {
  const { t } = useTranslation();
  let text: string;
  switch (run.status) {
    case 'applied':
    case 'ready': {
      const { applied, skipped, failed } = run.counts;
      // A ready run shown here has nothing left to apply; one that never applied
      // anything found nothing (no candidates, or a repick with none to switch).
      text =
        run.status === 'ready' && applied + skipped + failed === 0
          ? t('health.bulkMatch.last.nothing')
          : t('health.bulkMatch.last.applied', {
              ...counted(applied, lang),
              when: formatRelative(run.applied_at ?? run.finished_at ?? run.started_at, lang),
            });
      break;
    }
    case 'failed':
      text = t(`health.bulkMatch.last.failed.${run.error || 'internal'}`);
      break;
    case 'cancelled':
    case 'interrupted':
      text = t(`health.bulkMatch.last.${run.status}`);
      break;
    default:
      return null;
  }
  return <p className="text-[12.5px] text-muted-foreground">{text}</p>;
}

const OUTCOMES: MatchOutcome[] = ['auto', 'review', 'none', 'error'];

/**
 * The review: the run's books by outcome, each with the community match and what
 * the chosen scope would write; confident ones ticked, ones to review not. Apply
 * runs on the server; the card follows it.
 */
function ReviewDialog({
  run,
  open,
  onOpenChange,
}: {
  run: MatchRun;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const repick = run.mode === 'repick';
  // A refresh never changes an identifier, so ASIN and ISBN only would write nothing.
  const scopes = run.mode === 'refresh' ? MATCH_SCOPES.filter((s) => s !== 'ids') : MATCH_SCOPES;
  const firstTab: MatchOutcome = run.counts.pending || repick ? 'auto' : 'review';
  const [tab, setTab] = useState<MatchOutcome>(firstTab);
  const [picks, setPicks] = useState<Picks>(NO_PICKS);
  const [scope, setScope] = useState<MatchScope>('fill');
  const [busy, setBusy] = useState(false);
  // The book being matched by hand, in a dialog over the review (which keeps its
  // tab, picks and loaded pages underneath).
  const [byHand, setByHand] = useState<MatchRunItem>();
  // The ids matched by hand whose rows wait for their list to read again: a
  // Shift-click range passes over them until it has (their rows then say what
  // the server makes of them: no checkbox once matched since, a checkbox again
  // otherwise).
  const [matchedByHand, setMatchedByHand] = useState<ReadonlySet<number>>(() => new Set());
  // Each opening starts afresh (the run may have changed since).
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    if (open) {
      setTab(firstTab);
      setPicks(NO_PICKS);
      setScope('fill');
      setByHand(undefined);
      setMatchedByHand(new Set());
    }
  }
  // A book matched by hand leaves the apply, and the Health page under the review
  // reads its issues again. A match run's book that took an ASIN or ISBN is one
  // the server now calls matched since (matchrun.matchedSince): its row reads so
  // at once (no checkbox, so a range passes over it with nothing to wait for),
  // without asking again for every page the review has loaded. Any other
  // (another mode, an applied item, a hand match that took no identifier) the
  // server says what applying would do now, and its row may show a checkbox
  // until the list has read again.
  const onMatchedByHand = (item: MatchRunItem) => {
    setPicks((p) => leaveOut(p, item));
    invalidateIssues(qc);
    const book = qc.getQueryData<AdminBookDetail>(keys.book(item.library_id, item.path))?.book;
    const itemsKey = keys.matchRunItems(run.id, item.outcome);
    if (run.mode !== 'match' || item.applied || !(book?.asin || book?.isbn)) {
      setMatchedByHand((ids) => withId(ids, item.id));
      // The id stays until a read of the whole list lands (server#198): not when
      // the invalidation's promise settles, which it does at once while the read
      // waits offline.
      const since = Date.now();
      void qc.invalidateQueries({ queryKey: itemsKey });
      void whenReadAgain(qc, itemsKey, since).then(() =>
        setMatchedByHand((ids) => withoutId(ids, item.id)),
      );
      return;
    }
    const now = { title: book.title, author: book.author };
    qc.setQueryData<InfiniteData<MatchRunItemPage>>(
      itemsKey,
      (data) =>
        data && {
          ...data,
          pages: data.pages.map((page) => ({
            ...page,
            items: (page.items ?? []).map((it) =>
              it.id === item.id ? { ...it, matched_since: true, changes: {}, book: now } : it,
            ),
          })),
        },
    );
  };
  // A loaded confident item the apply would leave alone (gone, matched since)
  // is left out, so the count is what the apply writes.
  const onLoaded = useCallback(
    (items: MatchRunItem[]) => setPicks((p) => leaveOutStale(p, items)),
    [],
  );
  const count = applyCount(run, picks);
  const country = regionName(run.region, lang);
  const headings: Record<MatchRunMode, { title: string; description: string }> = {
    match: {
      title: t('health.bulkMatch.reviewTitle'),
      description: t('health.bulkMatch.reviewDescription'),
    },
    repick: {
      title: t('health.bulkMatch.repickTitle', { country }),
      description: t('health.bulkMatch.repickDescription', { store: storeOf(run.region) }),
    },
    refresh: {
      title: t('health.bulkMatch.refreshTitle'),
      description: t('health.bulkMatch.refreshDescription'),
    },
  };
  // A mode this console doesn't know reads as a match.
  const heading = headings[run.mode] ?? headings.match;

  const apply = async () => {
    setBusy(true);
    try {
      await api.applyMatchRun(run.id, applyRequest(scope, picks));
      await qc.invalidateQueries({ queryKey: keys.matchRuns });
      toast.add({
        title: t('health.bulkMatch.applyStarted'),
        description: t('health.bulkMatch.applyStartedBody', counted(count, lang)),
        type: 'success',
      });
      onOpenChange(false);
    } catch (err) {
      toastError(t('health.bulkMatch.applyFailed'), err);
    } finally {
      setBusy(false);
    }
  };

  const counts: Record<MatchOutcome, number> = {
    auto: run.counts.auto,
    review: run.counts.review,
    none: run.counts.none,
    error: run.counts.error,
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        size="lg"
        tone="community"
        icon={Sparkles}
        title={heading.title}
        description={heading.description}
      >
        {open ? (
          <>
            <DialogBody className="flex flex-col gap-4">
              {repick ? null : (
                <>
                  <RadioCards
                    label={t('health.bulkMatch.scope.label')}
                    value={scope}
                    onValueChange={setScope}
                    options={scopes.map((s) => ({
                      value: s,
                      title: t(`health.bulkMatch.scope.${s}`),
                      description: t(`health.bulkMatch.scope.${s}Body`),
                    }))}
                  />
                  <SegmentedControl
                    label={t('health.bulkMatch.outcomes')}
                    value={tab}
                    onChange={setTab}
                    className="self-start"
                    options={OUTCOMES.filter((o) => counts[o] > 0 || o === 'auto').map((o) => ({
                      value: o,
                      label: (
                        <>
                          {t(`health.bulkMatch.tab.${o}`)}
                          <span className="ml-1.5 text-subtle-foreground tabular-nums">
                            {formatNumber(counts[o], lang)}
                          </span>
                        </>
                      ),
                    }))}
                  />
                </>
              )}
              <ItemList
                key={tab}
                run={run}
                outcome={tab}
                scope={repick ? 'ids' : scope}
                picks={picks}
                onPick={setPicks}
                matchedByHand={matchedByHand}
                onMatchByHand={setByHand}
                onLoaded={onLoaded}
              />
            </DialogBody>
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="ghost" />}>
                {t('common.cancel')}
              </DialogClose>
              <Button onClick={() => void apply()} disabled={busy || count === 0}>
                {busy ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
                {t('health.bulkMatch.apply', counted(count, lang))}
              </Button>
            </DialogFooter>
            {byHand ? (
              <MatchDialog
                key={byHand.id}
                open
                onOpenChange={() => setByHand(undefined)}
                libraryId={byHand.library_id}
                path={byHand.path}
                onMatched={() => onMatchedByHand(byHand)}
              />
            ) : null}
          </>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

/**
 * Settles once the query at `key` holds data read after `since` from a read of
 * the whole list (not invalidated since, nothing still fetching), or once the
 * query is gone (the review closed). A read paused offline waits for the network.
 */
function whenReadAgain(qc: QueryClient, key: QueryKey, since: number): Promise<void> {
  const cache = qc.getQueryCache();
  const readAgain = () => {
    const q = cache.find({ queryKey: key, exact: true });
    if (!q) return true;
    const st = q.state;
    return st.dataUpdatedAt >= since && !st.isInvalidated && st.fetchStatus === 'idle';
  };
  return new Promise((resolve) => {
    if (readAgain()) return resolve();
    const unsubscribe = cache.subscribe(() => {
      if (!readAgain()) return;
      unsubscribe();
      resolve();
    });
  });
}

/**
 * One outcome's books, a page at a time. A click on a checkbox flips that
 * book's pick; a Shift-click gives every book from the last one clicked to
 * this one the pick this one takes (togglePickRange).
 */
function ItemList({
  run,
  outcome,
  scope,
  picks,
  onPick,
  matchedByHand,
  onMatchByHand,
  onLoaded,
}: {
  run: MatchRun;
  outcome: MatchOutcome;
  scope: MatchScope;
  picks: Picks;
  onPick: (change: (picks: Picks) => Picks) => void;
  /** Ids matched by hand from the review, which a range leaves as they are. */
  matchedByHand: ReadonlySet<number>;
  onMatchByHand: (item: MatchRunItem) => void;
  /** Each time a page arrives: the items loaded so far (the apply leaves out the stale ones). */
  onLoaded: (items: MatchRunItem[]) => void;
}) {
  const { t } = useTranslation();
  const list = useMatchRunItems(run.id, outcome);
  const roots = useLibraryRoots();
  const data = list.data;
  const items = useMemo(() => data?.pages.flatMap((p) => p.items ?? []) ?? [], [data]);
  useEffect(() => {
    if (items.length) onLoaded(items);
  }, [items, onLoaded]);
  // The last item clicked, where a Shift-click's range starts (each tab its own list).
  const anchor = useRef<number | undefined>(undefined);
  const toggle = (item: MatchRunItem, shift: boolean) => {
    const from = anchor.current;
    anchor.current = item.id;
    onPick((p) =>
      shift ? togglePickRange(p, items, from, item, matchedByHand) : togglePick(p, item),
    );
  };
  if (list.isError) {
    return (
      <QueryError
        title={t('health.bulkMatch.listError')}
        error={list.error}
        onRetry={() => void list.refetch()}
      />
    );
  }
  if (list.isPending) {
    return (
      <div className="flex flex-col gap-2" role="status" aria-label={t('common.loading')}>
        {[0, 1, 2].map((i) => (
          <div key={i} className="skel h-16 rounded-[12px]" />
        ))}
      </div>
    );
  }
  if (!items.length) {
    return (
      <p className="rounded-[12px] border border-dashed px-4 py-6 text-center text-muted-foreground">
        {t('health.bulkMatch.empty')}
      </p>
    );
  }
  return (
    <>
      <ul
        className="divide-y rounded-[12px] border"
        aria-label={t(`health.bulkMatch.tab.${outcome}`)}
      >
        {items.map((item) => (
          <ItemRow
            key={item.id}
            item={item}
            root={roots[item.library_id]}
            scope={scope}
            chosen={isChosen(item, picks)}
            onToggle={(shift) => toggle(item, shift)}
            onMatchByHand={() => onMatchByHand(item)}
          />
        ))}
      </ul>
      {list.hasNextPage ? (
        // Not while the list reads again either: a next page then would cancel that
        // read, and the pages already loaded would keep what they said before.
        <Button
          variant="outline"
          className="self-center"
          onClick={() => void list.fetchNextPage()}
          disabled={list.isFetching}
        >
          {list.isFetchingNextPage ? t('common.loading') : t('health.more')}
        </Button>
      ) : null}
    </>
  );
}

/** A book, the match the run found for it, and what applying writes. */
function ItemRow({
  item,
  root,
  scope,
  chosen,
  onToggle,
  onMatchByHand,
}: {
  item: MatchRunItem;
  /** The library's folder: the book's whole path is the path's tooltip. */
  root: string | undefined;
  scope: MatchScope;
  chosen: boolean;
  onToggle: (shift: boolean) => void;
  onMatchByHand: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const p = item.proposal;
  const title = item.book.title || item.path;
  const change = item.changes[scope];
  // What became of the book: applied, skipped or failed, or matched since the run
  // (an apply leaves it alone, and says so).
  const status =
    item.matched_since || item.detail === 'matched_since' ? 'matchedSince' : item.applied;
  const changed = change
    ? [
        ...(change.cover ? [t('book.match.cover')] : []),
        ...change.fields.map((f) => t(`book.field.${f}`)),
      ]
    : [];
  const asin = p.values.asin;
  const candidate = item.outcome === 'auto' || item.outcome === 'review';
  const seriesLine = (refs: SeriesRef[]) =>
    refs
      .map((r) => seriesLabel(r.name, r.position, lang, t, r.position_end))
      .filter(Boolean)
      .join('; ');
  // What the scope does to the book's series, old -> new (a series it removes
  // shows), else the community's series.
  const seriesText = change?.series
    ? t('health.bulkMatch.seriesChange', {
        from: seriesLine(change.series.from) || t('health.bulkMatch.noSeries'),
        to: seriesLine(change.series.to) || t('health.bulkMatch.noSeries'),
      })
    : '';
  // The community's main series, then any others the work is in ("Discworld #8; City Watch #1").
  const series = seriesLine([
    seriesRef(p.values.series ?? '', p.values.series_index ?? ''),
    ...moreSeriesRefs(p.values.more_series ?? ''),
  ]);
  // The path tells two books of one title apart, read out with the row's controls
  // too. A book with no title (or one gone) already shows its path as the title.
  const pathId = useId();
  const pathLine = item.book.title ? pathId : undefined;
  const where = joinLibraryPath(root, item.path);

  return (
    <li
      className={cn(
        'flex items-start gap-3 px-3 py-3',
        chosen && 'bg-[color-mix(in_oklab,var(--brand)_7%,transparent)]',
      )}
      // A Shift-click picks a range: it mustn't select the rows' text too.
      onMouseDown={keepShiftClickFromSelectingText(true)}
    >
      <span className="grid size-5 shrink-0 place-items-center pt-0.5">
        {isPickable(item) ? (
          <Checkbox
            checked={chosen}
            onCheckedChange={(_, d) => onToggle(shiftHeld(d.event))}
            aria-label={t('health.bulkMatch.pick', { title })}
            aria-describedby={pathLine}
          />
        ) : status === 'applied' || status === 'matchedSince' ? (
          <Check
            className="size-4 text-success"
            aria-label={t(`health.bulkMatch.item.${status}`)}
          />
        ) : null}
      </span>
      <span className="w-10 shrink-0">
        <BookCover
          libraryId={item.library_id}
          path={item.path}
          title={item.book.title}
          author={item.book.author}
          size={160}
        />
      </span>
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="truncate font-semibold" title={pathLine ? undefined : where}>
          {title}
        </span>
        {/* The path in its library, which tells two books apart; the whole one on hover. */}
        {pathLine ? (
          <span
            id={pathLine}
            className="font-mono text-[11.5px] text-subtle-foreground [overflow-wrap:anywhere]"
            title={where}
          >
            {item.path}
          </span>
        ) : null}
        {candidate ? (
          <span className="text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">
            <ArrowRight className="mr-1 inline size-3.5 align-[-2px]" aria-hidden="true" />
            {[
              p.title,
              p.authors,
              p.narrators ? t('book.match.readBy', { narrators: p.narrators }) : '',
              p.runtime_min ? formatDuration(p.runtime_min * 60, lang) : '',
            ]
              .filter(Boolean)
              .join(' · ')}
          </span>
        ) : (
          <span className="text-[12.5px] text-muted-foreground">
            {t(
              item.outcome === 'error'
                ? 'health.bulkMatch.item.error'
                : item.detail === 'identifier_unknown'
                  ? 'health.bulkMatch.item.identifierUnknown'
                  : 'health.bulkMatch.item.none',
            )}
          </span>
        )}
        {candidate && item.detail === 'length_differs' && p.runtime_min && item.book.duration ? (
          <span className="text-[12.5px] text-warning [overflow-wrap:anywhere]">
            {t('health.bulkMatch.item.lengthDiffers', {
              book: formatDuration(item.book.duration, lang),
              recording: formatDuration(p.runtime_min * 60, lang),
            })}
          </span>
        ) : null}
        {candidate && (seriesText || series) ? (
          <span className="text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">
            {seriesText || t('health.bulkMatch.inSeries', { series })}
          </span>
        ) : null}
        {asin ? (
          <span className="font-mono text-[11.5px] text-subtle-foreground">
            {asin}
            {p.asin_region ? ` · ${regionTag(p.asin_region)}` : ''}
          </span>
        ) : null}
        {item.gone ? (
          <span className="text-[12px] text-subtle-foreground">
            {t('health.bulkMatch.item.gone')}
          </span>
        ) : status ? (
          <span className="text-[12px] text-subtle-foreground">
            {t(`health.bulkMatch.item.${status}`)}
          </span>
        ) : candidate ? (
          <span className="text-[12px] text-subtle-foreground">
            {changed.length
              ? t('health.bulkMatch.changes', { fields: formatList(changed, lang) })
              : t('health.bulkMatch.nothingToChange')}
          </span>
        ) : null}
      </div>
      <div className="flex shrink-0 flex-col items-end gap-1.5">
        {candidate ? (
          <Badge variant={scoreTone(item.score)}>
            {t('book.match.score', { score: item.score })}
          </Badge>
        ) : null}
        {item.outcome === 'review' && item.runner_up ? (
          <span className="text-[11.5px] text-subtle-foreground">
            {t('health.bulkMatch.runnerUp', { score: item.runner_up })}
          </span>
        ) : null}
        {item.gone ? null : (
          <span className="flex items-center gap-0.5">
            <Button
              variant="ghost"
              size="sm"
              onClick={onMatchByHand}
              aria-label={t('health.bulkMatch.matchByHand', { title })}
              aria-describedby={pathLine}
            >
              <span className="max-md:hidden">{t('health.bulkMatch.matchByHandShort')}</span>
              <Sparkles aria-hidden="true" className="md:hidden" />
            </Button>
            {/* The book page in a new tab, to listen or look closer without leaving the review. */}
            <Link
              {...bookRoute(item.library_id, item.path)}
              target="_blank"
              rel="noopener noreferrer"
              className={buttonVariants({ variant: 'ghost', size: 'icon-sm' })}
              aria-label={t('health.bulkMatch.openBook', { title })}
              aria-describedby={pathLine}
            >
              <ExternalLink aria-hidden="true" />
            </Link>
          </span>
        )}
      </div>
    </li>
  );
}
