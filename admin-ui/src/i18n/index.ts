import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import { readStorage, writeStorage } from '@/lib/storage';
import de from './locales/de.json';
import en from './locales/en.json';
import es from './locales/es.json';
import fr from './locales/fr.json';
import it from './locales/it.json';
import pt from './locales/pt.json';

// Flat dotted keys (`home.greeting.morning`), one JSON file per language.
// English is the base and the fallback; every other file must carry the same
// key set (i18n.test.ts enforces it). The language choice shares the connect
// page's storage key, so a choice made on either carries across.

export const LANGUAGES = {
  en: 'English',
  es: 'Español',
  fr: 'Français',
  de: 'Deutsch',
  pt: 'Português',
  it: 'Italiano',
} as const;

export type Language = keyof typeof LANGUAGES;

export const resources = { en, es, fr, de, pt, it } as const;

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
  void i18n.changeLanguage(lang);
}

void i18n.use(initReactI18next).init({
  resources: Object.fromEntries(
    Object.entries(resources).map(([lng, translation]) => [lng, { translation }]),
  ),
  lng: detectLanguage(),
  fallbackLng: 'en',
  keySeparator: false,
  nsSeparator: false,
  interpolation: { escapeValue: false }, // React escapes
  returnNull: false,
});

i18n.on('languageChanged', (lng) => {
  document.documentElement.lang = lng;
});
document.documentElement.lang = i18n.language;

export default i18n;
