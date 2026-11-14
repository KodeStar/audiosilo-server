/**
 * The community metadata database's own site (audiosilo-meta's site/), where
 * anyone can add a book it doesn't hold yet. Always the public one: a server
 * pointed at another metadata service (metadata.base_url) still adds books to
 * the community database there.
 */
const META_SITE = 'https://meta.audiosilo.app';

/**
 * The site's "Add a book" page (site/src/pages/add.astro). It reads one query
 * parameter, `q` (site/src/lib/search-cta.ts ADD_QUERY_PARAM), and acts on it at
 * once: an Audible ASIN opens that record, anything else is searched for.
 * Nothing else can be prefilled.
 */
const ADD_PAGE = `${META_SITE}/add`;

/** An Audible ASIN as the site recognises one (site/src/lib/api.ts looksLikeAsin). */
const AUDIBLE_ASIN = /^B0[A-Z0-9]{8}$/;

/**
 * The "Add a book" link for a book the community database doesn't have,
 * prefilled with the best way to find it there: its ASIN (the caller's pick:
 * one the admin typed, else the book's own), else the words the admin searched
 * with, else its title and author. A book with none of those gets the bare page.
 */
export function addBookUrl(book: {
  asin?: string;
  query?: string;
  title?: string;
  author?: string;
}): string {
  const asin = (book.asin ?? '').trim().toUpperCase();
  const q = AUDIBLE_ASIN.test(asin)
    ? asin
    : book.query?.trim() ||
      [book.title, book.author]
        .map((s) => s?.trim())
        .filter(Boolean)
        .join(' ');
  return q ? `${ADD_PAGE}?q=${encodeURIComponent(q)}` : ADD_PAGE;
}
