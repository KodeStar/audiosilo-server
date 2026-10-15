import { describe, expect, it } from 'vitest';
import type { BridgeJobState } from '@/api/types';
import {
  BRIDGE_JOB_STATES,
  codeExchanged,
  holdParams,
  isOpen,
  jobActions,
  JOBS_POLL_IDLE_MS,
  JOBS_POLL_OPEN_MS,
  jobsPollMs,
  kindName,
  mappingState,
  presence,
  progressOf,
  resultSummary,
  safeHttpUrl,
  secondsLeft,
  stateLook,
  STATUS_POLL_MS,
  STATUS_POLL_PAIRING_MS,
  statusPollMs,
} from './bridge-model';

const job = (state: BridgeJobState, cancel_requested = false) => ({ state, cancel_requested });

describe('job states', () => {
  it('knows which states are still open (bridge.IsOpen)', () => {
    expect(BRIDGE_JOB_STATES.filter(isOpen)).toEqual([
      'queued',
      'claimed',
      'running',
      'needs_attention',
    ]);
  });

  it('offers Cancel on an open job, not once it ended or while a cancel is waiting', () => {
    expect(jobActions(job('queued'))).toEqual({ cancel: true, cancelling: false });
    expect(jobActions(job('needs_attention'))).toEqual({ cancel: true, cancelling: false });
    expect(jobActions(job('running'))).toEqual({ cancel: true, cancelling: false });
    expect(jobActions(job('running', true))).toEqual({ cancel: false, cancelling: true });
    expect(jobActions(job('claimed', true))).toEqual({ cancel: false, cancelling: true });
    for (const s of ['done', 'failed', 'cancelled'] as const)
      expect(jobActions(job(s))).toEqual({ cancel: false, cancelling: false });
    // A finished job that had a cancel asked for is just finished.
    expect(jobActions(job('cancelled', true))).toEqual({ cancel: false, cancelling: false });
  });

  it('labels each state, "cancelling" over a running one', () => {
    expect(stateLook(job('done'))).toEqual({ key: 'bridge.state.done', tone: 'success' });
    expect(stateLook(job('failed'))).toEqual({ key: 'bridge.state.failed', tone: 'destructive' });
    expect(stateLook(job('needs_attention')).tone).toBe('warning');
    expect(stateLook(job('running', true))).toEqual({
      key: 'bridge.state.cancelling',
      tone: 'warning',
    });
  });

  it('names known kinds and leaves the rest as their code', () => {
    const t = (key: string, opts?: Record<string, unknown>) =>
      key === 'bridge.kind.bridge.verify' ? 'Verify' : String(opts?.defaultValue);
    expect(kindName(t, 'bridge.verify')).toBe('Verify');
    expect(kindName(t, 'podcast.fetch')).toBe('podcast.fetch');
  });
});

describe('polling', () => {
  it('asks for jobs every few seconds while any is open', () => {
    expect(jobsPollMs([job('done'), job('running')])).toBe(JOBS_POLL_OPEN_MS);
    expect(jobsPollMs([job('done'), job('failed')])).toBe(JOBS_POLL_IDLE_MS);
    expect(jobsPollMs([])).toBe(JOBS_POLL_IDLE_MS);
  });

  it('asks for the status every few seconds while a pairing code waits', () => {
    expect(statusPollMs({ pairing: { expires_at: '2026-10-15T12:00:00Z' } })).toBe(
      STATUS_POLL_PAIRING_MS,
    );
    expect(statusPollMs({ pairing: null })).toBe(STATUS_POLL_MS);
    expect(statusPollMs(undefined)).toBe(STATUS_POLL_MS);
  });
});

describe('pairing', () => {
  it('tells when a Bridge took the code: paired at a new moment', () => {
    expect(codeExchanged(undefined, { paired: true, paired_at: '2026-10-15T12:00:00Z' })).toBe(
      true,
    );
    expect(codeExchanged(undefined, { paired: false })).toBe(false);
    // Making a code while a Bridge is paired: the old pairing is not the new one.
    const before = '2026-10-01T09:00:00Z';
    expect(codeExchanged(before, { paired: true, paired_at: before })).toBe(false);
    expect(codeExchanged(before, { paired: true, paired_at: '2026-10-15T12:00:00Z' })).toBe(true);
  });

  it('counts the code down to zero', () => {
    const now = Date.parse('2026-10-15T12:00:00Z');
    expect(secondsLeft('2026-10-15T12:15:00Z', now)).toBe(900);
    expect(secondsLeft('2026-10-15T11:59:00Z', now)).toBe(0);
    expect(secondsLeft('nonsense', now)).toBe(0);
  });
});

describe('status', () => {
  it('reads online, offline (heard before) or waiting (never heard)', () => {
    expect(presence({ online: true, heartbeat_at: '2026-10-15T12:00:00Z' })).toBe('online');
    expect(presence({ online: false, heartbeat_at: '2026-10-15T12:00:00Z' })).toBe('offline');
    expect(presence({ online: false })).toBe('waiting');
  });

  it('sums up a mapping', () => {
    const m = {
      library_id: 1,
      mount_path: '/libraries/fiction',
      mapped: true,
      writable: true,
      checked_path: 'A/B',
      error: '',
    };
    expect(mappingState(m)).toBe('ready');
    expect(mappingState({ ...m, writable: false })).toBe('readOnly');
    expect(mappingState({ ...m, mapped: false, error: 'size differs' })).toBe('notMapped');
    expect(mappingState(null)).toBe('unknown');
  });

  it('links only to an http(s) page', () => {
    expect(safeHttpUrl('http://192.168.1.20:8090/')).toBe('http://192.168.1.20:8090/');
    expect(safeHttpUrl('https://bridge.lan')).toBe('https://bridge.lan/');
    expect(safeHttpUrl('javascript:alert(1)')).toBeUndefined();
    expect(safeHttpUrl('data:text/html,hi')).toBeUndefined();
    expect(safeHttpUrl('/relative')).toBeUndefined();
    expect(safeHttpUrl(undefined)).toBeUndefined();
  });
});

describe('progress and results', () => {
  it('reads progress leniently', () => {
    expect(progressOf({ step: 'stat', message: 'Checking 3 files', done: 1, total: 3 })).toEqual({
      text: 'Checking 3 files',
      done: 1,
      total: 3,
    });
    expect(progressOf({ step: 'probe' })).toEqual({ text: 'probe' });
    expect(progressOf({ done: 2 })).toEqual({ text: undefined });
    expect(progressOf({ done: '2', total: 3 })).toEqual({ text: undefined });
    expect(progressOf({})).toEqual({});
    expect(progressOf(null)).toEqual({});
  });

  it("sums up bridge.verify's result, and nothing for an unknown shape", () => {
    expect(
      resultSummary({
        library_id: 1,
        mount_path: '/libraries/fiction',
        path: 'A/B',
        files: [
          { path: 'A/B/1.mp3', size: 10, expected_size: 10, ok: true },
          { path: 'A/B/2.mp3', size: 9, expected_size: 10, ok: false },
        ],
        writable: true,
        elapsed_ms: 1250,
      }),
    ).toEqual({
      files: { checked: 2, ok: 1 },
      writable: true,
      elapsedMs: 1250,
      mountPath: '/libraries/fiction',
    });
    expect(resultSummary({ episodes: 4 })).toBeNull();
    expect(resultSummary({})).toBeNull();
    expect(resultSummary(null)).toBeNull();
  });

  it('turns the hold field into params', () => {
    expect(holdParams('')).toBeUndefined();
    expect(holdParams('0')).toBeUndefined();
    expect(holdParams(' 60 ')).toEqual({ hold_seconds: 60 });
    expect(holdParams('600')).toEqual({ hold_seconds: 600 });
    expect(holdParams('601')).toBeNull();
    expect(holdParams('-1')).toBeNull();
    expect(holdParams('1.5')).toBeNull();
    expect(holdParams('soon')).toBeNull();
  });
});
