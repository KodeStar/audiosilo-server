import { useTranslation } from 'react-i18next';
import { ShieldCheck, Unplug } from 'lucide-react';
import type { AdminLibrary } from '@/api/types';
import { Notice } from '@/components/notice';
import { formatNumber } from '@/lib/format';

/**
 * Why a library's folder can't be read, and that nothing was lost. With books
 * indexed it is a celebrated safety stop (STYLEGUIDE.md "Safety stops"); with
 * none it is just a folder to check.
 */
export function OfflineNotice({
  library: l,
  title,
  actions,
  listeners,
  className,
}: {
  library: AdminLibrary;
  /** Overrides the default headline (the overview names the library). */
  title?: string;
  actions?: React.ReactNode;
  /** How many people's progress is kept with it, said when known (Health). */
  listeners?: number;
  className?: string;
}) {
  const { t, i18n } = useTranslation();
  if (l.book_count === 0) {
    return (
      <Notice
        tone="warn"
        icon={Unplug}
        title={title ?? t('libraries.unreadable.title')}
        actions={actions}
        className={className}
      >
        {t('libraries.unreadable.body', { root: l.root })}
      </Notice>
    );
  }
  return (
    <Notice
      tone="safe"
      icon={ShieldCheck}
      title={title ?? t('libraries.safety.title')}
      actions={actions}
      className={className}
    >
      {t('libraries.safety.body', {
        root: l.root,
        count: l.book_count,
        formatted: formatNumber(l.book_count, i18n.resolvedLanguage ?? 'en'),
      })}
      {listeners ? (
        <>
          {' '}
          {t('health.offline.listeners', {
            count: listeners,
            formatted: formatNumber(listeners, i18n.resolvedLanguage ?? 'en'),
          })}
        </>
      ) : null}
    </Notice>
  );
}
