import { useEffect, useMemo, useState } from 'react';
import { followSystemTheme, readThemePref, setThemePref, type ThemePref } from './theme';
import { ThemeContext, type ThemeState } from './theme-context';

/** One owner for the theme preference, so every menu and the palette agree. */
export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [pref, setPrefState] = useState<ThemePref>(readThemePref);

  // Track OS changes only while the preference is "system".
  useEffect(() => (pref === 'system' ? followSystemTheme() : undefined), [pref]);

  const value = useMemo<ThemeState>(
    () => ({
      pref,
      setPref: (p) => {
        setThemePref(p);
        setPrefState(p);
      },
    }),
    [pref],
  );
  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}
