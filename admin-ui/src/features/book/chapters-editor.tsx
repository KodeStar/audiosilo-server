import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  ClipboardPaste,
  Headphones,
  Lock,
  Pause,
  Play,
  Plus,
  Trash2,
  TriangleAlert,
} from 'lucide-react';
import { api, streamUrl } from '@/api/client';
import {
  invalidateIssues,
  keys,
  settleBookEdit,
  useBookMediaToken,
  useServerInfo,
} from '@/api/hooks';
import type { AdminBookDetail } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { LeaveGuard } from '@/components/leave-guard';
import { Button } from '@/components/ui/button';
import { FormError } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { errorMessage } from '@/lib/errors';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import {
  addAfter,
  addAt,
  detailSpans,
  draftChanged,
  draftFromChapters,
  type DraftRow,
  type Edit,
  editRequest,
  type FileSpan,
  formatStamp,
  parseTime,
  removeRow,
  roundMs,
  saveProblem,
  setStart,
  setTitle,
  shiftAll,
  timelineEnd,
  toBookTime,
  toFileTime,
  validateDraft,
} from './chapters-edit-model';
import { PasteTimestampsDialog } from './chapters-paste-dialog';

/**
 * The Chapters card's edit mode (server#240): one row per chapter, its start
 * (whole-book time) and title, a file's first chapter fixed in time; Add (at the
 * playhead, after a row), Delete, Shift all, Paste timestamps; a player on the
 * chosen row's file to find the right moment; Save sends the list against the
 * files it was opened on (a 409 says they changed since).
 */
export function ChaptersEditor({
  detail,
  onClose,
}: {
  detail: AdminBookDetail;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const b = detail.book;
  // What the editor opened on: a page refresh mid-edit changes none of it.
  const [opened] = useState(() => {
    const spans = detailSpans(detail);
    return {
      spans,
      basis: detail.chapter_basis,
      initial: draftFromChapters(detail.chapters ?? [], spans),
    };
  });
  const { spans, basis } = opened;
  const end = timelineEnd(spans);
  const withHours = end >= 3600;
  const [rows, setRows] = useState(opened.initial);
  const [selected, setSelected] = useState(opened.initial[0]?.key);
  // Why the last action was refused, or the server's answer to a save.
  const [notice, setNotice] = useState<string>();
  const [problem, setProblem] = useState<string>();
  const [saving, setSaving] = useState(false);
  const [pasting, setPasting] = useState(false);
  const [discarding, setDiscarding] = useState(false);
  const [shift, setShift] = useState('');
  const shiftId = useId();

  const dirty = useMemo(() => draftChanged(rows, opened.initial), [rows, opened.initial]);
  const check = useMemo(() => validateDraft(rows, spans), [rows, spans]);
  // The file each fixed row starts, by its start (of files that start together,
  // the last: the one with audio).
  const fileAt = useMemo(() => new Map(spans.map((s) => [roundMs(s.start), s.name])), [spans]);
  const number = (key?: string) => rows.findIndex((r) => r.key === key) + 1;
  const newTitle = () => t('book.chapters.edit.newTitle');

  const apply = (edit: Edit) => {
    if ('refused' in edit) {
      setNotice(t(`book.chapters.edit.refused.${edit.refused}`, { n: number(edit.key) }));
      return false;
    }
    setNotice(undefined);
    setRows(edit.rows);
    if (edit.key) setSelected(edit.key);
    return true;
  };

  const doShift = () => {
    const delta = Number(shift.trim().replace(',', '.'));
    if (!shift.trim() || !Number.isFinite(delta)) {
      setNotice(t('book.chapters.edit.refused.shift'));
      return;
    }
    if (apply(shiftAll(rows, spans, delta))) setShift('');
  };

  const save = async () => {
    setSaving(true);
    setProblem(undefined);
    try {
      settleBookEdit(qc, await api.saveChapters(b.library_id, b.path, editRequest(rows, basis)));
      invalidateIssues(qc);
      // The save cancelled the book's chapter detection if it was still queued (server#410).
      void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
      toast.add({
        title: t('book.chapters.edit.saved'),
        description: t('book.chapters.edit.savedBody'),
        type: 'success',
      });
      onClose();
    } catch (err) {
      const key = saveProblem(err);
      setProblem(key ? t(key) : errorMessage(err, t));
      // The files changed: the page reads them again for the next edit.
      if (key === 'book.chapters.edit.basis') {
        void qc.invalidateQueries({ queryKey: keys.book(b.library_id, b.path) });
      }
      setSaving(false);
    }
  };

  const chosen = rows.find((r) => r.key === selected);

  return (
    <div className="flex flex-col gap-3.5">
      <p className="text-[13.5px] text-muted-foreground">{t('book.chapters.edit.intro')}</p>
      <Scrubber
        libraryId={b.library_id}
        bookPath={b.path}
        spans={spans}
        row={chosen}
        n={number(selected)}
        withHours={withHours}
        onSetHere={(at) => chosen && apply(setStart(rows, spans, chosen.key, at))}
        onAddHere={(at) => apply(addAt(rows, spans, at, newTitle()))}
      />
      <div className="flex flex-wrap items-end gap-2">
        <div className="flex items-end gap-2">
          <label className="flex flex-col gap-1 text-[12.5px] font-medium" htmlFor={shiftId}>
            {t('book.chapters.edit.shiftLabel')}
            <Input
              id={shiftId}
              inputMode="decimal"
              value={shift}
              placeholder="+2.5"
              onChange={(e) => setShift(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault();
                  doShift();
                }
              }}
              className="h-[30px] w-24 text-[13px] tabular-nums"
            />
          </label>
          <Button variant="outline" size="sm" onClick={doShift}>
            {t('book.chapters.edit.shift')}
          </Button>
        </div>
        <Button variant="outline" size="sm" onClick={() => setPasting(true)}>
          <ClipboardPaste aria-hidden="true" />
          {t('book.chapters.edit.paste')}
        </Button>
      </div>
      <p aria-live="polite" className="min-h-0 text-[13px] text-warning empty:hidden">
        {notice}
      </p>
      <ol
        aria-label={t('book.chapters.edit.listLabel')}
        className="flex max-h-[60vh] flex-col overflow-y-auto"
      >
        {rows.map((r, i) => (
          <EditRow
            key={r.key}
            row={r}
            n={i + 1}
            file={r.fixed ? fileAt.get(r.start) : undefined}
            withHours={withHours}
            selected={r.key === selected}
            problem={check.rows.get(r.key)}
            onSelect={() => setSelected(r.key)}
            onTitle={(title) => setRows(setTitle(rows, r.key, title))}
            onStart={(at) => apply(setStart(rows, spans, r.key, at))}
            onBadTime={() => setNotice(t('book.chapters.edit.refused.badTime'))}
            onAddAfter={() => apply(addAfter(rows, spans, r.key, newTitle()))}
            onDelete={() => {
              // The player stays on a chapter: the one before (a file's start, at least).
              if (apply(removeRow(rows, r.key)) && r.key === selected) {
                setSelected(rows[i - 1]?.key);
              }
            }}
          />
        ))}
      </ol>
      {check.first ? (
        <p className="flex items-start gap-2 text-[13px] text-destructive">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
          {t(`book.chapters.edit.invalid.${check.first}`)}
        </p>
      ) : null}
      <FormError>{problem}</FormError>
      <div className="flex flex-wrap items-center justify-end gap-2 border-t pt-3.5">
        <Button variant="ghost" onClick={() => (dirty ? setDiscarding(true) : onClose())}>
          {t('common.cancel')}
        </Button>
        <Button disabled={!dirty || !!check.first || saving} onClick={() => void save()}>
          {t('book.chapters.edit.save')}
        </Button>
      </div>
      <PasteTimestampsDialog
        open={pasting}
        onOpenChange={setPasting}
        spans={spans}
        current={rows}
        withHours={withHours}
        onReplace={(next) => {
          setRows(next);
          setSelected(next[0]?.key);
          setNotice(undefined);
        }}
      />
      <ConfirmDialog
        open={discarding}
        onOpenChange={setDiscarding}
        icon={TriangleAlert}
        title={t('book.chapters.edit.discardTitle')}
        description={t('book.chapters.edit.discardBody')}
        confirmLabel={t('book.chapters.edit.discard')}
        onConfirm={async () => onClose()}
      />
      <LeaveGuard
        dirty={dirty && !saving}
        title={t('book.chapters.edit.discardTitle')}
        description={t('book.chapters.edit.discardBody')}
        stay={t('book.leave.stay')}
        discard={t('book.leave.discard')}
      />
    </div>
  );
}

function EditRow({
  row,
  n,
  file,
  withHours,
  selected,
  problem,
  onSelect,
  onTitle,
  onStart,
  onBadTime,
  onAddAfter,
  onDelete,
}: {
  row: DraftRow;
  n: number;
  /** The file a fixed row starts. */
  file?: string;
  withHours: boolean;
  selected: boolean;
  problem?: string;
  onSelect: () => void;
  onTitle: (title: string) => void;
  onStart: (at: number) => void;
  onBadTime: () => void;
  onAddAfter: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const timeBad = problem === 'order' || problem === 'out_of_range';
  return (
    <li
      className={cn(
        'grid grid-cols-[28px_minmax(0,1fr)] items-center gap-x-2 gap-y-1.5 rounded-[10px] px-2 py-1.5 sm:grid-cols-[28px_118px_minmax(0,1fr)_auto]',
        selected && 'bg-muted',
      )}
    >
      <Button
        variant="ghost-muted"
        size="icon-sm"
        aria-pressed={selected}
        aria-label={t('book.chapters.edit.listen', { n })}
        title={t('book.chapters.edit.listen', { n })}
        onClick={onSelect}
        className={cn(selected && 'text-brand-ink')}
      >
        {selected ? (
          <Headphones aria-hidden="true" />
        ) : (
          <span className="text-[12px] tabular-nums">{n}</span>
        )}
      </Button>
      {row.fixed ? (
        <span
          className="flex h-[30px] items-center gap-1.5 px-1 text-[13px] text-muted-foreground tabular-nums"
          title={t('book.chapters.edit.fixed', { file })}
        >
          <Lock className="size-3.5 shrink-0" aria-hidden="true" />
          {formatStamp(row.start, withHours)}
          <span className="sr-only">{t('book.chapters.edit.fixed', { file })}</span>
        </span>
      ) : (
        <TimeInput
          value={row.start}
          withHours={withHours}
          label={t('book.chapters.edit.time', { n })}
          invalid={timeBad}
          onCommit={onStart}
          onBad={onBadTime}
        />
      )}
      <Input
        value={row.title}
        onChange={(e) => onTitle(e.target.value)}
        aria-label={t('book.chapters.edit.titleOf', { n })}
        aria-invalid={problem === 'title' ? true : undefined}
        className="col-span-2 h-[30px] text-[13.5px] sm:col-span-1"
      />
      <span className="col-span-2 flex justify-end gap-1 sm:col-span-1">
        <Button
          variant="ghost-muted"
          size="icon-sm"
          aria-label={t('book.chapters.edit.addAfter', { n })}
          title={t('book.chapters.edit.addAfter', { n })}
          onClick={onAddAfter}
        >
          <Plus aria-hidden="true" />
        </Button>
        <Button
          variant="ghost-muted"
          size="icon-sm"
          disabled={row.fixed}
          aria-label={t('book.chapters.edit.delete', { n })}
          title={
            row.fixed
              ? t('book.chapters.edit.fixed', { file })
              : t('book.chapters.edit.delete', { n })
          }
          onClick={onDelete}
        >
          <Trash2 aria-hidden="true" />
        </Button>
      </span>
    </li>
  );
}

/** A start typed as a time: committed on Enter or leaving the field, back on Escape. */
function TimeInput({
  value,
  withHours,
  label,
  invalid,
  onCommit,
  onBad,
}: {
  value: number;
  withHours: boolean;
  label: string;
  invalid: boolean;
  onCommit: (at: number) => void;
  onBad: () => void;
}) {
  const shown = formatStamp(value, withHours);
  const [text, setText] = useState(shown);
  const [bad, setBad] = useState(false);
  // A moved row shows its new time, in the same field (it keeps the focus).
  const [last, setLast] = useState(shown);
  if (last !== shown) {
    setLast(shown);
    setText(shown);
    setBad(false);
  }
  const commit = () => {
    if (text.trim() === shown) return;
    const at = parseTime(text);
    if (at === undefined) {
      setBad(true);
      onBad();
      return;
    }
    setBad(false);
    onCommit(at);
  };
  return (
    <Input
      value={text}
      inputMode="decimal"
      spellCheck={false}
      aria-label={label}
      aria-invalid={invalid || bad ? true : undefined}
      onChange={(e) => setText(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => {
        if (e.key === 'Enter') {
          e.preventDefault();
          commit();
        } else if (e.key === 'Escape') {
          setText(shown);
          setBad(false);
        }
      }}
      className="h-[30px] font-mono text-[12.5px] tabular-nums"
    />
  );
}

/** How long "Play 5 s" plays. */
const PREVIEW_MS = 5000;

/**
 * How long a transcoded file's playhead rests before the stream is asked for
 * again from there: a drag of the slider asks once, not at every step.
 */
const RESTREAM_MS = 250;

/**
 * How far before a file's end a transcode is ever asked for (seconds): one asked
 * for at the very end leaves ffmpeg no audio to send, which the <audio> reads as
 * an error (the player's TRANSCODE_TAIL_S).
 */
const TRANSCODE_TAIL_S = 1;

/**
 * The editor's player: the chosen row's file, streamed with a book media token
 * (never the session's), at the row's start. Nudges by a second or a tenth, plays
 * five seconds from the start, and sets the row's start (or adds a chapter) at
 * the playhead. Times shown are whole-book. A file no browser plays as stored
 * (direct_playable false: Opus or FLAC in .mka, ALAC, AC-3) is transcoded when
 * the server can (server#410): the stream starts where the playhead is, so a move
 * of it asks again from there (a transcode can't be seeked), the audio's own
 * clock counting from that start.
 */
function Scrubber({
  libraryId,
  bookPath,
  spans,
  row,
  n,
  withHours,
  onSetHere,
  onAddHere,
}: {
  libraryId: number;
  bookPath: string;
  spans: FileSpan[];
  row?: DraftRow;
  n: number;
  withHours: boolean;
  onSetHere: (at: number) => void;
  onAddHere: (at: number) => void;
}) {
  const { t } = useTranslation();
  const token = useBookMediaToken(libraryId, bookPath);
  const canTranscode = useServerInfo().data?.capabilities.transcode ?? false;
  const audio = useRef<HTMLAudioElement>(null);
  const preview = useRef<ReturnType<typeof setTimeout>>(undefined);
  // Whether "Play 5 s" waits for a transcode to start, to time the five seconds.
  const previewing = useRef(false);
  const restream = useRef<ReturnType<typeof setTimeout>>(undefined);
  // The seek (seconds into the file) waiting for a newly chosen file to load,
  // or for a transcode to be asked for again.
  const pending = useRef<number | null>(null);
  const [pos, setPos] = useState(0);
  const [playing, setPlaying] = useState(false);
  const [failed, setFailed] = useState(false);
  // Whether a failed load has already asked for a new token: the one held may
  // have run out (four hours) or been dropped by the cap on editor tokens (a
  // sixth book's editor), so the first failure mints again before giving up.
  const reminted = useRef(false);
  const target = row ? toFileTime(spans, row.start) : undefined;
  const span = target?.span;
  const offset = target?.offset ?? 0;
  const length = span ? span.end - span.start : 0;
  const transcoded = !!span && !span.playable && canTranscode;
  // A transcode's start: the row's, until the playhead moves (for this row:
  // `at`, or `kept`, the start just set at the playhead, where it already
  // plays); `n` makes each move a new <audio>, even to the same start. Never in
  // the file's last second (TRANSCODE_TAIL_S).
  const at = span ? `${span.path}@${offset}` : '';
  const [stream, setStream] = useState<{
    at: string;
    kept?: string;
    from: number;
    n: number;
    play: boolean;
  }>();
  const moved = transcoded && (stream?.at === at || stream?.kept === at) ? stream : undefined;
  const from = transcoded
    ? Math.min(moved?.from ?? offset, roundMs(Math.max(0, length - TRANSCODE_TAIL_S)))
    : 0;
  const src =
    token.data && span
      ? streamUrl(libraryId, span.path, token.data, transcoded ? from : undefined)
      : undefined;

  // A replaced <audio> (another file, a transcode asked again) or a closed
  // editor lets go of its stream: a detached element goes on loading until it
  // is collected, and a transcode's ffmpeg holds one of the server's few
  // transcode slots until its connection closes.
  const attach = useCallback((el: HTMLAudioElement | null) => {
    audio.current = el;
    if (!el) return;
    return () => {
      if (audio.current === el) audio.current = null;
      el.pause();
      el.removeAttribute('src');
      el.load();
    };
  }, []);

  // A chosen row (or a moved one) puts the playhead at its start. A transcode
  // starts there: its stream is asked for from there (src).
  useEffect(() => {
    const a = audio.current;
    if (!a || !src) return;
    if (transcoded) {
      // Another row's seek waits for nothing now.
      clearTimeout(restream.current);
      pending.current = null;
      return;
    }
    if (a.readyState >= 1 && a.currentSrc.endsWith(src)) a.currentTime = offset;
    else pending.current = offset;
  }, [src, offset, transcoded]);

  useEffect(
    () => () => {
      clearTimeout(preview.current);
      clearTimeout(restream.current);
    },
    [],
  );

  // Where the audio is, in seconds into the file.
  const now = () => from + (audio.current?.currentTime ?? 0);
  const seek = (s: number) => {
    const a = audio.current;
    if (!a) return;
    const to = roundMs(Math.min(Math.max(s, 0), length));
    if (!transcoded) {
      a.currentTime = to;
      return;
    }
    pending.current = to;
    setPos(to);
    clearTimeout(restream.current);
    restream.current = setTimeout(() => {
      // Asked for: the seek waits no more (its stream may be the same URL).
      pending.current = null;
      setStream({ at, from: to, n: (stream?.n ?? 0) + 1, play: !a.paused || previewing.current });
    }, RESTREAM_MS);
  };
  const nudge = (d: number) => seek((pending.current ?? now()) + d);
  const toggle = () => {
    const a = audio.current;
    if (!a) return;
    clearTimeout(preview.current);
    previewing.current = false;
    if (a.paused) void a.play().catch(() => {});
    else a.pause();
  };
  const playFive = () => {
    const a = audio.current;
    if (!a) return;
    clearTimeout(preview.current);
    previewing.current = transcoded;
    if (transcoded) {
      seek(offset);
      return;
    }
    a.currentTime = offset;
    void a.play().catch(() => {});
    preview.current = setTimeout(() => a.pause(), PREVIEW_MS);
  };
  // The playhead: the seek a file still loading waits for, else where the audio is.
  // Never past the file's end: audio that plays on a little longer than the length
  // the book has for it is still this file, not the start of the next.
  const here = () => {
    if (!span) return 0;
    const head = pending.current ?? (audio.current ? now() : pos);
    return toBookTime(span, Math.min(head, length));
  };
  // A start set (or a chapter added) at the playhead is where a transcode
  // already plays: it plays on from there (`kept`), not asked for again from
  // the same moment. Not while a seek waits to be asked for: the stream is from
  // before it.
  const atHead = (then: (at: number) => void) => {
    const t = here();
    const to = toFileTime(spans, t);
    if (transcoded && span && to?.span === span && pending.current === null) {
      const kept = `${span.path}@${to.offset}`;
      setStream({ at, kept, from, n: moved?.n ?? 0, play: moved?.play ?? false });
    }
    then(t);
  };
  const unavailable = failed || token.isError;
  // The controls wait for a file to play and give up with it.
  const off = !src || unavailable;

  return (
    <section
      aria-label={span ? t('book.chapters.edit.player', { file: span.name }) : undefined}
      className="flex flex-col gap-2.5 rounded-[12px] border bg-muted/40 p-3.5"
    >
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 text-[12.5px]">
        <span className="min-w-0 truncate font-mono text-muted-foreground" title={span?.name}>
          {span?.name}
        </span>
        <span className="text-muted-foreground tabular-nums">
          {t('book.chapters.edit.playhead')}{' '}
          <b className="font-semibold text-foreground">
            {span ? formatStamp(toBookTime(span, pos), withHours) : '-'}
          </b>
        </span>
      </div>
      {src ? (
        <audio
          key={transcoded ? `${span?.path}#${moved?.n ?? 0}` : undefined}
          ref={attach}
          src={src}
          preload="metadata"
          autoPlay={moved?.play}
          // A new <audio> (another file, or a transcode asked again) plays only when told to.
          onLoadStart={() => setPlaying(false)}
          onLoadedMetadata={(e) => {
            setFailed(false);
            reminted.current = false;
            // A transcode starts where the seek (or the row) asked; a seek made
            // since it was asked for waits for its own.
            if (transcoded) {
              if (pending.current === null) setPos(from);
              return;
            }
            if (pending.current !== null) e.currentTarget.currentTime = pending.current;
            pending.current = null;
          }}
          // While a transcode waits to be asked for again, the audio playing on
          // is not the playhead.
          onTimeUpdate={(e) => {
            if (!transcoded || pending.current === null) setPos(from + e.currentTarget.currentTime);
          }}
          onSeeked={(e) => setPos(from + e.currentTarget.currentTime)}
          onPlay={() => setPlaying(true)}
          onPlaying={() => {
            if (!previewing.current) return;
            previewing.current = false;
            clearTimeout(preview.current);
            preview.current = setTimeout(() => audio.current?.pause(), PREVIEW_MS);
          }}
          onPause={() => setPlaying(false)}
          onError={() => {
            if (reminted.current) {
              setFailed(true);
              return;
            }
            reminted.current = true;
            void token.refetch();
          }}
        />
      ) : null}
      <input
        type="range"
        min={0}
        max={length || 1}
        step={0.1}
        value={Math.min(pos, length)}
        disabled={off}
        aria-label={t('book.chapters.edit.position', { file: span?.name ?? '' })}
        aria-valuetext={span ? formatStamp(toBookTime(span, pos), withHours) : undefined}
        onChange={(e) => seek(Number(e.target.value))}
        className="w-full accent-[var(--brand)]"
      />
      <div className="flex flex-wrap items-center gap-1.5">
        <Button
          variant="outline"
          size="icon-sm"
          disabled={off}
          aria-label={playing ? t('book.chapters.edit.pause') : t('book.chapters.edit.play')}
          onClick={toggle}
        >
          {playing ? <Pause aria-hidden="true" /> : <Play aria-hidden="true" />}
        </Button>
        {(
          [
            [-1, '−1 s', 'back1'],
            [-0.1, '−0.1 s', 'back01'],
            [0.1, '+0.1 s', 'forward01'],
            [1, '+1 s', 'forward1'],
          ] as const
        ).map(([d, label, key]) => (
          <Button
            key={key}
            variant="outline"
            size="sm"
            disabled={off}
            aria-label={t(`book.chapters.edit.${key}`)}
            onClick={() => nudge(d)}
            className="tabular-nums"
          >
            {label}
          </Button>
        ))}
        <Button variant="outline" size="sm" disabled={off} onClick={playFive}>
          {t('book.chapters.edit.playFive')}
        </Button>
        <span className="flex-1" />
        <Button
          variant="outline"
          size="sm"
          disabled={off || !row || row.fixed}
          aria-label={t('book.chapters.edit.setHereLabel', { n })}
          onClick={() => atHead(onSetHere)}
        >
          {t('book.chapters.edit.setHere')}
        </Button>
        <Button variant="outline" size="sm" disabled={off} onClick={() => atHead(onAddHere)}>
          <Plus aria-hidden="true" />
          {t('book.chapters.edit.addHere')}
        </Button>
      </div>
      {unavailable ? (
        <p role="alert" className="text-[12.5px] text-muted-foreground">
          {t('book.chapters.edit.unavailable')}
        </p>
      ) : null}
    </section>
  );
}
