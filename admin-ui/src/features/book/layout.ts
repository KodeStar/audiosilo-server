// The book page's grids, shared by the page and its loading skeleton so the
// skeleton holds the same shape.

/** The hero: the cover, then the title block, bottom-aligned. */
export const HERO_GRID =
  'mx-auto grid max-w-[1440px] grid-cols-1 items-end gap-5 px-4 pt-5 pb-7 md:grid-cols-[200px_minmax(0,1fr)] md:gap-6 md:px-6 md:pt-7 md:pb-9 lg:grid-cols-[300px_minmax(0,1fr)] lg:gap-11';

/** Under the hero: the cards, then the aside from xl. */
export const PAGE_GRID = 'grid grid-cols-1 items-start gap-6 xl:grid-cols-[minmax(0,1fr)_360px]';
