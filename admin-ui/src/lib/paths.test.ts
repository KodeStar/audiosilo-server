import {
  absoluteBaseName,
  absoluteCrumbs,
  isAbsolutePath,
  joinLibraryPath,
  libraryCrumbs,
  relBaseName,
  relParent,
} from './paths';

describe('paths', () => {
  it('builds library crumbs from the root down', () => {
    expect(libraryCrumbs('Fiction', '')).toEqual([{ label: 'Fiction', path: '' }]);
    expect(libraryCrumbs('Fiction', 'Andy Weir/Project Hail Mary')).toEqual([
      { label: 'Fiction', path: '' },
      { label: 'Andy Weir', path: 'Andy Weir' },
      { label: 'Project Hail Mary', path: 'Andy Weir/Project Hail Mary' },
    ]);
  });

  it('builds absolute crumbs for Unix paths', () => {
    expect(absoluteCrumbs('/')).toEqual([{ label: '/', path: '/' }]);
    expect(absoluteCrumbs('/mnt/tank')).toEqual([
      { label: '/', path: '/' },
      { label: 'mnt', path: '/mnt' },
      { label: 'tank', path: '/mnt/tank' },
    ]);
  });

  it('builds absolute crumbs for Windows paths', () => {
    expect(absoluteCrumbs('C:\\Books\\Fiction')).toEqual([
      { label: 'C:\\', path: 'C:\\' },
      { label: 'Books', path: 'C:\\Books' },
      { label: 'Fiction', path: 'C:\\Books\\Fiction' },
    ]);
  });

  it('recognizes absolute server paths and names their last folder', () => {
    expect(isAbsolutePath('/mnt/x')).toBe(true);
    expect(isAbsolutePath('C:\\Books')).toBe(true);
    expect(isAbsolutePath('books')).toBe(false);
    expect(absoluteBaseName('/mnt/tank/drama')).toBe('drama');
    expect(absoluteBaseName('C:\\Books\\Drama')).toBe('Drama');
    expect(absoluteBaseName('/')).toBe('');
  });

  it("joins a library root and a relative path in the root's syntax", () => {
    expect(joinLibraryPath('/mnt/tank/fiction', 'A/B')).toBe('/mnt/tank/fiction/A/B');
    expect(joinLibraryPath('/mnt/tank/fiction/', 'A')).toBe('/mnt/tank/fiction/A');
    expect(joinLibraryPath('/', 'A')).toBe('/A');
    expect(joinLibraryPath('C:\\Books\\', 'A/B')).toBe('C:\\Books\\A\\B');
    expect(joinLibraryPath('/mnt/x', '')).toBe('/mnt/x');
    expect(joinLibraryPath(undefined, 'A/B')).toBe('A/B');
  });

  it("names a relative path's last segment and its parent", () => {
    expect(relBaseName('A/B/C')).toBe('C');
    expect(relBaseName('A')).toBe('A');
    expect(relParent('A/B/C')).toBe('A/B');
    expect(relParent('A')).toBe('');
  });
});
