import { useTranslation } from 'react-i18next';
import { Menu } from '@base-ui/react/menu';
import { ExternalLink, HeartHandshake, Languages, LogOut, Moon, Sun } from 'lucide-react';
import { useServerInfo } from '@/api/hooks';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { LANGUAGES, setLanguage, type Language } from '@/i18n';
import { useCurrentUser, useSession } from '@/lib/session';
import { SPONSOR_URL } from '@/lib/support';
import type { ThemePref } from '@/lib/theme';
import { THEME_OPTIONS, useTheme } from '@/lib/theme-context';
import { Monogram } from '@/components/monogram';
import { buttonVariants } from '@/components/ui/button';

function ThemeRadioItems() {
  const { t } = useTranslation();
  const { pref, setPref } = useTheme();
  return (
    <DropdownMenuGroup>
      <DropdownMenuLabel>{t('shell.theme.title')}</DropdownMenuLabel>
      <DropdownMenuRadioGroup value={pref} onValueChange={(v) => setPref(v as ThemePref)}>
        {THEME_OPTIONS.map(({ pref: p, icon: Icon }) => (
          <DropdownMenuRadioItem key={p} value={p}>
            <Icon aria-hidden="true" />
            {t(`shell.theme.${p}`)}
          </DropdownMenuRadioItem>
        ))}
      </DropdownMenuRadioGroup>
    </DropdownMenuGroup>
  );
}

export function ThemeMenu() {
  const { t } = useTranslation();
  const { pref, resolved } = useTheme();
  const Icon = resolved === 'dark' ? Moon : Sun;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        className={buttonVariants({ variant: 'ghost-muted', size: 'icon' })}
        aria-label={t('shell.theme.aria', { theme: t(`shell.theme.${pref}`) })}
      >
        <Icon className="size-[18px]" aria-hidden="true" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <ThemeRadioItems />
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

const linkItemClass =
  'flex min-h-9 items-center gap-2.5 rounded-[9px] px-2.5 py-2 text-[13.5px] outline-hidden select-none focus:bg-accent [&_svg]:size-4 [&_svg]:text-muted-foreground';

export function UserMenu() {
  const { t, i18n } = useTranslation();
  const user = useCurrentUser();
  const { signOut } = useSession();
  const server = useServerInfo();

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        className={buttonVariants({ variant: 'ghost', size: 'icon' })}
        aria-label={t('shell.account.aria')}
      >
        <Monogram name={user.username} size={26} />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuGroup>
          <DropdownMenuLabel className="normal-case tracking-normal">
            {t('shell.account.signedInAs', { name: user.username })}
          </DropdownMenuLabel>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <ThemeRadioItems />
        <DropdownMenuSub>
          <DropdownMenuSubTrigger>
            <Languages aria-hidden="true" />
            {t('ui.language')}
          </DropdownMenuSubTrigger>
          <DropdownMenuSubContent>
            <DropdownMenuRadioGroup
              value={i18n.resolvedLanguage}
              onValueChange={(v) => setLanguage(v as Language)}
            >
              {Object.entries(LANGUAGES).map(([code, label]) => (
                <DropdownMenuRadioItem key={code} value={code}>
                  {label}
                </DropdownMenuRadioItem>
              ))}
            </DropdownMenuRadioGroup>
          </DropdownMenuSubContent>
        </DropdownMenuSub>
        <DropdownMenuSeparator />
        {server.data?.capabilities.web_player ? (
          <Menu.LinkItem href="/web/" className={linkItemClass}>
            <ExternalLink aria-hidden="true" />
            {t('shell.account.webPlayer')}
          </Menu.LinkItem>
        ) : null}
        <Menu.LinkItem
          href={SPONSOR_URL}
          target="_blank"
          rel="noreferrer noopener"
          className={linkItemClass}
        >
          <HeartHandshake aria-hidden="true" />
          {t('support.title')}
        </Menu.LinkItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onClick={() => void signOut()}>
          <LogOut aria-hidden="true" />
          {t('shell.account.signOut')}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
