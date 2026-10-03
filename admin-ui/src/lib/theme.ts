// Theme preference (light / dark / system). public/theme-init.js applies the
// stored preference before first paint; this module owns changing it at runtime
// and following the OS while the preference is "system".

export type ThemePref = 'light' | 'dark' | 'system';

export const THEME_KEY = 'audiosilo.admin.theme';

export function readThemePref(): ThemePref {
  try {
    const v = localStorage.getItem(THEME_KEY);
    if (v === 'light' || v === 'dark' || v === 'system') return v;
  } catch {
    // storage unavailable: fall through to the default
  }
  return 'system';
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

export function applyTheme(pref: ThemePref) {
  document.documentElement.setAttribute('data-theme', resolveTheme(pref));
}

export function setThemePref(pref: ThemePref) {
  try {
    localStorage.setItem(THEME_KEY, pref);
  } catch {
    // storage unavailable: the choice lasts for this page only
  }
  applyTheme(pref);
}

/** Re-applies the theme when the OS scheme changes while the preference is "system". */
export function followSystemTheme(getPref: () => ThemePref): () => void {
  if (typeof window.matchMedia !== 'function') return () => {};
  const mq = matchMedia('(prefers-color-scheme: dark)');
  const onChange = () => {
    if (getPref() === 'system') applyTheme('system');
  };
  mq.addEventListener('change', onChange);
  return () => mq.removeEventListener('change', onChange);
}
