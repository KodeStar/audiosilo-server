import { landAndSettle, offsetFromLanding } from './use-keep-list-in-view';

// jsdom lays nothing out: the anchor's place, its scroll margin and the page's
// scroll are set by hand, and the ResizeObserver is one the test can fire.
let top = 0;
let scrollY = 0;
let observers: { fire: () => void; disconnect: ReturnType<typeof vi.fn>; target?: Element }[] = [];

function setup() {
  const list = document.createElement('div');
  const anchor = document.createElement('div');
  list.append(anchor);
  document.body.append(list);
  // The page's own scroll-padding-top (globals.css), which the landing must not add.
  document.documentElement.style.scrollPaddingTop = '120px';
  vi.spyOn(anchor, 'getBoundingClientRect').mockImplementation(() => ({ top }) as DOMRect);
  const style = getComputedStyle;
  vi.spyOn(window, 'getComputedStyle').mockImplementation((el, pseudo) =>
    el === anchor ? ({ scrollMarginTop: '114px' } as CSSStyleDeclaration) : style(el, pseudo),
  );
  vi.spyOn(window, 'scrollY', 'get').mockImplementation(() => scrollY);
  const scrollTo = vi.spyOn(window, 'scrollTo').mockImplementation(() => {});
  observers = [];
  vi.stubGlobal(
    'ResizeObserver',
    class {
      disconnect = vi.fn();
      target?: Element;
      constructor(cb: ResizeObserverCallback) {
        observers.push(this as never);
        (this as unknown as { fire: () => void }).fire = () =>
          cb([], this as unknown as ResizeObserver);
      }
      observe(el: Element) {
        this.target = el;
      }
    },
  );
  return { anchor, list, scrollTo };
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  document.body.innerHTML = '';
  document.documentElement.style.scrollPaddingTop = '';
});

describe('keep list in view: the landing spot', () => {
  it('measures from the scroll margin alone, so the page scroll padding is not added', () => {
    const { anchor, scrollTo } = setup();
    top = -500;
    scrollY = 1800;
    expect(offsetFromLanding(anchor)).toBe(-614);
    landAndSettle(anchor);
    // 1800 - 614: the anchor's top at exactly 114 px, the header's bottom (not 234).
    expect(scrollTo).toHaveBeenCalledWith({ top: 1186 });
  });

  it('counts the page as scrolled past only above the header bottom', () => {
    const { anchor } = setup();
    top = 113;
    expect(offsetFromLanding(anchor)).toBeLessThan(0);
    top = 114;
    expect(offsetFromLanding(anchor)).toBe(0);
    top = 200;
    expect(offsetFromLanding(anchor)).toBeGreaterThan(0);
  });

  it('watches nothing once the landing gets there', () => {
    const { anchor, scrollTo } = setup();
    // The scroll lands (the mocked rect stands for the page after it).
    top = 114;
    landAndSettle(anchor);
    expect(scrollTo).toHaveBeenCalledTimes(1);
    expect(observers).toHaveLength(0);
  });
});

describe('keep list in view: a landing the page was too short for', () => {
  // The page couldn't reach the spot: the view's placeholder was shorter than the view.
  function clamped() {
    const ctx = setup();
    top = 724;
    scrollY = 76;
    const stop = landAndSettle(ctx.anchor);
    expect(ctx.scrollTo).toHaveBeenCalledTimes(1);
    expect(observers).toHaveLength(1);
    return { ...ctx, stop, ro: observers[0] };
  }

  it('lands again once the list grows, then stops watching', () => {
    const { list, scrollTo, ro } = clamped();
    expect(ro.target).toBe(list);
    // The real view rendered: the page is tall enough now.
    top = 724;
    scrollY = 76;
    ro.fire();
    expect(scrollTo).toHaveBeenLastCalledWith({ top: 76 + 610 });
    expect(scrollTo).toHaveBeenCalledTimes(2);
    // That got there (the rect after the scroll):
    top = 114;
    ro.fire();
    expect(ro.disconnect).toHaveBeenCalled();
  });

  it('leaves a reader who scrolls in the meantime alone', () => {
    const { scrollTo, ro } = clamped();
    window.dispatchEvent(new Event('wheel'));
    expect(ro.disconnect).toHaveBeenCalled();
    ro.fire();
    expect(scrollTo).toHaveBeenCalledTimes(1);
  });

  it('takes a scrolling key as the reader scrolling, but not typing in a field', () => {
    const { ro } = clamped();
    const field = document.createElement('input');
    document.body.append(field);
    field.dispatchEvent(new KeyboardEvent('keydown', { key: ' ', bubbles: true }));
    field.dispatchEvent(new KeyboardEvent('keydown', { key: 'End', bubbles: true }));
    expect(ro.disconnect).not.toHaveBeenCalled();
    document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'PageDown', bubbles: true }));
    expect(ro.disconnect).toHaveBeenCalled();
  });

  it('gives up after a while', () => {
    vi.useFakeTimers();
    const { ro } = clamped();
    vi.advanceTimersByTime(4999);
    expect(ro.disconnect).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(ro.disconnect).toHaveBeenCalled();
  });

  it('stops when told to (a newer change, or the page going)', () => {
    const { scrollTo, ro, stop } = clamped();
    stop();
    expect(ro.disconnect).toHaveBeenCalled();
    // A size change queued before the stop lands nothing.
    ro.fire();
    expect(scrollTo).toHaveBeenCalledTimes(1);
  });
});
