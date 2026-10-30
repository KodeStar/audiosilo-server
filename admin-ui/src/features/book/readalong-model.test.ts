import { describe, expect, it } from 'vitest';
import type { AdminBook, BridgeJob, BridgeJobState, ServerEvent, ServerInfo } from '@/api/types';
import i18n from '@/i18n';
import en from '@/i18n/locales/en.json';
import {
  blocksGenerate,
  coverageDown,
  generateBlocker,
  generatePollMs,
  generateResult,
  isBookKind,
  isFileKind,
  KNOWN_STAGES,
  readalongFailure,
} from '@/features/bridge/generate-model';
import { describeEvent } from '@/lib/server-events';
import { serverInfo } from '@/test/fixtures';
import {
  blocksReadalong,
  readalongBridge,
  readalongJobShown,
  readalongBlocker,
  readalongCardShown,
  readalongDialogView,
  readalongJobsKnown,
  readalongView,
} from './readalong-model';

// The Read-along card's rules (Bridge Phase 3c): when it is offered, what a
// map's state reads as, what holds it back, and how the kind is worded.

const ebook = { path: 'A/B/Book.epub', size: 1000 };

const job = (over: Partial<BridgeJob>) =>
  ({
    id: 1,
    kind: 'readalong.align',
    state: 'queued',
    cancel_requested: false,
    ...over,
  }) as BridgeJob;

const withBridge = (features: string[], online = true): ServerInfo => ({
  ...serverInfo,
  capabilities: { ...serverInfo.capabilities, bridge: online, readalong: true },
  bridge: online ? { version: '0.4.0', features } : undefined,
});

describe('readalongBridge', () => {
  it('tells a Bridge that makes read-alongs from a too old one, an offline one and none', () => {
    expect(readalongBridge(withBridge(['bridge', 'readalong']), undefined)).toBe('ready');
    // Online without the readalong family: a release before it, not offline.
    expect(readalongBridge(withBridge(['bridge', 'sidecars']), undefined)).toBe('too_old');
    expect(readalongBridge(withBridge([], false), true)).toBe('offline');
    expect(readalongBridge(withBridge([], false), false)).toBe('none');
    // Before the Bridge status answers (or when it failed): neither paired nor not.
    expect(readalongBridge(withBridge([], false), undefined)).toBe('unknown');
    expect(readalongBridge(undefined, undefined)).toBe('unknown');
  });
});

describe('readalongJobShown (S265-CARD)', () => {
  const done = (over: Partial<BridgeJob> = {}) =>
    job({ state: 'done', result: { coverage: { text: 0.97 } }, ...over });

  it('leaves out a done job over the ready map it made: the map line says it', () => {
    expect(readalongJobShown('ready', done())).toBe(false);
    // Unless it says more: the transcript was reused.
    expect(readalongJobShown('ready', done({ result: { transcribed: false } }))).toBe(true);
  });

  it('never shows a done job over a stale map', () => {
    expect(readalongJobShown('stale', done())).toBe(false);
  });

  it('shows an open, parked or failed job, and a cancel the server gave a reason for', () => {
    for (const state of ['queued', 'running', 'needs_attention', 'failed'] as BridgeJobState[]) {
      expect(readalongJobShown('ready', job({ state }))).toBe(true);
    }
    expect(readalongJobShown('none', job({ state: 'cancelled', cancel_requested: true }))).toBe(
      false,
    );
    expect(
      readalongJobShown(
        'none',
        job({
          state: 'cancelled',
          error: 'a generate job for the book replaced it; make the read-along again after',
        }),
      ),
    ).toBe(true);
    expect(readalongJobShown('none', undefined)).toBe(false);
  });
});

describe('readalongFailure (S265-CARD)', () => {
  it("turns the Bridge's no-match refusal into a sentence that says what to do", () => {
    expect(readalongFailure("the ebook and the narration don't match: 0% lined up")).toEqual({
      key: 'book.readalong.failed.noMatch',
      percent: 0,
    });
    expect(readalongFailure("the ebook and the narration don't match: 31% lined up")).toEqual({
      key: 'book.readalong.failed.noMatch',
      percent: 0.31,
    });
    expect(en).toHaveProperty(['book.readalong.failed.noMatch']);
  });

  it('starts any other refusal with a capital', () => {
    expect(readalongFailure('the ebook changed while aligning; run it again')).toEqual({
      text: 'The ebook changed while aligning; run it again',
    });
    expect(readalongFailure('  ')).toBeUndefined();
  });
});

describe('the read-along gate', () => {
  it("follows a book's read-along jobs on a server that knows the Bridge, for a book with an ebook", () => {
    expect(readalongJobsKnown(withBridge([], false), { ebook })).toBe(true);
    expect(readalongJobsKnown(withBridge(['readalong']), {})).toBe(false);
    const noBridge = {
      ...serverInfo,
      capabilities: { ...serverInfo.capabilities, bridge: undefined },
    };
    expect(readalongJobsKnown(noBridge, { ebook })).toBe(false);
  });
});

describe('readalongView', () => {
  const book = (over: Partial<AdminBook> = {}) => ({ ebook, ...over });

  it('reads a ready map with its coverage rounded down and when it was made, offering Make again', () => {
    const v = readalongView(
      book({
        readalong: { state: 'ready', coverage: 0.996, generated_at: '2026-11-02T10:11:12Z' },
      }),
      undefined,
      [],
      'ready',
    );
    expect(v).toEqual({
      map: 'ready',
      coverage: 0.99,
      generatedAt: '2026-11-02T10:11:12Z',
      action: 'remake',
    });
  });

  it('reads a stale map by its reason, a reason it does not know generically', () => {
    const stale = (reason?: string) =>
      readalongView(
        book({ readalong: { state: 'stale', reason } as AdminBook['readalong'] }),
        undefined,
        [],
        'ready',
      );
    expect(stale('ebook_changed')).toEqual({
      map: 'stale',
      staleKey: 'book.readalong.stale.ebook_changed',
      action: 'remake',
    });
    expect(stale('audio_changed').staleKey).toBe('book.readalong.stale.audio_changed');
    expect(stale('cover_changed').staleKey).toBe('book.readalong.stale.other');
    expect(stale().staleKey).toBe('book.readalong.stale.other');
    for (const k of [
      'book.readalong.stale.ebook_changed',
      'book.readalong.stale.audio_changed',
      'book.readalong.stale.other',
    ]) {
      expect(en).toHaveProperty([k]);
    }
  });

  it('offers Make for a book with an ebook and no map', () => {
    expect(readalongView(book(), undefined, [], 'ready')).toEqual({ map: 'none', action: 'make' });
  });

  it('offers nothing without a Bridge that makes read-alongs, or without an ebook', () => {
    expect(readalongView(book(), undefined, [], 'none').action).toBeUndefined();
    expect(readalongView({}, undefined, [], 'ready').action).toBeUndefined();
  });

  it('says why there is no offer when the map needs making again (ADMIN-03, S265-DIALOG)', () => {
    const stale = book({ readalong: { state: 'stale', reason: 'ebook_changed' } });
    // No Bridge, an offline one, and an online one too old for read-alongs.
    expect(readalongView(stale, undefined, [], 'none')).toEqual({
      map: 'stale',
      staleKey: 'book.readalong.stale.ebook_changed',
      bridgeNote: 'none',
    });
    expect(readalongView(stale, undefined, [], 'offline').bridgeNote).toBe('offline');
    expect(readalongView(stale, undefined, [], 'too_old').bridgeNote).toBe('too_old');
    // A failed job needs making again too.
    expect(readalongView(book(), job({ state: 'failed' }), [], 'none').bridgeNote).toBe('none');
    // A ready map, or no map, without a Bridge: nothing to make again, no note.
    const ready = book({ readalong: { state: 'ready', coverage: 0.9 } });
    expect(readalongView(ready, undefined, [], 'none').bridgeNote).toBeUndefined();
    expect(readalongView(book(), undefined, [], 'offline').bridgeNote).toBeUndefined();
    // A Bridge too old to make one says so even with no map.
    expect(readalongView(book(), undefined, [], 'too_old')).toEqual({
      map: 'none',
      bridgeNote: 'too_old',
    });
    // With an offer, no note; with an open job or a blocker, neither.
    expect(readalongView(stale, undefined, [], 'ready').bridgeNote).toBeUndefined();
    expect(readalongView(stale, job({ state: 'running' }), [], 'none').bridgeNote).toBeUndefined();
    for (const k of ['none', 'offline', 'too_old', 'setUp', 'settings']) {
      expect(en).toHaveProperty([`book.readalong.bridge.${k}`]);
    }
  });

  it("offers nothing while the book's read-along job is open, parked too", () => {
    for (const state of ['queued', 'claimed', 'running', 'needs_attention'] as BridgeJobState[]) {
      expect(readalongView(book(), job({ state }), [], 'ready').action).toBeUndefined();
    }
    for (const state of ['done', 'failed', 'cancelled'] as BridgeJobState[]) {
      expect(readalongView(book(), job({ state }), [], 'ready').action).toBe('make');
    }
  });

  it('waits for an open generate or detect job, parked too, as the server refuses one then', () => {
    for (const kind of ['sidecars.generate', 'chapters.detect']) {
      for (const state of ['queued', 'running', 'needs_attention'] as BridgeJobState[]) {
        const holding = job({ kind, state });
        const v = readalongView(book(), undefined, [holding], 'ready');
        expect(v.blocker).toBe(holding);
        expect(v.action).toBeUndefined();
      }
    }
    // Being cancelled, it ends without writing: Make is offered again.
    const stopping = job({ kind: 'sidecars.generate', state: 'running', cancel_requested: true });
    expect(readalongView(book(), undefined, [stopping], 'ready').action).toBe('make');
    // A share or a description holds nothing back.
    for (const kind of ['sidecars.contribute', 'sidecars.describe', 'meta.correct']) {
      expect(readalongView(book(), undefined, [job({ kind })], 'ready').action).toBe('make');
    }
  });

  it('shows the card for a book with an ebook and a map, an offer, a blocker or a job', () => {
    const none = { map: 'none' as const };
    expect(readalongCardShown(book(), none, undefined)).toBe(false);
    expect(readalongCardShown(book(), { map: 'stale' }, undefined)).toBe(true);
    expect(readalongCardShown(book(), { ...none, action: 'make' }, undefined)).toBe(true);
    expect(readalongCardShown(book(), { ...none, blocker: job({}) }, undefined)).toBe(true);
    expect(readalongCardShown(book(), none, job({ state: 'failed' }))).toBe(true);
    // Never without an ebook, whatever else.
    expect(readalongCardShown({}, { map: 'ready', action: 'make' }, job({}))).toBe(false);
  });
});

describe('readalongBlocker', () => {
  it('takes the newest open generate or detect job only', () => {
    const generate = job({ kind: 'sidecars.generate', state: 'running' });
    expect(readalongBlocker([generate])).toBe(generate);
    expect(readalongBlocker([job({ kind: 'sidecars.generate', state: 'done' })])).toBeUndefined();
    expect(readalongBlocker(undefined)).toBeUndefined();
    expect(blocksReadalong(job({ kind: 'readalong.align', state: 'running' }))).toBe(false);
  });
});

describe('the confirmation', () => {
  it('confirms only for a writable library and a book with an ebook', () => {
    expect(readalongDialogView({ writable: true, ebook }, 'ready')).toEqual({
      notWritable: false,
      canConfirm: true,
    });
    expect(readalongDialogView({ writable: false, ebook }, 'ready')).toEqual({
      notWritable: true,
      canConfirm: false,
    });
    expect(readalongDialogView({ writable: true, ebook: null }, 'ready').canConfirm).toBe(false);
  });
});

describe('the read-along kind elsewhere', () => {
  it('holds Generate back while a read-along is queued or working, never while parked', () => {
    for (const state of ['queued', 'claimed', 'running'] as BridgeJobState[]) {
      expect(blocksGenerate(job({ state }))).toBe(true);
    }
    // Parked, it gives way: the generate job cancels it.
    expect(blocksGenerate(job({ state: 'needs_attention' }))).toBe(false);
    expect(blocksGenerate(job({ state: 'running', cancel_requested: true }))).toBe(false);
    const running = job({ state: 'running' });
    expect(generateBlocker([running], undefined)).toBe(running);
    expect(generateBlocker(undefined, running)).toBe(running);
  });

  it('is a book kind that writes the book, polled while it works', () => {
    expect(isBookKind('readalong.align')).toBe(true);
    expect(isFileKind('readalong.align')).toBe(true);
    expect(generatePollMs([job({ state: 'running' })])).toBe(10_000);
  });

  it('labels the aligning stage and names the kind', () => {
    expect(KNOWN_STAGES).toContain('readalong_aligning');
    const t = i18n.getFixedT('en');
    expect(t('bridge.stage.readalong_aligning')).toBe('Lining up the ebook with the narration');
    expect(t('bridge.kind.readalong.align')).toBe('Read-along');
  });

  it("reads a read-along result's text coverage and whether it transcribed", () => {
    expect(
      generateResult({
        coverage: { text: 0.996, audio: 0.98 },
        sections: 12,
        transcribed: false,
      }),
    ).toEqual({ contributed: [], coverage: 0.996, transcribed: false });
    // A coverage outside 0..1 is no coverage.
    expect(generateResult({ coverage: { text: 1.5 } })).toBeNull();
    expect(generateResult({ coverage: 0.9 })).toBeNull();
  });

  it('rounds coverage down to a whole percent', () => {
    expect(coverageDown(0.996)).toBe(0.99);
    expect(coverageDown(0.57)).toBe(0.57);
    expect(coverageDown(1)).toBe(1);
    expect(coverageDown(-1)).toBe(0);
    expect(coverageDown(Number.NaN)).toBe(0);
  });

  it('words a finished read-along event with how much of the text lined up', () => {
    const ev = (data: Record<string, unknown>): ServerEvent => ({
      id: 1,
      at: '2026-11-02T10:00:00Z',
      kind: 'bridge_job_finished',
      data: { job_kind: 'readalong.align', title: 'Call of the Wild', state: 'done', ...data },
    });
    const d = describeEvent(ev({ coverage: 0.996 }));
    expect(d.title).toEqual({
      key: 'events.bridge_job_finished.readalong',
      values: { name: 'Call of the Wild' },
    });
    expect(d.body).toEqual({
      key: 'events.bridge_job_finished.readalongCoverage',
      values: { percent: 99 },
    });
    // A server that sends no coverage says nothing more.
    expect(describeEvent(ev({})).body).toBeUndefined();
    // Made, not "ready": the player follows a read-along only from 3c-2 (S265-NOTIFY).
    expect(en['events.bridge_job_finished.readalong']).toBe('Read-along made for {{name}}');
    expect(en['events.bridge_job_finished.readalongCoverage']).toContain(
      'once the player supports read-alongs',
    );
  });
});
