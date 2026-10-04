import { useTranslation } from 'react-i18next';
import type { SessionState as State } from '@/api/types';

/** A live session's state: a pulsing dot and "Playing", or a grey dot and "Paused". */
export function SessionState({ state }: { state: State }) {
  const { t } = useTranslation();
  const playing = state === 'playing';
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="dot" data-tone={playing ? 'live' : 'off'} aria-hidden="true" />
      {playing ? t('live.playing') : t('live.pausedState')}
    </span>
  );
}
