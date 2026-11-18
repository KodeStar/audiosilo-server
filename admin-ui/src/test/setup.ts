import '@testing-library/jest-dom/vitest';
import { afterEach, beforeAll, expect } from 'vitest';
import { cleanup, configure } from '@testing-library/react';
import '@/i18n';

// Each screen is a lazy chunk. Loading them all before a component test
// file's tests keeps that cold import out of every test's own time
// (preloadScreens); a plain .ts unit test renders no screen, so it skips the
// console's module graph. The hook's budget is for the import alone, so a slow
// machine has room. findBy keeps 3 s for a screen's own requests and renders
// under a full parallel run.
configure({ asyncUtilTimeout: 3000 });
beforeAll(async () => {
  if (!expect.getState().testPath?.endsWith('.tsx')) return;
  const { preloadScreens } = await import('./preload-screens');
  await preloadScreens();
}, 60_000);

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
// jsdom plays no media: an <audio> let go of (the chapter editor's) is paused
// and loaded again with no source, and one is asked to play, which it only
// logs as not implemented (play answering with no promise).
HTMLMediaElement.prototype.load = function () {};
HTMLMediaElement.prototype.pause = function () {};
HTMLMediaElement.prototype.play = function () {
  return Promise.resolve();
};
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
