import { useEffect, useMemo, useRef, useState } from 'react';
import { followSystemTheme, readThemePref, setThemePref, type ThemePref } from './theme';
import { ThemeContext, type ThemeState } from './theme-context';

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [pref, setPrefState] = useState<ThemePref>(readThemePref);
  const prefRef = useRef(pref);
  prefRef.current = pref;

  useEffect(() => followSystemTheme(() => prefRef.current), []);

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
