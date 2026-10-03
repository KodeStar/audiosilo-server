import { createContext, useContext } from 'react';
import { Monitor, Moon, Sun, type LucideIcon } from 'lucide-react';
import type { ThemePref } from './theme';

export const THEME_OPTIONS: readonly { pref: ThemePref; icon: LucideIcon }[] = [
  { pref: 'light', icon: Sun },
  { pref: 'dark', icon: Moon },
  { pref: 'system', icon: Monitor },
];

export interface ThemeState {
  pref: ThemePref;
  setPref: (p: ThemePref) => void;
}

export const ThemeContext = createContext<ThemeState | null>(null);

/** One owner for the theme preference, so every menu and the palette agree. */
export function useTheme(): ThemeState {
  const v = useContext(ThemeContext);
  if (!v) throw new Error('useTheme must be used inside <ThemeProvider>');
  return v;
}
