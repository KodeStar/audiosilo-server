import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { LeaveGuard } from '@/components/leave-guard';

import { DraftsContext } from './use-settings-draft';

/** All settings cards share one navigation prompt and before-unload guard. */
export function SettingsDrafts({ children }: { children: React.ReactNode }) {
  const { t } = useTranslation();
  const [cards, setCards] = useState<ReadonlySet<string>>(() => new Set());
  const register = useCallback((id: string, dirty: boolean) => {
    setCards((was) => {
      if (was.has(id) === dirty) return was;
      const next = new Set(was);
      if (dirty) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);
  return (
    <DraftsContext value={register}>
      {children}
      <LeaveGuard
        dirty={cards.size > 0}
        title={t('settings.leave.title')}
        description={t('settings.leave.description')}
        stay={t('common.cancel')}
        discard={t('settings.leave.discard')}
      />
    </DraftsContext>
  );
}
