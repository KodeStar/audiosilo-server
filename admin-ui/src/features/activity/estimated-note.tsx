import { useTranslation } from 'react-i18next';
import { History } from 'lucide-react';
import type { Activity } from '@/api/types';
import { Notice } from '@/components/notice';
import { formatHours } from '@/lib/format';

/**
 * Says how much of a period's listening is estimated: listening from before the
 * server recorded sessions that the players' own history didn't cover, worked out
 * from where each book was left (migration 0021). Nothing when there is none.
 */
export function EstimatedNote({ a }: { a: Activity }) {
  const { t, i18n } = useTranslation();
  if (!(a.estimated > 0)) return null;
  return (
    <Notice tone="info" icon={History}>
      {t('activity.estimated', { hours: formatHours(a.estimated, i18n.resolvedLanguage ?? 'en') })}
    </Notice>
  );
}
