import { libraries, podcastLibrary } from '@/test/fixtures';
import { bookLibraryOf, offersLibraryChoice } from './library-param';

describe('bookLibraryOf', () => {
  const all = [...libraries(), podcastLibrary];

  it("keeps a book library's id, and all libraries as all", () => {
    expect(bookLibraryOf(2, all)).toBe(2);
    expect(bookLibraryOf(undefined, all)).toBeUndefined();
  });

  it("reads a podcast library's id as all libraries", () => {
    expect(bookLibraryOf(3, all)).toBeUndefined();
  });

  it('keeps the id while the libraries load, or for one it does not know', () => {
    expect(bookLibraryOf(3, undefined)).toBe(3);
    expect(bookLibraryOf(9, all)).toBe(9);
  });
});

describe('offersLibraryChoice', () => {
  it('offers a choice from two book libraries, never for one or none', () => {
    expect(offersLibraryChoice([])).toBe(false);
    expect(offersLibraryChoice(libraries().slice(0, 1))).toBe(false);
    expect(offersLibraryChoice(libraries().slice(0, 2))).toBe(true);
  });
});
