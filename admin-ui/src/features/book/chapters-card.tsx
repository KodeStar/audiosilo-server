import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { AudioLines, Info, ListRestart, Lock, Pencil, Undo2 } from 'lucide-react';
import { api } from '@/api/client';
import { invalidateIssues, keys, settleBookEdit, useLibraries, useServerInfo } from '@/api/hooks';
import type { AdminBookDetail, AdminChapter, BookEditRequest, ChapterSource } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { InlineEdit } from '@/components/inline-edit';
import { Notice } from '@/components/notice';
import { ProvenanceMarker } from '@/components/provenance';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { detectAvailable, detectJobsKnown, KIND_DETECT } from '@/features/bridge/generate-model';
import { JobActions } from '@/features/bridge/generate-parts';
import { GenerateStatus } from '@/features/bridge/generate-status';
import { useBookGenerateJob, useBridgeLink } from '@/features/bridge/use-generate';
import { DetectChaptersDialog } from '@/features/health/detect-chapters-dialog';
import { toastError } from '@/lib/errors';
import { formatClock, formatDateTime, formatDuration } from '@/lib/format';
import { isBookLibrary } from '@/lib/library-kind';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import {
  chapterProblem,
  chapterSourceAction,
  communityState,
  detectCard,
  fileStrip,
  ribbonSegments,
} from './book-model';
import { ChaptersEditor } from './chapters-editor';
import { CommunityChapters } from './community-chapters';
import { FIRST_ROWS, ShowAllToggle } from './show-all-toggle';

/** A chapter source whose titles a rename's hints name, as the keys' suffix. */
const CHAPTER_ORIGIN: Partial<Record<ChapterSource, string>> = {
  community: 'Community',
  generated: 'Generated',
};

/**
 * The Chapters card (STYLEGUIDE.md "Chapter ribbon"): a proportional timeline
 * coloured per file, a file strip for multi-file books, and the list, where a
 * title is renamed in place and saved at once as an override. Edit chapters
 * opens the editor (chapters-editor.tsx), whose saved list stands over every
 * other source (chapter_source "edited", titles renamed only there) until it is
 * reverted, and is set aside (dormant) while the files differ from the ones it
 * was made for.
 */
export function ChaptersCard({ detail }: { detail: AdminBookDetail }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const b = detail.book;
  const server = useServerInfo().data;
  // An episode's chapters are its feed's: the server refuses an edit in a
  // podcast library (409 not_book_library), so none is offered there.
  const library = useLibraries().data?.find((l) => l.id === b.library_id);
  const podcast = library !== undefined && !isBookLibrary(library);
  // The book's chapter detection: when its job ends the book is fetched again
  // (useBookGenerateJob), so the chapters it found show here, metadata or not.
  // Its state shows here while it is open or ended without chapters (a parked
  // one says why: waiting for an ASR worker, say, BRIDGE-10), with Detect
  // chapters offered as Health offers it (BRIDGE-08).
  const detect = useBookGenerateJob(b.library_id, b.path, detectJobsKnown(server), KIND_DETECT).job;
  const detection = detectCard(
    detail,
    detect,
    detectAvailable(server) && library !== undefined && !podcast,
  );
  const bridgeLink = useBridgeLink(detection?.job?.state === 'needs_attention');
  const [detecting, setDetecting] = useState(false);
  const chapters = detail.chapters ?? [];
  const files = detail.files ?? [];
  const [active, setActive] = useState<number | null>(null);
  const [all, setAll] = useState(false);
  const [renaming, setRenaming] = useState<number | null>(null);
  const [editing, setEditing] = useState(false);
  const [reverting, setReverting] = useState(false);
  const edited = detail.chapter_source === 'edited';
  const chapterEdit = detail.chapter_edit;
  // The chapter a ribbon click asked for: its row scrolls into view.
  const [jump, setJump] = useState<number | null>(null);
  const problem = chapterProblem(chapters, b.duration);
  const segments = ribbonSegments(chapters, files);
  const strip = files.length > 1 ? fileStrip(files, b.path) : [];
  const shown = all ? chapters : chapters.slice(0, FIRST_ROWS);
  const hovered = chapters.find((c) => c.index === active);
  const withHours = b.duration >= 3600;
  // A rename's "before" is the community's title, or the narration's, when the
  // chapters are theirs (the i18n keys' suffix).
  const origin = CHAPTER_ORIGIN[detail.chapter_source] ?? '';
  // Rejecting the found chapters, or undoing a choice that keeps them (or a
  // detection) out; the community's part has its own.
  const sourceAction = chapterSourceAction(
    detail,
    communityState(detail, (sec) => formatDuration(sec, lang)),
  );
  const [switching, setSwitching] = useState(false);
  const switchSource = async (source: 'files' | 'auto') => {
    setSwitching(true);
    try {
      settleBookEdit(qc, await api.editBook(b.library_id, b.path, { chapter_source: source }));
      invalidateIssues(qc);
      // A choice cancels the book's chapter detection if it was queued or parked (server#410).
      void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
      toast.add({ title: t('book.community.switched'), type: 'success' });
    } catch (err) {
      toastError(t('book.community.editFailed'), err);
    } finally {
      setSwitching(false);
    }
  };

  const edit = async (req: BookEditRequest, done: () => void) => {
    try {
      settleBookEdit(qc, await api.editBook(b.library_id, b.path, req));
      done();
    } catch (err) {
      toastError(t('book.chapters.failed'), err);
    }
  };

  const rename = (c: AdminChapter, raw: string) => {
    setRenaming(null);
    const title = raw.trim();
    if (title === c.title) return;
    // An emptied title can't be saved: it means "back to the file's title".
    if (!title) {
      if (c.edited) revert(c);
      return;
    }
    void edit({ chapters: { set: { [c.index]: title } } }, () =>
      toast.add({
        title: t('book.chapters.renamed'),
        description: !c.scanned_title
          ? t('book.chapters.renamedNoTag')
          : t(origin ? `book.chapters.renamed${origin}` : 'book.chapters.renamedBody', {
              title: c.scanned_title,
            }),
        type: 'success',
      }),
    );
  };

  const revert = (c: AdminChapter) =>
    void edit({ chapters: { revert: [c.index] } }, () =>
      toast.add({
        title: t('book.chapters.reverted'),
        description: t('book.chapters.revertedBody', { title: c.scanned_title }),
        type: 'success',
      }),
    );

  const revertEdit = async () => {
    settleBookEdit(qc, await api.revertChapters(b.library_id, b.path));
    invalidateIssues(qc);
    toast.add({ title: t('book.chapters.edit.reverted'), type: 'success' });
  };

  const header = (
    <CardHeader
      titleId="chapters-title"
      title={t('book.chapters.title')}
      description={t('book.chapters.summary', {
        chapters: t('book.chapters.count', { count: chapters.length }),
        files: t('book.chapters.files', { count: files.length }),
        length: formatDuration(b.duration, lang),
      })}
      action={
        !editing && !podcast && files.length ? (
          <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
            <Pencil aria-hidden="true" />
            {t('book.chapters.edit.open')}
          </Button>
        ) : undefined
      }
    />
  );

  if (editing) {
    return (
      <Card aria-labelledby="chapters-title">
        {header}
        <div className="p-5">
          <ChaptersEditor detail={detail} onClose={() => setEditing(false)} />
        </div>
      </Card>
    );
  }

  const editedBy = chapterEdit
    ? t(chapterEdit.edited_by ? 'book.chapters.edit.editedBy' : 'book.chapters.edit.editedAt', {
        user: chapterEdit.edited_by,
        when: formatDateTime(chapterEdit.edited_at, lang, true),
      })
    : '';
  const revertButton = (
    <Button variant="outline" size="sm" onClick={() => setReverting(true)}>
      <ListRestart aria-hidden="true" />
      {t('book.chapters.edit.revert')}
    </Button>
  );

  return (
    <Card aria-labelledby="chapters-title">
      {header}
      <div className="flex flex-col gap-3.5 p-5">
        {chapterEdit?.state === 'dormant' ? (
          <Notice
            tone="warn"
            icon={Info}
            title={t('book.chapters.edit.dormantTitle')}
            actions={revertButton}
          >
            {t('book.chapters.edit.dormantBody')} {editedBy}
          </Notice>
        ) : null}
        {edited ? (
          <div className="flex flex-wrap items-start justify-between gap-x-3 gap-y-2">
            <p className="flex min-w-0 flex-1 basis-60 items-start gap-2 text-[13.5px] text-muted-foreground">
              <Lock className="mt-0.5 size-4 shrink-0 text-prov-edit" aria-hidden="true" />
              <span>
                <b className="font-semibold text-foreground">{t('book.chapters.edit.source')}</b>{' '}
                {editedBy}
              </span>
            </p>
            {revertButton}
          </div>
        ) : null}
        {detection ? (
          <div className="flex flex-col gap-1.5">
            <p className="text-[13px] font-semibold">{t('book.chapters.detectJob')}</p>
            {detection.job ? (
              <GenerateStatus job={detection.job} uiUrl={bridgeLink} />
            ) : (
              <p className="text-[12.5px] text-muted-foreground">
                {t('book.chapters.detectOffer')}
              </p>
            )}
            <JobActions job={detection.job} cancelAria={t('book.chapters.detectCancelAria')}>
              {detection.offer ? (
                <Button variant="outline" size="sm" onClick={() => setDetecting(true)}>
                  <AudioLines aria-hidden="true" />
                  {t('health.fix.detect')}
                </Button>
              ) : null}
            </JobActions>
          </div>
        ) : null}
        <DetectChaptersDialog
          books={detecting ? [b] : null}
          onOpenChange={(open) => setDetecting(open)}
        />
        <CommunityChapters detail={detail} />
        {detail.chapter_source === 'generated' || sourceAction ? (
          <div className="flex flex-wrap items-start justify-between gap-x-3 gap-y-2">
            <p className="flex min-w-0 flex-1 basis-60 items-start gap-2 text-[13.5px] text-muted-foreground">
              {detail.chapter_source === 'generated' ? (
                <>
                  <AudioLines className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
                  {t('book.chapters.generated')}
                </>
              ) : (
                <>
                  <Info className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
                  {sourceAction?.note ? t(sourceAction.note) : null}
                </>
              )}
            </p>
            {sourceAction ? (
              <Button
                variant="outline"
                size="sm"
                disabled={switching}
                onClick={() => void switchSource(sourceAction.source)}
              >
                {t(sourceAction.label)}
              </Button>
            ) : null}
          </div>
        ) : null}
        {problem ? (
          <p className="flex items-start gap-2 text-[13.5px] text-muted-foreground">
            <Info className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
            {problem === 'none'
              ? t('book.chapters.none')
              : t('book.chapters.single', { length: formatDuration(b.duration, lang) })}
          </p>
        ) : null}
        {chapters.length > 1 ? (
          <div>
            <div
              className="ribbon"
              role="img"
              aria-label={t('book.chapters.ribbon', { count: chapters.length })}
              onMouseLeave={() => setActive(null)}
            >
              {segments.map((s) => (
                <div
                  key={s.index}
                  className="ribbon-seg"
                  data-active={active === s.index ? '' : undefined}
                  style={{ flexGrow: s.flex, flexBasis: 0, '--ch': s.color } as React.CSSProperties}
                  onMouseEnter={() => setActive(s.index)}
                  onClick={() => {
                    setActive(s.index);
                    setJump(s.index);
                    if (chapters.findIndex((c) => c.index === s.index) >= FIRST_ROWS) setAll(true);
                  }}
                />
              ))}
            </div>
            {strip.length ? (
              <div className="mt-1.5 flex gap-0.5" aria-hidden="true">
                {strip.map((f, i) => (
                  <div
                    key={f.path}
                    className="flex h-[22px] min-w-0 items-center gap-1.5 overflow-hidden rounded-[6px] bg-muted px-2 font-mono text-[11px] whitespace-nowrap text-muted-foreground"
                    style={{ flexGrow: f.flex, flexBasis: 0 }}
                    title={f.name}
                  >
                    <span
                      className="size-2 shrink-0 rounded-full"
                      style={{ background: f.color }}
                    />
                    <span className="truncate">{strip.length <= 6 ? f.name : i + 1}</span>
                  </div>
                ))}
              </div>
            ) : null}
            <div className="mt-1.5 flex justify-between gap-3 text-[11px] text-subtle-foreground tabular-nums">
              <span>{formatClock(0, withHours)}</span>
              <span className="min-w-0 truncate text-center" aria-live="polite">
                {hovered ? (
                  <>
                    <b className="font-semibold text-foreground">{hovered.title}</b> ·{' '}
                    {formatClock(hovered.book_offset, withHours)}
                  </>
                ) : (
                  t('book.chapters.hover')
                )}
              </span>
              <span>{formatClock(b.duration)}</span>
            </div>
          </div>
        ) : null}
        {chapters.length ? (
          <ol className="flex flex-col">
            {shown.map((c, i) => (
              <ChapterRow
                key={c.index}
                chapter={c}
                n={i + 1}
                withHours={withHours}
                active={active === c.index}
                jump={jump === c.index}
                editing={renaming === c.index}
                renamable={!edited}
                onActive={() => setActive(c.index)}
                onEdit={() => setRenaming(c.index)}
                onCancel={() => setRenaming(null)}
                onRename={(title) => rename(c, title)}
                onRevert={() => revert(c)}
                origin={origin}
              />
            ))}
          </ol>
        ) : null}
        <ShowAllToggle
          all={all}
          total={chapters.length}
          label={t('book.chapters.all', { count: chapters.length })}
          onToggle={() => setAll(!all)}
        />
      </div>
      <ConfirmDialog
        open={reverting}
        onOpenChange={setReverting}
        icon={ListRestart}
        title={t('book.chapters.edit.revertTitle')}
        description={t('book.chapters.edit.revertBody')}
        confirmLabel={t('book.chapters.edit.revertConfirm')}
        onConfirm={revertEdit}
      />
    </Card>
  );
}

function ChapterRow({
  chapter: c,
  n,
  withHours,
  active,
  jump,
  editing,
  renamable,
  onActive,
  onEdit,
  onCancel,
  onRename,
  onRevert,
  origin,
}: {
  chapter: AdminChapter;
  n: number;
  withHours: boolean;
  active: boolean;
  jump: boolean;
  editing: boolean;
  /** False while an edited list applies: its titles change in the editor. */
  renamable: boolean;
  onActive: () => void;
  onEdit: () => void;
  onCancel: () => void;
  onRename: (title: string) => void;
  onRevert: () => void;
  /** Where the chapters' titles came from, as the hint keys' suffix ("" for the files). */
  origin: string;
}) {
  const { t, i18n } = useTranslation();
  const row = useRef<HTMLLIElement>(null);
  // A segment clicked in the ribbon brings its row into view.
  useEffect(() => {
    if (jump) row.current?.scrollIntoView({ block: 'nearest' });
  }, [jump]);
  return (
    <li
      ref={row}
      onMouseEnter={onActive}
      onFocus={onActive}
      className={cn(
        'grid grid-cols-[28px_minmax(0,1fr)_64px] items-center gap-3 rounded-[10px] px-2.5 py-[7px] text-[13.5px] hover:bg-muted md:grid-cols-[34px_minmax(0,1fr)_84px_72px]',
        active && 'bg-muted',
      )}
    >
      <span className="text-[12px] text-subtle-foreground tabular-nums">{n}</span>
      {editing ? (
        <InlineEdit
          initial={c.title}
          aria-label={t('book.chapters.titleLabel')}
          onDone={(title) => (title === undefined ? onCancel() : onRename(title))}
          className="h-[30px] px-2.5 text-[13.5px]"
        />
      ) : (
        <span className="flex min-w-0 items-center gap-1.5">
          {renamable ? (
            <button
              type="button"
              onClick={onEdit}
              aria-label={t('book.chapters.rename', { title: c.title })}
              className="-mx-1.5 -my-0.5 min-w-0 cursor-text truncate rounded-[6px] px-1.5 py-0.5 text-left font-medium hover:bg-card hover:shadow-[inset_0_0_0_1px_var(--border-strong)]"
            >
              {c.title}
            </button>
          ) : (
            <span className="min-w-0 truncate font-medium">{c.title}</span>
          )}
          {c.edited ? (
            <>
              <ProvenanceMarker source="edited" short />
              <button
                type="button"
                onClick={onRevert}
                title={t(`book.chapters.revertHint${origin}`, { title: c.scanned_title })}
                aria-label={t('book.chapters.revert', { title: c.title })}
                className="grid size-6 shrink-0 place-items-center rounded-[6px] text-muted-foreground hover:bg-accent hover:text-foreground"
              >
                <Undo2 className="size-3.5" aria-hidden="true" />
              </button>
            </>
          ) : null}
        </span>
      )}
      <span className="text-right text-[12.5px] text-muted-foreground tabular-nums">
        {formatClock(c.book_offset, withHours)}
      </span>
      <span className="hidden text-right text-[12px] text-subtle-foreground tabular-nums md:block">
        {formatDuration(c.end - c.start, i18n.resolvedLanguage ?? 'en')}
      </span>
    </li>
  );
}
