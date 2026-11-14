import { useEffect, useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import {
  BookCheck,
  BookOpenText,
  CloudDownload,
  CloudOff,
  LoaderCircle,
  Search,
} from 'lucide-react';
import { keys, useBookLibraries, useServerInfo } from '@/api/hooks';
import type { LibrivoxHit, LibrivoxRecording } from '@/api/types';
import { Notice } from '@/components/notice';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { SearchBox } from '@/components/search-box';
import { StatusText, type StatusTone } from '@/components/status-text';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { NativeSelect } from '@/components/ui/native-select';
import { bookRoute } from '@/lib/book-route';
import { formatDuration, formatList, formatNumber } from '@/lib/format';
import { cn } from '@/lib/utils';
import { isOpen, jobGone, openProgress } from '@/features/bridge/bridge-model';
import { BridgeSettingsLink, ExternalTextLink } from '@/features/bridge/generate-parts';
import { useBridge, useTrackedJob } from '@/features/bridge/use-bridge';
import { CardEmpty } from '@/features/settings/card-empty';
import { DONE_RECHECK_MS, bridgeNote, fetchReason, type AddBlock } from './librivox-model';
import { useAddLibrivox, useLibrivoxSearch, useLibrivoxWork } from './use-librivox';

/**
 * Library > LibriVox (LibriVox L6): find a free LibriVox recording in the
 * community metadata database (never librivox.org) and add it to a books
 * library. The Bridge downloads its sections from archive.org, places the book
 * and rescans it, and the server records it as that recording, so it is
 * matched with no console match. Needs community metadata on (else a note
 * says so) and, for Add, a paired Bridge that runs librivox.fetch.
 */
export function LibrivoxPage() {
  const { t } = useTranslation();
  const server = useServerInfo();
  const metadataOn = server.data?.capabilities.metadata;
  const [workId, setWorkId] = useState<string>();

  return (
    <Page>
      <PageHead title={t('librivox.title')} description={t('librivox.description')} />
      {metadataOn === false ? (
        <Notice tone="info" icon={CloudOff} title={t('librivox.metadataOff.title')}>
          {t('librivox.metadataOff.body')}{' '}
          <Link
            to="/server/{-$section}"
            params={{ section: undefined }}
            search={{ topic: 'metadata' }}
            className="font-semibold text-brand-ink hover:underline"
          >
            {t('librivox.metadataOff.link')}
          </Link>
        </Notice>
      ) : (
        <div className="grid grid-cols-1 items-start gap-5 lg:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]">
          <SearchCard enabled={metadataOn === true} selected={workId} onSelect={setWorkId} />
          {workId ? (
            <WorkCard key={workId} id={workId} />
          ) : (
            <Card>
              <CardEmpty
                icon={BookOpenText}
                title={t('librivox.work.none')}
                body={t('librivox.work.noneBody')}
              />
            </Card>
          )}
        </div>
      )}
    </Page>
  );
}

function SearchCard({
  enabled,
  selected,
  onSelect,
}: {
  enabled: boolean;
  selected?: string;
  onSelect: (id: string) => void;
}) {
  const { t } = useTranslation();
  const [q, setQ] = useState('');
  const search = useLibrivoxSearch(q, enabled);
  const hits = search.data?.results ?? [];
  return (
    <Card aria-labelledby="librivox-search-title">
      <CardHeader titleId="librivox-search-title" title={t('librivox.search.title')} />
      <div className="px-5 py-3">
        <SearchBox
          value={q}
          onChange={setQ}
          label={t('librivox.search.label')}
          placeholder={t('librivox.search.placeholder')}
          clearLabel={t('librivox.search.clear')}
        />
      </div>
      {!q.trim() ? (
        <CardEmpty
          icon={Search}
          title={t('librivox.search.prompt')}
          body={t('librivox.search.promptBody')}
        />
      ) : search.isError ? (
        <div className="border-t p-5">
          <QueryError
            title={t('librivox.search.error')}
            error={search.error}
            onRetry={() => void search.refetch()}
          />
        </div>
      ) : search.isPending ? (
        <div className="border-t p-5">
          <div className="skel h-24 rounded-lg" role="status" aria-label={t('common.loading')} />
        </div>
      ) : hits.length === 0 ? (
        <CardEmpty
          icon={Search}
          title={t('librivox.search.none')}
          body={t('librivox.search.noneBody')}
        />
      ) : (
        <ul className="flex flex-col divide-y border-t" aria-label={t('librivox.search.results')}>
          {hits.map((h) => (
            <HitRow key={h.work_id} hit={h} selected={h.work_id === selected} onSelect={onSelect} />
          ))}
        </ul>
      )}
    </Card>
  );
}

function HitRow({
  hit,
  selected,
  onSelect,
}: {
  hit: LibrivoxHit;
  selected: boolean;
  onSelect: (id: string) => void;
}) {
  const { i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const byline = formatList(
    hit.authors.map((a) => a.name),
    lang,
  );
  const facts = [byline, hit.release_date, hit.language].filter(Boolean).join(' · ');
  return (
    <li>
      <button
        type="button"
        aria-pressed={selected}
        onClick={() => onSelect(hit.work_id)}
        className={cn(
          'flex w-full flex-col gap-0.5 px-5 py-3 text-left hover:bg-muted/60 focus-visible:bg-muted/60 focus-visible:outline-none',
          selected && 'bg-muted',
        )}
      >
        <b className="font-semibold [overflow-wrap:anywhere]">{hit.title}</b>
        {facts ? <span className="text-[13px] text-muted-foreground">{facts}</span> : null}
      </button>
    </li>
  );
}

function WorkCard({ id }: { id: string }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const work = useLibrivoxWork(id);
  const bridge = useBridge();
  const noteId = useId();
  const note = bridgeNote(bridge.data, bridge.isError);
  // An offline Bridge is a note, not a block: the job waits for it.
  const block = note === 'offline' ? undefined : note;

  // Only with nothing to show: a failed ask for the work again (after an Add, or
  // when a job ends) keeps the rows, and with them the job each one follows.
  if (work.isError && !work.data)
    return (
      <QueryError
        title={t('librivox.work.error')}
        error={work.error}
        onRetry={() => void work.refetch()}
      />
    );
  if (!work.data)
    return <div className="skel h-48 rounded-xl" role="status" aria-label={t('common.loading')} />;
  const { work: w, recordings } = work.data;
  return (
    <Card aria-labelledby="librivox-work-title">
      <CardHeader
        titleId="librivox-work-title"
        title={w.title}
        description={formatList(
          w.authors.map((a) => a.name),
          lang,
        )}
      />
      {note ? (
        <p id={noteId} className="border-b px-5 py-3 text-[13px] text-muted-foreground">
          {t(`librivox.bridge.${note}`)}{' '}
          {note === 'unpaired' ? (
            <BridgeSettingsLink>{t('librivox.bridge.pair')}</BridgeSettingsLink>
          ) : null}
        </p>
      ) : null}
      {recordings.length === 0 ? (
        <CardEmpty
          icon={BookOpenText}
          title={t('librivox.work.noRecordings')}
          body={t('librivox.work.noRecordingsBody')}
        />
      ) : (
        <ul className="flex flex-col divide-y" aria-label={t('librivox.work.recordings')}>
          {recordings.map((r) => (
            <RecordingRow
              key={r.recording_id}
              workId={id}
              recording={r}
              block={block}
              describedBy={note ? noteId : undefined}
            />
          ))}
        </ul>
      )}
    </Card>
  );
}

function RecordingRow({
  workId,
  recording: r,
  block,
  describedBy,
}: {
  /** The id the work was asked by (its query's key), which a retired id's answer no longer carries. */
  workId: string;
  recording: LibrivoxRecording;
  block?: AddBlock;
  describedBy?: string;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const libraries = useBookLibraries() ?? [];
  const [picked, setPicked] = useState<number>();
  const libraryId = picked ?? libraries[0]?.id;
  const { add, busy } = useAddLibrivox(workId);
  // The job this row follows: the last one it queued or saw open on the work,
  // kept once the work stops naming it (it only names open jobs), so a failed
  // fetch still says why.
  const [followed, setFollowed] = useState(r.job?.id);
  if (r.job && r.job.id !== followed) setFollowed(r.job.id);
  const tracked = useTrackedJob(followed);
  // A job that is gone (deleted with its library) is followed no more: the row
  // goes by what the work says.
  const gone = jobGone(tracked.error);
  const job = gone ? undefined : tracked.data;
  const open = job ? isOpen(job.state) : r.job !== null;
  const state = job?.state;
  const ended = gone || (state !== undefined && !isOpen(state));
  // A job that ends (or is gone) moves the recording (done: it is in the
  // library now), so the work is asked again. The server records a done
  // fetch's book just after the job reads done, so a done one asks once more a
  // moment later: the first answer can still miss the book.
  useEffect(() => {
    if (!ended) return;
    const ask = () => void qc.invalidateQueries({ queryKey: keys.librivoxWork(workId) });
    ask();
    if (state !== 'done') return;
    const timer = setTimeout(ask, DONE_RECHECK_MS);
    return () => clearTimeout(timer);
  }, [ended, state, qc, workId]);

  const readers = formatList(
    r.narrators.map((n) => n.name),
    lang,
  );
  const facts = [
    r.runtime_min > 0 ? formatDuration(r.runtime_min * 60, lang) : '',
    r.chapter_count > 0
      ? t('librivox.recording.sections', {
          count: r.chapter_count,
          n: formatNumber(r.chapter_count, lang),
        })
      : '',
    t('librivox.recording.project', { id: r.librivox_id }),
  ].filter(Boolean);
  const here = r.in_library.length > 0;
  const progress = job ? openProgress(job) : {};
  const reason = job ? fetchReason(job) : undefined;
  const status = ((): { tone: StatusTone; text: string } | undefined => {
    if (open) {
      // A parked fetch says why (the disk is full, the library is read-only).
      if (state === 'needs_attention' && reason) return { tone: 'warn', text: t(reason) };
      const count = progress.total
        ? t('bridge.jobs.count', {
            done: formatNumber(progress.done ?? 0, lang),
            total: formatNumber(progress.total, lang),
          })
        : '';
      const label = progress.text ?? t(`bridge.state.${state ?? r.job?.state ?? 'queued'}`);
      // With several libraries, which one it is going into (the open job is
      // the project's in any library, so not always the one picked here).
      const into =
        libraries.length > 1 && job?.library_id != null
          ? libraries.find((l) => l.id === job.library_id)?.name
          : undefined;
      return {
        tone: 'warn',
        text: [into ? t('librivox.recording.addingTo', { library: into }) : '', label, count]
          .filter(Boolean)
          .join(' · '),
      };
    }
    if (job && !here && (state === 'failed' || state === 'cancelled'))
      return {
        tone: 'bad',
        text: reason ? t(reason) : job.error || t(`bridge.state.${job.state}`),
      };
    return undefined;
  })();

  return (
    <li className="flex flex-wrap items-start gap-x-4 gap-y-2 px-5 py-3.5">
      <div className="flex min-w-0 flex-1 basis-64 flex-col gap-0.5">
        <b className="font-semibold [overflow-wrap:anywhere]">
          {readers
            ? t('librivox.recording.readBy', { names: readers })
            : t('librivox.recording.unknownReader')}
        </b>
        <span className="font-mono text-[12px] text-muted-foreground">{facts.join(' · ')}</span>
        {r.url ? (
          <ExternalTextLink href={r.url}>{t('librivox.recording.page')}</ExternalTextLink>
        ) : null}
        {status ? (
          <StatusText tone={status.tone} colored className="text-[12.5px] font-semibold">
            {status.text}
          </StatusText>
        ) : null}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        {here ? (
          r.in_library.map((b) => (
            <Link
              key={`${b.library_id}/${b.path}`}
              {...bookRoute(b.library_id, b.path)}
              className="inline-flex items-center gap-1 text-[12.5px] font-semibold text-primary hover:underline"
            >
              <BookCheck className="size-3.5" aria-hidden="true" />
              {t('librivox.recording.inLibrary')}
            </Link>
          ))
        ) : libraries.length === 0 ? (
          <span className="text-[12.5px] text-muted-foreground">{t('librivox.add.noLibrary')}</span>
        ) : (
          <>
            {libraries.length > 1 ? (
              <NativeSelect
                className="w-[180px]"
                value={String(libraryId)}
                onChange={(e) => setPicked(Number(e.target.value))}
                aria-label={t('librivox.add.library')}
              >
                {libraries.map((l) => (
                  <option key={l.id} value={l.id}>
                    {l.name}
                  </option>
                ))}
              </NativeSelect>
            ) : null}
            <Button
              size="sm"
              disabled={Boolean(block) || open || busy || libraryId === undefined}
              aria-describedby={block ? describedBy : undefined}
              onClick={() => {
                if (libraryId === undefined) return;
                void add(libraryId, r.librivox_id).then((j) => j && setFollowed(j.id));
              }}
            >
              {busy ? (
                <LoaderCircle className="animate-spin" aria-hidden="true" />
              ) : (
                <CloudDownload aria-hidden="true" />
              )}
              {libraries.length > 1
                ? t('librivox.add.action')
                : t('librivox.add.actionTo', { library: libraries[0]?.name ?? '' })}
            </Button>
          </>
        )}
      </div>
    </li>
  );
}
