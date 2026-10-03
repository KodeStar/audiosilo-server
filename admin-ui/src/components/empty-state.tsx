import type { LucideIcon } from 'lucide-react';
import { cn } from '@/lib/utils';

/** Empty state (STYLEGUIDE.md): an icon, a one-line headline, one sentence, one action. */
export function EmptyState({
  icon: Icon,
  title,
  body,
  action,
  className,
}: {
  icon: LucideIcon;
  title: React.ReactNode;
  body?: React.ReactNode;
  action?: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        'flex flex-col items-center gap-2 rounded-xl border bg-card px-6 py-12 text-center',
        className,
      )}
    >
      <span
        className="mb-1 grid size-12 place-items-center rounded-[14px] bg-muted text-muted-foreground"
        aria-hidden="true"
      >
        <Icon className="size-6" />
      </span>
      <h2 className="h3">{title}</h2>
      {body ? <p className="max-w-[440px] text-muted-foreground">{body}</p> : null}
      {action ? <div className="mt-2">{action}</div> : null}
    </div>
  );
}
