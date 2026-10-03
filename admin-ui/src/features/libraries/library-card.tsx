import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { useSortable } from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import {
  ArrowDown,
  ArrowUp,
  Download,
  Ellipsis,
  FolderTree,
  GripVertical,
  Pencil,
  RefreshCw,
  Split,
  Trash2,
  Unplug,
} from 'lucide-react';
import { downloadLibraryExport } from '@/api/client';
import { useRecentBooks } from '@/api/hooks';
import type { AdminLibrary, ScanProgress } from '@/api/types';
import { BookCover } from '@/components/book-cover';
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
import { formatNumber, progressFraction } from '@/lib/format';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { DeleteLibraryDialog } from './delete-library-dialog';
import { DetectionDialog } from './detection-dialog';
import { LibraryFormDialog } from './library-form-dialog';
import { OfflineNotice } from './offline-notice';
import { rescanLibrary } from './rescan';

type OpenDialog = 'edit' | 'detect' | 'delete' | null;

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

  const exportBooks = async () => {
    setExporting(true);
    try {
      const file = await downloadLibraryExport(l.id);
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
            {!l.available ? (
              <Badge variant="destructive">
                <Unplug aria-hidden="true" />
                {t('libraries.status.unavailable')}
              </Badge>
            ) : running ? (
              <Badge variant="brand">
                <RefreshCw className="animate-spin" aria-hidden="true" />
                {t('libraries.status.scanning')}
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
            {t('libraries.bookCount', {
              count: l.book_count,
              formatted: formatNumber(l.book_count, lang),
            })}
          </span>
        </div>

        <div className="flex flex-wrap items-center gap-1.5">
          <Button
            variant="outline"
            size="sm"
            onClick={() => rescanLibrary(qc, l)}
            disabled={running}
          >
            <RefreshCw className={cn(running && 'animate-spin')} aria-hidden="true" />
            {running
              ? t('libraries.scanning')
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
              <DropdownMenuItem onClick={() => setDialog('detect')}>
                <Split aria-hidden="true" />
                {t('libraries.menu.detection')}
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => void exportBooks()} disabled={exporting}>
                <Download aria-hidden="true" />
                {exporting ? t('libraries.menu.exporting') : t('libraries.menu.export')}
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

      {running ? <ScanProgressBar progress={l.scan} lang={lang} /> : null}

      {!l.available && !running ? (
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

function ScanProgressBar({ progress: p, lang }: { progress: ScanProgress; lang: string }) {
  const { t } = useTranslation();
  return (
    <div className="px-4 pb-4 md:px-5 md:pb-5">
      <ProgressBar
        fraction={p.total ? progressFraction(p.done, p.total) : undefined}
        label={t('libraries.scanProgress')}
      />
      <div className="mt-1.5 text-[12px] text-muted-foreground tabular-nums">
        {p.total
          ? t('libraries.scanCount', {
              done: formatNumber(p.done, lang),
              total: formatNumber(p.total, lang),
            })
          : t('libraries.scanDiscovering')}
      </div>
    </div>
  );
}
