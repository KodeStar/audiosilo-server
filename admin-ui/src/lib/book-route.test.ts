import { refKey } from './book-route';

describe('refKey', () => {
  it('keys a book by library and path, unambiguously', () => {
    expect(refKey({ library_id: 2, path: 'a/b' })).toBe('2\0a/b');
    expect(refKey({ library_id: 1, path: '2\0a/b' })).not.toBe(
      refKey({ library_id: 12, path: 'a/b' }),
    );
  });
});
