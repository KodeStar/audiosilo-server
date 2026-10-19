import { settingsWith } from '@/test/fixtures';
import { outOfRange, SETTINGS_PAGES, visiblePages } from './settings-model';

describe('outOfRange', () => {
  it('refuses what is not a whole number inside the range, and blank', () => {
    for (const bad of ['0', '51', '-3', '2.5', 'six', '1e3', '', '  ']) {
      expect(outOfRange(bad, 1, 50)).toBe(true);
    }
  });

  it('accepts whole numbers inside the range', () => {
    for (const ok of ['1', '50', ' 6 ']) {
      expect(outOfRange(ok, 1, 50)).toBe(false);
    }
  });
});

describe('visiblePages', () => {
  const pages = ['general', 'bridge', 'modules'] as const;

  it('lists every page while no module brings one (Phase 2a)', () => {
    expect(visiblePages(SETTINGS_PAGES, settingsWith())).toEqual([...SETTINGS_PAGES]);
    expect(SETTINGS_PAGES).toContain('modules');
    expect(SETTINGS_PAGES.indexOf('modules')).toBe(SETTINGS_PAGES.indexOf('bridge') + 1);
  });

  it("drops a module's page while its module is off, unknown or the settings unloaded", () => {
    const modulePages = { bridge: 'contributions' };
    expect(
      visiblePages(pages, settingsWith({ modules: { contributions: true } }), modulePages),
    ).toEqual(['general', 'bridge', 'modules']);
    expect(
      visiblePages(pages, settingsWith({ modules: { contributions: false } }), modulePages),
    ).toEqual(['general', 'modules']);
    expect(visiblePages(pages, settingsWith({ modules: {} }), modulePages)).toEqual([
      'general',
      'modules',
    ]);
    expect(visiblePages(pages, undefined, modulePages)).toEqual(['general', 'modules']);
  });
});
