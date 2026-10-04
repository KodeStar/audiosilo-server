import { DESTINATIONS } from '@/components/shell/destinations';
import { PAGES } from './section-page';

// Every section the sub bar offers opens a screen (none is a placeholder any more).
describe('section pages', () => {
  it('exist for every destination section', () => {
    const missing = DESTINATIONS.flatMap((d) =>
      d.sections.filter((s) => !PAGES[d.key][s]).map((s) => `${d.key}/${s}`),
    );
    expect(missing).toEqual([]);
  });
});
