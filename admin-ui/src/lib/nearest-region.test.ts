import {
  browserNearestRegion,
  nearestRegion,
  regionFromLanguage,
  regionFromTimeZone,
} from './nearest-region';

// server#192: the marketplace preselected on a server whose region was never
// chosen, from the browser's languages, then its time zone, then the US store.

describe('regionFromLanguage', () => {
  it.each([
    ['en-GB', 'uk'],
    ['de-AT', 'de'],
    ['de-CH', 'de'],
    ['en-IN', 'in'],
    ['pt-BR', 'br'],
    ['pt-PT', 'es'],
    ['en-IE', 'uk'],
    ['en-NZ', 'au'],
    ['es-MX', 'us'],
    ['fr-CA', 'ca'],
    ['fr-BE', 'fr'],
    ['ja-JP', 'jp'],
    ['en-us', 'us'],
  ])('%s is %s', (tag, want) => {
    expect(regionFromLanguage(tag)).toBe(want);
  });

  it.each(['en', 'de', 'nl-NL', 'es-419', 'sv-SE', 'not a tag', ''])(
    '%j places no marketplace',
    (tag) => {
      expect(regionFromLanguage(tag)).toBeNull();
    },
  );
});

describe('regionFromTimeZone', () => {
  it.each([
    ['Europe/London', 'uk'],
    ['Europe/Dublin', 'uk'],
    ['Europe/Vienna', 'de'],
    ['Europe/Paris', 'fr'],
    ['Asia/Kolkata', 'in'],
    ['Australia/Sydney', 'au'],
    ['Australia/Perth', 'au'],
    ['Pacific/Auckland', 'au'],
    ['America/Toronto', 'ca'],
    ['America/Sao_Paulo', 'br'],
    ['Asia/Tokyo', 'jp'],
  ])('%s is %s', (zone, want) => {
    expect(regionFromTimeZone(zone)).toBe(want);
  });

  it.each(['America/New_York', 'Europe/Amsterdam', 'Asia/Singapore', 'UTC', ''])(
    '%j places no marketplace',
    (zone) => {
      expect(regionFromTimeZone(zone)).toBeNull();
    },
  );
});

describe('nearestRegion', () => {
  it('takes the first language whose region it places', () => {
    expect(nearestRegion(['en', 'nl-NL', 'en-GB', 'de-DE'], 'Europe/Berlin')).toBe('uk');
  });

  it('falls back to the time zone when no language has a region it places', () => {
    expect(nearestRegion(['en', 'nl-NL'], 'Europe/London')).toBe('uk');
    expect(nearestRegion([], 'Australia/Melbourne')).toBe('au');
  });

  it('falls back to the US store when nothing places one', () => {
    expect(nearestRegion(['nl-NL'], 'Europe/Amsterdam')).toBe('us');
    expect(nearestRegion([], undefined)).toBe('us');
  });
});

describe('browserNearestRegion', () => {
  afterEach(() => vi.restoreAllMocks());

  it("reads the browser's languages", () => {
    vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['de-AT', 'en-US']);
    expect(browserNearestRegion()).toBe('de');
  });

  it('reads the time zone when the languages carry no region', () => {
    vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['en']);
    vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions').mockReturnValue({
      timeZone: 'Europe/London',
    } as Intl.ResolvedDateTimeFormatOptions);
    expect(browserNearestRegion()).toBe('uk');
  });
});
