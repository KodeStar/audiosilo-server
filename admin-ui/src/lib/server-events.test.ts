import { beforeEach, describe, expect, it } from 'vitest';
import type { ServerEvent } from '@/api/types';
import { describeEvent, EVENT_KINDS, readSeen, unreadCount, writeSeen } from './server-events';

const ev = (kind: ServerEvent['kind'], data: Record<string, unknown>, id = 1): ServerEvent => ({
  id,
  at: '2026-10-04T09:00:00.000Z',
  kind,
  data,
});

describe('describeEvent', () => {
  it('lists a few added titles, then how many more', () => {
    const d = describeEvent(
      ev('book_added', {
        library: 'Fiction',
        count: 7,
        titles: ['Dune', 'Emma', 'Ulysses', 'Kim'],
      }),
    );
    expect(d.title).toEqual({
      key: 'events.book_added.title',
      values: { count: 7, library: 'Fiction' },
    });
    expect(d.body).toEqual({
      key: 'events.book_added.bodyMore',
      values: { titles: 'Dune, Emma, Ulysses', count: 4 },
    });
    expect(d.link).toEqual({ destination: 'library' });
    const one = describeEvent(ev('book_added', { library: 'Kids', count: 1, titles: ['Matilda'] }));
    expect(one.body).toEqual({ key: 'events.book_added.body', values: { titles: 'Matilda' } });
  });

  it('words every kind, with a tone and a place to go', () => {
    for (const kind of EVENT_KINDS) {
      const d = describeEvent(ev(kind, { library: 'Fiction', user: 'sam', version: 'v2.0.0' }));
      expect(d.title.key).toBe(`events.${kind}.title`);
      expect(d.link.destination).toBeTruthy();
    }
    expect(describeEvent(ev('scan_failed', {})).tone).toBe('bad');
    expect(describeEvent(ev('library_unavailable', {})).tone).toBe('warn');
    expect(describeEvent(ev('backup_failed', { error: 'disk_full' })).body?.key).toBe(
      'backups.failure.disk_full',
    );
    expect(describeEvent(ev('backup_failed', { error: 'odd' })).body?.key).toBe(
      'backups.failure.failed',
    );
  });

  it('joins a device and its app, and survives missing or odd data', () => {
    expect(
      describeEvent(ev('new_device', { user: 'sam', device: "Sam's iPhone", app: 'AudioSilo 1.4' }))
        .body?.values,
    ).toEqual({ detail: "Sam's iPhone · AudioSilo 1.4" });
    expect(describeEvent(ev('new_device', { user: 7, device: null })).body).toBeUndefined();
    const unknown = describeEvent(ev('something_new' as ServerEvent['kind'], {}));
    expect(unknown.title).toEqual({ key: 'events.unknown', values: { kind: 'something_new' } });
  });
});

describe('Bridge job events', () => {
  const job = {
    job_id: 31,
    job_kind: 'sidecars.generate',
    library: 'Fiction',
    library_id: 1,
    path: 'Brandon Sanderson/The Way of Kings',
    title: 'The Way of Kings',
  };
  const link = { destination: 'server', topic: 'bridge' };

  it('says characters and recaps are ready for the book', () => {
    expect(describeEvent(ev('bridge_job_finished', { ...job, state: 'done' }))).toEqual({
      title: {
        key: 'events.bridge_job_finished.generate',
        values: { name: 'The Way of Kings' },
      },
      tone: 'info',
      link,
    });
  });

  it('words a finished description job as the description', () => {
    expect(
      describeEvent(
        ev('bridge_job_finished', { ...job, job_kind: 'sidecars.describe', state: 'done' }),
      ).title,
    ).toEqual({ key: 'events.bridge_job_finished.describe', values: { name: 'The Way of Kings' } });
  });

  it('words another kind generically, naming the kind', () => {
    const d = describeEvent(
      ev('bridge_job_finished', { ...job, job_kind: 'bridge.verify', title: '', state: 'done' }),
    );
    expect(d.title).toEqual({
      key: 'events.bridge_job_finished.title',
      values: { name: 'Brandon Sanderson/The Way of Kings' },
    });
    expect(d.body).toEqual({
      key: 'bridge.kind.bridge.verify',
      values: { defaultValue: 'bridge.verify' },
    });
    // A library-wide job names the library.
    expect(
      describeEvent(ev('bridge_job_finished', { job_kind: 'bridge.verify', library: 'Kids' })).title
        .values,
    ).toEqual({ name: 'Kids' });
  });

  it('words a parked job by its park code, as a warning', () => {
    const d = describeEvent(
      ev('bridge_job_failed', {
        ...job,
        state: 'needs_attention',
        code: 'budget_exceeded',
        detail: 'book budget reached',
      }),
    );
    expect(d.title).toEqual({
      key: 'events.bridge_job_failed.attention',
      values: { name: 'The Way of Kings' },
    });
    expect(d.body).toEqual({
      key: 'bridge.park.budget_exceeded',
      values: { code: 'budget_exceeded' },
    });
    expect(d.tone).toBe('warn');
    expect(d.link).toEqual(link);
    // An unknown code still reads, naming the code; no code falls back to the detail.
    expect(
      describeEvent(ev('bridge_job_failed', { ...job, state: 'needs_attention', code: 'odd' }))
        .body,
    ).toEqual({ key: 'bridge.park.unknown', values: { code: 'odd' } });
    expect(
      describeEvent(ev('bridge_job_failed', { ...job, state: 'needs_attention', detail: 'why' }))
        .body,
    ).toEqual({ key: 'events.detail', values: { detail: 'why' } });
  });

  it('words a failed job with its detail, as bad', () => {
    const d = describeEvent(
      ev('bridge_job_failed', { ...job, state: 'failed', code: '', detail: 'ffprobe: exit 1' }),
    );
    expect(d.title).toEqual({
      key: 'events.bridge_job_failed.title',
      values: { name: 'The Way of Kings' },
    });
    expect(d.body).toEqual({ key: 'events.detail', values: { detail: 'ffprobe: exit 1' } });
    expect(d.tone).toBe('bad');
  });

  it('lists both kinds, in the server order', () => {
    expect(EVENT_KINDS.slice(-2)).toEqual(['bridge_job_finished', 'bridge_job_failed']);
  });
});

describe('seen cursor', () => {
  beforeEach(() => localStorage.clear());
  it('counts what came after the last look', () => {
    expect(readSeen()).toBe(0);
    const list = [ev('new_device', {}, 5), ev('new_device', {}, 4), ev('new_device', {}, 3)];
    expect(unreadCount(list, 0)).toBe(3);
    writeSeen(4);
    expect(unreadCount(list, readSeen())).toBe(1);
  });
});
