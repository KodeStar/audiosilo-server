import { describe, expect, it } from 'vitest';
import type { BridgeEstimate, BridgeJob, BridgeJobState, ServerInfo } from '@/api/types';
import en from '@/i18n/locales/en.json';
import { formatUsd } from '@/lib/format';
import { serverInfo } from '@/test/fixtures';
import { adminBook } from '@/test/library-fixtures';
import { BRIDGE_JOB_STATES } from './bridge-model';
import {
  baseName,
  batchCheck,
  batchPlan,
  bridgeJobLink,
  destinationKey,
  estimateView,
  GENERATE_POLL_MS,
  generateAction,
  generateAvailable,
  generateJobsKnown,
  generateLook,
  generatePlan,
  generatePollMs,
  generateProgress,
  generateResult,
  justFinished,
  KIND_GENERATE,
  KNOWN_PARKS,
  KNOWN_STAGES,
  latestGenerateJob,
  parkKey,
  roundEta,
  seriesBatchOrder,
  settleLimited,
  stageLabel,
  titleCase,
  type BatchCheck,
} from './generate-model';

const strings = en as Record<string, string>;

function server(over: {
  metadata?: boolean;
  bridge?: boolean;
  features?: string[] | null;
}): ServerInfo {
  const { metadata = true, bridge, features = ['bridge', 'sidecars'] } = over;
  return {
    ...serverInfo,
    capabilities: {
      ...serverInfo.capabilities,
      metadata,
      ...(bridge === undefined ? {} : { bridge }),
    },
    ...(features ? { bridge: { version: '0.2.0', features } } : {}),
  };
}

function estimate(over: Partial<BridgeEstimate> = {}): BridgeEstimate {
  return {
    kind: KIND_GENERATE,
    library_id: 1,
    path: 'Author/Book',
    source: 'audio',
    duration_seconds: 36000,
    asr_hours: 2.5,
    agent_cost_usd: 16,
    subscription: false,
    agent_ready: true,
    writable: true,
    has: {
      community: { characters: false, recaps: false },
      local: { characters: false, recaps: false },
    },
    earlier: [],
    ...over,
  };
}

const job = (state: BridgeJobState, over: Partial<BridgeJob> = {}) =>
  ({ id: 9, kind: KIND_GENERATE, state, ...over }) as BridgeJob;

describe('the action gate', () => {
  it('offers Generate only with metadata on and an online Bridge with the sidecars feature', () => {
    expect(generateAvailable(server({ bridge: true }))).toBe(true);
    expect(generateAvailable(server({ bridge: true, metadata: false }))).toBe(false);
    expect(generateAvailable(server({ bridge: false }))).toBe(false);
    expect(generateAvailable(server({ bridge: true, features: ['bridge'] }))).toBe(false);
    expect(generateAvailable(server({ bridge: true, features: null }))).toBe(false);
    expect(generateAvailable(undefined)).toBe(false);
  });

  it('asks for a book jobs on any server that knows the Bridge, online or not', () => {
    expect(generateJobsKnown(server({ bridge: false }))).toBe(true);
    expect(generateJobsKnown(server({ bridge: true }))).toBe(true);
    // A server from before the Bridge, or metadata off: no request at all.
    expect(generateJobsKnown(server({}))).toBe(false);
    expect(generateJobsKnown(server({ bridge: true, metadata: false }))).toBe(false);
  });

  it('hides the action when the community has both, and says Regenerate over local data', () => {
    const none = { characters: false, recaps: false };
    const both = { characters: true, recaps: true };
    expect(generateAction({ community: none, local: none })).toBe('generate');
    expect(generateAction({ community: both, local: none })).toBe('hidden');
    expect(generateAction({ community: both, local: both })).toBe('hidden');
    expect(generateAction({ community: { characters: true, recaps: false }, local: none })).toBe(
      'generate',
    );
    expect(generateAction({ community: none, local: { characters: false, recaps: true } })).toBe(
      'regenerate',
    );
  });
});

describe('polling a book', () => {
  it('polls about every 10 s while a generate job is open, and stops when it ends', () => {
    for (const s of ['queued', 'claimed', 'running', 'needs_attention'] as const) {
      expect(generatePollMs([job(s)])).toBe(GENERATE_POLL_MS);
    }
    for (const s of ['done', 'failed', 'cancelled'] as const) {
      expect(generatePollMs([job(s)])).toBe(false);
    }
    expect(GENERATE_POLL_MS).toBe(10_000);
    expect(generatePollMs([])).toBe(false);
    expect(generatePollMs(undefined)).toBe(false);
  });

  it("ignores another kind's open job on the same book", () => {
    expect(generatePollMs([job('running', { kind: 'bridge.verify' })])).toBe(false);
    expect(generatePollMs([job('running', { kind: 'bridge.verify' }), job('running')])).toBe(
      GENERATE_POLL_MS,
    );
  });

  it('takes the newest generate job of the list', () => {
    const jobs = [
      job('running', { id: 12, kind: 'bridge.verify' }),
      job('done', { id: 11 }),
      job('failed', { id: 10 }),
    ];
    expect(latestGenerateJob(jobs)?.id).toBe(11);
    expect(latestGenerateJob([])).toBeUndefined();
  });

  it('notices a job moving from open to done, once', () => {
    expect(justFinished(job('running'), job('done'))).toBe(true);
    expect(justFinished(job('needs_attention'), job('done'))).toBe(true);
    expect(justFinished(job('done'), job('done'))).toBe(false);
    expect(justFinished(job('running'), job('failed'))).toBe(false);
    expect(justFinished(job('running', { id: 1 }), job('done', { id: 2 }))).toBe(false);
    expect(justFinished(undefined, job('done'))).toBe(false);
  });

  it('words every state as the card shows it', () => {
    expect(BRIDGE_JOB_STATES.map((s) => generateLook({ state: s }))).toEqual([
      'waiting',
      'working',
      'working',
      'parked',
      'done',
      'failed',
      'cancelled',
    ]);
  });
});

describe('stage labels', () => {
  it('has a label for every engine stage and Bridge step', () => {
    for (const s of KNOWN_STAGES) {
      expect(strings[stageLabel(s).key], s).toBeTruthy();
    }
    expect(stageLabel('asr').key).toBe('bridge.stage.asr');
    expect(strings['bridge.stage.asr']).toBe('Transcribing');
    expect(strings['bridge.stage.qa_sweep']).toBe('Checking the transcript');
  });

  it('reads a stage it has no label for title-cased', () => {
    expect(stageLabel('voice_cloning').fallback).toBe('Voice cloning');
    expect(strings[stageLabel('voice_cloning').key]).toBeUndefined();
    expect(titleCase('qa.sweep-2')).toBe('Qa sweep 2');
    expect(titleCase('')).toBe('');
  });
});

describe('parks', () => {
  it('has a sentence for every park code, and a generic one naming an unknown code', () => {
    for (const c of KNOWN_PARKS) {
      expect(strings[parkKey(c)], c).toBeTruthy();
    }
    expect(parkKey('core_needed')).toBe('bridge.park.core_needed');
    expect(parkKey('new_thing')).toBe('bridge.park.unknown');
    expect(strings['bridge.park.unknown']).toContain('{{code}}');
  });

  it("links a parked job into the Bridge's own page", () => {
    expect(bridgeJobLink('http://192.168.1.20:8090/', 42)).toBe(
      'http://192.168.1.20:8090/#/jobs/42',
    );
    expect(bridgeJobLink('https://bridge.lan/ui', 7)).toBe('https://bridge.lan/ui#/jobs/7');
    // A ui_url with a hash of its own: the job's replaces it.
    expect(bridgeJobLink('http://bridge.lan/#/settings', 7)).toBe('http://bridge.lan/#/jobs/7');
    expect(bridgeJobLink('javascript:alert(1)', 7)).toBeUndefined();
    expect(bridgeJobLink('/relative', 7)).toBeUndefined();
    expect(bridgeJobLink(undefined, 7)).toBeUndefined();
  });

  it('reads a park from the progress, with its hint and retry time', () => {
    expect(
      generateProgress({
        step: 'asr',
        stage: 'fact_pass',
        park: {
          code: 'agent_rate_limited',
          hint: 'Resets at 14:00',
          retry_at: '2026-10-15T14:00:00Z',
        },
      }).park,
    ).toEqual({
      code: 'agent_rate_limited',
      hint: 'Resets at 14:00',
      retryAt: '2026-10-15T14:00:00Z',
    });
    // A park without a code, or an unreadable retry time, is read as far as it goes.
    expect(generateProgress({ park: { hint: 'x' } }).park).toBeUndefined();
    expect(generateProgress({ park: { code: 'core_needed', retry_at: 'soon' } }).park).toEqual({
      code: 'core_needed',
    });
  });
});

describe('progress and result', () => {
  it('reads the stage (else the step), counts, ETA, cost and source', () => {
    expect(
      generateProgress({
        step: 'asr',
        stage: 'asr',
        message: 'Transcribing chapter 12 of 40',
        done: 12,
        total: 40,
        eta_seconds: 5400,
        cost_usd: 3.42,
        source: 'audio',
      }),
    ).toEqual({
      stage: 'asr',
      message: 'Transcribing chapter 12 of 40',
      done: 12,
      total: 40,
      etaSeconds: 5400,
      costUsd: 3.42,
      source: 'audio',
    });
    expect(generateProgress({ step: 'writing' }).stage).toBe('writing');
  });

  it('drops what it cannot use', () => {
    expect(
      generateProgress({ step: 7, done: 1, total: 0, eta_seconds: -5, cost_usd: 'a', source: 'x' }),
    ).toEqual({});
    expect(generateProgress(null)).toEqual({});
    expect(generateProgress('running')).toEqual({});
  });

  it('reads a result, keeping only http(s) issue links', () => {
    expect(
      generateResult({
        characters: 41,
        recaps: 38,
        cost_usd: 12.5,
        source: 'ebook',
        contributed: {
          characters: 'https://github.com/KodeStar/audiosilo-meta-community/issues/12',
          recaps: 'javascript:alert(1)',
        },
      }),
    ).toEqual({
      characters: 41,
      recaps: 38,
      costUsd: 12.5,
      source: 'ebook',
      contributed: [
        {
          what: 'characters',
          url: 'https://github.com/KodeStar/audiosilo-meta-community/issues/12',
        },
      ],
    });
    expect(generateResult({})).toBeNull();
    expect(generateResult({ files: ['characters.json'] })).toBeNull();
  });

  it('rounds an ETA to what a person can use', () => {
    expect(roundEta(0)).toBe(0);
    expect(roundEta(20)).toBe(60);
    expect(roundEta(125)).toBe(120);
    expect(roundEta(5400 + 100)).toBe(5400);
    expect(roundEta(5400 + 200)).toBe(5700);
    expect(roundEta(40_000)).toBe(39_600);
  });

  it('formats dollars, dropping cents from $100', () => {
    expect(formatUsd(3.4, 'en')).toBe('$3.40');
    expect(formatUsd(0, 'en')).toBe('$0.00');
    expect(formatUsd(140.4, 'en')).toBe('$140');
  });
});

describe('the estimate', () => {
  it('words an audio book: transcription hours, the expected cost', () => {
    const v = estimateView(estimate());
    expect(v).toEqual({
      source: 'audio',
      asrHours: 2.5,
      costUsd: 16,
      subscription: false,
      agentNotReady: false,
      notWritable: false,
      canConfirm: true,
    });
  });

  it('names the ebook beside the audio and needs no transcription', () => {
    const v = estimateView(
      estimate({ source: 'ebook', ebook: 'Author/Book/The Book.epub', asr_hours: 0 }),
    );
    expect(v.source).toBe('ebook');
    expect(v.ebook).toEqual({ name: 'The Book.epub', path: 'Author/Book/The Book.epub' });
    expect(v.asrHours).toBe(0);
    expect(baseName('a/b/')).toBe('b');
  });

  it('says there is no estimate yet without history', () => {
    const v = estimateView(estimate({ agent_cost_usd: null, asr_hours: 0 }));
    expect(v.costUsd).toBeNull();
    // Audio with no ASR figure is "no estimate", never "none needed".
    expect(v.asrHours).toBeNull();
  });

  it("flags a subscription's cost as an estimate of usage", () => {
    expect(estimateView(estimate({ subscription: true })).subscription).toBe(true);
  });

  it('refuses to confirm while the library is not writable, and warns of an agent not ready', () => {
    const v = estimateView(estimate({ writable: false, agent_ready: false }));
    expect(v.notWritable).toBe(true);
    expect(v.agentNotReady).toBe(true);
    expect(v.canConfirm).toBe(false);
    // An agent not ready only warns: the job waits for it.
    expect(estimateView(estimate({ agent_ready: false })).canConfirm).toBe(true);
  });

  it('says where the results appear', () => {
    expect(strings[destinationKey('local')]).toBe('On this server only');
    expect(strings[destinationKey('issue')]).toBe(
      'On this server, and offered to the community metadata',
    );
  });
});

describe('queueing', () => {
  const book = { library_id: 1, path: 'A/Series/Book 3' };
  const earlier = [
    { library_id: 1, path: 'A/Series/Book 1' },
    { library_id: 1, path: 'A/Series/Book 2' },
  ];

  it('queues the earlier books first, in order, then this one', () => {
    const plan = generatePlan(book, earlier, { includeEarlier: true, contribute: 'local' });
    expect(plan.map((r) => r.path)).toEqual([
      'A/Series/Book 1',
      'A/Series/Book 2',
      'A/Series/Book 3',
    ]);
    for (const r of plan) {
      expect(r).toMatchObject({
        kind: KIND_GENERATE,
        library_id: 1,
        params: { source: 'auto', contribute: 'local' },
      });
    }
  });

  it('queues only this book when the earlier ones are left out, with the contribute choice', () => {
    const plan = generatePlan(book, earlier, { includeEarlier: false, contribute: 'issue' });
    expect(plan).toEqual([
      {
        kind: KIND_GENERATE,
        library_id: 1,
        path: 'A/Series/Book 3',
        params: { source: 'auto', contribute: 'issue' },
      },
    ]);
  });

  it('never queues a book twice', () => {
    const plan = generatePlan(book, [...earlier, book, earlier[0]], {
      includeEarlier: true,
      contribute: 'local',
    });
    expect(plan.map((r) => r.path)).toEqual([
      'A/Series/Book 1',
      'A/Series/Book 2',
      'A/Series/Book 3',
    ]);
  });
});

describe('the series batch', () => {
  const s = 'The Stormlight Archive';
  const b = (path: string, position: number, series = s) =>
    adminBook({ path, title: path, series_list: position ? [{ name: series, position }] : [] });

  it('orders by position in this series, unnumbered books last, each once', () => {
    const books = [
      b('Three', 3),
      b('Novella', 0),
      b('One', 1),
      b('Two and a half', 2.5),
      b('One', 1),
      // Its position is in this series under another spelling.
      b('Two', 2, 'the stormlight archive'),
      b('Extra', 0),
    ];
    expect(seriesBatchOrder(books, s).map((x) => x.path)).toEqual([
      'One',
      'Two',
      'Two and a half',
      'Three',
      'Novella',
      'Extra',
    ]);
  });

  it('skips the books that have recaps or an open job, and stops on a library it cannot write', () => {
    const books = [b('One', 1), b('Two', 2), b('Three', 3), b('Four', 4)];
    const checks = new Map<string, BatchCheck>([
      [
        `1\0One`,
        batchCheck(
          estimate({
            has: {
              community: { characters: false, recaps: true },
              local: { characters: false, recaps: false },
            },
          }),
        ),
      ],
      [`1\0Two`, batchCheck(estimate({ open_job: job('running') }))],
      [`1\0Three`, batchCheck(undefined)],
      [
        `1\0Four`,
        batchCheck(
          estimate({
            has: {
              community: { characters: true, recaps: false },
              local: { characters: true, recaps: false },
            },
          }),
        ),
      ],
    ]);
    const plan = batchPlan(books, checks);
    // A failed estimate is queued anyway; characters alone don't count as covered.
    expect(plan.queue.map((x) => x.path)).toEqual(['Three', 'Four']);
    expect(plan.covered).toBe(1);
    expect(plan.open).toBe(1);
    expect(plan.notWritable).toBe(false);
    const locked = new Map([[`1\0One`, batchCheck(estimate({ writable: false }))]]);
    expect(batchPlan([books[0]], locked).notWritable).toBe(true);
  });

  it('checks a few books at a time, keeping their order and turning failures into undefined', async () => {
    let running = 0;
    let most = 0;
    const seen: number[] = [];
    const out = await settleLimited(
      [1, 2, 3, 4, 5, 6, 7],
      3,
      async (n) => {
        running++;
        most = Math.max(most, running);
        await new Promise((r) => setTimeout(r, (8 - n) * 2));
        running--;
        if (n === 4) throw new Error('nope');
        return n * 10;
      },
      (settled) => seen.push(settled),
    );
    expect(out).toEqual([10, 20, 30, undefined, 50, 60, 70]);
    expect(most).toBe(3);
    expect(seen).toEqual([1, 2, 3, 4, 5, 6, 7]);
    expect(await settleLimited([], 3, async () => 1)).toEqual([]);
  });

  it('stops starting new checks once aborted', async () => {
    const ctl = new AbortController();
    const started: number[] = [];
    await settleLimited(
      [1, 2, 3, 4],
      1,
      async (n) => {
        started.push(n);
        if (n === 2) ctl.abort();
        return n;
      },
      undefined,
      ctl.signal,
    );
    expect(started).toEqual([1, 2]);
  });
});
