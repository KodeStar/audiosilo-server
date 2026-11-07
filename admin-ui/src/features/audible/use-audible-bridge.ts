import { useId } from 'react';
import type { BridgeStatus } from '@/api/types';
import { useBridge } from '@/features/bridge/use-bridge';

/** Offline Bridges still accept supported jobs: the server queues them until they return. */
export function audibleJobBlocked(status: BridgeStatus | undefined, kind: string): boolean {
  return !status?.paired || !status.kinds.includes(kind);
}

/** One shared status read per card, including after re-pairing. */
export function useAudibleBridge() {
  const bridge = useBridge();
  const descriptionId = useId();
  return {
    bridge,
    descriptionId,
    pollBlocked: audibleJobBlocked(bridge.data, 'audible.poll'),
    backupBlocked: audibleJobBlocked(bridge.data, 'audible.import'),
  };
}
