import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { FolderPlus } from 'lucide-react';
import { api } from '@/api/client';
import { invalidatePeople, useBrowse, useLibraries } from '@/api/hooks';
import type { AdminLibrary, Share } from '@/api/types';
import { FolderBrowser } from '@/components/folder-browser';
import { Button } from '@/components/ui/button';
import { Dialog, DialogBody, DialogContent } from '@/components/ui/dialog';
import { Field } from '@/components/ui/field';
import { NativeSelect } from '@/components/ui/native-select';
import { errorMessage, toastError } from '@/lib/errors';
import { libraryCrumbs } from '@/lib/paths';
import { toast } from '@/lib/toast';

/**
 * Adds a folder (or one book, or a whole library) to a share, picked by browsing
 * the library as the player sees it.
 */
export function AddFolderDialog({
  share,
  open,
  onOpenChange,
}: {
  share: Share;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        size="lg"
        icon={FolderPlus}
        title={t('shares.addFolderTitle', { name: share.name })}
        description={t('shares.addFolderDescription')}
      >
        <DialogBody>
          {open ? <Picker share={share} onDone={() => onOpenChange(false)} /> : null}
        </DialogBody>
      </DialogContent>
    </Dialog>
  );
}

function Picker({ share, onDone }: { share: Share; onDone: () => void }) {
  const { t } = useTranslation();
  const libraries = useLibraries();
  const libs = libraries.data ?? [];
  const [libraryId, setLibraryId] = useState<number>();
  const library = libs.find((l) => l.id === libraryId) ?? libs[0];
  return (
    <div className="flex flex-col gap-4">
      {libs.length > 1 ? (
        <Field htmlFor="share-library" label={t('shares.library')}>
          <NativeSelect
            id="share-library"
            value={library?.id ?? ''}
            onChange={(e) => setLibraryId(Number(e.target.value))}
          >
            {libs.map((l) => (
              <option key={l.id} value={l.id}>
                {l.name}
              </option>
            ))}
          </NativeSelect>
        </Field>
      ) : null}
      {library ? (
        <LibraryPicker key={library.id} share={share} library={library} onDone={onDone} />
      ) : null}
    </div>
  );
}

function LibraryPicker({
  share,
  library,
  onDone,
}: {
  share: Share;
  library: AdminLibrary;
  onDone: () => void;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [path, setPath] = useState('');
  const listing = useBrowse(library.id, path);
  const has = new Set(
    (share.paths ?? []).filter((r) => r.library_id === library.id).map((r) => r.path),
  );

  const add = async (rulePath: string, label: string) => {
    try {
      await api.addSharePath(share.id, { library_id: library.id, path: rulePath });
      invalidatePeople(qc);
      toast.add({
        title: t('shares.toast.folderAdded', { what: label, share: share.name }),
        type: 'success',
      });
      onDone();
    } catch (err) {
      toastError(t('shares.toast.failed'), err);
    }
  };

  const entries = (listing.data?.entries ?? []).filter((e) => e.is_dir || e.is_audio);
  const hereLabel = path ? path : t('shares.wholeLibraryOf', { name: library.name });
  return (
    <FolderBrowser
      label={t('shares.pickerLabel')}
      crumbs={libraryCrumbs(library.name, path)}
      onNavigate={setPath}
      loading={listing.isPending}
      error={listing.isError ? errorMessage(listing.error, t) : undefined}
      emptyText={t('shares.pickerEmpty')}
      current={
        <div className="flex items-center justify-between gap-3">
          <span className="min-w-0 truncate text-[13px] text-muted-foreground">
            {path ? t('shares.thisFolder') : t('shares.wholeLibraryOf', { name: library.name })}
          </span>
          <Button
            type="button"
            size="sm"
            disabled={has.has(path)}
            onClick={() => void add(path, hereLabel)}
          >
            {has.has(path)
              ? t('shares.alreadyIn')
              : path
                ? t('shares.addThisFolder')
                : t('shares.addWholeLibrary')}
          </Button>
        </div>
      }
      rows={entries.map((e) => ({
        path: e.path,
        name: e.is_book && e.title ? e.title : e.name,
        kind: e.is_book ? ('book' as const) : e.is_dir ? ('dir' as const) : ('audio' as const),
        openable: e.is_dir,
        detail: e.is_book ? e.author || e.name : undefined,
      }))}
      action={(row) => (
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={has.has(row.path)}
          onClick={() => void add(row.path, row.path)}
          aria-label={t('shares.addAria', { name: row.name })}
        >
          {has.has(row.path) ? t('shares.alreadyIn') : t('shares.add')}
        </Button>
      )}
    />
  );
}
