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
} from '@/features/bridge/generate-model';
import { describeEvent } from '@/lib/server-events';
import { serverInfo } from '@/test/fixtures';
import {
  blocksReadalong,
  readalongAvailable,
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

describe('the read-along gate', () => {
  it('offers Make only while an online Bridge offers the readalong feature', () => {
    expect(readalongAvailable(withBridge(['bridge', 'sidecars', 'readalong']))).toBe(true);
    expect(readalongAvailable(withBridge(['bridge', 'sidecars']))).toBe(false);
    expect(readalongAvailable(withBridge([], false))).toBe(false);
    expect(readalongAvailable(serverInfo)).toBe(false);
    expect(readalongAvailable(undefined)).toBe(false);
  });

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
      true,
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
        true,
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
    expect(readalongView(book(), undefined, [], true)).toEqual({ map: 'none', action: 'make' });
  });

  it('offers nothing without a Bridge that makes read-alongs, or without an ebook', () => {
    expect(readalongView(book(), undefined, [], false).action).toBeUndefined();
    expect(readalongView({}, undefined, [], true).action).toBeUndefined();
  });

  it("offers nothing while the book's read-along job is open, parked too", () => {
    for (const state of ['queued', 'claimed', 'running', 'needs_attention'] as BridgeJobState[]) {
      expect(readalongView(book(), job({ state }), [], true).action).toBeUndefined();
    }
    for (const state of ['done', 'failed', 'cancelled'] as BridgeJobState[]) {
      expect(readalongView(book(), job({ state }), [], true).action).toBe('make');
    }
  });

  it('waits for an open generate or detect job, parked too, as the server refuses one then', () => {
    for (const kind of ['sidecars.generate', 'chapters.detect']) {
      for (const state of ['queued', 'running', 'needs_attention'] as BridgeJobState[]) {
        const holding = job({ kind, state });
        const v = readalongView(book(), undefined, [holding], true);
        expect(v.blocker).toBe(holding);
        expect(v.action).toBeUndefined();
      }
    }
    // Being cancelled, it ends without writing: Make is offered again.
    const stopping = job({ kind: 'sidecars.generate', state: 'running', cancel_requested: true });
    expect(readalongView(book(), undefined, [stopping], true).action).toBe('make');
    // A share or a description holds nothing back.
    for (const kind of ['sidecars.contribute', 'sidecars.describe', 'meta.correct']) {
      expect(readalongView(book(), undefined, [job({ kind })], true).action).toBe('make');
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
    expect(readalongDialogView({ writable: true, ebook })).toEqual({
      notWritable: false,
      canConfirm: true,
    });
    expect(readalongDialogView({ writable: false, ebook })).toEqual({
      notWritable: true,
      canConfirm: false,
    });
    expect(readalongDialogView({ writable: true, ebook: null }).canConfirm).toBe(false);
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
  });
});
