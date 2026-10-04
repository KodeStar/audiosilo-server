import { useTranslation } from 'react-i18next';
import type { ClientInfo } from '@/api/types';
import { clientKind, clientParts } from './activity-model';

/**
 * Names a client: "AudioSilo 1.4.2 · iOS", "Admin console", or "Unknown app" for
 * one that never named itself (players released before the header).
 */
export function useClientName() {
  const { t } = useTranslation();
  return (client: ClientInfo | null): string => {
    const kind = clientKind(client);
    if (kind === 'unknown') return t('activity.client.unknown');
    if (kind === 'console') return t('activity.client.console');
    const { app, platform } = clientParts(client!);
    return platform ? `${app} · ${platform}` : app;
  };
}
