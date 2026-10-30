import { isChunkLoadError, reloadOnce } from './stale-build';

// UPG-01: a tab on an older build reloads into the new one once, never in a loop.

afterEach(() => vi.restoreAllMocks());

describe('reloadOnce', () => {
  it('reloads the first time for a build, and not again', () => {
    const reload = vi.fn();
    expect(reloadOnce('build-a', reload)).toBe(true);
    expect(reloadOnce('build-a', reload)).toBe(false);
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it('reloads again for a newer build in the same tab (the next upgrade)', () => {
    const reload = vi.fn();
    reloadOnce('build-a', reload);
    expect(reloadOnce('build-b', reload)).toBe(true);
    expect(reload).toHaveBeenCalledTimes(2);
  });

  it('never reloads without sessionStorage, so it cannot loop', () => {
    const reload = vi.fn();
    const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(reloadOnce('build-a', reload)).toBe(false);
    expect(reload).not.toHaveBeenCalled();
    expect(get).toHaveBeenCalled();
  });
});

describe('isChunkLoadError', () => {
  it("knows each engine's words for a missing chunk", () => {
    expect(
      isChunkLoadError(
        new TypeError(
          'Failed to fetch dynamically imported module: http://h/admin/assets/issues-page-x.js',
        ),
      ),
    ).toBe(true);
    expect(isChunkLoadError(new TypeError('Importing a module script failed.'))).toBe(true);
    expect(isChunkLoadError(new Error('error loading dynamically imported module'))).toBe(true);
    expect(isChunkLoadError(new Error('Unable to preload CSS for /admin/assets/x.css'))).toBe(true);
    expect(isChunkLoadError(new Error('Cannot read properties of undefined'))).toBe(false);
    expect(isChunkLoadError(undefined)).toBe(false);
  });
});
