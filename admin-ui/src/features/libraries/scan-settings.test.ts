import i18n from '@/i18n';
import {
  joinSchedule,
  patternsToText,
  samePatterns,
  scheduleLabel,
  splitSchedule,
  textToPatterns,
} from './scan-settings';

it('round-trips schedules through the form', () => {
  expect(splitSchedule('')).toEqual({ choice: '', time: '03:00' });
  expect(splitSchedule('every:6h')).toEqual({ choice: 'every:6h', time: '03:00' });
  expect(splitSchedule('daily:04:30')).toEqual({ choice: 'daily', time: '04:30' });
  // An unknown stored value reads as off rather than breaking the form.
  expect(splitSchedule('weekly:mon').choice).toBe('');
  expect(joinSchedule('daily', '04:30')).toBe('daily:04:30');
  expect(joinSchedule('daily', '')).toBe('daily:03:00');
  expect(joinSchedule('every:12h', '04:30')).toBe('every:12h');
  expect(joinSchedule('', '04:30')).toBe('');
});

it('words schedules', () => {
  const t = i18n.t;
  expect(scheduleLabel('', t)).toBe('Off');
  expect(scheduleLabel('every:1h', t)).toBe('Every hour');
  expect(scheduleLabel('every:6h', t)).toBe('Every 6 hours');
  expect(scheduleLabel('daily:03:00', t)).toBe('Daily at 03:00');
});

it('turns the textarea into rules the way the server stores them', () => {
  expect(textToPatterns('  *.sample.mp3 \n\n# extras\nExtras/\n')).toEqual([
    '*.sample.mp3',
    '# extras',
    'Extras/',
  ]);
  expect(patternsToText(['a', 'b'])).toBe('a\nb');
  expect(samePatterns(['a', 'b'], textToPatterns('a\n b \n'))).toBe(true);
  expect(samePatterns(['a'], ['a', 'b'])).toBe(false);
});
