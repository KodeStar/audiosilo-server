import { useEffect, useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Sparkles } from 'lucide-react';
import { useServerInfo } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { FactList } from '@/components/fact-list';
import { ProgressBar } from '@/components/progress-bar';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import { refKey } from '@/lib/book-route';
import { formatNumber, formatUsd } from '@/lib/format';
import { toast } from '@/lib/toast';
import {
  batchCheck,
  batchPlan,
  contributeOffered,
  destinationKey,
  generatePlan,
  seriesBatchOrder,
  settleLimited,
  type BatchCheck,
  type Contribute,
} from './generate-model';
import { ContributeSwitch, NotWritableNotice } from './generate-parts';
import {
  estimateQuery,
  GENERATE_SKIPS,
  queueInOrder,
  queueReport,
  requestTitle,
  skipNotes,
} from './use-generate';

/** How many books' estimates are asked for at once. */
const CHECK_AT_ONCE = 4;

/**
 * Generate for a whole series: each book's brief estimate is asked for first (a few at
 * a time), so the books that already have recaps (from the community or their
 * own folder) and the ones with a job already open are left out, then the rest
 * are queued in reading order, the earliest first, with what they cost in all. A
 * book that couldn't be checked (its estimate failed, or the community couldn't
 * be asked) is left out too and counted, never queued on a guess.
 */
export function SeriesGenerateDialog({
  open,
  onOpenChange,
  series,
  books,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  series: string;
  books: AdminBook[];
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        title={t('bridge.series.title', { name: series })}
        description={t('bridge.series.description')}
        icon={Sparkles}
        tone="community"
      >
        {open ? (
          <SeriesForm series={series} books={books} onDone={() => onOpenChange(false)} />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function SeriesForm({
  series,
  books,
  onDone,
}: {
  series: string;
  books: AdminBook[];
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const server = useServerInfo().data;
  const ordered = useMemo(() => seriesBatchOrder(books, series), [books, series]);
  const [checked, setChecked] = useState(0);
  const [checks, setChecks] = useState<ReadonlyMap<string, BatchCheck> | null>(null);
  // On by default once offered (Phase 2b): a signed-in Bridge contributes unless told not to.
  const [contribute, setContribute] = useState<Contribute>('issue');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    const ctl = new AbortController();
    void settleLimited(
      ordered,
      CHECK_AT_ONCE,
      // The brief estimate says what each book has; asked afresh, whatever is cached.
      (b) =>
        qc.fetchQuery({ ...estimateQuery(b.library_id, b.path, { brief: true }), staleTime: 0 }),
      (n) => !ctl.signal.aborted && setChecked(n),
      ctl.signal,
    ).then((estimates) => {
      if (ctl.signal.aborted) return;
      setChecks(new Map(ordered.map((b, i) => [refKey(b), batchCheck(estimates[i])])));
    });
    return () => ctl.abort();
  }, [ordered, qc]);

  const plan = checks ? batchPlan(ordered, checks) : undefined;
  const n = plan?.queue.length ?? 0;
  // Contributing only while the module is on and the Bridge can for every book
  // queued (the server refuses it otherwise).
  const canContribute = contributeOffered(server, Boolean(plan?.canContribute));
  const contributeAs: Contribute = canContribute ? contribute : 'local';
  const fmt = (v: number) => formatNumber(v, lang);
  // What the queued books cost in all, naming how many runs that is.
  let cost: string | undefined;
  if (plan && plan.costUsd !== null) {
    const what = plan.subscription
      ? t('bridge.generate.costUsage', { cost: formatUsd(plan.costUsd, lang) })
      : t('bridge.generate.about', { what: formatUsd(plan.costUsd, lang) });
    cost = n > 1 ? t('bridge.generate.forBooks', { count: n, n: fmt(n), what }) : what;
  }

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!plan || !n || plan.notWritable || busy) return;
    setBusy(true);
    setError(undefined);
    // The queue is already in reading order, earlier books included.
    const requests = generatePlan(plan.queue, contributeAs);
    // A book whose chapters started being found since the check is passed over
    // (named), not the end of the batch.
    const out = await queueInOrder(qc, requests, { skip: GENERATE_SKIPS });
    setBusy(false);
    const failed = queueReport(out, requests.length, t, lang);
    if (failed !== undefined) {
      setError(failed);
      return;
    }
    const titles = new Map(plan.queue.map((b) => [refKey(b), b.title]));
    const notes = skipNotes(out, t, lang, (req) => requestTitle(titles, req)).join(' ');
    toast.add(
      out.queued
        ? {
            title: t('bridge.series.queued', { count: out.queued, n: fmt(out.queued) }),
            description: notes || t('bridge.series.queuedBody'),
            type: 'success',
          }
        : { title: t('bridge.queue.none'), description: notes },
    );
    onDone();
  };

  return (
    <form onSubmit={(ev) => void submit(ev)} className="contents" noValidate>
      <DialogBody className="flex flex-col gap-4">
        {!plan ? (
          <div className="flex flex-col gap-2" role="status">
            <span className="text-[13px] text-muted-foreground tabular-nums">
              {t('bridge.series.checking', { done: fmt(checked), total: fmt(ordered.length) })}
            </span>
            <ProgressBar fraction={ordered.length ? checked / ordered.length : undefined} />
          </div>
        ) : (
          <>
            {plan.notWritable ? <NotWritableNotice /> : null}
            <p className="font-semibold">
              {n ? t('bridge.series.willQueue', { count: n, n: fmt(n) }) : t('bridge.series.none')}
            </p>
            {plan.covered || plan.open || plan.unknown ? (
              <ul className="ml-5 flex list-disc flex-col gap-0.5 text-[12.5px] text-muted-foreground">
                {plan.covered ? (
                  <li>
                    {t('bridge.series.skippedCovered', {
                      count: plan.covered,
                      n: fmt(plan.covered),
                    })}
                  </li>
                ) : null}
                {plan.open ? (
                  <li>{t('bridge.series.skippedOpen', { count: plan.open, n: fmt(plan.open) })}</li>
                ) : null}
                {plan.unknown ? (
                  <li>
                    {t('bridge.series.skippedUnknown', {
                      count: plan.unknown,
                      n: fmt(plan.unknown),
                    })}
                  </li>
                ) : null}
              </ul>
            ) : null}
            {n ? (
              <ol className="flex max-h-[220px] flex-col gap-0.5 overflow-y-auto rounded-lg border px-4 py-2.5 text-[12.5px]">
                {plan.queue.map((b) => (
                  <li key={refKey(b)} className="[overflow-wrap:anywhere]">
                    {b.title || b.path}
                  </li>
                ))}
              </ol>
            ) : null}
            {n ? (
              <>
                <FactList
                  rows={[
                    [
                      t('bridge.generate.cost'),
                      cost ?? (
                        <span className="text-muted-foreground">
                          {t('bridge.generate.noEstimate')}
                        </span>
                      ),
                    ],
                  ]}
                />
                {canContribute ? (
                  <ContributeSwitch value={contribute} onChange={setContribute} />
                ) : null}
                <p className="text-[12.5px] text-muted-foreground">
                  {t('bridge.generate.whereLabel', { where: t(destinationKey(contributeAs)) })}
                </p>
              </>
            ) : null}
          </>
        )}
        <FormError>{error}</FormError>
      </DialogBody>
      <DialogFormFooter
        busy={busy}
        disabled={!plan || !n || plan.notWritable}
        submitLabel={t('bridge.series.submit', { count: n, n: fmt(n) })}
      />
    </form>
  );
}
