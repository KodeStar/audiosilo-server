import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup, configure } from '@testing-library/react';
import '@/i18n';

// Each screen is a lazy chunk; its first import, with every core running a test
// file (vitest 5's default pool), can take longer than findBy's 1 s default.
configure({ asyncUtilTimeout: 3000 });

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
  document.documentElement.removeAttribute('data-theme');
});
