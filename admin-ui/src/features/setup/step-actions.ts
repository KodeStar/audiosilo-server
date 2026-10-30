import { queryOptions } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type {
  AdminLibrary,
  AdminSettings,
  SettingsPatch,
  SetupEnvelope,
  SetupMark,
} from '@/api/types';
import { patchIds } from '@/features/settings/settings-model';
import { useSaveSettings } from '@/features/settings/use-save-settings';
import { toast } from '@/lib/toast';
import type { SetupDraft } from './setup-model';

/** What every step is handed by the flow. */
export interface StepProps {
  env: SetupEnvelope;
  settings: AdminSettings;
  libraries: AdminLibrary[];
  /** The library to create after the metadata step (this browser's draft). */
  draft: SetupDraft | null;
  setDraft: (d: SetupDraft | null) => void;
  /** Records the step as done or skipped and moves on (throws when the save failed). */
  leave: (mark: SetupMark) => Promise<void>;
  /** Goes to the step before; null on the first. */
  back: (() => void) | null;
  /** This is where the flow ends (Continue reads Finish). */
  last: boolean;
  /** The library the flow created, to follow its scan. */
  onCreated: (id: number) => void;
  /** The folder step is marked done, but its folder was chosen in another browser (folderLost). */
  folderLost?: boolean;
}

/** Thrown by a step that has already shown why it can't go on (no toast follows). */
export class StepStop extends Error {}

/**
 * Saves a step's settings (nothing when the patch is empty) and says when a
 * saved change waits for a restart.
 */
export function useSaveStepSettings() {
  const { t } = useTranslation();
  const save = useSaveSettings();
  return async (patch: SettingsPatch | undefined) => {
    if (!patch) return;
    const saved = await save(patch);
    if (patchIds(patch).some((id) => saved.restart_pending.includes(id))) {
      toast.add({
        title: t('setup.restartTitle'),
        description: t('setup.restartBody'),
        type: 'info',
      });
    }
  };
}

/**
 * A folder's preview, kept for the visit: the folder step's check and the
 * naming step's samples are the same answer. A failed check isn't retried.
 */
export const previewQuery = (root: string) =>
  queryOptions({
    queryKey: keys.setupPreview(root),
    queryFn: () => api.previewLibrary({ root }),
    staleTime: Infinity,
    retry: false,
  });
