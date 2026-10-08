import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Eraser } from 'lucide-react';
import { api } from '@/api/client';
import { invalidateMatches, keys, useLibraries } from '@/api/hooks';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { DangerZone } from '@/components/danger-zone';
import { LibrarySelect } from '@/components/library-select';
import { SettingRow } from '@/components/setting-row';
import { Button } from '@/components/ui/button';
import { counted } from '@/lib/format';
import { toast } from '@/lib/toast';

/**
 * Server > Metadata's Danger zone: clear the community matches of one library (or
 * every library), so its books show as not matched and can be matched from fresh.
 * The admin's own edits and uploads stay; only matching again brings the
 * community values back, so it asks for the word first.
 */
export function ClearMatchesZone() {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const libraries = useLibraries().data ?? [];
  const [libraryId, setLibraryId] = useState(0);
  const [confirming, setConfirming] = useState(false);
  const library = libraries.find((l) => l.id === libraryId);

  return (
    <DangerZone>
      <SettingRow
        title={t('settings.metadata.clear.title')}
        description={t('settings.metadata.clear.body')}
      >
        <div className="flex flex-wrap items-center gap-2">
          <LibrarySelect
            value={libraryId}
            onChange={setLibraryId}
            label={t('settings.metadata.clear.library')}
          />
          <Button variant="destructive-outline" onClick={() => setConfirming(true)}>
            <Eraser aria-hidden="true" />
            {t('settings.metadata.clear.action')}
          </Button>
        </div>
      </SettingRow>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        icon={Eraser}
        title={
          library
            ? t('settings.metadata.clear.titleLibrary', { name: library.name })
            : t('settings.metadata.clear.titleAll')
        }
        description={t('settings.metadata.clear.confirmBody')}
        confirmLabel={t('settings.metadata.clear.confirm')}
        typeToConfirm={t('settings.metadata.clear.word')}
        onConfirm={async () => {
          const cleared = await api.clearCommunityMatches(libraryId || undefined);
          invalidateMatches(qc);
          void qc.invalidateQueries({ queryKey: keys.matchRuns });
          toast.add({
            title: t('settings.metadata.clear.done'),
            description: t('settings.metadata.clear.doneBody', counted(cleared.books, lang)),
            type: 'success',
          });
        }}
      >
        <ul className="ml-5 flex list-disc flex-col gap-1 text-[13px] text-muted-foreground">
          <li>{t('settings.metadata.clear.point.values')}</li>
          <li>{t('settings.metadata.clear.point.kept')}</li>
          <li>{t('settings.metadata.clear.point.runs')}</li>
        </ul>
      </ConfirmDialog>
    </DangerZone>
  );
}
