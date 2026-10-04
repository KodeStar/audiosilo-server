import { formatTook } from '@/lib/format';
import { eventPhrase, runSeconds, runSummary, statusTone } from './jobs-model';

const run = { status: 'ok' as const, added: 0, updated: 0, moved: 0, removed: 0, errors: 0 };

it('sums up what a scan found', () => {
  expect(runSummary(run)).toEqual([{ key: 'jobs.noChanges' }]);
  expect(runSummary({ ...run, added: 14, moved: 1 })).toEqual([
    { key: 'jobs.count.added', values: { count: 14 } },
    { key: 'jobs.count.moved', values: { count: 1 } },
  ]);
  // A partial scan says nothing was removed, after what it did find.
  expect(runSummary({ ...run, status: 'partial', added: 2 }).map((p) => p.key)).toEqual([
    'jobs.count.added',
    'jobs.status.partial',
  ]);
  expect(runSummary({ ...run, status: 'unavailable', added: 3 })).toEqual([
    { key: 'jobs.status.unavailable' },
  ]);
});

it('tones a status', () => {
  expect(statusTone('ok')).toBe('ok');
  expect(statusTone('running')).toBe('live');
  expect(statusTone('cancelled')).toBe('warn');
  expect(statusTone('unavailable')).toBe('bad');
});

it('measures and words how long a scan took', () => {
  expect(runSeconds({ started_at: '2026-10-04T08:00:00Z', finished_at: null })).toBeUndefined();
  expect(
    runSeconds({ started_at: '2026-10-04T08:00:00Z', finished_at: '2026-10-04T08:01:52Z' }),
  ).toBe(112);
  expect(formatTook(0.42, 'en')).toBe('0.4s');
  expect(formatTook(22.4, 'en')).toBe('22s');
  expect(formatTook(119.6, 'en')).toBe('2m 0s');
  expect(formatTook(3780, 'en')).toBe('1h 3m');
});

it('words a log line, naming a read problem by its code', () => {
  const p = eventPhrase(
    { at: 'x', level: 'warn', kind: 'problem', path: 'A/02.mp3', code: 'empty_file' },
    (c) => `word:${c}`,
  );
  expect(p.key).toBe('jobs.event.problem');
  expect(p.values).toMatchObject({ path: 'A/02.mp3', problem: 'word:empty_file' });
  expect(eventPhrase({ at: 'x', level: 'info', kind: 'brand-new' }, String).key).toBe(
    'jobs.event.other',
  );
});
