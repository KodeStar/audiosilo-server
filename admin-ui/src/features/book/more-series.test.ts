import { describe, expect, it } from 'vitest';
import { checkField, commitDraft, displayValue } from './book-model';

describe('more_series', () => {
  it('reads the edited line into the canonical list the server stores', () => {
    expect(
      checkField('more_series', ' Discworld: City Watch #1;  Omnibus ; ; City Watch #1.5 '),
    ).toEqual({
      value:
        '[{"name":"Discworld: City Watch","position":1},{"name":"Omnibus","position":0},{"name":"City Watch","position":1.5}]',
    });
    // Each name once (the first), a "#" inside a name kept, blank is none.
    expect(checkField('more_series', 'A #2; A #3; C# in Depth').value).toBe(
      '[{"name":"A","position":2},{"name":"C# in Depth","position":0}]',
    );
    expect(checkField('more_series', '  ')).toEqual({ value: '' });
  });

  it('takes a stored value back as it is (a revert undo sends it)', () => {
    const stored = '[{"name":"Discworld","position":8}]';
    expect(checkField('more_series', stored)).toEqual({ value: stored });
  });

  it('reads a line that only starts with "[" as a line', () => {
    expect(checkField('more_series', '[Untitled] #2').value).toBe(
      '[{"name":"[Untitled]","position":2}]',
    );
    expect(checkField('more_series', '[1]')).toEqual({
      value: '[{"name":"[1]","position":0}]',
    });
  });

  it('keeps no draft for a list left as it was shown', () => {
    // As the server stores it; a name with ";" or "#2" doesn't read back the same.
    const stored = '[{"name":"Tom & Jerry; Friends","position":1}]';
    expect(commitDraft({}, 'more_series', displayValue('more_series', stored), stored)).toEqual({});
  });

  it('refuses what the server would', () => {
    expect(checkField('more_series', 'A #two').error).toBe('book.invalid.moreSeries');
    expect(checkField('more_series', 'A #100001').error).toBe('book.invalid.moreSeries');
    expect(checkField('more_series', 'A\u0000B').error).toBe('book.invalid.control');
  });

  it('shows a stored list as its line', () => {
    expect(
      displayValue(
        'more_series',
        '[{"name":"Discworld","position":8},{"name":"Omnibus","position":0}]',
      ),
    ).toBe('Discworld #8; Omnibus');
    expect(displayValue('series', 'Discworld')).toBe('Discworld');
  });
});
