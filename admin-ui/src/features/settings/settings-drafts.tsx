import { useCallback, useState } from 'react';
import { useBlocker } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { TriangleAlert } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogFooter } from '@/components/ui/dialog';

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
  const dirty = cards.size > 0;
  const blocker = useBlocker({
    shouldBlockFn: () => true,
    disabled: !dirty,
    enableBeforeUnload: dirty,
    withResolver: true,
  });
  return (
    <DraftsContext value={register}>
      {children}
      <Dialog
        open={blocker.status === 'blocked'}
        onOpenChange={(open) => !open && blocker.reset?.()}
      >
        <DialogContent
          title={t('settings.leave.title')}
          description={t('settings.leave.description')}
          icon={TriangleAlert}
          tone="danger"
        >
          <DialogFooter>
            <Button variant="ghost" onClick={() => blocker.reset?.()}>
              {t('common.cancel')}
            </Button>
            <Button variant="destructive" onClick={() => blocker.proceed?.()}>
              {t('settings.leave.discard')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </DraftsContext>
  );
}
