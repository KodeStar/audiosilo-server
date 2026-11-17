import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// public/theme-init.js, the theme set before first paint, run against a stub
// matchMedia. The connect and setup pages run the same code
// (internal/web/assets/theme-init.js; shelf_sync_test.go keeps them identical)
// and have no React theme provider, so this file alone follows the OS there.

function loadThemeInit(dark: boolean) {
  const listeners = new Set<() => void>();
  const mq = {
    matches: dark,
    addEventListener: (_: string, fn: () => void) => listeners.add(fn),
  };
  vi.stubGlobal(
    'matchMedia',
    vi.fn(() => mq),
  );
  const code = readFileSync(resolve(process.cwd(), 'public/theme-init.js'), 'utf8');
  new Function(code)();
  const flip = (to: boolean) => {
    mq.matches = to;
    for (const fn of listeners) fn();
  };
  return { flip };
}

const theme = () => document.documentElement.getAttribute('data-theme');

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
  document.documentElement.removeAttribute('data-theme');
});

describe('theme-init.js', () => {
  it('follows the OS switching while the preference is system', () => {
    const { flip } = loadThemeInit(false);
    expect(theme()).toBe('light');
    flip(true);
    expect(theme()).toBe('dark');
    flip(false);
    expect(theme()).toBe('light');
  });

  it('keeps a chosen theme when the OS switches', () => {
    localStorage.setItem('audiosilo.admin.theme', 'light');
    const { flip } = loadThemeInit(true);
    expect(theme()).toBe('light');
    flip(true);
    expect(theme()).toBe('light');
  });

  it('reads the preference again on a switch, so one chosen since the load holds', () => {
    const { flip } = loadThemeInit(false);
    localStorage.setItem('audiosilo.admin.theme', 'light');
    flip(true);
    expect(theme()).toBe('light');
  });
});
