import { adminBook, authors } from '@/test/library-fixtures';
import {
  filterPeople,
  mergeSteps,
  otherSpellingBooks,
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

  it('counts the books of the other spellings, not the whole suggestion', () => {
    expect(otherSpellingBooks(s, authors.authors)).toBe(1);
    // Spellings the list doesn't name: fall back to the suggestion's count.
    expect(otherSpellingBooks(s, [])).toBe(3);
  });
});

describe('bulk steps', () => {
  const fresh = adminBook({ path: 'a', author: 'Sanderson, Brandon' });
  const editedA = adminBook({ path: 'b', author: 'Sanderson, Brandon', edited: true });
  const editedB = adminBook({ path: 'c', author: 'B. Sanderson', edited: true });

  it('chunks at the bulk limit', () => {
    const refs = Array.from({ length: 2001 }, (_, i) => adminBook({ path: String(i) }));
    expect(mergeSteps(refs, 'author', 'X').map((st) => st.books.length)).toEqual([1000, 1000, 1]);
  });

  it('sets the suggested spelling on every book', () => {
    expect(mergeSteps([fresh, editedA], 'narrator', 'Kate Reading')).toEqual([
      {
        books: [
          { library_id: 1, path: 'a' },
          { library_id: 1, path: 'b' },
        ],
        edit: { set: { narrator: 'Kate Reading' } },
      },
    ]);
  });

  it('reverts unedited books and restores each old spelling on edited ones', () => {
    expect(undoSteps([fresh, editedA, editedB], 'author')).toEqual([
      { books: [{ library_id: 1, path: 'a' }], edit: { revert: ['author'] } },
      { books: [{ library_id: 1, path: 'b' }], edit: { set: { author: 'Sanderson, Brandon' } } },
      { books: [{ library_id: 1, path: 'c' }], edit: { set: { author: 'B. Sanderson' } } },
    ]);
    expect(undoSteps([fresh], 'author')).toHaveLength(1);
    expect(undoSteps([editedA], 'author')).toHaveLength(1);
  });
});
