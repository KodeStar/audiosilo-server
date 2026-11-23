import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { KeyRound } from 'lucide-react';
import { Notice } from '@/components/notice';

/** The warning for an admin still signing in with the password printed in the log. */
export function PrintedPasswordNotice({ actions }: { actions?: ReactNode }) {
  const { t } = useTranslation();
  return (
    <Notice
      tone="warn"
      icon={KeyRound}
      role="status"
      title={t('setup.account.printed')}
      actions={actions}
    >
      {t('setup.account.printedBody')}
    </Notice>
  );
}
