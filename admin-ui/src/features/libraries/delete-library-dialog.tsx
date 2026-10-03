import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Trash2 } from 'lucide-react';
import { api } from '@/api/client';
import { invalidateLibraries } from '@/api/hooks';
import type { AdminLibrary } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { formatNumber } from '@/lib/format';
import { toast } from '@/lib/toast';

/**
 * Deletes a library after typing its name. Says exactly what goes (the index and
 * everyone's progress in it, which cascade) and what stays (the files).
 */
export function DeleteLibraryDialog({
  library: l,
  open,
  onOpenChange,
}: {
  library: AdminLibrary;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      icon={Trash2}
      title={t('libraries.delete.title', { name: l.name })}
      description={t('libraries.delete.description', {
        count: l.book_count,
        formatted: formatNumber(l.book_count, i18n.resolvedLanguage ?? 'en'),
      })}
      confirmLabel={t('libraries.delete.confirm')}
      typeToConfirm={l.name}
      onConfirm={async () => {
        await api.deleteLibrary(l.id);
        invalidateLibraries(qc);
        toast.add({
          title: t('libraries.toast.deleted', { name: l.name }),
          description: t('libraries.toast.deletedBody'),
          type: 'success',
        });
      }}
    />
  );
}
