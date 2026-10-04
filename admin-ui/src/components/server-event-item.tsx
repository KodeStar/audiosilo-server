import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import {
  Archive,
  Bell,
  BookPlus,
  MailCheck,
  MonitorSmartphone,
  Package,
  TriangleAlert,
  Unplug,
  type LucideIcon,
} from 'lucide-react';
import type { ServerEvent, ServerEventKind } from '@/api/types';
import { formatDateTime, formatRelative } from '@/lib/format';
import { describeEvent, type EventLink, type EventTone } from '@/lib/server-events';
import { cn } from '@/lib/utils';

// One event from the server's feed (GET /admin/events), as the top bar's bell and
// Server > Events list it: an icon toned by how serious it is, what happened, and
// a link to where it can be dealt with.

const ICONS: Record<ServerEventKind, LucideIcon> = {
  book_added: BookPlus,
  scan_failed: TriangleAlert,
  library_unavailable: Unplug,
  new_device: MonitorSmartphone,
  invite_redeemed: MailCheck,
  update_available: Package,
  backup_failed: Archive,
};

const TONE: Record<EventTone, string> = {
  info: 'bg-info-soft text-info',
  warn: 'bg-warning-soft text-warning',
  bad: 'bg-destructive-soft text-destructive',
};

export function ServerEventItem({
  event,
  isNew = false,
  time = 'relative',
  onPick,
}: {
  event: ServerEvent;
  /** Newer than the last time this browser opened the bell. */
  isNew?: boolean;
  /** "relative" (the bell: "3 hours ago") or "absolute" (the full list: a date and time). */
  time?: 'relative' | 'absolute';
  onPick?: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const d = describeEvent(event);
  const Icon = ICONS[event.kind] ?? Bell;
  return (
    <EventLinkTo link={d.link} onClick={onPick}>
      <span
        className={cn('grid size-[30px] shrink-0 place-items-center rounded-[9px]', TONE[d.tone])}
        aria-hidden="true"
      >
        <Icon className="size-[15px]" />
      </span>
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="flex items-start gap-1.5">
          <b className="min-w-0 flex-1 text-[13px] leading-snug font-semibold [overflow-wrap:anywhere]">
            {t(d.title.key, d.title.values)}
          </b>
          {isNew ? (
            <span
              className="mt-1 size-1.5 shrink-0 rounded-full bg-brand"
              aria-label={t('shell.notifications.new')}
            />
          ) : null}
        </span>
        {d.body ? (
          <span
            className={cn(
              'text-[12px] text-muted-foreground',
              time === 'relative' ? 'line-clamp-2' : '[overflow-wrap:anywhere]',
            )}
          >
            {t(d.body.key, d.body.values)}
          </span>
        ) : null}
        <time
          dateTime={event.at}
          title={time === 'relative' ? formatDateTime(event.at, lang) : undefined}
          className="text-[11.5px] text-subtle-foreground tabular-nums"
        >
          {time === 'relative' ? formatRelative(event.at, lang) : formatDateTime(event.at, lang)}
        </time>
      </span>
    </EventLinkTo>
  );
}

const itemClass =
  'flex w-full items-start gap-2.5 rounded-md px-2 py-2.5 text-left transition-colors duration-(--dur-1) hover:bg-accent focus-visible:bg-accent';

/** A link to where an event can be dealt with. */
function EventLinkTo({
  link,
  onClick,
  children,
}: {
  link: EventLink;
  onClick?: () => void;
  children: React.ReactNode;
}) {
  return (
    <Link
      to={`/${link.destination}/{-$section}`}
      params={{ section: link.section }}
      search={link.topic ? { topic: link.topic } : {}}
      onClick={onClick}
      className={itemClass}
    >
      {children}
    </Link>
  );
}
