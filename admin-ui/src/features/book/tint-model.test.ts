import { tintFromPalette, tintFromPixels } from './tint-model';

const pixels = (...px: [number, number, number, number][]) => px.flat();

describe('tintFromPixels', () => {
  it('takes the most vivid colour and the average', () => {
    const tint = tintFromPixels(pixels([219, 39, 119, 255], [40, 40, 40, 255]));
    expect(tint).toEqual({ tint1: '#db2777', tint2: '#822850', glow: '#db277766' });
  });

  it('ignores transparent pixels and near-black ones for the vivid colour', () => {
    const tint = tintFromPixels(pixels([0, 0, 30, 255], [255, 0, 0, 0], [60, 180, 120, 255]));
    expect(tint?.tint1).toBe('#3cb478');
    expect(tint?.tint2).toBe('#1e5a4b');
  });

  it('falls back to the average for a greyscale cover', () => {
    expect(tintFromPixels(pixels([200, 200, 200, 255], [100, 100, 100, 255]))).toEqual({
      tint1: '#969696',
      tint2: '#969696',
      glow: '#96969666',
    });
  });

  it('has nothing to say about an empty or transparent image', () => {
    expect(tintFromPixels([])).toBeUndefined();
    expect(tintFromPixels(pixels([255, 0, 0, 0]))).toBeUndefined();
  });
});

describe('tintFromPalette', () => {
  it('uses the accent and the background', () => {
    expect(tintFromPalette('#1f3a8a', ['#14532d', '#1f3a8a', '#e14b4b', '#0f1a33'])).toEqual({
      tint1: '#1f3a8a',
      tint2: '#14532d',
      glow: '#1f3a8a66',
    });
  });

  it('uses the highlight instead of a near-white background', () => {
    expect(tintFromPalette('#d99a06', ['#fbfbfc', '#d99a06', '#111827', '#111827']).tint2).toBe(
      '#111827',
    );
  });
});
