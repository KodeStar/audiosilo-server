import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Split } from 'lucide-react';
import { api } from '@/api/client';
import { keys, useBrowse } from '@/api/hooks';
import type { AdminLibrary, FolderMode, FsEntry } from '@/api/types';
import { FolderBrowser } from '@/components/folder-browser';
import { Dialog, DialogBody, DialogContent } from '@/components/ui/dialog';
import { NativeSelect } from '@/components/ui/native-select';
import { errorMessage, toastError } from '@/lib/errors';
import { libraryCrumbs } from '@/lib/paths';
import { toast } from '@/lib/toast';

/**
 * Folder detection overrides: AudioSilo treats a folder that directly holds audio
 * as one book; an admin corrects the folders it gets wrong. Each change is saved
 * at once and rescans the library.
 */
export function DetectionDialog({
  library,
  open,
  onOpenChange,
}: {
  library: AdminLibrary;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        size="lg"
        icon={Split}
        title={t('detect.title', { name: library.name })}
        description={t('detect.description')}
      >
        <DialogBody>{open ? <DetectionBrowser library={library} /> : null}</DialogBody>
      </DialogContent>
    </Dialog>
  );
}

function DetectionBrowser({ library }: { library: AdminLibrary }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [path, setPath] = useState('');
  const listing = useBrowse(library.id, path);
  const folders = (listing.data?.entries ?? []).filter((e) => e.is_dir);

  const setMode = async (entry: FsEntry, mode: FolderMode | null) => {
    try {
      await api.setFolderOverride(library.id, entry.path, mode);
      void qc.invalidateQueries({ queryKey: keys.browse(library.id, path) });
      void qc.invalidateQueries({ queryKey: keys.libraries }); // the rescan it started
      toast.add({
        title: t('detect.toast.saved', { name: entry.name }),
        description: t('detect.toast.savedBody', { library: library.name }),
        type: 'success',
      });
    } catch (err) {
      toastError(t('detect.toast.failed'), err);
    }
  };

  return (
    <FolderBrowser
      label={t('detect.listLabel')}
      crumbs={libraryCrumbs(library.name, path)}
      onNavigate={setPath}
      loading={listing.isPending}
      error={listing.isError ? errorMessage(listing.error, t) : undefined}
      emptyText={t('detect.empty')}
      rows={folders.map((e) => ({
        path: e.path,
        name: e.name,
        kind: e.is_book ? ('book' as const) : ('dir' as const),
        openable: true,
        detail: e.is_book
          ? e.title
            ? t('detect.isBookTitled', { title: e.title })
            : t('detect.isBook')
          : undefined,
      }))}
      action={(row) => {
        const entry = folders.find((e) => e.path === row.path)!;
        return (
          <NativeSelect
            className="w-[170px]"
            aria-label={t('detect.modeAria', { name: row.name })}
            value={entry.override ?? ''}
            onChange={(e) => void setMode(entry, (e.target.value || null) as FolderMode | null)}
          >
            <option value="">{t('detect.auto')}</option>
            <option value="book">{t('detect.oneBook')}</option>
            <option value="collection">{t('detect.separate')}</option>
          </NativeSelect>
        );
      }}
    />
  );
}
