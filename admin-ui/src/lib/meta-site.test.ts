import { addBookUrl } from './meta-site';

describe('addBookUrl', () => {
  const add = 'https://meta.audiosilo.app/add';
  const q = (url: string) => new URL(url).searchParams.get('q');

  it("prefills an ASIN typed, else the book's own, which open that record", () => {
    expect(q(addBookUrl({ typedAsin: 'b0typed001', asin: 'B0OWNASIN1', title: 'T' }))).toBe(
      'B0TYPED001',
    );
    expect(q(addBookUrl({ asin: ' b0ownasin1 ', query: 'words', title: 'T' }))).toBe('B0OWNASIN1');
  });

  it('skips an ASIN the site would not read as one (an ISBN-10, a bad shape)', () => {
    expect(q(addBookUrl({ asin: '0441013597', title: 'Dune', author: 'Frank Herbert' }))).toBe(
      'Dune Frank Herbert',
    );
  });

  it('prefills the words searched with, else the title and author', () => {
    expect(q(addBookUrl({ query: '  the long way  ', title: 'T', author: 'A' }))).toBe(
      'the long way',
    );
    expect(q(addBookUrl({ title: 'The Long Way', author: '' }))).toBe('The Long Way');
    expect(addBookUrl({ title: 'Wayfarers & Co', author: 'Becky Chambers' })).toBe(
      `${add}?q=Wayfarers%20%26%20Co%20Becky%20Chambers`,
    );
  });

  it('is the bare page with nothing to go on', () => {
    expect(addBookUrl({})).toBe(add);
    expect(addBookUrl({ title: ' ', query: '' })).toBe(add);
  });
});
