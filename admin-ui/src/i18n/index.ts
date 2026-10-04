import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import { readStorage, writeStorage } from '@/lib/storage';
import en from './locales/en.json';

// Flat dotted keys (`home.greeting.morning`), one JSON file per language.
// English is the base and the fallback; every other file must carry the same
// key set (i18n.test.ts enforces it). The language choice shares the connect
// page's storage key, so a choice made on either carries across.
//
// Only English ships in the entry chunk; another language is its own chunk,
// loaded when chosen (or detected), so the first paint doesn't carry five
// languages nobody reads. main.tsx waits for `i18nReady` before rendering.

export const LANGUAGES = {
  en: 'English',
  es: 'Español',
  fr: 'Français',
  de: 'Deutsch',
  pt: 'Português',
  it: 'Italiano',
} as const;

export type Language = keyof typeof LANGUAGES;

const loaders = import.meta.glob<Record<string, string>>(
  ['./locales/*.json', '!./locales/en.json'],
  { import: 'default' },
);

/** Loads a language's strings into i18next (English is always there). */
async function loadLanguage(lang: Language): Promise<void> {
  if (i18n.hasResourceBundle(lang, 'translation')) return;
  const load = loaders[`./locales/${lang}.json`];
  if (load) i18n.addResourceBundle(lang, 'translation', await load());
}

const LANG_KEY = 'audiosilo.lang';

function isLanguage(v: string): v is Language {
  return Object.hasOwn(LANGUAGES, v);
}

function detectLanguage(): Language {
  const saved = readStorage(LANG_KEY);
  if (saved && isLanguage(saved)) return saved;
  for (const l of navigator.languages ?? [navigator.language]) {
    const code = (l ?? '').slice(0, 2).toLowerCase();
    if (isLanguage(code)) return code;
  }
  return 'en';
}

export function setLanguage(lang: Language) {
  writeStorage(LANG_KEY, lang);
  void loadLanguage(lang).then(() => i18n.changeLanguage(lang));
}

const initial = detectLanguage();

void i18n.use(initReactI18next).init({
  resources: { en: { translation: en } },
  // English until the detected language has loaded (i18nReady), so the first
  // render never shows keys.
  lng: 'en',
  fallbackLng: 'en',
  keySeparator: false,
  nsSeparator: false,
  interpolation: { escapeValue: false }, // React escapes
  returnNull: false,
});

/**
 * Settles once the detected language's strings are loaded and active (at once
 * for English). A failed chunk load leaves the console in English.
 */
export const i18nReady: Promise<unknown> = loadLanguage(initial)
  .then(() => i18n.changeLanguage(initial))
  .catch(() => undefined);

i18n.on('languageChanged', (lng) => {
  document.documentElement.lang = lng;
});
document.documentElement.lang = i18n.language;

export default i18n;
