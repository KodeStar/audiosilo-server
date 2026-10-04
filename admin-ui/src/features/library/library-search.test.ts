import { describe, expect, it } from 'vitest';
import { validateLibrarySearch } from './library-search';

describe('validateLibrarySearch', () => {
  it('keeps a hand-written numeric ?added= (the router parses it as a number)', () => {
    expect(validateLibrarySearch({ added: 7 })).toEqual({ added: '7' });
    expect(validateLibrarySearch({ added: '30' })).toEqual({ added: '30' });
    expect(validateLibrarySearch({ added: 8 })).toEqual({});
  });
});
