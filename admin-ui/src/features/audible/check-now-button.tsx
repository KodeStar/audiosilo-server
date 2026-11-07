import { useTranslation } from 'react-i18next';
import { LoaderCircle, RefreshCw } from 'lucide-react';
import { Button } from '@/components/ui/button';

/** An account's Check now: a spinner while it is being queued, off while the account is paused. */
export function CheckNowButton({
  busy,
  paused,
  onCheck,
  blocked = false,
  describedBy,
}: {
  busy: boolean;
  paused: boolean;
  onCheck: () => void;
  blocked?: boolean;
  describedBy?: string;
}) {
  const { t } = useTranslation();
  return (
    <Button
      variant="outline"
      size="sm"
      disabled={busy || paused || blocked}
      aria-describedby={blocked ? describedBy : undefined}
      onClick={onCheck}
    >
      {busy ? (
        <LoaderCircle className="animate-spin" aria-hidden="true" />
      ) : (
        <RefreshCw aria-hidden="true" />
      )}
      {t('audible.check.action')}
    </Button>
  );
}
