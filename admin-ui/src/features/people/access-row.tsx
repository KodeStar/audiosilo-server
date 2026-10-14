import type { LucideIcon } from 'lucide-react';

/**
 * One line of a person's access on their Access tab: an icon, what it is, a
 * muted detail and, for a grant the admin can take back, its action.
 */
export function AccessRow({
  icon: Icon,
  title,
  detail,
  action,
}: {
  icon: LucideIcon;
  title: string;
  detail?: string;
  action?: React.ReactNode;
}) {
  return (
    <li className="flex items-center gap-3 px-5 py-3">
      <span
        className="grid size-9 shrink-0 place-items-center rounded-[10px] bg-muted text-muted-foreground"
        aria-hidden="true"
      >
        <Icon className="size-[17px]" />
      </span>
      <div className="flex min-w-0 flex-1 flex-col">
        <b className="truncate font-semibold">{title}</b>
        {detail ? (
          <span className="truncate text-[12.5px] text-muted-foreground">{detail}</span>
        ) : null}
      </div>
      {action}
    </li>
  );
}
