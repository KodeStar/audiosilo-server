import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { useSortable } from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import {
  ArrowDown,
  ArrowUp,
  CalendarClock,
  Clock,
  Download,
  Ellipsis,
  FileWarning,
  FolderTree,
  GripVertical,
  Lock,
  Pencil,
  RefreshCw,
  Split,
  Trash2,
  TriangleAlert,
  Unplug,
} from 'lucide-react';
import { downloadLibraryExport } from '@/api/client';
import { useRecentBooks, useSetup } from '@/api/hooks';
import type { AdminLibrary, ScanProgress } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Notice } from '@/components/notice';
import { ProgressBar } from '@/components/progress-bar';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { toastError } from '@/lib/errors';
import { formatNumber, formatRelative, progressFraction } from '@/lib/format';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { DeleteLibraryDialog } from './delete-library-dialog';
import { configLibrary } from './library-access';
import { DetectionDialog } from './detection-dialog';
import { LibraryFormDialog } from './library-form-dialog';
import { OfflineNotice } from './offline-notice';
import { rescanLibrary } from './rescan';
import { scheduleLabel } from './scan-settings';

type OpenDialog = 'edit' | 'detect' | 'delete' | 'exportPaths' | null;

/** One library: covers, name, status, folder, and everything an admin does to it. */
export function LibraryCard({
  library: l,
  onMoveUp,
  onMoveDown,
}: {
  library: AdminLibrary;
  onMoveUp?: () => void;
  onMoveDown?: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const [dialog, setDialog] = useState<OpenDialog>(null);
  const [exporting, setExporting] = useState(false);
  const sortable = useSortable({ id: l.id });

  const running = l.scan.running;
  // Waiting in the job queue behind another library's scan (a running one wins).
  const queued = !running && !!l.scan.queued;
  // A podcast library has no book tooling (detection, export).
  const isPodcasts = l.kind === 'podcasts';
  const fromConfig = configLibrary(useSetup().data, l.name);

  // Personal, each copy is its own row with its path, tags and description: a
  // file for the admin's own scripts (the menu warns before it is made), never
  // the default.
  const exportBooks = async (personal = false) => {
    setExporting(true);
    try {
      const file = await downloadLibraryExport(l.id, { personal });
      toast.add({
        title: t('libraries.toast.exported', { name: l.name }),
        description: file,
        type: 'success',
      });
    } catch (err) {
      toastError(t('libraries.toast.exportFailed'), err);
    } finally {
      setExporting(false);
    }
  };

  return (
    <li
      ref={sortable.setNodeRef}
      style={{
        transform: CSS.Transform.toString(sortable.transform),
        transition: sortable.transition,
      }}
      className={cn(
        'relative rounded-xl border bg-card',
        !l.available && 'border-[color-mix(in_oklab,var(--destructive)_35%,var(--border))]',
        sortable.isDragging && 'z-10 shadow-overlay',
      )}
      aria-labelledby={`library-${l.id}-name`}
    >
      <div className="flex flex-wrap items-center gap-x-[18px] gap-y-3 p-4 md:p-5">
        <div className="flex flex-col items-center gap-0.5">
          <MoveButton
            label={t('libraries.moveUp', { name: l.name })}
            onClick={onMoveUp}
            icon={ArrowUp}
          />
          <button
            type="button"
            className="grid size-7 cursor-grab touch-none place-items-center rounded-sm text-subtle-foreground hover:bg-accent hover:text-foreground active:cursor-grabbing"
            aria-label={t('libraries.drag', { name: l.name })}
            {...sortable.attributes}
            {...sortable.listeners}
          >
            <GripVertical className="size-4" aria-hidden="true" />
          </button>
          <MoveButton
            label={t('libraries.moveDown', { name: l.name })}
            onClick={onMoveDown}
            icon={ArrowDown}
          />
        </div>

        <FannedCovers library={l} />

        <div className="flex min-w-[200px] flex-1 flex-col gap-1">
          <div className="flex flex-wrap items-center gap-2.5">
            <h2 id={`library-${l.id}-name`} className="h2 [overflow-wrap:anywhere]">
              {l.name}
            </h2>
            {isPodcasts ? <Badge variant="info">{t('libraries.kind.podcasts')}</Badge> : null}
            {fromConfig ? (
              <Badge variant="outline" title={t('libraries.form.configLocked')}>
                <Lock aria-hidden="true" />
                {t('setup.folder.fromConfig')}
              </Badge>
            ) : null}
            {/* A running scan wins: a Retry's scan is the newer news than the
                availability probe, which the server caches for a few seconds. */}
            {running ? (
              <Badge variant="brand">
                <RefreshCw className="animate-spin" aria-hidden="true" />
                {t('libraries.status.scanning')}
              </Badge>
            ) : queued ? (
              <Badge variant="info">
                <Clock aria-hidden="true" />
                {t('libraries.status.queued')}
              </Badge>
            ) : !l.available ? (
              <Badge variant="destructive">
                <Unplug aria-hidden="true" />
                {t('libraries.status.unavailable')}
              </Badge>
            ) : (
              <span className="inline-flex items-center gap-1.5 text-[12.5px] text-muted-foreground">
                <span className="dot" aria-hidden="true" />
                {t('libraries.status.online')}
              </span>
            )}
          </div>
          <span className="font-mono text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">
            {l.root}
          </span>
          <span className="text-[13px] text-muted-foreground tabular-nums">
            {t(isPodcasts ? 'libraries.episodeCount' : 'libraries.bookCount', {
              count: l.book_count,
              formatted: formatNumber(l.book_count, lang),
            })}
            {l.next_scan_at ? (
              <span className="ml-3 inline-flex items-center gap-1">
                <CalendarClock className="size-3.5" aria-hidden="true" />
                {t('libraries.nextScan', {
                  schedule: scheduleLabel(l.scan_schedule, t),
                  when: formatRelative(l.next_scan_at, lang),
                })}
              </span>
            ) : null}
            {!isPodcasts && l.metadata_source === 'path' ? (
              <span className="ml-3 inline-flex items-center gap-1">
                <FolderTree className="size-3.5" aria-hidden="true" />
                {t('libraries.sourcePath')}
              </span>
            ) : null}
          </span>
        </div>

        <div className="flex flex-wrap items-center gap-1.5">
          <Button
            variant="outline"
            size="sm"
            onClick={() => rescanLibrary(qc, l)}
            disabled={running || queued}
          >
            <RefreshCw className={cn(running && 'animate-spin')} aria-hidden="true" />
            {running
              ? t('libraries.scanning')
              : queued
                ? t('libraries.queued')
                : l.available
                  ? t('libraries.rescan')
                  : t('libraries.retry')}
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger
              className={buttonVariants({ variant: 'ghost', size: 'icon-sm' })}
              aria-label={t('libraries.actions', { name: l.name })}
            >
              <Ellipsis className="size-4" aria-hidden="true" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onClick={() => setDialog('edit')}>
                <Pencil aria-hidden="true" />
                {t('libraries.menu.edit')}
              </DropdownMenuItem>
              <DropdownMenuItem disabled={isPodcasts} onClick={() => setDialog('detect')}>
                <Split aria-hidden="true" />
                {t('libraries.menu.detection')}
              </DropdownMenuItem>
              <DropdownMenuItem
                onClick={() => void exportBooks()}
                disabled={exporting || isPodcasts}
              >
                <Download aria-hidden="true" />
                {exporting ? t('libraries.menu.exporting') : t('libraries.menu.export')}
              </DropdownMenuItem>
              <DropdownMenuItem
                onClick={() => setDialog('exportPaths')}
                disabled={exporting || isPodcasts}
              >
                <FileWarning aria-hidden="true" />
                {t('libraries.menu.exportPaths')}
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onClick={() => setDialog('delete')}>
                <Trash2 aria-hidden="true" />
                {t('libraries.menu.delete')}
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      {running ? (
        <ScanProgressBar
          progress={l.scan}
          podcasts={isPodcasts}
          lang={lang}
          className="px-4 pb-4 md:px-5 md:pb-5"
        />
      ) : null}

      {!l.available && !running && !queued ? (
        <OfflineNotice library={l} className="mx-4 mb-4 md:mx-5 md:mb-5" />
      ) : null}

      <LibraryFormDialog
        library={l}
        open={dialog === 'edit'}
        onOpenChange={(o) => setDialog(o ? 'edit' : null)}
      />
      <DetectionDialog
        library={l}
        open={dialog === 'detect'}
        onOpenChange={(o) => setDialog(o ? 'detect' : null)}
      />
      <DeleteLibraryDialog
        library={l}
        open={dialog === 'delete'}
        onOpenChange={(o) => setDialog(o ? 'delete' : null)}
      />
      <ConfirmDialog
        open={dialog === 'exportPaths'}
        onOpenChange={(o) => setDialog(o ? 'exportPaths' : null)}
        icon={FileWarning}
        tone="info"
        destructive={false}
        title={t('libraries.exportPaths.title', { name: l.name })}
        description={t('libraries.exportPaths.description')}
        confirmLabel={t('libraries.exportPaths.confirm')}
        onConfirm={() => exportBooks(true)}
      >
        <Notice tone="warn" icon={TriangleAlert} title={t('libraries.exportPaths.warnTitle')}>
          {t('libraries.exportPaths.warnBody')}
        </Notice>
      </ConfirmDialog>
    </li>
  );
}

function MoveButton({
  label,
  onClick,
  icon: Icon,
}: {
  label: string;
  onClick?: () => void;
  icon: typeof ArrowUp;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={!onClick}
      aria-label={label}
      className="hidden size-7 place-items-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-30 md:grid"
    >
      <Icon className="size-3.5" aria-hidden="true" />
    </button>
  );
}

/** Up to four of the library's newest covers, fanned like books on a table. */
function FannedCovers({ library: l }: { library: AdminLibrary }) {
  const books = useRecentBooks(l.id, 4);
  const list = books.data ?? [];
  if (books.isPending || list.length === 0) {
    return (
      <span
        className="grid size-[62px] shrink-0 place-items-center rounded-[12px] bg-muted text-muted-foreground"
        aria-hidden="true"
      >
        <FolderTree className="size-6" />
      </span>
    );
  }
  return (
    <div
      className={cn('flex shrink-0 items-center', !l.available && 'opacity-50 grayscale')}
      aria-hidden="true"
    >
      {list.map((b, k) => (
        <div
          key={b.rel_path}
          className="w-[62px] first:ml-0"
          style={{
            marginLeft: k ? -26 : 0,
            transform: `rotate(${(k - (list.length - 1) / 2) * 4}deg)`,
          }}
        >
          <BookCover libraryId={l.id} path={b.rel_path} title={b.title} />
        </div>
      ))}
    </div>
  );
}

/** A scan's progress bar and "N of M books checked" (the library card, Health > Jobs). */
export function ScanProgressBar({
  progress: p,
  podcasts = false,
  lang,
  className,
}: {
  progress: ScanProgress;
  podcasts?: boolean;
  lang: string;
  className?: string;
}) {
  const { t } = useTranslation();
  return (
    <div className={className}>
      <ProgressBar
        fraction={p.total ? progressFraction(p.done, p.total) : undefined}
        label={t('libraries.scanProgress')}
      />
      <div className="mt-1.5 text-[12px] text-muted-foreground tabular-nums">
        {p.total
          ? t(podcasts ? 'libraries.scanEpisodeCount' : 'libraries.scanCount', {
              done: formatNumber(p.done, lang),
              total: formatNumber(p.total, lang),
            })
          : t(podcasts ? 'libraries.scanPodcastDiscovering' : 'libraries.scanDiscovering')}
      </div>
    </div>
  );
}
