import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  ArrowRight,
  CircleCheck,
  Eye,
  LoaderCircle,
  RotateCcw,
  Trash2,
  TriangleAlert,
  Undo2,
  type LucideIcon,
} from 'lucide-react';
import { api } from '@/api/client';
import { importBusy, invalidateImported } from '@/api/hooks';
import type { Import, ImportStatus } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import { formatDate, formatHours } from '@/lib/format';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { sourceHost } from './imports-model';
import { useDiscardImport } from './use-discard-import';

/** How each status reads: the badge it wears and its icon (spinning while busy). */
const STATUS: Record<
  ImportStatus,
  { badge: React.ComponentProps<typeof Badge>['variant']; icon: LucideIcon }
> = {
  fetching: { badge: 'info', icon: LoaderCircle },
  review: { badge: 'secondary', icon: Eye },
  applying: { badge: 'info', icon: LoaderCircle },
  applied: { badge: 'success', icon: CircleCheck },
  failed: { badge: 'destructive', icon: TriangleAlert },
  undone: { badge: 'outline', icon: Undo2 },
};

/** An import's status as a badge: an icon and a word. */
export function ImportStatusBadge({ status }: { status: ImportStatus }) {
  const { t } = useTranslation();
  const { badge, icon: Icon } = STATUS[status];
  return (
    <Badge variant={badge}>
      <Icon className={cn(importBusy({ status }) && 'animate-spin')} aria-hidden="true" />
      {t(`imports.status.${status}`)}
    </Badge>
  );
}

/** A link to Settings > Import, as a small button. */
export function ImportSettingsLink({
  variant,
  children,
}: {
  variant: 'outline' | 'ghost';
  children: React.ReactNode;
}) {
  return (
    <Link
      to="/server/{-$section}"
      params={{ section: undefined }}
      search={{ topic: 'import' }}
      className={buttonVariants({ variant, size: 'sm' })}
    >
      {children}
      <ArrowRight aria-hidden="true" />
    </Link>
  );
}

/**
 * Asks before undoing an applied import: what goes, and that progress the
 * person changed since stays.
 */
export function UndoImportDialog({
  imp,
  open,
  onOpenChange,
}: {
  imp: Import;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('imports.undo.title', { user: imp.username })}
      description={t('imports.undo.body', { user: imp.username })}
      icon={RotateCcw}
      confirmLabel={t('imports.undo.confirm')}
      onConfirm={async () => {
        try {
          await api.undoImport(imp.id);
        } finally {
          invalidateImported(qc, imp);
        }
        toast.add({ title: t('imports.undo.done', { user: imp.username }), type: 'success' });
      }}
    />
  );
}

/**
 * A decided import as a row (Settings > Import's history, a person's Listening
 * tab): who it went to (unless `showUser` is off), where from, when, how much,
 * and Undo (applied), Delete (undone, failed) or a link to its review.
 */
export function ImportRow({ imp, showUser = true }: { imp: Import; showUser?: boolean }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const { discard, pending: deleting } = useDiscardImport();
  const [undoing, setUndoing] = useState(false);
  const facts = [
    imp.applied_at ? t('imports.row.applied', { date: formatDate(imp.applied_at, lang) }) : '',
    imp.summary ? t('imports.row.hours', { hours: formatHours(imp.summary.listened, lang) }) : '',
  ].filter(Boolean);
  return (
    <li className="flex flex-wrap items-center gap-x-3 gap-y-2 px-5 py-3">
      <div className="flex min-w-0 flex-1 basis-56 flex-col gap-0.5">
        <span className="flex flex-wrap items-center gap-2">
          <b className="font-semibold [overflow-wrap:anywhere]">
            {showUser
              ? t('imports.row.title', { source: imp.source_user, user: imp.username })
              : t('imports.row.titleFrom', { source: imp.source_user })}
          </b>
          <ImportStatusBadge status={imp.status} />
        </span>
        <span className="text-[12.5px] text-muted-foreground tabular-nums [overflow-wrap:anywhere]">
          {[t('imports.row.from', { host: sourceHost(imp.source_url) }), ...facts].join(' · ')}
        </span>
      </div>
      {imp.status === 'applied' ? (
        <Button
          variant="outline"
          size="sm"
          aria-label={t('imports.undo.aria', { source: imp.source_user, user: imp.username })}
          onClick={() => setUndoing(true)}
        >
          <RotateCcw aria-hidden="true" />
          {t('imports.undo.action')}
        </Button>
      ) : imp.status === 'undone' || imp.status === 'failed' ? (
        <Button
          variant="ghost"
          size="sm"
          className="text-destructive hover:bg-destructive-soft"
          aria-label={t('imports.deleteAria', { source: imp.source_user, user: imp.username })}
          disabled={deleting}
          onClick={() => void discard(imp)}
        >
          <Trash2 aria-hidden="true" />
          {t('imports.delete')}
        </Button>
      ) : (
        <ImportSettingsLink variant="outline">{t('imports.row.review')}</ImportSettingsLink>
      )}
      <UndoImportDialog imp={imp} open={undoing} onOpenChange={setUndoing} />
    </li>
  );
}
