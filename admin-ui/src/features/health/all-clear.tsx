import { useTranslation } from 'react-i18next';
import { CheckCircle2 } from 'lucide-react';
import { EmptyState } from '@/components/empty-state';

/** An issue list with nothing in it: all clear, or nothing ignored. */
export function AllClear({ ignored }: { ignored: boolean }) {
  const { t } = useTranslation();
  return (
    <EmptyState
      icon={CheckCircle2}
      tone="success"
      title={ignored ? t('health.ignoredEmpty.title') : t('health.clear.title')}
      body={ignored ? t('health.ignoredEmpty.body') : t('health.clear.body')}
    />
  );
}
