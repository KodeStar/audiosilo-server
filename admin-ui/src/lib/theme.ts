import { readStorage, writeStorage } from './storage';

// Theme preference (light / dark / system). public/theme-init.js applies the
// stored preference before first paint (same storage key); this module owns
// changing it at runtime and following the OS while the preference is "system".

export type ThemePref = 'light' | 'dark' | 'system';

const THEME_KEY = 'audiosilo.admin.theme';

export function readThemePref(): ThemePref {
  const v = readStorage(THEME_KEY);
  return v === 'light' || v === 'dark' ? v : 'system';
}

function systemDark(): boolean {
  return (
    typeof window.matchMedia === 'function' && matchMedia('(prefers-color-scheme: dark)').matches
  );
}

export function resolveTheme(pref: ThemePref): 'light' | 'dark' {
  if (pref === 'system') return systemDark() ? 'dark' : 'light';
  return pref;
}

/** Applies `pref` to <html data-theme> and returns what it resolved to. */
function applyTheme(pref: ThemePref): 'light' | 'dark' {
  const resolved = resolveTheme(pref);
  document.documentElement.setAttribute('data-theme', resolved);
  return resolved;
}

/** Stores and applies `pref`; returns the theme now on screen. */
export function setThemePref(pref: ThemePref): 'light' | 'dark' {
  writeStorage(THEME_KEY, pref);
  return applyTheme(pref);
}

/**
 * Re-applies the system theme whenever the OS scheme changes, reporting the new
 * resolved theme. Returns the unsubscribe.
 */
export function followSystemTheme(onResolved: (theme: 'light' | 'dark') => void): () => void {
  if (typeof window.matchMedia !== 'function') return () => {};
  const mq = matchMedia('(prefers-color-scheme: dark)');
  const onChange = () => onResolved(applyTheme('system'));
  mq.addEventListener('change', onChange);
  return () => mq.removeEventListener('change', onChange);
}
