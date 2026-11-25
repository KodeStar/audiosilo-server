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
      const d = describeEvent(
        ev(kind, { library: 'Fiction', user: 'sam', version: 'v2.0.0', person: 'Bob' }),
      );
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

  it("keeps the offline scan's details and reads older events without them", () => {
    const detail = 'The server runs as user 1000:1000 and the folder belongs to 1026:100.';
    expect(describeEvent(ev('library_unavailable', { detail })).body?.values?.detail).toBe(
      ' ' + detail,
    );
    expect(describeEvent(ev('library_unavailable', {})).body?.values?.detail).toBe('');
    expect(
      describeEvent(ev('library_unavailable', { detail: "the server can't read /books" })).body
        ?.values?.detail,
    ).toBe(" The server can't read /books");
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

  it('words a finished chapter detection job as the chapters', () => {
    expect(
      describeEvent(
        ev('bridge_job_finished', { ...job, job_kind: 'chapters.detect', state: 'done' }),
      ).title,
    ).toEqual({ key: 'events.bridge_job_finished.detect', values: { name: 'The Way of Kings' } });
  });

  it('says when the chapters a detection found are not the ones the book uses', () => {
    const done = (chapters_source?: string) =>
      describeEvent(
        ev('bridge_job_finished', {
          ...job,
          job_kind: 'chapters.detect',
          state: 'done',
          ...(chapters_source ? { chapters_source } : {}),
        }),
      ).body;
    expect(done('files')).toEqual({ key: 'events.bridge_job_finished.detectUnused' });
    expect(done('community')).toEqual({ key: 'events.bridge_job_finished.detectUnused' });
    // An edit saved while the job ran: reverting it brings them in, no source choice does.
    expect(done('edited')).toEqual({ key: 'events.bridge_job_finished.detectUnusedEdited' });
    expect(done('generated')).toBeUndefined();
    // A server that names no source says nothing more.
    expect(done()).toBeUndefined();
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
    // A LibriVox fetch's park codes have words of their own; another kind's
    // job with the same code reads as any unknown park.
    const parked = { ...job, state: 'needs_attention', code: 'not_writable' };
    expect(
      describeEvent(ev('bridge_job_failed', { ...parked, job_kind: 'librivox.fetch' })).body,
    ).toEqual({ key: 'librivox.code.not_writable', values: { code: 'not_writable' } });
    expect(describeEvent(ev('bridge_job_failed', parked)).body).toEqual({
      key: 'bridge.park.unknown',
      values: { code: 'not_writable' },
    });
  });

  it("says when a LibriVox book isn't matched to its recording", () => {
    const done = { job_kind: 'librivox.fetch', state: 'done', title: 'Garden', library: 'Kids' };
    const plain = describeEvent(ev('bridge_job_finished', done));
    expect(plain.title).toEqual({
      key: 'events.bridge_job_finished.librivox',
      values: { name: 'Garden' },
    });
    expect(plain.body).toBeUndefined();
    expect(
      describeEvent(ev('bridge_job_finished', { ...done, librivox_match: 'matched' })).body,
    ).toBeUndefined();
    const unmatched = describeEvent(
      ev('bridge_job_finished', { ...done, librivox_match: 'unmatched' }),
    );
    expect(unmatched.body).toEqual({ key: 'events.bridge_job_finished.librivoxUnmatched' });
    expect(unmatched.tone).toBe('warn');
    const kept = describeEvent(ev('bridge_job_finished', { ...done, librivox_match: 'kept' }));
    expect(kept.body).toEqual({ key: 'events.bridge_job_finished.librivoxKept' });
    expect(kept.tone).toBe('info');
    // Another kind's event never reads it.
    expect(
      describeEvent(
        ev('bridge_job_finished', {
          ...done,
          job_kind: 'chapters.detect',
          librivox_match: 'unmatched',
        }),
      ).body,
    ).toBeUndefined();
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

  it('words a release in a series the library holds', () => {
    const d = describeEvent(
      ev('book_released', {
        count: 4,
        titles: ["Tiamat's Wrath (The Expanse #8)", 'Artemis (Mars #2)', 'A', 'B'],
      }),
    );
    expect(d.title).toEqual({ key: 'events.book_released.title', values: { count: 4 } });
    expect(d.body).toEqual({
      key: 'events.book_released.bodyMore',
      values: { titles: "Tiamat's Wrath (The Expanse #8), Artemis (Mars #2), A", count: 1 },
    });
    expect(d.link).toEqual({ destination: 'library', section: 'series' });
    const one = describeEvent(ev('book_released', { count: 1, titles: ['Next (Mars #3)'] }));
    expect(one.body).toEqual({
      key: 'events.book_released.body',
      values: { titles: 'Next (Mars #3)' },
    });
    expect(describeEvent(ev('book_released', { count: 2 })).body).toBeUndefined();
  });

  it('lists the kinds in the server order', () => {
    expect(EVENT_KINDS.slice(-7)).toEqual([
      'bridge_job_finished',
      'bridge_job_failed',
      'book_released',
      'podcast_episodes_new',
      'podcast_feed_failing',
      'podcast_feed_recovered',
      'oidc_pending',
    ]);
  });

  it('words a waiting single sign-on by the person, or someone, and its reason', () => {
    const d = describeEvent(
      ev('oidc_pending', { provider: 'Keycloak', person: 'Bob', reason: 'admin_account' }),
    );
    expect(d.title).toEqual({
      key: 'events.oidc_pending.title',
      values: { person: 'Bob', provider: 'Keycloak' },
    });
    expect(d.body).toEqual({ key: 'people.pending.reason.admin_account' });
    expect(d.tone).toBe('warn');
    expect(d.link).toEqual({ destination: 'people' });
    const someone = describeEvent(ev('oidc_pending', { provider: 'Keycloak' }));
    expect(someone.title).toEqual({
      key: 'events.oidc_pending.titleSomeone',
      values: { provider: 'Keycloak' },
    });
    expect(someone.body).toEqual({ key: 'people.pending.reason.unknown' });
  });

  it("words a show's new episodes by its name", () => {
    const d = describeEvent(
      ev('podcast_episodes_new', {
        library: 'Pods',
        show: 'Example Show',
        count: 5,
        titles: ['Five', 'Four', 'Three', 'Two', 'One'],
      }),
    );
    expect(d.title).toEqual({
      key: 'events.podcast_episodes_new.title',
      values: { count: 5, show: 'Example Show' },
    });
    expect(d.body).toEqual({
      key: 'events.podcast_episodes_new.bodyMore',
      values: { titles: 'Five, Four, Three', count: 2 },
    });
    expect(d.tone).toBe('info');
    expect(d.link).toEqual({ destination: 'library', section: 'libraries' });
  });

  it("words a failing feed's code, never more", () => {
    const failing = (code: string) =>
      describeEvent(ev('podcast_feed_failing', { show: 'Example Show', code, failures: 3 }));
    const d = failing('timeout');
    expect(d.title).toEqual({
      key: 'events.podcast_feed_failing.title',
      values: { show: 'Example Show' },
    });
    expect(d.body).toEqual({ key: 'events.podcast_feed_failing.code.timeout' });
    expect(d.tone).toBe('warn');
    expect(d.link).toEqual({ destination: 'library', section: 'libraries' });
    expect(failing('gone').body).toEqual({ key: 'events.podcast_feed_failing.code.gone' });
    expect(failing('feed_secret_missing').body).toEqual({
      key: 'events.podcast_feed_failing.code.feed_secret_missing',
    });
    expect(failing('http_503').body).toEqual({
      key: 'events.podcast_feed_failing.http',
      values: { status: '503' },
    });
    // server#403: the Bridge's redirect loop is worded as one, not "HTTP 310".
    expect(failing('http_310').body).toEqual({ key: 'events.podcast_feed_failing.code.http_310' });
    // ...and under its own code from a newer Bridge.
    expect(failing('redirect_loop').body).toEqual({
      key: 'events.podcast_feed_failing.code.redirect_loop',
    });
    expect(failing('failed').body).toEqual({ key: 'events.podcast_feed_failing.failed' });
    expect(failing('http_5000').body).toEqual({ key: 'events.podcast_feed_failing.failed' });
    expect(failing('http_999').body).toEqual({ key: 'events.podcast_feed_failing.failed' });
  });

  it('words a feed that is back', () => {
    const d = describeEvent(ev('podcast_feed_recovered', { show: 'Example Show' }));
    expect(d.title).toEqual({
      key: 'events.podcast_feed_recovered.title',
      values: { show: 'Example Show' },
    });
    expect(d.body).toEqual({ key: 'events.podcast_feed_recovered.body' });
    expect(d.tone).toBe('info');
  });

  it('never names a private show', () => {
    const news = describeEvent(ev('podcast_episodes_new', { private: true, count: 2, titles: [] }));
    expect(news.title).toEqual({
      key: 'events.podcast_episodes_new.privateTitle',
      values: { count: 2 },
    });
    expect(news.body).toBeUndefined();
    expect(
      describeEvent(ev('podcast_feed_failing', { private: true, code: 'gone', failures: 1 })).title,
    ).toEqual({ key: 'events.podcast_feed_failing.privateTitle' });
    expect(describeEvent(ev('podcast_feed_recovered', { private: true })).title).toEqual({
      key: 'events.podcast_feed_recovered.privateTitle',
    });
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
