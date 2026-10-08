import type { TFunction } from 'i18next';
import { formatNumber } from '@/lib/format';

/** An i18n key and its values: a sentence a model chose, worded where it's shown. */
export interface Phrase {
  key: string;
  values?: Record<string, string | number>;
}

/**
 * A phrase in words: its values plus `formatted`, the count with the language's
 * separators (the count itself picks the plural form).
 */
export function say(t: TFunction, p: Phrase, lang: string): string {
  const count = p.values?.count;
  return t(
    p.key,
    typeof count === 'number' ? { ...p.values, formatted: formatNumber(count, lang) } : p.values,
  );
}
