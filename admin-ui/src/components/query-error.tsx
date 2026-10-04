import { useTranslation } from 'react-i18next';
import { RotateCw, TriangleAlert } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { errorMessage } from '@/lib/errors';
import { Notice } from './notice';

/** A query that failed: what didn't load, why (errorMessage), and a retry. */
export function QueryError({
  title,
  error,
  onRetry,
}: {
  title: string;
  error: unknown;
  onRetry: () => void;
}) {
  const { t } = useTranslation();
  return (
    <Notice
      tone="bad"
      icon={TriangleAlert}
      title={title}
      role="alert"
      actions={
        <Button variant="outline" size="sm" onClick={onRetry}>
          <RotateCw aria-hidden="true" />
          {t('common.tryAgain')}
        </Button>
      }
    >
      {errorMessage(error, t)}
    </Notice>
  );
}
