import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Trash2 } from 'lucide-react';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type { AudibleAccount } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Button } from '@/components/ui/button';
import { toast } from '@/lib/toast';
import { accountName } from './audible-model';

/**
 * An account's Remove, behind a confirm: takes the account and its titles off
 * this server (its waiting check is cancelled), for one the Bridge can no
 * longer remove itself (unpaired, paired again). It signs nothing out of
 * Amazon, which the confirm says: that is the Bridge's Accounts page.
 */
export function RemoveAccountButton({ account }: { account: AudibleAccount }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const name = accountName(t, account);
  return (
    <>
      <Button variant="destructive-outline" size="sm" onClick={() => setConfirming(true)}>
        <Trash2 aria-hidden="true" />
        {t('audible.remove.action')}
      </Button>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        icon={Trash2}
        title={t('audible.remove.confirmTitle', { name })}
        description={t('audible.remove.confirmBody')}
        confirmLabel={t('audible.remove.confirm')}
        onConfirm={async () => {
          await api.removeAudibleAccount(account.id);
          toast.add({ title: t('audible.remove.done', { name }), type: 'success' });
          void qc.invalidateQueries({ queryKey: keys.audibleAccounts });
          // Marked stale only: on the titles page useTitlesAfterPolls refetches
          // them once the accounts move, so this would fetch them twice.
          void qc.invalidateQueries({ queryKey: keys.audibleTitleLists, refetchType: 'none' });
          void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
        }}
      >
        <ul className="ml-5 flex list-disc flex-col gap-1 text-[13px] text-muted-foreground">
          <li>{t('audible.remove.point.server')}</li>
          <li>{t('audible.remove.point.bridge')}</li>
        </ul>
      </ConfirmDialog>
    </>
  );
}
