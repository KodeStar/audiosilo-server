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
export const STORES: Record<(typeof MATCH_REGIONS)[number], string> = {
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

const names = new Map<string, Intl.DisplayNames>();

/** A marketplace in words: its country in the console's language ("United Kingdom"). */
export function regionName(region: string, lang: string): string {
  const code = COUNTRY[region] ?? region.toUpperCase();
  try {
    let f = names.get(lang);
    if (!f) names.set(lang, (f = new Intl.DisplayNames([lang], { type: 'region' })));
    return f.of(code) ?? code;
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

/** A marketplace's store ("audible.co.uk"), the code itself for one this console doesn't know. */
export function storeOf(region: string): string {
  return (STORES as Record<string, string>)[region] ?? region;
}
