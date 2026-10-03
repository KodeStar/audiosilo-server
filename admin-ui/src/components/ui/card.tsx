import { cn } from '@/lib/utils';

// Shelf card: radius 16, a hairline, never a shadow. The header row holds a
// title, an optional muted description and an optional action.

export function Card({ className, ...props }: React.ComponentProps<'section'>) {
  return <section className={cn('min-w-0 rounded-xl border bg-card', className)} {...props} />;
}

export function CardHeader({
  title,
  description,
  action,
  titleId,
  className,
}: {
  title: React.ReactNode;
  description?: React.ReactNode;
  action?: React.ReactNode;
  /** Pass to label the card section (aria-labelledby). */
  titleId?: string;
  className?: string;
}) {
  return (
    <div
      className={cn(
        'flex flex-wrap items-center justify-between gap-3 border-b px-5 py-4',
        className,
      )}
    >
      <div className="flex min-w-0 flex-col gap-0.5">
        <h3 id={titleId} className="h3">
          {title}
        </h3>
        {description ? (
          <span className="text-[12.5px] text-muted-foreground">{description}</span>
        ) : null}
      </div>
      {action}
    </div>
  );
}
