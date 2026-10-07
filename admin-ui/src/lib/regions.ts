import { MATCH_REGIONS } from '@/api/types';

// Audible marketplaces, as the community metadata names them ("uk", "us"...):
// how the console words one, for the preferred-marketplace setting and beside an
// ASIN.

/**
 * An Audible marketplace's country as ISO 3166 says it (the community data says
 * "uk"), for Intl.DisplayNames.
 */
const COUNTRY: Record<string, string> = { uk: 'GB' };

/** Each marketplace's store, so an admin can tell which one they buy from. */
export const STORES: Record<string, string> = {
  us: 'audible.com',
  uk: 'audible.co.uk',
  ca: 'audible.ca',
  au: 'audible.com.au',
  de: 'audible.de',
  fr: 'audible.fr',
  es: 'audible.es',
  it: 'audible.it',
  jp: 'audible.co.jp',
  in: 'audible.in',
  br: 'audible.com.br',
};

/** A marketplace in words: its country in the console's language ("United Kingdom"). */
export function regionName(region: string, lang: string): string {
  const code = COUNTRY[region] ?? region.toUpperCase();
  try {
    return new Intl.DisplayNames([lang], { type: 'region' }).of(code) ?? code;
  } catch {
    return code;
  }
}

/** A marketplace as a short tag beside an ASIN ("UK"). */
export function regionTag(region: string | undefined): string {
  return region ? region.toUpperCase() : '';
}

/** The marketplaces as select options: "United Kingdom (audible.co.uk)". */
export function regionOptions(lang: string): { value: string; label: string }[] {
  return MATCH_REGIONS.map((r) => ({ value: r, label: `${regionName(r, lang)} (${STORES[r]})` }));
}
