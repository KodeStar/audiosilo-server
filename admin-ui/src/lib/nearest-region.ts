import { MATCH_REGIONS } from '@/api/types';

// The Audible marketplace nearest the admin (server#192), for a server whose
// metadata.region was never chosen (settings metadata.region_chosen false): from
// the region of the browser's languages in order, then its time zone, else the
// US store. Only ever offered: nothing is saved until the admin saves.

export type Region = (typeof MATCH_REGIONS)[number];

/**
 * A country (ISO 3166-1 alpha-2) to its marketplace: each marketplace's own
 * country, and a country without one to the store nearest it. A country not
 * here falls through to the next clue.
 */
const COUNTRY: Record<string, Region> = {
  US: 'us',
  GB: 'uk',
  CA: 'ca',
  AU: 'au',
  DE: 'de',
  FR: 'fr',
  ES: 'es',
  IT: 'it',
  JP: 'jp',
  IN: 'in',
  BR: 'br',
  // Without a store of their own: the nearest one.
  IE: 'uk',
  IM: 'uk',
  JE: 'uk',
  GG: 'uk',
  GI: 'uk',
  NZ: 'au',
  AT: 'de',
  CH: 'de',
  LI: 'de',
  LU: 'de',
  BE: 'fr',
  MC: 'fr',
  PT: 'es',
  AD: 'es',
  SM: 'it',
  VA: 'it',
  MX: 'us',
  PR: 'us',
};

/** The marketplace for a language tag's region ("en-GB": uk); null without a region we place. */
export function regionFromLanguage(tag: string): Region | null {
  let country: string | undefined;
  try {
    country = new Intl.Locale(tag).region;
  } catch {
    return null;
  }
  return country ? (COUNTRY[country.toUpperCase()] ?? null) : null;
}

/** IANA zones of the marketplaces' countries outside the US, beyond their own prefix rule. */
const ZONES: Record<string, Region> = {
  'Europe/London': 'uk',
  'Europe/Belfast': 'uk',
  'Europe/Dublin': 'uk',
  'Europe/Isle_of_Man': 'uk',
  'Europe/Jersey': 'uk',
  'Europe/Guernsey': 'uk',
  'Europe/Gibraltar': 'uk',
  'Europe/Berlin': 'de',
  'Europe/Busingen': 'de',
  'Europe/Vienna': 'de',
  'Europe/Zurich': 'de',
  'Europe/Vaduz': 'de',
  'Europe/Luxembourg': 'de',
  'Europe/Paris': 'fr',
  'Europe/Monaco': 'fr',
  'Europe/Brussels': 'fr',
  'Europe/Madrid': 'es',
  'Africa/Ceuta': 'es',
  'Atlantic/Canary': 'es',
  'Europe/Andorra': 'es',
  'Europe/Lisbon': 'es',
  'Atlantic/Madeira': 'es',
  'Atlantic/Azores': 'es',
  'Europe/Rome': 'it',
  'Europe/San_Marino': 'it',
  'Europe/Vatican': 'it',
  'Asia/Tokyo': 'jp',
  'Asia/Kolkata': 'in',
  'Asia/Calcutta': 'in',
  'Pacific/Auckland': 'au',
  'Pacific/Chatham': 'au',
  'Antarctica/McMurdo': 'au',
};

const CANADA = new Set([
  'Toronto',
  'Montreal',
  'Vancouver',
  'Edmonton',
  'Calgary',
  'Winnipeg',
  'Regina',
  'Swift_Current',
  'Halifax',
  'Glace_Bay',
  'Moncton',
  'Goose_Bay',
  'St_Johns',
  'Whitehorse',
  'Dawson',
  'Dawson_Creek',
  'Fort_Nelson',
  'Creston',
  'Yellowknife',
  'Inuvik',
  'Iqaluit',
  'Pangnirtung',
  'Rankin_Inlet',
  'Resolute',
  'Cambridge_Bay',
  'Atikokan',
  'Blanc-Sablon',
  'Nipigon',
  'Thunder_Bay',
  'Rainy_River',
]);

const BRAZIL = new Set([
  'Sao_Paulo',
  'Bahia',
  'Belem',
  'Fortaleza',
  'Recife',
  'Maceio',
  'Araguaina',
  'Santarem',
  'Noronha',
  'Manaus',
  'Cuiaba',
  'Campo_Grande',
  'Porto_Velho',
  'Boa_Vista',
  'Rio_Branco',
  'Eirunepe',
]);

/** The marketplace for an IANA time zone ("Europe/London": uk); null for one we don't place. */
export function regionFromTimeZone(zone: string): Region | null {
  if (ZONES[zone]) return ZONES[zone];
  if (zone.startsWith('Australia/')) return 'au';
  if (zone.startsWith('Canada/')) return 'ca';
  if (zone.startsWith('Brazil/')) return 'br';
  if (zone.startsWith('America/')) {
    const city = zone.slice('America/'.length);
    if (CANADA.has(city)) return 'ca';
    if (BRAZIL.has(city)) return 'br';
  }
  return null;
}

/**
 * The marketplace nearest the admin: the first of `languages` whose region we
 * place, else the time zone's, else the US store.
 */
export function nearestRegion(languages: readonly string[], timeZone: string | undefined): Region {
  for (const tag of languages) {
    const r = regionFromLanguage(tag);
    if (r) return r;
  }
  return (timeZone && regionFromTimeZone(timeZone)) || 'us';
}

/** nearestRegion from this browser's languages and time zone. */
export function browserNearestRegion(): Region {
  let zone: string | undefined;
  try {
    zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  } catch {
    zone = undefined;
  }
  const langs =
    typeof navigator === 'undefined' ? [] : (navigator.languages ?? [navigator.language]);
  return nearestRegion(langs.filter(Boolean), zone);
}
