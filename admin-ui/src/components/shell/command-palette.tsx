import { useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { Command, useCommandState } from 'cmdk';
import { useTranslation } from 'react-i18next';
import { Dialog } from '@base-ui/react/dialog';
import {
  CornerDownLeft,
  DatabaseZap,
  ExternalLink,
  Globe,
  Home,
  Languages,
  LogOut,
  RefreshCw,
  Search,
  UserPlus,
  type LucideIcon,
} from 'lucide-react';
import { useServerInfo, useStats } from '@/api/hooks';
import { rescanLibrary } from '@/features/libraries/rescan';
import { LANGUAGES, setLanguage, type Language } from '@/i18n';
import { useSession } from '@/lib/session';
import { THEME_OPTIONS, useTheme } from '@/lib/theme-context';
import { DESTINATIONS } from './destinations';
import { usePalette } from './palette-context';
import { paletteFilter } from './palette-filter';

// The ⌘K palette (STYLEGUIDE.md "Command palette"). cmdk supplies the combobox +
// listbox semantics, filtering and keyboard model; Base UI's Dialog supplies the
// modal. Never render cmdk's own <Command.Dialog>: it is Radix-based and injects
// a <style> element, which the CSP blocks (eslint forbids it, and
// app.test.tsx asserts the open palette leaves no <style> behind).
// Phase 1a covers navigation, settings and actions; book/people search is 2b.

interface Entry {
  id: string;
  title: string;
  subtitle: string;
  icon: LucideIcon;
  /** Extra words cmdk matches on but doesn't show. */
  keywords?: string[];
  run: () => void;
}

export function CommandPalette() {
  const { t } = useTranslation();
  const { isOpen, setOpen } = usePalette();
  return (
    <Dialog.Root open={isOpen} onOpenChange={setOpen}>
      <Dialog.Portal>
        <Dialog.Backdrop className="fixed inset-0 z-80 bg-overlay data-closed:animate-out data-closed:fade-out-0 data-open:animate-in data-open:fade-in-0" />
        <Dialog.Popup className="fixed top-2.5 left-1/2 z-95 w-[calc(100vw-20px)] max-w-[720px] -translate-x-1/2 overflow-hidden rounded-2xl border bg-popover text-popover-foreground shadow-overlay outline-none data-closed:animate-out data-closed:fade-out-0 data-open:animate-in data-open:fade-in-0 data-open:zoom-in-[.96] md:top-[9px]">
          <Dialog.Title className="sr-only">{t('palette.title')}</Dialog.Title>
          {/* The popup unmounts when closed, so the body's queries only run while it's open. */}
          <PaletteBody close={() => setOpen(false)} />
        </Dialog.Popup>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function PaletteBody({ close }: { close: () => void }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { signOut } = useSession();
  const { setPref } = useTheme();
  const server = useServerInfo();
  const stats = useStats();

  const go = (
    to: string,
    params?: Record<string, string | undefined>,
    search?: Record<string, unknown>,
  ) => {
    close();
    void navigate({ to, params, search });
  };

  const actions: Entry[] = [
    {
      id: 'invite',
      title: t('palette.action.invite'),
      subtitle: t('palette.action.inviteSub'),
      icon: UserPlus,
      keywords: ['person', 'user', 'pair', 'qr'],
      run: () => go('/people/{-$section}', { section: undefined }, { invite: true }),
    },
    {
      id: 'add-library',
      title: t('palette.action.addLibrary'),
      subtitle: t('palette.action.addLibrarySub'),
      icon: DatabaseZap,
      keywords: ['folder', 'library', 'new'],
      run: () => go('/library/{-$section}', { section: 'libraries' }, { add: true }),
    },
    ...(stats.data?.libraries ?? []).map<Entry>((lib) => ({
      id: `rescan-${lib.id}`,
      title: t('palette.action.rescan', { name: lib.name }),
      subtitle: t('palette.action.rescanSub'),
      icon: RefreshCw,
      keywords: ['scan', lib.name],
      run: () => {
        close();
        rescanLibrary(queryClient, lib);
      },
    })),
    ...(server.data?.capabilities.web_player
      ? [
          {
            id: 'web-player',
            title: t('shell.account.webPlayer'),
            subtitle: t('palette.action.webPlayerSub'),
            icon: ExternalLink,
            run: () => window.location.assign('/web/'),
          },
        ]
      : []),
    {
      id: 'sign-out',
      title: t('shell.account.signOut'),
      subtitle: t('palette.action.signOutSub'),
      icon: LogOut,
      run: () => {
        close();
        void signOut();
      },
    },
  ];

  const pages: Entry[] = [
    {
      id: 'home',
      title: t('shell.homeTitle'),
      subtitle: t('palette.page'),
      icon: Home,
      run: () => go('/'),
    },
    ...DESTINATIONS.map<Entry>((d) => ({
      id: d.key,
      title: t(`shell.dest.${d.key}`),
      subtitle: t('palette.page'),
      icon: d.icon,
      run: () => go(d.route, { section: undefined }),
    })),
  ];

  // Sections are only offered while searching, so the empty palette stays short.
  const sections: Entry[] = DESTINATIONS.flatMap((d) =>
    d.sections.slice(1).map<Entry>((s) => ({
      id: `${d.key}-${s}`,
      title: t(`shell.section.${d.key}.${s}`),
      subtitle: `${t(`shell.dest.${d.key}`)} › ${t(`shell.section.${d.key}.${s}`)}`,
      icon: d.icon,
      run: () => go(d.route, { section: s }),
    })),
  );

  const settings: Entry[] = [
    {
      id: 'metadata',
      title: t('settings.metadata.title'),
      subtitle: t('shell.section.server.settings'),
      icon: Globe,
      keywords: ['meta', 'community', 'lookup', 'asin'],
      run: () => go('/server/{-$section}', { section: undefined }),
    },
    ...THEME_OPTIONS.map<Entry>(({ pref, icon }) => ({
      id: `theme-${pref}`,
      title: t(`palette.theme.${pref}`),
      subtitle: t('shell.theme.title'),
      icon,
      keywords: ['theme', 'appearance', 'mode'],
      run: () => {
        close();
        setPref(pref);
      },
    })),
    ...Object.entries(LANGUAGES).map<Entry>(([code, label]) => ({
      id: `lang-${code}`,
      title: label,
      subtitle: t('ui.language'),
      icon: Languages,
      keywords: ['language', code],
      run: () => {
        close();
        setLanguage(code as Language);
      },
    })),
  ];

  return (
    <Command label={t('palette.title')} loop filter={paletteFilter} className="flex flex-col">
      <div className="flex h-[60px] items-center gap-3 border-b px-[18px]">
        <Search className="size-5 shrink-0 text-muted-foreground" aria-hidden="true" />
        <Command.Input
          autoFocus
          placeholder={t('palette.placeholder')}
          className="min-w-0 flex-1 border-0 bg-transparent text-[17px] outline-none placeholder:text-subtle-foreground focus-visible:outline-none"
        />
        <span className="kbd hidden md:inline-block">esc</span>
      </div>
      <Command.List className="max-h-[min(460px,60vh)] overflow-auto px-2 pt-1.5 pb-2.5 **:[[cmdk-group-heading]]:px-2.5 **:[[cmdk-group-heading]]:pt-3 **:[[cmdk-group-heading]]:pb-1.5 **:[[cmdk-group-heading]]:text-[11.5px] **:[[cmdk-group-heading]]:font-[650] **:[[cmdk-group-heading]]:tracking-[0.05em] **:[[cmdk-group-heading]]:text-subtle-foreground **:[[cmdk-group-heading]]:uppercase">
        <Command.Empty className="flex flex-col items-center gap-2 px-5 py-10 text-center">
          <EmptyState />
        </Command.Empty>
        <PaletteGroup heading={t('palette.group.actions')} entries={actions} />
        <PaletteGroup heading={t('palette.group.goTo')} entries={pages} />
        <SearchOnly>
          <PaletteGroup heading={t('palette.group.sections')} entries={sections} />
        </SearchOnly>
        <PaletteGroup heading={t('palette.group.settings')} entries={settings} />
      </Command.List>
      <Footer />
    </Command>
  );
}

function SearchOnly({ children }: { children: React.ReactNode }) {
  const search = useCommandState((s) => s.search);
  return search ? children : null;
}

function PaletteGroup({ heading, entries }: { heading: string; entries: Entry[] }) {
  if (entries.length === 0) return null;
  return (
    <Command.Group heading={heading}>
      {entries.map((e) => (
        <Command.Item
          key={e.id}
          value={e.id}
          // Title first: paletteFilter treats keywords[0] as the title for ranking.
          keywords={[e.title, e.subtitle, ...(e.keywords ?? [])]}
          onSelect={e.run}
          className="group flex min-h-12 cursor-pointer items-center gap-3 rounded-[12px] px-2.5 py-2 data-[selected=true]:bg-accent"
        >
          <span className="grid size-9 shrink-0 place-items-center rounded-md bg-muted text-muted-foreground">
            <e.icon className="size-[17px]" aria-hidden="true" />
          </span>
          <span className="flex min-w-0 flex-1 flex-col">
            <span className="truncate text-sm font-semibold">
              <Highlight text={e.title} />
            </span>
            <span className="truncate text-[12.5px] text-muted-foreground">{e.subtitle}</span>
          </span>
          <CornerDownLeft
            className="hidden size-4 shrink-0 text-muted-foreground group-data-[selected=true]:block"
            aria-hidden="true"
          />
        </Command.Item>
      ))}
    </Command.Group>
  );
}

/** Bolds the first case-insensitive occurrence of the search in brand ink. */
function Highlight({ text }: { text: string }) {
  const search = useCommandState((s) => s.search).trim();
  const i = search ? text.toLowerCase().indexOf(search.toLowerCase()) : -1;
  if (i < 0) return text;
  return (
    <>
      {text.slice(0, i)}
      <mark className="bg-transparent font-[750] text-brand-ink">
        {text.slice(i, i + search.length)}
      </mark>
      {text.slice(i + search.length)}
    </>
  );
}

function EmptyState() {
  const { t } = useTranslation();
  const search = useCommandState((s) => s.search);
  return (
    <>
      <Search className="size-7 text-subtle-foreground" aria-hidden="true" />
      <p className="h3">{t('palette.empty.title', { query: search })}</p>
      <p className="max-w-sm text-muted-foreground">{t('palette.empty.body')}</p>
    </>
  );
}

function Footer() {
  const { t } = useTranslation();
  const count = useCommandState((s) => s.filtered.count);
  return (
    <div className="hidden gap-4 border-t bg-muted px-4 py-2.5 text-xs text-muted-foreground md:flex">
      <span>
        <span className="kbd">↑</span> <span className="kbd">↓</span> {t('palette.hint.move')}
      </span>
      <span>
        <span className="kbd">↵</span> {t('palette.hint.open')}
      </span>
      <span>
        <span className="kbd">esc</span> {t('palette.hint.close')}
      </span>
      <span className="ml-auto tabular-nums">{t('palette.results', { count })}</span>
    </div>
  );
}
