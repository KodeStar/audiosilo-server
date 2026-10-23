import { useTranslation } from 'react-i18next';
import { RotateCcw } from 'lucide-react';
import type { AdminSettings } from '@/api/types';
import { Notice } from '@/components/notice';
import { restartList } from './settings-model';

/**
 * Saved settings the running server doesn't use yet (Server settings, and the
 * Overview, so a choice made in the setup flow isn't silently pending).
 */
export function RestartNotice({
  settings,
  actions,
}: {
  settings: AdminSettings;
  actions?: React.ReactNode;
}) {
  const { t, i18n } = useTranslation();
  if (settings.restart_pending.length === 0) return null;
  return (
    <Notice
      tone="warn"
      icon={RotateCcw}
      title={t('settings.restart.title')}
      role="status"
      actions={actions}
    >
      {t('settings.restart.body', {
        list: restartList(t, settings, i18n.resolvedLanguage ?? 'en'),
      })}
    </Notice>
  );
}
