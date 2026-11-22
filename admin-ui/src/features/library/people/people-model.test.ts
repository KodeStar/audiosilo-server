import { authors } from '@/test/library-fixtures';
import {
  filterPeople,
  otherSpellings,
  sortByBooks,
  sortByDuration,
  undoSteps,
} from './people-model';

const people = [
  { name: 'Charlotte Brontë', books: 3, duration: 7200 },
  { name: 'Anne Brontë', books: 3, duration: 90000 },
  { name: 'Emily Brontë', books: 1, duration: 36000 },
];

describe('ordering and filtering', () => {
  it('sorts authors by books and narrators by hours, ties by name', () => {
    expect(sortByBooks(people).map((p) => p.name)).toEqual([
      'Anne Brontë',
      'Charlotte Brontë',
      'Emily Brontë',
    ]);
    expect(sortByDuration(people).map((p) => p.name)).toEqual([
      'Anne Brontë',
      'Emily Brontë',
      'Charlotte Brontë',
    ]);
  });

  it('filters ignoring case and accents', () => {
    expect(filterPeople(people, 'BRONTE').length).toBe(3);
    expect(filterPeople(people, ' emily ').map((p) => p.name)).toEqual(['Emily Brontë']);
    expect(filterPeople(people, '')).toBe(people);
    expect(filterPeople(people, 'Austen')).toEqual([]);
  });
});

describe('merge suggestions', () => {
  const s = authors.merge_suggestions[0];

  it('rewrites every spelling but the suggested one', () => {
    expect(otherSpellings(s)).toEqual(['Sanderson, Brandon']);
  });
});

describe('merge Undo', () => {
  it('chunks returned paths at the bulk limit', () => {
    const books = Array.from({ length: 2001 }, (_, i) => ({
      library_id: 1,
      path: String(i),
      previous: null,
    }));
    expect(undoSteps(books, 'author').map((s) => s.books.length)).toEqual([1000, 1000, 1]);
  });

  it('restores identical values with different sources separately', () => {
    const previous = 'Sanderson, Brandon';
    const a = { library_id: 1, path: 'a' };
    const b = { library_id: 2, path: 'a' };
    const c = { library_id: 1, path: 'c' };
    expect(
      undoSteps(
        [
          { ...a, previous, source: 'community' },
          { ...b, previous, source: 'edited' },
          { ...c, previous, source: 'community' },
        ],
        'author',
      ),
    ).toEqual([
      { books: [a, c], edit: { set: { author: previous }, source: 'community' } },
      { books: [b], edit: { set: { author: previous }, source: 'edited' } },
    ]);
  });
});
