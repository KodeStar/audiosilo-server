import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';

/**
 * Whether browsers play a book (or file) as it is, or the server transcodes it: a
 * dot and a word, with the codec when it's known ("Direct · AAC").
 */
export function PlaybackStatus({ direct, codec }: { direct: boolean; codec?: string }) {
  const { t } = useTranslation();
  const label = direct ? t('playback.direct') : t('playback.transcode');
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 whitespace-nowrap',
        !direct && 'text-warning',
      )}
    >
      <span className="dot" data-tone={direct ? undefined : 'warn'} aria-hidden="true" />
      {codec ? `${label} · ${codec.toUpperCase()}` : label}
    </span>
  );
}
