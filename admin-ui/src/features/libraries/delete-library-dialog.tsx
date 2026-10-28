import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Trash2 } from 'lucide-react';
import { ApiError, api } from '@/api/client';
import { invalidateLibraries, keys } from '@/api/hooks';
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
        try {
          await api.deleteLibrary(l.id);
        } catch (err) {
          // The config declares it, which the console's copy of the setup (read
          // once) didn't know yet. Retrying can't help, so close the dialog, say
          // why in the delete's own words (the code's usual copy is about a
          // rename), and read the setup again so the card locks its Delete.
          if (err instanceof ApiError && err.code === 'config_library') {
            void qc.invalidateQueries({ queryKey: keys.setup });
            toast.add({ title: t('libraries.menu.deleteConfigLocked'), type: 'error' });
            return;
          }
          // Gone already (deleted in another tab, or by another admin): what was
          // asked for is done, so it reads as deleted and the list reads again.
          if (!(err instanceof ApiError && err.status === 404)) throw err;
        }
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
