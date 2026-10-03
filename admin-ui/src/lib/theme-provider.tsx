import { useEffect, useMemo, useState } from 'react';
import {
  followSystemTheme,
  readThemePref,
  resolveTheme,
  setThemePref,
  type ThemePref,
} from './theme';
import { ThemeContext, type ThemeState } from './theme-context';

/** One owner for the theme preference, so every menu and the palette agree. */
export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [pref, setPrefState] = useState<ThemePref>(readThemePref);
  const [resolved, setResolved] = useState(() => resolveTheme(pref));

  // Track OS changes only while the preference is "system".
  useEffect(() => (pref === 'system' ? followSystemTheme(setResolved) : undefined), [pref]);

  const value = useMemo<ThemeState>(
    () => ({
      pref,
      resolved,
      setPref: (p) => {
        setThemePref(p);
        setPrefState(p);
        setResolved(resolveTheme(p));
      },
    }),
    [pref, resolved],
  );
  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}
