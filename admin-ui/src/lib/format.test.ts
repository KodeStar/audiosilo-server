import {
  counted,
  formatBytes,
  formatClock,
  formatDate,
  formatDateTime,
  formatDay,
  formatDuration,
  formatHours,
  formatLongDate,
  formatNumber,
  formatPercent,
  formatRelative,
  formatVersion,
  progressFraction,
  seriesIndexLabel,
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

  it('formats a length to the minute, seconds under a minute', () => {
    expect(formatDuration(163800, 'en')).toBe('45h 30m');
    expect(formatDuration(7200, 'en')).toBe('2h');
    expect(formatDuration(2280, 'en')).toBe('38m');
    expect(formatDuration(4530, 'en')).toBe('1h 16m');
    expect(formatDuration(40, 'en')).toBe('40s');
    expect(formatDuration(0, 'en')).toBe('');
    expect(formatDuration(Number.NaN, 'en')).toBe('');
  });

  it('formats sizes in decimal units, one decimal under 10', () => {
    expect(formatBytes(1_310_000_000, 'en')).toBe('1.3 GB');
    expect(formatBytes(48_000_000, 'en')).toBe('48 MB');
    expect(formatBytes(245_000_000, 'en')).toBe('245 MB');
    expect(formatBytes(512, 'en')).toBe('512 byte');
  });

  it('formats a clock position', () => {
    expect(formatClock(0)).toBe('0:00');
    expect(formatClock(125)).toBe('2:05');
    expect(formatClock(3725)).toBe('1:02:05');
    expect(formatClock(1200, true)).toBe('0:20:00');
  });

  it('counts for plural keys and labels series positions', () => {
    expect(counted(3249, 'en')).toEqual({ count: 3249, formatted: '3,249' });
    const t = (key: string, opts: Record<string, unknown>) => `${key}:${String(opts.index)}`;
    expect(seriesIndexLabel(2, 'en', t)).toBe('books.tile.seriesIndex:2');
    expect(seriesIndexLabel(0, 'en', t)).toBe('');
  });

  it('formats listening in hours, one decimal under 10', () => {
    expect(formatHours(0, 'en')).toBe('0h');
    expect(formatHours(8640, 'en')).toBe('2.4h');
    // Under an hour, minutes: a short period isn't all "0h".
    expect(formatHours(1440, 'en')).toBe('24m');
    expect(formatHours(30, 'en')).toBe('30s');
    expect(formatHours(460_800, 'en')).toBe('128h');
  });

  it('reads a server day as a calendar date in any zone', () => {
    expect(formatDay('2026-10-01', 'en', { month: 'short', day: 'numeric' })).toBe('Oct 1');
    expect(formatDay('nonsense', 'en', { month: 'short' })).toBe('');
    expect(formatDate('2026-10-04T10:00:00Z', 'en')).toBe('Oct 4, 2026');
    expect(formatDate(null, 'en')).toBe('');
  });

  it('prefixes numeric versions only', () => {
    expect(formatVersion('0.9.2')).toBe('v0.9.2');
    expect(formatVersion('dev')).toBe('dev');
  });
});
