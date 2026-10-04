import { describe, expect, it } from 'vitest';
import type { BackupStatus } from '@/api/types';
import {
  backupHealth,
  describeSchedule,
  formatSchedule,
  parseSchedule,
  restoreFailureKey,
  totalSize,
} from './backups-model';

describe('schedule', () => {
  it('reads and writes the server form', () => {
    for (const s of ['', 'daily:03:00', 'weekly:sun:23:59', 'weekly:mon:00:00']) {
      expect(formatSchedule(parseSchedule(s))).toBe(s);
    }
    expect(parseSchedule('weekly:fri:04:30')).toEqual({
      frequency: 'weekly',
      day: 'fri',
      time: '04:30',
    });
  });

  it('reads anything unknown as off, keeping the defaults for switching on', () => {
    for (const s of ['every:6h', 'daily:3:00', 'daily:24:00', 'weekly:Sun:03:00', 'nonsense']) {
      expect(parseSchedule(s)).toEqual({ frequency: 'off', day: 'sun', time: '03:00' });
    }
  });

  it('keeps the day and time while the frequency changes', () => {
    const p = parseSchedule('weekly:tue:05:15');
    expect(formatSchedule({ ...p, frequency: 'daily' })).toBe('daily:05:15');
    expect(formatSchedule({ ...p, frequency: 'off' })).toBe('');
  });

  it('describes a schedule in words', () => {
    expect(describeSchedule('')).toEqual({ key: 'backups.schedule.off' });
    expect(describeSchedule('daily:03:00')).toEqual({
      key: 'backups.schedule.daily',
      values: { time: '03:00' },
    });
    expect(describeSchedule('weekly:sat:01:00')).toEqual({
      key: 'backups.schedule.weekly.sat',
      values: { time: '01:00' },
    });
  });
});

describe('state', () => {
  const st = (over: Partial<BackupStatus>): BackupStatus => ({
    dir: '/data/backups',
    running: false,
    last: null,
    latest: null,
    next: '2026-10-05T03:00:00Z',
    ...over,
  });
  const backup = {
    name: 'a',
    size: 10,
    created_at: '2026-10-04T03:00:00Z',
    kind: 'scheduled' as const,
  };

  it('says whether backups are healthy', () => {
    expect(backupHealth(st({ latest: backup }))).toBe('ok');
    expect(backupHealth(st({}))).toBe('none');
    expect(backupHealth(st({ next: null, latest: backup }))).toBe('off');
    expect(
      backupHealth(
        st({
          latest: backup,
          last: { at: 'x', ok: false, trigger: 'scheduled', error: 'disk_full' },
        }),
      ),
    ).toBe('failed');
  });

  it('words failures, unknown ones generically', () => {
    expect(restoreFailureKey('newer')).toBe('backups.restore.failure.newer');
    expect(restoreFailureKey(undefined)).toBe('backups.restore.failure.failed');
  });

  it('adds up sizes', () => {
    expect(totalSize([backup, { ...backup, size: 5 }])).toBe(15);
  });
});
