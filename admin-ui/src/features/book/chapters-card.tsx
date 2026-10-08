import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ChevronDown, ChevronUp, Info, Undo2 } from 'lucide-react';
import { api } from '@/api/client';
import { settleBookEdit } from '@/api/hooks';
import type { AdminBookDetail, AdminChapter, BookEditRequest } from '@/api/types';
import { InlineEdit } from '@/components/inline-edit';
import { ProvenanceMarker } from '@/components/provenance';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { toastError } from '@/lib/errors';
import { formatClock, formatDuration } from '@/lib/format';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { chapterProblem, fileStrip, ribbonSegments } from './book-model';
import { CommunityChapters } from './community-chapters';

/** Rows shown before "Show all". */
const FIRST_ROWS = 10;

/**
 * The Chapters card (STYLEGUIDE.md "Chapter ribbon"): a proportional timeline
 * coloured per file, a file strip for multi-file books, and the list, where a
 * title is renamed in place and saved at once as an override.
 */
export function ChaptersCard({ detail }: { detail: AdminBookDetail }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const b = detail.book;
  const chapters = detail.chapters ?? [];
  const files = detail.files ?? [];
  const [active, setActive] = useState<number | null>(null);
  const [all, setAll] = useState(false);
  const [editing, setEditing] = useState<number | null>(null);
  // The chapter a ribbon click asked for: its row scrolls into view.
  const [jump, setJump] = useState<number | null>(null);
  const problem = chapterProblem(chapters, b.duration);
  const segments = ribbonSegments(chapters, files);
  const strip = files.length > 1 ? fileStrip(files, b.path) : [];
  const shown = all ? chapters : chapters.slice(0, FIRST_ROWS);
  const hovered = chapters.find((c) => c.index === active);
  const withHours = b.duration >= 3600;
  // A rename's "before" is the community's title when the chapters are theirs.
  const fromCommunity = detail.chapter_source === 'community';

  const edit = async (req: BookEditRequest, done: () => void) => {
    try {
      settleBookEdit(qc, await api.editBook(b.library_id, b.path, req));
      done();
    } catch (err) {
      toastError(t('book.chapters.failed'), err);
    }
  };

  const rename = (c: AdminChapter, raw: string) => {
    setEditing(null);
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
          : t(fromCommunity ? 'book.chapters.renamedCommunity' : 'book.chapters.renamedBody', {
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

  return (
    <Card aria-labelledby="chapters-title">
      <CardHeader
        titleId="chapters-title"
        title={t('book.chapters.title')}
        description={t('book.chapters.summary', {
          chapters: t('book.chapters.count', { count: chapters.length }),
          files: t('book.chapters.files', { count: files.length }),
          length: formatDuration(b.duration, lang),
        })}
      />
      <div className="flex flex-col gap-3.5 p-5">
        <CommunityChapters detail={detail} />
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
                editing={editing === c.index}
                onActive={() => setActive(c.index)}
                onEdit={() => setEditing(c.index)}
                onCancel={() => setEditing(null)}
                onRename={(title) => rename(c, title)}
                onRevert={() => revert(c)}
                fromCommunity={fromCommunity}
              />
            ))}
          </ol>
        ) : null}
        {chapters.length > FIRST_ROWS ? (
          <div>
            <Button variant="ghost" size="sm" onClick={() => setAll(!all)}>
              {all ? t('book.chapters.fewer') : t('book.chapters.all', { count: chapters.length })}
              {all ? <ChevronUp aria-hidden="true" /> : <ChevronDown aria-hidden="true" />}
            </Button>
          </div>
        ) : null}
      </div>
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
  onActive,
  onEdit,
  onCancel,
  onRename,
  onRevert,
  fromCommunity,
}: {
  chapter: AdminChapter;
  n: number;
  withHours: boolean;
  active: boolean;
  jump: boolean;
  editing: boolean;
  onActive: () => void;
  onEdit: () => void;
  onCancel: () => void;
  onRename: (title: string) => void;
  onRevert: () => void;
  fromCommunity: boolean;
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
          <button
            type="button"
            onClick={onEdit}
            aria-label={t('book.chapters.rename', { title: c.title })}
            className="-mx-1.5 -my-0.5 min-w-0 cursor-text truncate rounded-[6px] px-1.5 py-0.5 text-left font-medium hover:bg-card hover:shadow-[inset_0_0_0_1px_var(--border-strong)]"
          >
            {c.title}
          </button>
          {c.edited ? (
            <>
              <ProvenanceMarker source="edited" short />
              <button
                type="button"
                onClick={onRevert}
                title={t(
                  fromCommunity ? 'book.chapters.revertHintCommunity' : 'book.chapters.revertHint',
                  { title: c.scanned_title },
                )}
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
