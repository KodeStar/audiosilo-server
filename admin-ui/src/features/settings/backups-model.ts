import type { Backup, BackupStatus } from '@/api/types';

export { backupFailureKey as failureKey } from '@/lib/server-events';

// Settings > Backups: the schedule string the server stores ("", "daily:HH:MM",
// "weekly:DAY:HH:MM"; backup.ParseSchedule) as the form edits it, and the small
// rules the topic shows (what each backup is, why one failed).

export const WEEKDAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'] as const;
export type Weekday = (typeof WEEKDAYS)[number];

export type Frequency = 'off' | 'daily' | 'weekly';

/** A schedule as the form edits it. */
export interface ScheduleParts {
  frequency: Frequency;
  day: Weekday;
  /** "HH:MM", 24-hour. */
  time: string;
}

/** What a schedule turns to when it is switched on from off. */
export const DEFAULT_PARTS: ScheduleParts = { frequency: 'daily', day: 'sun', time: '03:00' };

const TIME = /^([01]\d|2[0-3]):[0-5]\d$/;

/** Reads a stored schedule; anything unknown reads as off. */
export function parseSchedule(s: string): ScheduleParts {
  const parts = s.trim().split(':');
  if (parts[0] === 'daily' && parts.length === 3) {
    const time = `${parts[1]}:${parts[2]}`;
    if (TIME.test(time)) return { ...DEFAULT_PARTS, frequency: 'daily', time };
  }
  if (parts[0] === 'weekly' && parts.length === 4) {
    const time = `${parts[2]}:${parts[3]}`;
    const day = parts[1] as Weekday;
    if (WEEKDAYS.includes(day) && TIME.test(time)) return { frequency: 'weekly', day, time };
  }
  return { ...DEFAULT_PARTS, frequency: 'off' };
}

/** The stored form of a schedule ("" when off). A malformed time is sent as is, so the server names it. */
export function formatSchedule(p: ScheduleParts): string {
  switch (p.frequency) {
    case 'daily':
      return `daily:${p.time}`;
    case 'weekly':
      return `weekly:${p.day}:${p.time}`;
  }
  return '';
}

/** The i18n key and values of a schedule in words ("Every day at 03:00"). */
export function describeSchedule(s: string): { key: string; values?: Record<string, string> } {
  const p = parseSchedule(s);
  if (p.frequency === 'off') return { key: 'backups.schedule.off' };
  if (p.frequency === 'daily') return { key: 'backups.schedule.daily', values: { time: p.time } };
  return { key: `backups.schedule.weekly.${p.day}`, values: { time: p.time } };
}

/** The i18n key of why a restore wasn't applied (backup.RestoreResult's error). */
export function restoreFailureKey(error: string | undefined): string {
  switch (error) {
    case 'missing':
    case 'unusable':
    case 'newer':
      return `backups.restore.failure.${error}`;
  }
  return 'backups.restore.failure.failed';
}

/** What the backups' state says at a glance: fine, failing, or not set up to run. */
export type BackupHealth = 'ok' | 'failed' | 'off' | 'none';

export function backupHealth(st: BackupStatus): BackupHealth {
  if (st.last && !st.last.ok) return 'failed';
  if (!st.next) return 'off';
  return st.latest ? 'ok' : 'none';
}

/** Total bytes the listed backups take. */
export function totalSize(list: Backup[]): number {
  return list.reduce((n, b) => n + b.size, 0);
}
