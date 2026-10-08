import { fireEvent, render, screen } from '@testing-library/react';
import { CoverArt } from './book-cover';

// Art that isn't square is shown whole over a blurred copy of itself; square
// art (most of it) gets no copy, so no filter layer.

const SRC = 'data:image/png;base64,AAAA';

/** Loads the cover's image as if its art were w×h. */
function load(w: number, h: number) {
  const img = screen.getByRole('img', { name: 'Dune' });
  Object.defineProperty(img, 'naturalWidth', { value: w });
  Object.defineProperty(img, 'naturalHeight', { value: h });
  fireEvent.load(img);
}

const fill = (c: HTMLElement) => c.querySelector('img.cover-fill');

describe('CoverArt', () => {
  it('fills around art that is not square', () => {
    const { container } = render(<CoverArt src={SRC} pending={false} title="Dune" />);
    expect(fill(container)).toBeNull();
    load(300, 450);
    expect(fill(container)).toHaveAttribute('src', SRC);
    expect(fill(container)).toHaveAttribute('aria-hidden', 'true');
  });

  it('leaves square art (within a rounding error) alone', () => {
    const { container } = render(<CoverArt src={SRC} pending={false} title="Dune" />);
    load(500, 496);
    expect(fill(container)).toBeNull();
  });

  it('measures new art afresh', () => {
    const { container, rerender } = render(<CoverArt src={SRC} pending={false} title="Dune" />);
    load(300, 450);
    rerender(<CoverArt src={`${SRC}B`} pending={false} title="Dune" />);
    expect(fill(container)).toBeNull();
  });
});
