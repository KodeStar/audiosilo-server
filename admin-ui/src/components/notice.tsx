import { cn } from '@/lib/utils';
import type { LucideIcon } from 'lucide-react';

// Notice (STYLEGUIDE.md section 8): an icon tile, a bold one-line headline, a
// muted explanation and at most two actions. `safe` is the green shield of a
// safety stop ("nothing was deleted"), not a failure.

const TONES = {
  bad: {
    box: 'border-[color-mix(in_oklab,var(--destructive)_30%,var(--border))]',
    icon: 'bg-destructive-soft text-destructive',
  },
  warn: {
    box: 'border-[color-mix(in_oklab,var(--warning)_30%,var(--border))]',
    icon: 'bg-warning-soft text-warning',
  },
  info: { box: '', icon: 'bg-info-soft text-info' },
  safe: {
    box: 'border-[color-mix(in_oklab,var(--success)_30%,var(--border))]',
    icon: 'bg-success-soft text-success',
  },
} as const;

export function Notice({
  tone,
  icon: Icon,
  title,
  children,
  actions,
  role,
  className,
}: {
  tone: keyof typeof TONES;
  icon: LucideIcon;
  title?: React.ReactNode;
  children?: React.ReactNode;
  actions?: React.ReactNode;
  role?: 'alert' | 'status';
  className?: string;
}) {
  return (
    <div
      role={role}
      className={cn(
        'flex flex-wrap items-start gap-3.5 rounded-xl border bg-card px-[18px] py-4',
        TONES[tone].box,
        className,
      )}
    >
      <span
        className={cn('grid size-9 shrink-0 place-items-center rounded-[11px]', TONES[tone].icon)}
        aria-hidden="true"
      >
        <Icon className="size-[18px]" />
      </span>
      <div className="flex min-w-0 flex-1 basis-60 flex-col gap-0.5">
        {title ? <b className="font-semibold">{title}</b> : null}
        {children ? <div className="text-muted-foreground">{children}</div> : null}
      </div>
      {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
    </div>
  );
}
