import '@testing-library/jest-dom/vitest';
import { afterEach, beforeAll } from 'vitest';
import { cleanup, configure } from '@testing-library/react';
import '@/i18n';
import { preloadScreens } from './preload-screens';

// Each screen is a lazy chunk. Loading them all before the file's tests keeps
// that cold import out of every test's own time (preloadScreens); the hook's
// budget is for the import alone, so a slow machine has room. findBy keeps 3 s
// for a screen's own requests and renders under a full parallel run.
configure({ asyncUtilTimeout: 3000 });
beforeAll(preloadScreens, 60_000);

// jsdom gaps the UI libraries touch: cmdk measures its list with ResizeObserver
// and scrolls the active item into view; the theme code reads matchMedia; Base
// UI's radio builds a PointerEvent.
class NoopResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}
globalThis.ResizeObserver ??= NoopResizeObserver as unknown as typeof ResizeObserver;
Element.prototype.scrollIntoView ??= function () {};
// Base UI's radio dispatches a synthetic PointerEvent, which jsdom doesn't define.
globalThis.PointerEvent ??= class PointerEvent extends MouseEvent {
  readonly pointerId: number;
  readonly pointerType: string;
  constructor(type: string, init: PointerEventInit = {}) {
    super(type, init);
    this.pointerId = init.pointerId ?? 0;
    this.pointerType = init.pointerType ?? 'mouse';
  }
} as unknown as typeof PointerEvent;
window.scrollTo = () => {};
window.matchMedia ??= ((query: string) => ({
  matches: false,
  media: query,
  onchange: null,
  addEventListener() {},
  removeEventListener() {},
  addListener() {},
  removeListener() {},
  dispatchEvent: () => false,
})) as typeof window.matchMedia;

afterEach(() => {
  cleanup();
  localStorage.clear();
  sessionStorage.clear();
  document.documentElement.removeAttribute('data-theme');
});
