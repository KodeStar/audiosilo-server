import { Link, useRouterState } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Popover } from '@base-ui/react/popover';
import { Bell, Search } from 'lucide-react';
import { useOfflineLibraries, useServerInfo } from '@/api/hooks';
import { LogoTile } from '@/components/logo';
import { formatVersion } from '@/lib/format';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { DESTINATIONS, destinationFor } from './destinations';
import { ThemeMenu, UserMenu } from './menus';
import { usePalette } from './palette-context';
import { SubBar } from './sub-bar';

/** True on macOS-family platforms, where the palette shortcut reads ⌘K, not Ctrl K. */
const isMac = typeof navigator !== 'undefined' && /mac|iphone|ipad/i.test(navigator.platform);

export function TopBar() {
  const { t } = useTranslation();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const current = destinationFor(pathname);
  const { setOpen } = usePalette();

  return (
    <header className="sticky top-0 z-40 border-b border-topbar-border bg-topbar backdrop-blur-[16px] backdrop-saturate-[1.6]">
      <div className="mx-auto grid h-(--topbar-h) max-w-[1440px] grid-cols-[auto_1fr_auto] items-center gap-2.5 px-4 md:gap-5 md:px-6">
        <Link
          to="/"
          className="flex items-center gap-2.5 rounded-[12px] py-1.5 pr-2 pl-1 hover:bg-accent"
          aria-label={t('shell.home')}
        >
          <LogoTile />
          <ServerIdentity />
        </Link>

        <div className="flex min-w-0 items-center justify-center gap-[18px]">
          <nav className="hidden gap-0.5 md:flex" aria-label={t('shell.primaryNav')}>
            {DESTINATIONS.map((d) => {
              const active = current?.key === d.key;
              return (
                <Link
                  key={d.key}
                  to={d.route}
                  params={{ section: undefined }}
                  aria-current={active ? 'page' : undefined}
                  className={cn(
                    'flex h-9 items-center gap-[7px] rounded-md px-2.5 font-[550] whitespace-nowrap text-muted-foreground transition-colors duration-(--dur-1) hover:bg-accent hover:text-foreground xl:px-3',
                    active &&
                      'bg-card text-foreground shadow-[inset_0_0_0_1px_var(--border)] [&>svg]:text-brand',
                  )}
                >
                  <d.icon className="size-[17px]" aria-hidden="true" />
                  {/* Icon-only below 1180px; the label stays for screen readers. */}
                  <span className="sr-only xl:not-sr-only">{t(`shell.dest.${d.key}`)}</span>
                </Link>
              );
            })}
          </nav>
          <button
            type="button"
            onClick={() => setOpen(true)}
            className="flex h-[38px] min-w-0 flex-1 items-center gap-[9px] rounded-[12px] border bg-card pr-2.5 pl-3 text-left text-muted-foreground transition-colors duration-(--dur-1) hover:border-border-strong md:max-w-[380px] md:min-w-[180px]"
            aria-label={t('shell.search.aria')}
            aria-keyshortcuts={isMac ? 'Meta+K' : 'Control+K'}
          >
            <Search className="size-[17px] shrink-0" aria-hidden="true" />
            <span className="min-w-0 flex-1 truncate text-[13.5px]">
              {t('shell.search.placeholder')}
            </span>
            <span className="kbd hidden md:inline-block">{isMac ? '⌘K' : 'Ctrl K'}</span>
          </button>
        </div>

        <div className="flex items-center gap-1.5">
          <NotificationsBell />
          <span className="hidden md:inline-flex">
            <ThemeMenu />
          </span>
          <UserMenu />
        </div>
      </div>
      <SubBar />
    </header>
  );
}

/**
 * The server's address plus a one-line health readout under the mark: online
 * with its version, a library whose folder is unreachable, or the server down.
 */
function ServerIdentity() {
  const { t } = useTranslation();
  const server = useServerInfo();
  const offline = useOfflineLibraries();
  const tone = server.isError
    ? 'bad'
    : server.isPending
      ? 'off'
      : offline.length
        ? 'warn'
        : undefined;
  const line = server.isError
    ? t('shell.health.unreachable')
    : !server.data
      ? t('shell.health.checking')
      : offline.length === 1
        ? t('shell.health.libraryOffline', { name: offline[0].name })
        : offline.length > 1
          ? t('shell.health.librariesOffline', { count: offline.length })
          : t('shell.health.online', { version: formatVersion(server.data.version) });
  return (
    <span className="hidden min-w-0 flex-col gap-[3px] md:flex">
      <span className="max-w-[220px] truncate font-display text-base leading-none font-bold tracking-[-0.02em]">
        {window.location.host}
      </span>
      <span className="flex items-center gap-[5px] text-[11.5px] leading-[1.2] text-muted-foreground">
        <span className="dot" data-tone={tone} aria-hidden="true" />
        {line}
      </span>
    </span>
  );
}

/** Placeholder until notifications land (Phase 5b): explains what will appear here. */
function NotificationsBell() {
  const { t } = useTranslation();
  return (
    <Popover.Root>
      <Popover.Trigger
        className={buttonVariants({ variant: 'ghost-muted', size: 'icon' })}
        aria-label={t('shell.notifications.title')}
      >
        <Bell className="size-[18px]" aria-hidden="true" />
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Positioner sideOffset={8} align="end" className="z-50">
          <Popover.Popup className="w-[min(340px,calc(100vw-24px))] rounded-lg border bg-popover p-4 shadow-overlay outline-none data-closed:animate-out data-closed:fade-out-0 data-open:animate-in data-open:fade-in-0">
            <Popover.Title className="font-display text-[15px] font-[650]">
              {t('shell.notifications.title')}
            </Popover.Title>
            <Popover.Description className="mt-1 text-[13px] text-muted-foreground">
              {t('shell.notifications.soon')}
            </Popover.Description>
          </Popover.Popup>
        </Popover.Positioner>
      </Popover.Portal>
    </Popover.Root>
  );
}
