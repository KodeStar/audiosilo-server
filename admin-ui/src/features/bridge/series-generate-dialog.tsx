import { useEffect, useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { LockKeyhole, Sparkles } from 'lucide-react';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { Notice } from '@/components/notice';
import { ProgressBar } from '@/components/progress-bar';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import { Switch } from '@/components/ui/switch';
import { refKey } from '@/lib/book-route';
import { formatNumber } from '@/lib/format';
import { toast } from '@/lib/toast';
import {
  batchCheck,
  batchPlan,
  destinationKey,
  generatePlan,
  KIND_GENERATE,
  seriesBatchOrder,
  settleLimited,
  type BatchCheck,
} from './generate-model';
import { generateError, queueInOrder } from './use-generate';

/** How many books' estimates are asked for at once. */
const CHECK_AT_ONCE = 4;

/**
 * Generate for a whole series: each book's estimate is asked for first (a few at
 * a time), so the books that already have recaps (from the community or their
 * own folder) and the ones with a job already open are left out, then the rest
 * are queued in reading order, the earliest first. A book whose estimate fails
 * is queued anyway: the server returns an open job instead of a second one.
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
  const ordered = useMemo(() => seriesBatchOrder(books, series), [books, series]);
  const [checked, setChecked] = useState(0);
  const [checks, setChecks] = useState<ReadonlyMap<string, BatchCheck> | null>(null);
  const [contribute, setContribute] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    const ctl = new AbortController();
    void settleLimited(
      ordered,
      CHECK_AT_ONCE,
      (b) =>
        qc.fetchQuery({
          queryKey: keys.bridgeEstimate(b.library_id, b.path),
          queryFn: () => api.bridgeEstimate(b.library_id, b.path, KIND_GENERATE),
          staleTime: 0,
          retry: false,
        }),
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
  const fmt = (v: number) => formatNumber(v, lang);

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!plan || !n || plan.notWritable || busy) return;
    setBusy(true);
    setError(undefined);
    // Each book on its own, without its earlier books: the batch already holds them, in order.
    const requests = plan.queue.flatMap((b) =>
      generatePlan(b, [], { includeEarlier: false, contribute: contribute ? 'issue' : 'local' }),
    );
    const out = await queueInOrder(qc, requests);
    setBusy(false);
    if (out.error !== undefined) {
      const why = generateError(out.error, t);
      setError(
        out.queued
          ? t('bridge.generate.partial', {
              done: fmt(out.queued),
              total: fmt(requests.length),
              error: why,
            })
          : why,
      );
      return;
    }
    toast.add({
      title: t('bridge.series.queued', { count: requests.length, n: fmt(requests.length) }),
      description: t('bridge.series.queuedBody'),
      type: 'success',
    });
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
            {plan.notWritable ? (
              <Notice
                tone="bad"
                icon={LockKeyhole}
                role="alert"
                title={t('bridge.generate.notWritable')}
              >
                {t('bridge.generate.notWritableBody')}
              </Notice>
            ) : null}
            <p className="font-semibold">
              {n ? t('bridge.series.willQueue', { count: n, n: fmt(n) }) : t('bridge.series.none')}
            </p>
            {plan.covered || plan.open ? (
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
                <label
                  htmlFor="series-contribute"
                  className="flex cursor-pointer items-start justify-between gap-4 rounded-lg border px-4 py-3"
                >
                  <span className="flex min-w-0 flex-col gap-0.5">
                    <span className="font-semibold">{t('bridge.generate.contribute')}</span>
                    <span className="text-[12.5px] text-muted-foreground">
                      {t('bridge.generate.contributeBody')}
                    </span>
                  </span>
                  <Switch
                    id="series-contribute"
                    checked={contribute}
                    onCheckedChange={(on) => setContribute(on)}
                  />
                </label>
                <p className="text-[12.5px] text-muted-foreground">
                  {t('bridge.generate.whereLabel', {
                    where: t(destinationKey(contribute ? 'issue' : 'local')),
                  })}
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
