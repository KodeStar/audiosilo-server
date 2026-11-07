import { useQueryClient } from '@tanstack/react-query';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type { AdminSettings, SettingsPatch } from '@/api/types';

/**
 * Saves a settings change: PATCH, then the new envelope into the cache and a
 * refetch of what reads settings (capabilities, system status, modules). Throws the
 * ApiError on a refusal so a form can show it on its field.
 */
export function useSaveSettings() {
  const qc = useQueryClient();
  return async (patch: SettingsPatch): Promise<AdminSettings> => {
    const saved = await api.updateSettings(patch);
    // A status poll begun before the save may still hold the old settings.
    // Discard that reply before publishing the PATCH's authoritative envelope.
    await qc.cancelQueries({ queryKey: keys.settings });
    qc.setQueryData(keys.settings, saved);
    for (const key of [keys.server, keys.system, keys.update, keys.modules]) {
      void qc.invalidateQueries({ queryKey: key });
    }
    return saved;
  };
}
