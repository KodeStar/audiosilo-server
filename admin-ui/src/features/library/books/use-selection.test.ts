import { describe, expect, it } from 'vitest';
import { adminBook } from '@/test/library-fixtures';
import { refKey } from '@/lib/book-route';
import { rangeOf, shiftHeld } from './use-selection';

const order = ['a', 'b', 'c', 'd'].map((p) => adminBook({ path: p, title: p }));
const paths = (bs: { path: string }[]) => bs.map((b) => b.path);

describe('rangeOf', () => {
  it('runs from the last book clicked to this one, either way, in list order', () => {
    expect(paths(rangeOf(order, refKey(order[0]), order[2]))).toEqual(['a', 'b', 'c']);
    expect(paths(rangeOf(order, refKey(order[3]), order[1]))).toEqual(['b', 'c', 'd']);
    expect(paths(rangeOf(order, refKey(order[1]), order[1]))).toEqual(['b']);
  });

  it('is just this book with no anchor, or one the list no longer holds', () => {
    expect(paths(rangeOf(order, null, order[2]))).toEqual(['c']);
    expect(paths(rangeOf(order, '9:gone', order[2]))).toEqual(['c']);
    const shelf = adminBook({ path: 'shelf-only', title: 's' });
    expect(paths(rangeOf(order, refKey(order[0]), shelf))).toEqual(['shelf-only']);
  });
});

describe('shiftHeld', () => {
  it('reads Shift off a mouse or keyboard event, and nothing off none', () => {
    expect(shiftHeld(new MouseEvent('click', { shiftKey: true }))).toBe(true);
    expect(shiftHeld(new MouseEvent('click'))).toBe(false);
    expect(shiftHeld(new Event('change'))).toBe(false);
    expect(shiftHeld(undefined)).toBe(false);
  });
});
