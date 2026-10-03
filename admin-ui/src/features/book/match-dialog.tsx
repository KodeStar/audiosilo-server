import { useState } from 'react';
import { Radio } from '@base-ui/react/radio';
import { RadioGroup } from '@base-ui/react/radio-group';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  ArrowRight,
  Check,
  ChevronLeft,
  ExternalLink,
  LoaderCircle,
  RotateCw,
  Search,
  Sparkles,
} from 'lucide-react';
import { ApiError, api } from '@/api/client';
import { settleBookEdit, useMatchCandidates } from '@/api/hooks';
import type { AdminBookDetail, MatchCandidate, MatchRecording, OverrideField } from '@/api/types';
import { GeneratedCover } from '@/components/generated-cover';
import { ProvenanceMarker } from '@/components/provenance';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import {
  Dialog,
  DialogBody,
  DialogClose,
  DialogContent,
  DialogFooter,
} from '@/components/ui/dialog';
import { NativeSelect } from '@/components/ui/native-select';
import { errorMessage, toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { formatLength } from './book-model';
import {
  acceptRequest,
  compareRows,
  defaultRecording,
  defaultTicks,
  lengthComparison,
  parseMatchQuery,
  scoreTone,
  type CompareRow,
  type MatchBy,
} from './match-model';

const clip = (s: string, n: number) => (s.length > n ? `${s.slice(0, n - 3)}...` : s);

/**
 * Match with community metadata (STYLEGUIDE.md "Match with community"): find
 * the work (by the book's own facts, words, or an ASIN / ISBN), then compare
 * field by field and accept the ticked ones. The admin's own edits start
 * unticked. Mount it fresh for each opening (a `key`), so it starts at the
 * search.
 */
export function MatchDialog({
  open,
  onOpenChange,
  detail,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  detail: AdminBookDetail;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const b = detail.book;
  const [step, setStep] = useState<'search' | 'compare'>('search');
  const [query, setQuery] = useState([b.title, b.author].filter(Boolean).join(' '));
  const [by, setBy] = useState<MatchBy>({});
  const [pickId, setPickId] = useState<string>();
  const [recId, setRecId] = useState<string>();
  const [ticks, setTicks] = useState<Set<OverrideField>>(new Set());
  const [busy, setBusy] = useState(false);
  const search = useMatchCandidates(b.library_id, b.path, by, open);
  const candidates = search.data ?? [];
  const picked = candidates.find((c) => c.work_id === pickId) ?? candidates[0];
  const recordingOf = (c: MatchCandidate, id = recId) =>
    c.recordings?.find((r) => r.id === id) ?? defaultRecording(c, b.duration);
  const rec = picked ? recordingOf(picked) : undefined;
  const rows = picked ? compareRows(detail.fields, picked, rec) : [];

  const compare = () => {
    setTicks(defaultTicks(rows));
    setStep('compare');
  };

  const pickRecording = (id: string) => {
    setRecId(id);
    if (picked) setTicks(defaultTicks(compareRows(detail.fields, picked, recordingOf(picked, id))));
  };

  const accept = async () => {
    setBusy(true);
    try {
      settleBookEdit(qc, await api.editBook(b.library_id, b.path, acceptRequest(rows, ticks)));
      const tookEdits = rows.some((r) => ticks.has(r.field) && r.offered && r.source === 'edited');
      toast.add({
        title: t('book.match.accepted', { count: ticks.size }),
        description: tookEdits
          ? t('book.match.acceptedTitle', { title: picked?.title ?? '' })
          : t('book.match.acceptedBody'),
        type: 'success',
      });
      onOpenChange(false);
    } catch (err) {
      setBusy(false);
      toastError(t('book.match.acceptFailed'), err);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        size="lg"
        tone="community"
        icon={Sparkles}
        title={step === 'search' ? t('book.match.title') : t('book.match.compareTitle')}
        description={
          step === 'search' ? t('book.match.description') : t('book.match.compareDescription')
        }
      >
        {step === 'search' ? (
          <>
            <DialogBody className="flex flex-col gap-3.5">
              <form
                role="search"
                className="flex flex-wrap items-center gap-2"
                onSubmit={(e) => {
                  e.preventDefault();
                  setBy(parseMatchQuery(query));
                  setPickId(undefined);
                  setRecId(undefined);
                }}
              >
                <label className="flex h-[38px] min-w-0 flex-1 basis-64 items-center gap-2 rounded-md border border-input bg-card px-3 focus-within:border-ring focus-within:shadow-[0_0_0_3px_color-mix(in_oklab,var(--ring)_20%,transparent)]">
                  <Search className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
                  <input
                    value={query}
                    onChange={(e) => setQuery(e.target.value)}
                    aria-label={t('book.match.searchLabel')}
                    className="h-full min-w-0 flex-1 bg-transparent text-sm outline-none"
                  />
                  <span className="hidden text-[12px] whitespace-nowrap text-subtle-foreground md:inline">
                    {t('book.match.idHint')}
                  </span>
                </label>
                <Button type="submit" variant="outline">
                  {t('book.match.search')}
                </Button>
              </form>
              <Candidates
                search={search}
                candidates={candidates}
                picked={picked}
                bookSeconds={b.duration}
                recordingOf={recordingOf}
                onPick={(id) => {
                  setPickId(id);
                  setRecId(undefined);
                }}
              />
            </DialogBody>
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="ghost" />}>
                {t('common.cancel')}
              </DialogClose>
              <Button onClick={compare} disabled={!picked}>
                {t('book.match.compareFields')}
                <ArrowRight aria-hidden="true" />
              </Button>
            </DialogFooter>
          </>
        ) : picked ? (
          <>
            <DialogBody className="flex flex-col gap-4">
              <CompareHeader
                candidate={picked}
                recording={rec}
                bookSeconds={b.duration}
                onRecording={pickRecording}
              />
              <CompareTable
                rows={rows}
                ticks={ticks}
                onToggle={(f, on) => {
                  const next = new Set(ticks);
                  if (on) next.add(f);
                  else next.delete(f);
                  setTicks(next);
                }}
              />
            </DialogBody>
            <DialogFooter>
              <Button variant="ghost" className="mr-auto" onClick={() => setStep('search')}>
                <ChevronLeft aria-hidden="true" />
                {t('book.match.back')}
              </Button>
              <DialogClose render={<Button type="button" variant="ghost" />}>
                {t('common.cancel')}
              </DialogClose>
              <Button onClick={() => void accept()} disabled={busy || ticks.size === 0}>
                {busy ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
                {t('book.match.accept', { count: ticks.size })}
              </Button>
            </DialogFooter>
          </>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function recordingLine(
  rec: MatchRecording | undefined,
  t: (key: string, opts?: Record<string, unknown>) => string,
): string {
  if (!rec) return '';
  const narrators = (rec.narrators ?? []).map((n) => n.name).join(', ');
  return [
    narrators ? t('book.match.readBy', { narrators }) : '',
    rec.runtime_min ? formatLength(rec.runtime_min * 60, t) : '',
    rec.publisher ?? '',
    rec.abridged ? t('book.match.abridged') : '',
  ]
    .filter(Boolean)
    .join(' · ');
}

function Candidates({
  search,
  candidates,
  picked,
  bookSeconds,
  recordingOf,
  onPick,
}: {
  search: ReturnType<typeof useMatchCandidates>;
  candidates: MatchCandidate[];
  picked: MatchCandidate | undefined;
  bookSeconds: number;
  recordingOf: (c: MatchCandidate) => MatchRecording | undefined;
  onPick: (workId: string) => void;
}) {
  const { t } = useTranslation();
  if (search.isPending) {
    return (
      <div className="flex flex-col gap-2" role="status" aria-label={t('book.match.searching')}>
        {[0, 1, 2].map((i) => (
          <div key={i} className="skel h-[84px] rounded-[12px]" />
        ))}
      </div>
    );
  }
  if (search.isError) {
    const err = search.error;
    const off = err instanceof ApiError && err.code === 'metadata_off';
    const message = off
      ? t('book.match.off')
      : err instanceof ApiError && err.status === 502
        ? t('book.match.unreachable')
        : errorMessage(err, t);
    return (
      <div
        role="alert"
        className="flex flex-wrap items-center justify-between gap-3 rounded-[12px] border px-4 py-3.5"
      >
        <p className="text-muted-foreground">{message}</p>
        {off ? null : (
          <Button variant="outline" size="sm" onClick={() => void search.refetch()}>
            <RotateCw aria-hidden="true" />
            {t('common.tryAgain')}
          </Button>
        )}
      </div>
    );
  }
  if (!candidates.length) {
    return (
      <p className="rounded-[12px] border border-dashed px-4 py-6 text-center text-muted-foreground">
        {t('book.match.none')}
      </p>
    );
  }
  return (
    <RadioGroup
      value={picked?.work_id ?? null}
      onValueChange={(v) => onPick(v as string)}
      aria-label={t('book.match.candidates')}
      className="flex flex-col gap-2"
    >
      {candidates.map((c) => {
        const rec = recordingOf(c);
        const length = lengthComparison(rec?.runtime_min, bookSeconds);
        const authors = (c.authors ?? []).map((a) => a.name).join(', ');
        return (
          <Radio.Root
            key={c.work_id}
            value={c.work_id}
            className="group flex w-full cursor-pointer items-center gap-3 rounded-[12px] border bg-card px-3.5 py-3 text-left transition-colors duration-(--dur-1) outline-none hover:border-border-strong data-checked:border-brand data-checked:bg-[color-mix(in_oklab,var(--brand)_6%,var(--card))]"
          >
            <span className="grid size-[18px] shrink-0 place-items-center rounded-full border-[1.5px] border-border-strong group-data-checked:border-brand">
              <Radio.Indicator className="size-2.5 rounded-full bg-brand" />
            </span>
            <span className="w-14 shrink-0">
              <GeneratedCover title={c.title} author={authors} />
            </span>
            <span className="flex min-w-0 flex-1 flex-col gap-0.5">
              <b className="font-semibold [overflow-wrap:anywhere]">
                {c.title}
                {c.subtitle ? (
                  <span className="font-medium text-muted-foreground"> · {c.subtitle}</span>
                ) : null}
              </b>
              <span className="text-[12.5px] text-muted-foreground">
                {[authors, recordingLine(rec, t)].filter(Boolean).join(' · ')}
              </span>
              {rec?.asins?.[0] || rec?.isbns?.[0] ? (
                <span className="font-mono text-[12px] text-subtle-foreground">
                  {rec.asins?.[0] || rec.isbns?.[0]}
                </span>
              ) : null}
            </span>
            <span className="flex shrink-0 flex-col items-end gap-1 text-right">
              <Badge variant={scoreTone(c.score)}>
                {t('book.match.score', { score: Math.round(c.score) })}
              </Badge>
              {length ? (
                <span className="text-[11.5px] text-subtle-foreground">
                  {length.kind === 'match'
                    ? t('book.match.lengthMatches')
                    : t(`book.match.${length.kind}`, { length: formatLength(length.seconds, t) })}
                </span>
              ) : null}
            </span>
          </Radio.Root>
        );
      })}
    </RadioGroup>
  );
}

function CompareHeader({
  candidate: c,
  recording,
  bookSeconds,
  onRecording,
}: {
  candidate: MatchCandidate;
  recording: MatchRecording | undefined;
  bookSeconds: number;
  onRecording: (id: string) => void;
}) {
  const { t } = useTranslation();
  const authors = (c.authors ?? []).map((a) => a.name).join(', ');
  const recs = c.recordings ?? [];
  const length = lengthComparison(recording?.runtime_min, bookSeconds);
  return (
    <div className="flex flex-wrap items-center gap-3.5">
      <span className="w-[52px] shrink-0">
        <GeneratedCover title={c.title} author={authors} />
      </span>
      <div className="flex min-w-0 flex-1 basis-60 flex-col gap-1">
        <b className="font-semibold [overflow-wrap:anywhere]">{c.title}</b>
        {recs.length > 1 ? (
          <label className="flex flex-wrap items-center gap-2 text-[12.5px] text-muted-foreground">
            {t('book.match.edition')}
            <NativeSelect
              className="max-w-full flex-1 basis-56"
              value={recording?.id}
              onChange={(e) => onRecording(e.target.value)}
            >
              {recs.map((r) => (
                <option key={r.id} value={r.id}>
                  {recordingLine(r, t) || r.id}
                </option>
              ))}
            </NativeSelect>
          </label>
        ) : (
          <span className="text-[12.5px] text-muted-foreground">{recordingLine(recording, t)}</span>
        )}
        {length ? (
          <span className="text-[12px] text-subtle-foreground">
            {length.kind === 'match'
              ? t('book.match.lengthMatches')
              : t(`book.match.${length.kind}`, { length: formatLength(length.seconds, t) })}
          </span>
        ) : null}
      </div>
      {c.web_url ? (
        <a
          href={c.web_url}
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center gap-1 text-[12.5px] font-semibold text-brand-ink hover:underline"
        >
          {t('book.match.open')}
          <ExternalLink className="size-3.5" aria-hidden="true" />
        </a>
      ) : null}
    </div>
  );
}

function CompareTable({
  rows,
  ticks,
  onToggle,
}: {
  rows: CompareRow[];
  ticks: Set<OverrideField>;
  onToggle: (field: OverrideField, on: boolean) => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="overflow-x-auto rounded-[12px] border">
      <table className="w-full min-w-[520px] text-[13px]">
        <thead className="text-[12px] text-muted-foreground">
          <tr className="border-b text-left">
            <th className="w-10 px-3 py-2.5">
              <span className="sr-only">{t('book.match.take')}</span>
            </th>
            <th className="px-2 py-2.5 font-medium">{t('book.match.field')}</th>
            <th className="px-2 py-2.5 font-medium">{t('book.match.yours')}</th>
            <th className="px-3 py-2.5 font-medium">{t('book.match.theirs')}</th>
          </tr>
        </thead>
        <tbody className="divide-y">
          {rows.map((r) => {
            const label = t(`book.field.${r.field}`);
            const on = ticks.has(r.field);
            return (
              <tr
                key={r.field}
                className={cn(
                  'align-top',
                  r.same && 'opacity-55',
                  on && 'bg-[color-mix(in_oklab,var(--brand)_7%,transparent)]',
                )}
              >
                <td className="px-3 py-2.5">
                  {r.offered ? (
                    <Checkbox
                      checked={on}
                      onCheckedChange={(v) => onToggle(r.field, v)}
                      aria-label={t('book.match.acceptField', { field: label })}
                    />
                  ) : (
                    <Check className="size-[15px] text-subtle-foreground" aria-hidden="true" />
                  )}
                </td>
                <th scope="row" className="px-2 py-2.5 text-left font-semibold whitespace-nowrap">
                  {label}
                </th>
                <td className="px-2 py-2.5">
                  <div className="flex flex-col items-start gap-1">
                    <span
                      className={cn(
                        '[overflow-wrap:anywhere]',
                        on && 'text-muted-foreground line-through',
                        r.field === 'description' && 'text-muted-foreground',
                      )}
                    >
                      {r.mine ? (
                        clip(r.mine, 120)
                      ) : (
                        <i className="text-subtle-foreground">{t('book.match.empty')}</i>
                      )}
                    </span>
                    <ProvenanceMarker source={r.source} />
                  </div>
                </td>
                <td
                  className={cn(
                    'px-3 py-2.5 [overflow-wrap:anywhere]',
                    on && 'font-[550] text-foreground',
                  )}
                >
                  {r.same ? (
                    <span className="text-subtle-foreground">{t('book.match.same')}</span>
                  ) : r.theirs ? (
                    clip(r.theirs, 150)
                  ) : (
                    <i className="text-subtle-foreground">{t('book.match.notProvided')}</i>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
