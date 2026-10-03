import { DESTINATIONS } from '@/components/shell/destinations';
import { LANGUAGES, resources } from '.';

// Every language must carry every English key (the DoD: others may lag in
// quality, never go missing), with the same {{placeholders}}. The only extra
// keys allowed are CLDR "_many" plural forms.

const en: Record<string, string> = resources.en;
const placeholders = (s: string) => [...s.matchAll(/\{\{(\w+)\}\}/g)].map((m) => m[1]).sort();

describe.each(Object.keys(LANGUAGES))('locale %s', (lang) => {
  const dict: Record<string, string> = resources[lang as keyof typeof resources];

  it('has every English key, non-empty', () => {
    const missing = Object.keys(en).filter((k) => !dict[k]?.trim());
    expect(missing).toEqual([]);
  });

  it('has no keys English lacks (except _many plurals)', () => {
    const extra = Object.keys(dict).filter((k) => !(k in en) && !/_many$/.test(k));
    expect(extra).toEqual([]);
  });

  it('keeps the same placeholders', () => {
    const mismatched = Object.keys(en).filter(
      (k) => dict[k] && placeholders(dict[k]).join() !== placeholders(en[k]).join(),
    );
    expect(mismatched).toEqual([]);
  });
});

describe('navigation labels', () => {
  it('exist for every destination, section and placeholder body', () => {
    const needed = DESTINATIONS.flatMap((d) => [
      `shell.dest.${d.key}`,
      `soon.body.${d.key}`,
      ...d.sections.map((s) => `shell.section.${d.key}.${s}`),
    ]);
    expect(needed.filter((k) => !(k in en))).toEqual([]);
  });
});
