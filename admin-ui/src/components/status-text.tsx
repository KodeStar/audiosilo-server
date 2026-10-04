import { cn } from '@/lib/utils';

export type StatusTone = 'ok' | 'warn' | 'bad' | 'off';

const TONE_TEXT: Record<StatusTone, string> = {
  ok: 'text-success',
  warn: 'text-warning',
  bad: 'text-destructive',
  off: 'text-muted-foreground',
};

/**
 * A status as a dot plus a word (status is never colour alone). `colored` tints
 * the word too, for a status column; inline in a sentence or a fact list the
 * dot carries the colour.
 */
export function StatusText({
  tone = 'ok',
  colored = false,
  className,
  children,
}: {
  tone?: StatusTone;
  colored?: boolean;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <span className={cn('inline-flex items-center gap-1.5', colored && TONE_TEXT[tone], className)}>
      <span className="dot" data-tone={tone === 'ok' ? undefined : tone} aria-hidden="true" />
      {children}
    </span>
  );
}
