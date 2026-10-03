import { absoluteBaseName, absoluteCrumbs, isAbsolutePath, libraryCrumbs } from './paths';

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
});
