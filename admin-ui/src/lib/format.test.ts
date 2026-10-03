import {
  formatDateTime,
  formatLongDate,
  formatNumber,
  formatPercent,
  formatRelative,
  formatVersion,
  progressFraction,
} from './format';

describe('format', () => {
  it('groups thousands per locale', () => {
    expect(formatNumber(3249, 'en')).toBe('3,249');
    expect(formatNumber(3249, 'de')).toBe('3.249');
  });

  it('clamps percentages', () => {
    expect(formatPercent(0.62, 'en')).toBe('62%');
    expect(formatPercent(1.4, 'en')).toBe('100%');
    expect(formatPercent(Number.NaN, 'en')).toBe('0%');
  });

  it('formats recency relative to now', () => {
    const now = Date.parse('2026-10-03T12:00:00Z');
    expect(formatRelative('2026-10-03T11:46:00Z', 'en', now)).toBe('14 min. ago');
    expect(formatRelative('2026-10-03T11:59:40Z', 'en', now)).toBe('now');
    expect(formatRelative('2026-10-02T12:00:00Z', 'en', now)).toBe('yesterday');
    expect(formatRelative('not a date', 'en', now)).toBe('');
  });

  it('formats a record time absolutely', () => {
    expect(formatDateTime(new Date(2026, 9, 10, 19, 52).toISOString(), 'en')).toBe(
      'Oct 10, 7:52 PM',
    );
    expect(formatDateTime('nope', 'en')).toBe('');
  });

  it('formats the long date', () => {
    expect(formatLongDate(new Date(2026, 9, 3), 'en')).toBe('Saturday, October 3');
  });

  it('computes progress safely', () => {
    expect(progressFraction(30, 60)).toBe(0.5);
    expect(progressFraction(30, 0)).toBe(0);
    expect(progressFraction(90, 60)).toBe(1);
  });

  it('prefixes numeric versions only', () => {
    expect(formatVersion('0.9.2')).toBe('v0.9.2');
    expect(formatVersion('dev')).toBe('dev');
  });
});
