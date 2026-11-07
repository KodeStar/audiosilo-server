import { useTranslation } from 'react-i18next';
import { BridgeSettingsLink } from '@/features/bridge/generate-parts';
import type { useAudibleBridge } from './use-audible-bridge';

export function AudibleBridgeNote({ state }: { state: ReturnType<typeof useAudibleBridge> }) {
  const { t } = useTranslation();
  const { bridge, descriptionId, pollBlocked, backupBlocked } = state;
  const status = bridge.data;
  const key = !status
    ? bridge.isError
      ? 'error'
      : 'loading'
    : !status.paired
      ? 'unpaired'
      : pollBlocked || backupBlocked
        ? 'unsupported'
        : !status.online
          ? 'offline'
          : undefined;
  if (!key) return null;
  return (
    <p id={descriptionId} className="px-5 py-3 text-[13px] text-muted-foreground">
      {t(`audible.bridge.${key}`)}{' '}
      {key === 'unpaired' ? (
        <BridgeSettingsLink>{t('audible.bridge.pair')}</BridgeSettingsLink>
      ) : null}
    </p>
  );
}
