import { DESTINATIONS } from '@/components/shell/destinations';
import { PAGES, preloadSection } from './section-page';

// Every section the sub bar offers opens a screen (none is a placeholder any more).
describe('section pages', () => {
  it('exist for every destination section', () => {
    const missing = DESTINATIONS.flatMap((d) =>
      d.sections.filter((s) => !PAGES[d.key][s]).map((s) => `${d.key}/${s}`),
    );
    expect(missing).toEqual([]);
  });

  it('preload the routed section, the first one without a section', () => {
    const health = DESTINATIONS.find((d) => d.key === 'health')!;
    const issues = vi.spyOn(PAGES.health.issues, 'preload').mockResolvedValue(undefined);
    const system = vi.spyOn(PAGES.health.system, 'preload').mockResolvedValue(undefined);
    preloadSection(health, undefined);
    expect(issues).toHaveBeenCalledTimes(1);
    preloadSection(health, 'system');
    expect(system).toHaveBeenCalledTimes(1);
    preloadSection(health, 'nope'); // a 404 has nothing to fetch
    preloadSection(health, 'constructor'); // nor does an inherited property's name
    expect(issues).toHaveBeenCalledTimes(1);
  });
});
