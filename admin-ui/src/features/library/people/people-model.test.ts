import { authors } from '@/test/library-fixtures';
import { filterPeople, otherSpellings, sortByBooks, sortByDuration } from './people-model';

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
