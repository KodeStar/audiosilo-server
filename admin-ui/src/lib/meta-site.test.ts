import { addBookUrl } from './meta-site';

describe('addBookUrl', () => {
  const add = 'https://meta.audiosilo.app/add';
  const q = (url: string) => new URL(url).searchParams.get('q');

  it('prefills an ASIN, which opens that record, before any words', () => {
    expect(q(addBookUrl({ asin: 'B0OWNASIN1', title: 'T' }))).toBe('B0OWNASIN1');
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

  it('keeps every character of the words in the one parameter', () => {
    for (const words of [
      'Tom & Jerry #1 + 100% = fun?',
      'a=b&q=other',
      'Les Misérables',
      '三体 劉慈欣',
      'Emoji 📚 book',
      'line\nbreak',
    ]) {
      const url = new URL(addBookUrl({ query: words }));
      expect([...url.searchParams.keys()]).toEqual(['q']);
      expect(url.searchParams.get('q')).toBe(words);
      expect(url.hash).toBe('');
    }
    expect(addBookUrl({ query: 'C++ & C# 100%' })).toBe(`${add}?q=C%2B%2B%20%26%20C%23%20100%25`);
  });

  it('drops half a surrogate pair rather than throw', () => {
    expect(q(addBookUrl({ query: 'cut \ud83d' }))).toBe('cut');
    expect(q(addBookUrl({ title: '\udcda Title', author: 'A' }))).toBe('Title A');
    // Nothing but the half: the next way to find the book, never an empty q.
    expect(q(addBookUrl({ query: '\ud83d', title: 'T' }))).toBe('T');
    expect(addBookUrl({ query: ' \ud83d ' })).toBe(add);
  });

  it('is the bare page with nothing to go on', () => {
    expect(addBookUrl({})).toBe(add);
    expect(addBookUrl({ title: ' ', query: '' })).toBe(add);
    expect(addBookUrl({ asin: '  ', query: ' \t\n ', title: '\u00a0', author: '   ' })).toBe(add);
  });
});
