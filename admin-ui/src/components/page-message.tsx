import type { LucideIcon } from 'lucide-react';
import { cn } from '@/lib/utils';

/**
 * A whole page's message in place of its content (the router's error page,
 * not found): an icon tile, a heading, a sentence and its actions, centred
 * inside the shell so the nav stays.
 */
export function PageMessage({
  icon: Icon,
  tone,
  title,
  body,
  actions,
  role,
}: {
  icon: LucideIcon;
  tone: 'bad' | 'muted';
  title: string;
  body: string;
  actions: React.ReactNode;
  role?: 'alert';
}) {
  return (
    <div
      role={role}
      className="mx-auto flex max-w-[1440px] flex-col items-center gap-2.5 px-4 py-16 text-center"
    >
      <span
        className={cn(
          'grid size-10 place-items-center rounded-[11px]',
          tone === 'bad'
            ? 'bg-destructive-soft text-destructive'
            : 'bg-muted text-muted-foreground',
        )}
      >
        <Icon className="size-5" aria-hidden="true" />
      </span>
      <h2 className="h2">{title}</h2>
      <p className="max-w-md text-muted-foreground">{body}</p>
      <div className="mt-2 flex flex-wrap justify-center gap-2">{actions}</div>
    </div>
  );
}
