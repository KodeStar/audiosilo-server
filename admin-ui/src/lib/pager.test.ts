import { clampPage, pageCount, pagerItems } from './pager';

describe('pagerItems', () => {
  it('shows every page of a short list', () => {
    expect(pagerItems(1, 1)).toEqual([]);
    expect(pagerItems(2, 3)).toEqual([1, 2, 3]);
    expect(pagerItems(1, 5)).toEqual([1, 2, 'gap', 5]);
  });

  it('keeps the ends and the pages around the current one, gaps between', () => {
    expect(pagerItems(9, 20)).toEqual([1, 'gap', 8, 9, 10, 'gap', 20]);
    expect(pagerItems(20, 20)).toEqual([1, 'gap', 19, 20]);
  });

  it('shows a single skipped page instead of a gap', () => {
    expect(pagerItems(4, 9)).toEqual([1, 2, 3, 4, 5, 'gap', 9]);
    expect(pagerItems(6, 9)).toEqual([1, 'gap', 5, 6, 7, 8, 9]);
  });
});

describe('pageCount and clampPage', () => {
  it('counts at least one page and keeps a page within them', () => {
    expect(pageCount(0, 24)).toBe(1);
    expect(pageCount(49, 24)).toBe(3);
    expect(clampPage(undefined, 3)).toBe(1);
    expect(clampPage(9, 3)).toBe(3);
    expect(clampPage(0, 3)).toBe(1);
  });
});
