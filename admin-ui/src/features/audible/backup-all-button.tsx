import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { CloudDownload } from 'lucide-react';
import type { AudibleAccount } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Button } from '@/components/ui/button';
import { formatBytes } from '@/lib/format';
import { accountName } from './audible-model';
import type { Backups } from './use-audible';

/**
 * "Back up everything" for an account (Bridge Phase 4b): every title that is
 * available, not on this server, not marked "Not this book" and not being
 * backed up, behind a confirm that says how many and about how much space
 * ("N books, ~X GB"), and the legal note. Off with nothing to back up or a
 * paused account.
 */
export function BackupAllButton({
  account,
  backups,
  blocked = false,
  describedBy,
}: {
  account: AudibleAccount;
  backups: Backups;
  blocked?: boolean;
  describedBy?: string;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [confirming, setConfirming] = useState(false);
  const est = account.backup_estimate ?? { count: 0, bytes: 0 };
  const size = formatBytes(est.bytes, lang);
  return (
    <>
      <Button
        variant="outline"
        size="sm"
        disabled={account.paused || est.count === 0 || blocked}
        aria-describedby={blocked ? describedBy : undefined}
        onClick={() => setConfirming(true)}
      >
        <CloudDownload aria-hidden="true" />
        {t('audible.backupAll.action')}
      </Button>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        icon={CloudDownload}
        tone="brand"
        destructive={false}
        title={t('audible.backupAll.confirmTitle', { name: accountName(t, account) })}
        description={t('audible.backupAll.confirmBody', { count: est.count, size })}
        confirmLabel={t('audible.backupAll.confirm', { count: est.count })}
        onConfirm={() => backups.backUpAll(account)}
      >
        <ul className="ml-5 flex list-disc flex-col gap-1 text-[13px] text-muted-foreground">
          <li>{t('audible.backupAll.point.order')}</li>
          <li>{t('audible.backupAll.point.size')}</li>
          <li>{t('audible.legal')}</li>
        </ul>
      </ConfirmDialog>
    </>
  );
}
