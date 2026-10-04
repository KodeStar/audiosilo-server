import type { TFunction } from 'i18next';
import { SCAN_INTERVALS } from '@/api/types';

// A library's scan settings as the form edits them: the schedule as a choice plus
// a time (library.ParseSchedule's "" | "every:<N>h" | "daily:HH:MM"), the ignore
// rules as text, one pattern per line.

export type ScheduleChoice = '' | (typeof SCAN_INTERVALS)[number] | 'daily';

export const SCHEDULE_CHOICES: readonly ScheduleChoice[] = ['', ...SCAN_INTERVALS, 'daily'];

/** The default time offered for a daily scan. */
export const DEFAULT_DAILY_TIME = '03:00';

/** A stored schedule as the form's choice and time. */
export function splitSchedule(schedule: string): { choice: ScheduleChoice; time: string } {
  if (schedule.startsWith('daily:')) return { choice: 'daily', time: schedule.slice(6) };
  const choice = SCHEDULE_CHOICES.includes(schedule as ScheduleChoice)
    ? (schedule as ScheduleChoice)
    : '';
  return { choice, time: DEFAULT_DAILY_TIME };
}

/** The form's choice and time as the stored schedule. */
export function joinSchedule(choice: ScheduleChoice, time: string): string {
  return choice === 'daily' ? `daily:${time || DEFAULT_DAILY_TIME}` : choice;
}

/** A schedule in words: "Every 6 hours", "Daily at 03:00", "Off". */
export function scheduleLabel(schedule: string, t: TFunction): string {
  const { choice, time } = splitSchedule(schedule);
  if (choice === 'daily') return t('libraries.schedule.dailyAt', { time });
  if (!choice) return t('libraries.schedule.off');
  return t('libraries.schedule.every', { count: Number(choice.slice(6, -1)) });
}

/** Ignore rules as the textarea shows them. */
export function patternsToText(patterns: readonly string[]): string {
  return patterns.join('\n');
}

/** The textarea's text as ignore rules: trimmed lines, blanks dropped (as the server stores them). */
export function textToPatterns(text: string): string[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean);
}
