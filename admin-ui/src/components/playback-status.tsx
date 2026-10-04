import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';

/** Whether browsers play a book (or file) as it is, or the server transcodes it: a dot and a word. */
export function PlaybackStatus({ direct }: { direct: boolean }) {
  const { t } = useTranslation();
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 whitespace-nowrap',
        !direct && 'text-warning',
      )}
    >
      <span className="dot" data-tone={direct ? undefined : 'warn'} aria-hidden="true" />
      {direct ? t('playback.direct') : t('playback.transcode')}
    </span>
  );
}
