import { libraries, podcastLibrary } from '@/test/fixtures';
import { bookLibraryOf } from './library-param';

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
