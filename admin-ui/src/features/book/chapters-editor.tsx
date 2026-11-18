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
import { api, streamUrl, transcodeRetryAfter } from '@/api/client';
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
      // The save cancelled the book's chapter detection if it was queued or parked (server#410).
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
 * How long a transcode that has played stays paused before it lets go of its
 * stream: its ffmpeg holds one of the server's few transcode slots until the
 * connection closes. Play asks for it again from the playhead.
 */
const PAUSED_HOLD_MS = 10_000;

/**
 * How far before a file's end a transcode is ever asked for (seconds): one asked
 * for at the very end leaves ffmpeg no audio to send, which the <audio> reads as
 * an error (the player's TRANSCODE_TAIL_S).
 */
const TRANSCODE_TAIL_S = 1;

/**
 * How often a transcode the server had no slot for (503, every one busy) is
 * asked for again, each after the server's Retry-After, before the player says
 * it can't play.
 */
const BUSY_TRIES = 3;

/**
 * The editor's player: the chosen row's file, streamed with a book media token
 * (never the session's), at the row's start. Nudges by a second or a tenth, plays
 * five seconds from the start, and sets the row's start (or adds a chapter) at
 * the playhead. Times shown are whole-book. A file no browser plays as stored
 * (direct_playable false: Opus or FLAC in .mka, ALAC, AC-3) is transcoded when
 * the server can (server#410): the stream starts where the playhead is, so a move
 * of it asks again from there (a transcode can't be seeked), the audio's own
 * clock counting from that start. It is asked for only to play (preload none):
 * a row chosen or a move made while paused starts no ffmpeg, and one paused for
 * PAUSED_HOLD_MS lets go of its stream. An MP3 in .mka whose remux the server
 * has not made yet comes as a live transcode from the file's start: the
 * player then asks for that file's transcode from the playhead too, as for any
 * other, until another file is chosen. One <audio> plays every row: a browser
 * that lets audio play only after a click on it (Safari) keeps letting it play
 * as the stream is asked for again.
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
  // A paused transcode's wait before it lets go of its stream (PAUSED_HOLD_MS).
  const hold = useRef<ReturnType<typeof setTimeout>>(undefined);
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
  // A transcode's new token is on its way: no stream is asked for with the
  // old one meanwhile (remint).
  const [minting, setMinting] = useState(false);
  // A transcode the server had no slot for: how often it was asked for again,
  // the wait before the next ask (said while it lasts), and the look at the
  // answer it failed with.
  const busy = useRef(0);
  const [waiting, setWaiting] = useState(false);
  const again = useRef<ReturnType<typeof setTimeout>>(undefined);
  const probe = useRef<AbortController>(undefined);
  const stopRetry = useCallback(() => {
    clearTimeout(again.current);
    probe.current?.abort();
    setWaiting(false);
  }, []);
  // An MP3 in .mka (remuxed) the server answered with a live transcode: its
  // remux is still being made. Another file forgets it, so coming back finds
  // the copy made since.
  const [live, setLive] = useState<string>();
  const target = row ? toFileTime(spans, row.start) : undefined;
  const span = target?.span;
  if (live !== undefined && live !== span?.path) setLive(undefined);
  const offset = target?.offset ?? 0;
  const length = span ? span.end - span.start : 0;
  const transcoded = !!span && canTranscode && (!span.playable || live === span.path);
  // A transcode's start: the row's, until the playhead moves (for this row:
  // `at`, or `kept`, the start just set at the playhead, where it already
  // plays); `n` counts the asks, so one to the same start loads again. Never in
  // the file's last second (TRANSCODE_TAIL_S).
  const at = span ? `${span.path}@${offset}` : '';
  const [stream, setStream] = useState<{
    at: string;
    kept?: string;
    from: number;
    n: number;
    play: boolean;
  }>();
  // A move is its row's: another row chosen forgets it, so coming back to the
  // row puts the playhead at its start again, not where the move left it.
  if (stream && stream.at !== at && stream.kept !== at) setStream(undefined);
  const moved = transcoded && (stream?.at === at || stream?.kept === at) ? stream : undefined;
  const from = transcoded
    ? Math.min(moved?.from ?? offset, roundMs(Math.max(0, length - TRANSCODE_TAIL_S)))
    : 0;
  const src =
    token.data && span && !minting
      ? streamUrl(libraryId, span.path, token.data, transcoded ? from : undefined)
      : undefined;
  // Asks for the transcode again from `to`, playing then when `play`. Counted
  // from the stream as it is then, not as this render saw it: an ask from a
  // timer or a promise must not repeat the count of one made since (asked).
  // One <audio> plays every ask, so a busy stream's look and its wait to be
  // asked for again were the last stream's: the new one's failure looks anew.
  const ask = (to: number, play: boolean) => {
    stopRetry();
    setStream((s) => ({ at, from: to, n: (s?.n ?? 0) + 1, play }));
  };

  // A closed editor lets go of its stream: a detached element goes on loading
  // until it is collected, and a transcode's ffmpeg holds one of the server's
  // few transcode slots until its connection closes. Only once it has left the
  // page (after the commit): StrictMode runs a ref's cleanup once on mount too,
  // the element still in place, and a src taken off then is never put back.
  const attach = useCallback((el: HTMLAudioElement | null) => {
    audio.current = el;
    if (!el) return;
    return () => {
      if (audio.current === el) audio.current = null;
      queueMicrotask(() => {
        if (el.isConnected) return;
        el.pause();
        el.removeAttribute('src');
        el.load();
      });
    };
  }, []);

  // Another row (or this one moved): a seek still waiting to be asked for, a
  // "Play 5 s" still waiting to start and a busy transcode waiting to be asked
  // for again were the last row's. A transcode that failed is not loaded again
  // until it plays, so its row's controls come back here, not at a load.
  useEffect(() => {
    clearTimeout(restream.current);
    clearTimeout(hold.current);
    previewing.current = false;
    stopRetry();
    busy.current = 0;
    if (transcoded) setFailed(false);
  }, [at, transcoded, stopRetry]);

  // A chosen row (or a moved one) puts the playhead at its start. A transcode
  // starts there: its stream is asked for from there (src), and nothing loads
  // until it plays, so the playhead shown is set here. Not for a start just set
  // at the playhead (kept): the stream plays on, the playhead where it was.
  const keptHere = moved?.kept === at;
  useEffect(() => {
    const a = audio.current;
    if (!a || !src) return;
    if (transcoded) {
      // Another row's seek waits for nothing now.
      clearTimeout(restream.current);
      pending.current = null;
      if (!keptHere) setPos(from);
      return;
    }
    if (a.readyState >= 1 && a.currentSrc.endsWith(src)) a.currentTime = offset;
    else pending.current = offset;
  }, [src, offset, transcoded, from, keptHere]);

  // A transcode asked for again plays on the same <audio>: a new src loads by
  // itself, the same one is loaded again, and one asked for to play plays.
  const asked = useRef<{ src?: string; n: number }>(undefined);
  const askN = moved?.n ?? 0;
  const askPlay = moved?.play ?? false;
  useEffect(() => {
    const a = audio.current;
    const last = asked.current;
    asked.current = { src, n: askN };
    if (!a || !src || !transcoded || (last?.src === src && last.n === askN)) return;
    if (last?.src === src) a.load();
    if (askPlay && last?.n !== askN) void a.play().catch(() => {});
  }, [src, askN, askPlay, transcoded]);

  useEffect(
    () => () => {
      clearTimeout(preview.current);
      clearTimeout(restream.current);
      clearTimeout(hold.current);
      clearTimeout(again.current);
      probe.current?.abort();
    },
    [],
  );

  // Where the audio is, in seconds into the file.
  const now = () => from + (audio.current?.currentTime ?? 0);
  // Where a transcode asked for again starts: a seek waiting to be asked for,
  // else where the audio is; never past the file's end.
  const head = () => roundMs(Math.min(pending.current ?? now(), length));
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
    clearTimeout(hold.current);
    restream.current = setTimeout(() => {
      // Asked for: the seek waits no more (its stream may be the same URL).
      pending.current = null;
      ask(to, !a.paused || previewing.current);
    }, RESTREAM_MS);
  };
  const nudge = (d: number) => seek((pending.current ?? now()) + d);
  const toggle = () => {
    const a = audio.current;
    if (!a) return;
    clearTimeout(preview.current);
    previewing.current = false;
    stopRetry();
    if (!a.paused) {
      a.pause();
      return;
    }
    // One that failed (a busy transcode given up on waiting for) loads again first.
    if (a.error) a.load();
    void a.play().catch(() => {});
  };
  // A failed load mints a new token once (reminted), then gives up. A
  // transcode is asked for again from where it was once the token is here,
  // playing if it was.
  const remint = (a: HTMLAudioElement) => {
    if (reminted.current) {
      setFailed(true);
      return;
    }
    reminted.current = true;
    if (!transcoded) {
      void token.refetch();
      return;
    }
    const place = head();
    const play = !a.paused || previewing.current;
    pending.current = null;
    setMinting(true);
    void token.refetch().then((minted) => {
      setMinting(false);
      if (!minted.isError) ask(place, play);
    });
  };
  // A transcode fails for want of a slot too (503, every one busy), which is
  // no fault of the token: it is asked for again after the server's
  // Retry-After, and plays then, since it was only asked for to play.
  const onError = () => {
    const a = audio.current;
    if (!a) return;
    if (!transcoded || !src) {
      remint(a);
      return;
    }
    stopRetry();
    const look = new AbortController();
    probe.current = look;
    void transcodeRetryAfter(src, look.signal).then((wait) => {
      if (look.signal.aborted || audio.current !== a) return;
      if (wait === undefined) {
        remint(a);
        return;
      }
      if (++busy.current > BUSY_TRIES) {
        setFailed(true);
        return;
      }
      setWaiting(true);
      again.current = setTimeout(() => {
        setWaiting(false);
        if (audio.current !== a) return;
        a.load();
        void a.play().catch(() => {});
      }, wait * 1000);
    });
  };
  // A transcode paused after it has played lets go of its stream after a
  // while (PAUSED_HOLD_MS): asked for again from the playhead, not to play, it
  // loads nothing (preload none), and its connection and ffmpeg's slot are let
  // go of. Read at the wait's end, as the row and its stream are then (letGo).
  const letGo = useRef<() => void>(undefined);
  useEffect(() => {
    letGo.current = () => {
      const a = audio.current;
      if (!a || !transcoded || !a.paused || a.currentTime <= 0 || pending.current !== null) return;
      ask(head(), false);
    };
  });
  const pausedHold = () => {
    clearTimeout(hold.current);
    hold.current = setTimeout(() => letGo.current?.(), PAUSED_HOLD_MS);
  };
  // The live transcode an MP3 in .mka is answered with while its remux is
  // being made (no length: it can't be seeked): its file is transcoded from
  // here on, asked for from where the playhead waits.
  const goneLive = (a: HTMLAudioElement) => {
    if (!span?.remuxed || !canTranscode || Number.isFinite(a.duration)) return false;
    const to = pending.current ?? offset;
    pending.current = null;
    setLive(span.path);
    ask(to, !a.paused);
    return true;
  };
  const playFive = () => {
    const a = audio.current;
    if (!a) return;
    clearTimeout(preview.current);
    previewing.current = transcoded;
    // A transcode already asked for from the row's start and not played yet
    // plays at once, in this click; else it is asked for from there.
    if (transcoded && (from !== offset || pending.current !== null || a.currentTime > 0)) {
      seek(offset);
      return;
    }
    if (transcoded) {
      if (a.error) a.load();
      void a.play().catch(() => {});
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
    const inFile = pending.current ?? (audio.current ? now() : pos);
    return toBookTime(span, Math.min(inFile, length));
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
      {span && (src || minting) ? (
        <audio
          ref={attach}
          src={src}
          // A transcode is asked for only to play: each ask starts an ffmpeg
          // that holds one of the server's few transcode slots.
          preload={transcoded ? (moved?.play ? 'auto' : 'none') : 'metadata'}
          // A new source (another file, or a transcode asked again) plays only
          // when told to: play() may have run before its load began (asked), so
          // the element says whether it plays, not the event.
          onLoadStart={(e) => setPlaying(!e.currentTarget.paused)}
          onLoadedMetadata={(e) => {
            setFailed(false);
            reminted.current = false;
            busy.current = 0;
            if (!transcoded && goneLive(e.currentTarget)) return;
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
          onPlay={() => {
            clearTimeout(hold.current);
            setPlaying(true);
          }}
          onPlaying={() => {
            if (!previewing.current) return;
            previewing.current = false;
            clearTimeout(preview.current);
            preview.current = setTimeout(() => audio.current?.pause(), PREVIEW_MS);
          }}
          onPause={() => {
            setPlaying(false);
            if (transcoded) pausedHold();
          }}
          onError={onError}
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
      ) : waiting ? (
        <p role="status" className="text-[12.5px] text-muted-foreground">
          {t('book.chapters.edit.busy')}
        </p>
      ) : null}
    </section>
  );
}
