import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
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
      {status && !status.paired ? (
        <Link
          to="/server/{-$section}"
          params={{ section: undefined }}
          search={{ topic: 'bridge' }}
          className="font-semibold text-primary hover:underline"
        >
          {t('audible.bridge.pair')}
        </Link>
      ) : null}
    </p>
  );
}
