import { useQueryClient } from '@tanstack/react-query';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type { AdminSettings, SettingsPatch } from '@/api/types';

/**
 * Saves a settings change: PATCH, then the new envelope into the cache and a
 * refetch of what reads settings (capabilities, system status). Throws the
 * ApiError on a refusal so a form can show it on its field.
 */
export function useSaveSettings() {
  const qc = useQueryClient();
  return async (patch: SettingsPatch): Promise<AdminSettings> => {
    const saved = await api.updateSettings(patch);
    qc.setQueryData(keys.settings, saved);
    for (const key of [keys.server, keys.system, keys.update]) {
      void qc.invalidateQueries({ queryKey: key });
    }
    return saved;
  };
}
