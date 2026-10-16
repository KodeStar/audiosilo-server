import { checkLabels, cleanLabels, encodeLabels, labelsText, parseLabels } from './labels';

describe('labels', () => {
  it('reads and writes the stored list', () => {
    expect(parseLabels('')).toEqual([]);
    expect(parseLabels('[]')).toEqual([]);
    expect(parseLabels('not json')).toEqual([]);
    expect(parseLabels('["Fantasy",3,"Epic"]')).toEqual(['Fantasy', 'Epic']);
    expect(encodeLabels([])).toBe('');
    expect(encodeLabels(['Tom & Jerry'])).toBe('["Tom & Jerry"]');
    expect(labelsText('["Fantasy","Epic"]')).toBe('Fantasy, Epic');
  });

  it('cleans a list as the server does: spacing collapsed, each label once', () => {
    expect(cleanLabels(['  Sci-Fi ', 'Space   Opera', 'sci-fi', ''])).toEqual({
      labels: ['Sci-Fi', 'Space Opera'],
    });
    expect(cleanLabels(['x'.repeat(65)]).error).toBe('book.invalid.labelTooLong');
    // Characters, not bytes.
    expect(cleanLabels(['é'.repeat(64)]).error).toBeUndefined();
    expect(cleanLabels(['a\u0007b']).error).toBe('book.invalid.control');
    expect(cleanLabels(Array.from({ length: 51 }, (_, i) => `t${i}`)).error).toBe(
      'book.invalid.tooManyLabels',
    );
  });

  it('takes a stored list or a line apart by commas', () => {
    expect(checkLabels('')).toEqual({ value: '' });
    expect(checkLabels(' Fantasy , epic, FANTASY ')).toEqual({ value: '["Fantasy","epic"]' });
    expect(checkLabels('["Owned"]')).toEqual({ value: '["Owned"]' });
    expect(checkLabels('[1]').error).toBe('book.invalid.labels');
    expect(checkLabels('[broken').error).toBe('book.invalid.labels');
  });
});
