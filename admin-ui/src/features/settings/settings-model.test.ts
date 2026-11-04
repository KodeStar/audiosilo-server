import i18n from '@/i18n';
import { settingsWith } from '@/test/fixtures';
import { lockLabel, outOfRange, restartList, SETTINGS_PAGES, visiblePages } from './settings-model';

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

  it("lists every page but a module's whose module is off", () => {
    expect(visiblePages(SETTINGS_PAGES, settingsWith())).toEqual(
      SETTINGS_PAGES.filter((p) => p !== 'audible'),
    );
    expect(
      visiblePages(
        SETTINGS_PAGES,
        settingsWith({ modules: { contributions: false, audible: true } }),
      ),
    ).toEqual([...SETTINGS_PAGES]);
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

describe('restartList', () => {
  it('names the settings waiting for a restart as a list in the language', () => {
    const one = settingsWith({ restart_pending: ['metadata.mode'] });
    const two = settingsWith({ restart_pending: ['metadata.mode', 'network.tls_mode'] });
    const t = i18n.getFixedT('en');
    expect(restartList(t, one, 'en')).toBe('Metadata source');
    expect(restartList(t, two, 'en')).toBe('Metadata source and HTTPS mode');
  });
});

describe('lockLabel', () => {
  it('names the variable, or the program that started the server for a launcher pin', () => {
    const t = i18n.getFixedT('en');
    expect(lockLabel(t, 'AUDIOSILO_PUBLIC_URL')).toBe('Set by AUDIOSILO_PUBLIC_URL');
    expect(lockLabel(t, 'launcher')).toBe('Set by the program that started the server');
  });
});
