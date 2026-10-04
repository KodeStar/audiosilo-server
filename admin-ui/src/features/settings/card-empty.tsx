import type { LucideIcon } from 'lucide-react';

/**
 * An empty list inside a settings card: the empty state's icon tile, headline and
 * sentence, sized for a card (its headline isn't a heading: the card has one).
 */
export function CardEmpty({
  icon: Icon,
  title,
  body,
  action,
}: {
  icon: LucideIcon;
  title: React.ReactNode;
  body: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <div className="flex flex-col items-center gap-1.5 border-t px-6 py-10 text-center">
      <span
        className="mb-1 grid size-11 place-items-center rounded-[13px] bg-muted text-muted-foreground"
        aria-hidden="true"
      >
        <Icon className="size-5" />
      </span>
      <b className="font-semibold">{title}</b>
      <p className="max-w-[420px] text-[13px] text-muted-foreground">{body}</p>
      {action ? <div className="mt-2">{action}</div> : null}
    </div>
  );
}
