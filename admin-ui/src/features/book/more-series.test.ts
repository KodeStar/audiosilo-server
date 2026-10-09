import { describe, expect, it } from 'vitest';
import type { FieldValue, OverrideField } from '@/api/types';
import { bookDetail } from '@/test/library-fixtures';
import {
  checkField,
  commitDraft,
  commitField,
  diffRows,
  displayValue,
  saveRequest,
  seriesSwap,
} from './book-model';

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

// A book in Discworld at 8 that is also City Watch #1 and in an Omnibus.
const LIST = '[{"name":"Omnibus","position":0},{"name":"City Watch","position":1}]';
function swapFields(
  series = 'Discworld',
  index = '8',
  more = LIST,
): Record<OverrideField, FieldValue> {
  const f = bookDetail().fields;
  return {
    ...f,
    series: { ...f.series, value: series },
    series_index: { ...f.series_index, value: index },
    more_series: { ...f.more_series, value: more, source: more ? 'edited' : '' },
  };
}

describe('seriesSwap', () => {
  it('trades the main series for an other series, as the server does', () => {
    expect(seriesSwap(swapFields(), 'City Watch')).toEqual({
      more_series: '[{"name":"Omnibus","position":0},{"name":"Discworld","position":8}]',
      series_index: '1',
    });
    // No position there is none here; no main series before just leaves the list.
    expect(seriesSwap(swapFields(), 'Omnibus')?.series_index).toBe('');
    expect(seriesSwap(swapFields('', ''), 'City Watch')).toEqual({
      more_series: '[{"name":"Omnibus","position":0}]',
      series_index: '1',
    });
    // An entry already naming the old main series gives way to it.
    const shadowed = '[{"name":"Discworld","position":3},{"name":"City Watch","position":1}]';
    expect(seriesSwap(swapFields('Discworld', '8', shadowed), 'City Watch')?.more_series).toBe(
      '[{"name":"Discworld","position":8}]',
    );
  });

  it('swaps nothing for a series the list lacks, the same series, or another spelling', () => {
    expect(seriesSwap(swapFields(), 'Rincewind')).toBeUndefined();
    expect(seriesSwap(swapFields(), 'city watch')).toBeUndefined();
    expect(seriesSwap(swapFields(), '')).toBeUndefined();
    expect(seriesSwap(swapFields('City Watch', '1'), 'City Watch')).toBeUndefined();
  });
});

describe('committing the series', () => {
  const fields = swapFields();

  it('drafts the swap beside it, shown in the diff and sent in the save', () => {
    const d = commitField({}, 'series', 'City Watch', fields);
    expect(d).toEqual({
      series: 'City Watch',
      series_index: '1',
      more_series: 'Omnibus; Discworld #8',
    });
    expect(diffRows(d, fields).map((r) => [r.field, r.after])).toEqual([
      ['series', 'City Watch'],
      ['series_index', '1'],
      ['more_series', '[{"name":"Omnibus","position":0},{"name":"Discworld","position":8}]'],
    ]);
    expect(saveRequest(d).set).toEqual({
      series: 'City Watch',
      series_index: '1',
      more_series: '[{"name":"Omnibus","position":0},{"name":"Discworld","position":8}]',
    });
  });

  it('takes the swap back when the old name is committed again', () => {
    const d = commitField({}, 'series', 'City Watch', fields);
    expect(commitField(d, 'series', 'Discworld', fields)).toEqual({});
    // Or swaps to another of the saved list instead, from the saved values.
    expect(commitField(d, 'series', 'Omnibus', fields)).toEqual({
      series: 'Omnibus',
      series_index: '',
      more_series: 'Discworld #8; City Watch #1',
    });
    // A series the list lacks: just the series.
    expect(commitField(d, 'series', 'Rincewind', fields)).toEqual({ series: 'Rincewind' });
  });

  it("leaves the admin's own other series and position be", () => {
    const own = commitField({}, 'more_series', 'Omnibus; City Watch #1; Extra', fields);
    expect(commitField(own, 'series', 'City Watch', fields)).toEqual({
      more_series: 'Omnibus; City Watch #1; Extra',
      series: 'City Watch',
    });
    const typed = commitField({}, 'series_index', '9', fields);
    expect(commitField(typed, 'series', 'City Watch', fields)).toEqual({
      series_index: '9',
      series: 'City Watch',
      more_series: 'Omnibus; Discworld #8',
    });
    expect(
      commitField(
        commitField(typed, 'series', 'City Watch', fields),
        'series',
        'Discworld',
        fields,
      ),
    ).toEqual({ series_index: '9' });
    // A list the admin changed after a swap is theirs: going back keeps it.
    let d = commitField({}, 'series', 'City Watch', fields);
    d = commitField(d, 'more_series', 'Discworld #8', fields);
    expect(commitField(d, 'series', 'Discworld', fields)).toEqual({ more_series: 'Discworld #8' });
  });

  it('drafts the list itself when its line would read back differently', () => {
    const odd = swapFields('Tom; Jerry', '2');
    expect(commitField({}, 'series', 'City Watch', odd).more_series).toBe(
      '[{"name":"Omnibus","position":0},{"name":"Tom; Jerry","position":2}]',
    );
  });
});
