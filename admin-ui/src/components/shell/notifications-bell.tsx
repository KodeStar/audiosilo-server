import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Popover } from '@base-ui/react/popover';
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
import { useServerEvents } from '@/api/hooks';
import type { ServerEvent, ServerEventKind } from '@/api/types';
import { buttonVariants } from '@/components/ui/button';
import { formatRelative } from '@/lib/format';
import {
  describeEvent,
  readSeen,
  unreadCount,
  writeSeen,
  type EventLink,
  type EventTone,
} from '@/lib/server-events';
import { cn } from '@/lib/utils';

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

/** How many events the bell lists. */
const SHOWN = 8;

/**
 * The top bar's bell: the server's newest events (a book added, a library gone
 * offline, a new sign-in...), each leading to where it can be dealt with. A dot
 * marks events newer than the last time this browser opened it.
 */
export function NotificationsBell() {
  const { t } = useTranslation();
  const events = useServerEvents();
  const list = events.data?.events ?? [];
  const [seen, setSeen] = useState(readSeen);
  // What was new when the bell opened stays marked while it is open.
  const [openedAt, setOpenedAt] = useState(seen);
  const [open, setOpen] = useState(false);
  const unread = unreadCount(list, seen);

  const onOpenChange = (o: boolean) => {
    setOpen(o);
    if (o) {
      setOpenedAt(seen);
      const newest = list[0]?.id ?? 0;
      if (newest > seen) {
        writeSeen(newest);
        setSeen(newest);
      }
    }
  };

  return (
    <Popover.Root open={open} onOpenChange={onOpenChange}>
      <Popover.Trigger
        className={cn(buttonVariants({ variant: 'ghost-muted', size: 'icon' }), 'relative')}
        aria-label={
          unread
            ? t('shell.notifications.unread', { count: unread })
            : t('shell.notifications.title')
        }
      >
        <Bell className="size-[18px]" aria-hidden="true" />
        {unread ? (
          <span
            className="absolute top-[7px] right-[8px] size-2 rounded-full bg-brand ring-2 ring-background"
            aria-hidden="true"
          />
        ) : null}
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Positioner sideOffset={8} align="end" className="z-50">
          <Popover.Popup className="w-[min(360px,calc(100vw-24px))] rounded-lg border bg-popover p-2 shadow-overlay outline-none data-closed:animate-out data-closed:fade-out-0 data-open:animate-in data-open:fade-in-0">
            <div className="flex items-center justify-between gap-2 px-2 pt-1.5 pb-2">
              <Popover.Title className="font-display text-[15px] font-[650]">
                {t('shell.notifications.title')}
              </Popover.Title>
              <Link
                to="/server/{-$section}"
                params={{ section: undefined }}
                search={{ topic: 'notifications' }}
                onClick={() => setOpen(false)}
                className="text-[12.5px] font-semibold text-brand-ink hover:underline"
              >
                {t('shell.notifications.settings')}
              </Link>
            </div>
            {events.isError ? (
              <p className="px-2 pb-2 text-[13px] text-muted-foreground">
                {t('shell.notifications.error')}
              </p>
            ) : !events.data ? (
              <div
                className="skel mx-2 mb-2 h-16 rounded-md"
                role="status"
                aria-label={t('common.loading')}
              />
            ) : list.length === 0 ? (
              <Popover.Description className="px-2 pb-2 text-[13px] text-muted-foreground">
                {t('shell.notifications.empty')}
              </Popover.Description>
            ) : (
              <ul className="flex max-h-[min(440px,70dvh)] flex-col overflow-y-auto">
                {list.slice(0, SHOWN).map((ev) => (
                  <li key={ev.id}>
                    <EventItem event={ev} isNew={ev.id > openedAt} onPick={() => setOpen(false)} />
                  </li>
                ))}
              </ul>
            )}
          </Popover.Popup>
        </Popover.Positioner>
      </Popover.Portal>
    </Popover.Root>
  );
}

function EventItem({
  event,
  isNew,
  onPick,
}: {
  event: ServerEvent;
  isNew: boolean;
  onPick: () => void;
}) {
  const { t, i18n } = useTranslation();
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
          <span className="line-clamp-2 text-[12px] text-muted-foreground">
            {t(d.body.key, d.body.values)}
          </span>
        ) : null}
        <time dateTime={event.at} className="text-[11.5px] text-subtle-foreground">
          {formatRelative(event.at, i18n.resolvedLanguage ?? 'en')}
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
  onClick: () => void;
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
