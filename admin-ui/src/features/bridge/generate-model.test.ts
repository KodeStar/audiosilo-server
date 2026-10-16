import { describe, expect, it } from 'vitest';
import type { BridgeEstimate, BridgeJob, BridgeJobState, ServerInfo } from '@/api/types';
import en from '@/i18n/locales/en.json';
import { formatUsd } from '@/lib/format';
import { serverInfo } from '@/test/fixtures';
import { adminBook } from '@/test/library-fixtures';
import {
  batchCheck,
  batchPlan,
  bridgeJobLink,
  clampsDescription,
  DESCRIPTION_CLAMP_CHARS,
  describeOffered,
  destinationKey,
  estimateView,
  GENERATE_PARKED_POLL_MS,
  GENERATE_POLL_MS,
  generateAction,
  generateAvailable,
  generateJobsKnown,
  generatePlan,
  generatePollMs,
  generateProgress,
  generateResult,
  KIND_DESCRIBE,
  KIND_GENERATE,
  KNOWN_PARKS,
  KNOWN_STAGES,
  latestGenerateJob,
  latestJobOf,
  parkKey,
  planEstimate,
  roundEta,
  seriesBatchOrder,
  settleLimited,
  stageLabel,
  titleCase,
  wordsItsOwnStatus,
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
    can_contribute: true,
    figures: true,
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
  it('polls about every 10 s while a generate job works, every minute while parked, then stops', () => {
    for (const s of ['queued', 'claimed', 'running'] as const) {
      expect(generatePollMs([job(s)])).toBe(GENERATE_POLL_MS);
    }
    expect(generatePollMs([job('needs_attention')])).toBe(GENERATE_PARKED_POLL_MS);
    // A working job sets the pace over a parked one.
    expect(generatePollMs([job('needs_attention'), job('running', { id: 8 })])).toBe(
      GENERATE_POLL_MS,
    );
    for (const s of ['done', 'failed', 'cancelled'] as const) {
      expect(generatePollMs([job(s)])).toBe(false);
    }
    expect(GENERATE_POLL_MS).toBe(10_000);
    expect(GENERATE_PARKED_POLL_MS).toBe(60_000);
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
  it('reads the stage (else the step), counts, ETA and cost, and a message only it has no label for', () => {
    // The Bridge's English wording of a stage this console labels is left out:
    // the label and the count already say it in the admin's language.
    expect(
      generateProgress({
        step: 'asr',
        stage: 'asr',
        message: 'Transcribing (12 of 40)',
        done: 12,
        total: 40,
        eta_seconds: 5400,
        cost_usd: 3.42,
      }),
    ).toEqual({
      stage: 'asr',
      done: 12,
      total: 40,
      etaSeconds: 5400,
      costUsd: 3.42,
    });
    // A stage from a newer Bridge keeps its message (the only wording there is).
    expect(generateProgress({ step: 'proofreading', message: 'Proofreading chapter 3' })).toEqual({
      stage: 'proofreading',
      message: 'Proofreading chapter 3',
    });
    // The step stands in for the stage, never for the message.
    expect(generateProgress({ step: 'writing' })).toEqual({ stage: 'writing' });
    expect(generateProgress({ step: 'asr', stage: 'fact_pass' })).toEqual({ stage: 'fact_pass' });
    // The Bridge's own last steps say more than the engine's "done".
    expect(generateProgress({ step: 'writing', stage: 'done' })).toEqual({ stage: 'writing' });
    expect(generateProgress({ step: 'rescanning', stage: 'done' })).toEqual({
      stage: 'rescanning',
    });
  });

  it('drops what it cannot use', () => {
    expect(
      generateProgress({ step: 7, done: 1, total: 0, eta_seconds: -5, cost_usd: 'a' }),
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
        contributed: {
          characters: 'https://github.com/KodeStar/audiosilo-meta-community/issues/12',
          recaps: 'javascript:alert(1)',
        },
      }),
    ).toEqual({
      characters: 41,
      recaps: 38,
      costUsd: 12.5,
      contributed: [
        {
          what: 'characters',
          url: 'https://github.com/KodeStar/audiosilo-meta-community/issues/12',
        },
      ],
    });
    expect(generateResult({})).toBeNull();
    expect(generateResult({ characters: -1, recaps: 'many' })).toBeNull();
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
      asrHours: 2.5,
      costUsd: 16,
      subscription: false,
      agentNotReady: false,
      noFigures: false,
      canContribute: true,
      notWritable: false,
    });
  });

  it('names the ebook beside the audio and needs no transcription', () => {
    const v = estimateView(
      estimate({ source: 'ebook', ebook: 'Author/Book/The Book.epub', asr_hours: 0 }),
    );
    expect(v.ebook).toEqual({ name: 'The Book.epub', path: 'Author/Book/The Book.epub' });
    expect(v.asrHours).toBe(0);
    // An audio book names no file, even with an ebook path on the answer.
    expect(estimateView(estimate({ ebook: 'Author/Book/The Book.epub' })).ebook).toBeUndefined();
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
    // An agent not ready only warns: the job waits for it.
    expect(estimateView(estimate({ agent_ready: false })).notWritable).toBe(false);
  });

  it('tells a Bridge that sent no figures yet from an agent that is not ready', () => {
    // No figures: agent_ready false is a default, not a report.
    const none = estimateView(
      estimate({ figures: false, agent_ready: false, can_contribute: false, agent_cost_usd: null }),
    );
    expect(none.noFigures).toBe(true);
    expect(none.agentNotReady).toBe(false);
    expect(none.canContribute).toBe(false);
    const notReady = estimateView(estimate({ agent_ready: false }));
    expect(notReady.noFigures).toBe(false);
    expect(notReady.agentNotReady).toBe(true);
  });

  it('says where the results appear', () => {
    expect(strings[destinationKey('local')]).toBe('On this server only');
    expect(strings[destinationKey('issue')]).toBe(
      'On this server, and offered to the community metadata',
    );
  });

  it('quotes the whole plan: this book and the earlier ones it queues first', () => {
    const earlier = [
      {
        library_id: 1,
        path: 'A/1',
        title: '1',
        series_index: '1',
        asr_hours: 2,
        agent_cost_usd: 3,
      },
      {
        library_id: 1,
        path: 'A/2',
        title: '2',
        series_index: '2',
        asr_hours: 1,
        agent_cost_usd: 1.5,
      },
    ];
    expect(planEstimate(estimate({ earlier }), true)).toEqual({
      books: 3,
      asrHours: 5.5,
      costUsd: 20.5,
    });
    // Left out, the book alone.
    expect(planEstimate(estimate({ earlier }), false)).toEqual({
      books: 1,
      asrHours: 2.5,
      costUsd: 16,
    });
    // An ebook needs no transcription; the earlier books' still count.
    expect(
      planEstimate(estimate({ source: 'ebook', asr_hours: 0, agent_cost_usd: 4, earlier }), true),
    ).toEqual({ books: 3, asrHours: 3, costUsd: 8.5 });
    // One book with no figure makes the total unknown.
    const unknown = [{ ...earlier[0], asr_hours: 0, agent_cost_usd: null }];
    expect(planEstimate(estimate({ earlier: unknown }), true)).toEqual({
      books: 2,
      asrHours: null,
      costUsd: null,
    });
  });
});

describe('queueing', () => {
  const book = { library_id: 1, path: 'A/Series/Book 3' };
  const earlier = [
    { library_id: 1, path: 'A/Series/Book 1' },
    { library_id: 1, path: 'A/Series/Book 2' },
  ];

  it('queues the books in the order given: the earlier ones first, then this one', () => {
    const plan = generatePlan([...earlier, book], 'local');
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
    const plan = generatePlan([book], 'issue');
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
    const plan = generatePlan([...earlier, book, earlier[0], book], 'local');
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
    // A failed estimate is left out, never queued on a guess; characters alone
    // don't count as covered.
    expect(plan.queue.map((x) => x.path)).toEqual(['Four']);
    expect(plan.covered).toBe(1);
    expect(plan.open).toBe(1);
    expect(plan.unknown).toBe(1);
    expect(plan.notWritable).toBe(false);
    expect(plan.costUsd).toBe(16);
    const locked = new Map([[`1\0One`, batchCheck(estimate({ writable: false }))]]);
    expect(batchPlan([books[0]], locked).notWritable).toBe(true);
  });

  it('only lets a book it would queue stop the batch on a library it cannot write', () => {
    const books = [b('One', 1), b('Two', 2)];
    const checks = new Map<string, BatchCheck>([
      [
        `1\0One`,
        batchCheck(
          estimate({
            writable: false,
            has: {
              community: { characters: false, recaps: true },
              local: { characters: false, recaps: false },
            },
          }),
        ),
      ],
      [`1\0Two`, batchCheck(estimate())],
    ]);
    const plan = batchPlan(books, checks);
    expect(plan.notWritable).toBe(false);
    expect(plan.queue.map((x) => x.path)).toEqual(['Two']);
  });

  it('queues a book whose open job is being cancelled, and leaves out one the community could not be asked about', () => {
    expect(
      batchCheck(estimate({ open_job: job('running', { cancel_requested: true }) })).open,
    ).toBe(false);
    const unasked = {
      community: { characters: false, recaps: false },
      local: { characters: false, recaps: false },
      community_unknown: true,
    };
    expect(batchCheck(estimate({ has: unasked })).unknown).toBe(true);
    // Recaps of its own make it covered, whatever the community says.
    expect(
      batchCheck(estimate({ has: { ...unasked, local: { characters: false, recaps: true } } })),
    ).toMatchObject({ covered: true, unknown: false });
  });

  it('totals what the queued books cost, unknown when one is', () => {
    const books = [b('One', 1), b('Two', 2)];
    const both = new Map<string, BatchCheck>([
      [`1\0One`, batchCheck(estimate({ agent_cost_usd: 4 }))],
      [`1\0Two`, batchCheck(estimate({ agent_cost_usd: 16, subscription: true }))],
    ]);
    expect(batchPlan(books, both)).toMatchObject({ costUsd: 20, subscription: true });
    const one = new Map(both).set(`1\0Two`, batchCheck(estimate({ agent_cost_usd: null })));
    expect(batchPlan(books, one).costUsd).toBeNull();
  });

  it('offers contributing only when the Bridge can for every book it queues', () => {
    const books = [b('One', 1), b('Two', 2)];
    const can = new Map<string, BatchCheck>([
      [`1\0One`, batchCheck(estimate())],
      [`1\0Two`, batchCheck(estimate())],
    ]);
    expect(batchPlan(books, can).canContribute).toBe(true);
    const cant = new Map(can).set(`1\0Two`, batchCheck(estimate({ can_contribute: false })));
    expect(batchPlan(books, cant).canContribute).toBe(false);
    // Nothing to queue: nothing to offer.
    expect(batchPlan(books, new Map()).canContribute).toBe(false);
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

describe('a description (Phase 1c)', () => {
  const has = (
    local: Partial<Record<string, boolean>>,
    community: Partial<Record<string, boolean>> = {},
  ) => ({
    community: { characters: false, recaps: false, description: false, ...community },
    local: { characters: false, recaps: false, description: false, ...local },
  });
  const open = (kind: string, state: BridgeJobState = 'running') => ({ kind, state });

  it('is offered for a book with generated content, no description and nothing open', () => {
    expect(describeOffered({ can_describe: true, has: has({ characters: true }) }, [])).toBe(true);
    expect(describeOffered({ can_describe: true, has: has({ recaps: true }) }, undefined)).toBe(
      true,
    );
    // A finished or cancelled job is no reason to hold back.
    expect(
      describeOffered({ can_describe: true, has: has({ recaps: true }) }, [
        open(KIND_DESCRIBE, 'failed'),
        open(KIND_GENERATE, 'done'),
      ]),
    ).toBe(true);
  });

  it('is not offered without a Bridge that can, content to write from, or with one already', () => {
    const local = has({ characters: true, recaps: true });
    expect(describeOffered(undefined, [])).toBe(false);
    expect(describeOffered({ has: local }, [])).toBe(false);
    expect(describeOffered({ can_describe: false, has: local }, [])).toBe(false);
    expect(describeOffered({ can_describe: true, has: has({}) }, [])).toBe(false);
    expect(
      describeOffered(
        { can_describe: true, has: has({ characters: true, description: true }) },
        [],
      ),
    ).toBe(false);
    expect(
      describeOffered(
        { can_describe: true, has: has({ characters: true }, { description: true }) },
        [],
      ),
    ).toBe(false);
  });

  it('is not offered while a generate or describe job is open', () => {
    const e = { can_describe: true, has: has({ characters: true }) };
    expect(describeOffered(e, [open(KIND_GENERATE)])).toBe(false);
    expect(describeOffered(e, [open(KIND_DESCRIBE, 'needs_attention')])).toBe(false);
    // Another kind (a verify) doesn't count.
    expect(describeOffered(e, [open('bridge.verify')])).toBe(true);
  });

  it('polls a describe job like a generate one, and finds each kind', () => {
    expect(generatePollMs([open(KIND_DESCRIBE)])).toBe(GENERATE_POLL_MS);
    expect(generatePollMs([open(KIND_DESCRIBE, 'needs_attention')])).toBe(GENERATE_PARKED_POLL_MS);
    const jobs = [open(KIND_DESCRIBE), open(KIND_GENERATE, 'done')];
    expect(latestJobOf(jobs, KIND_GENERATE)).toBe(jobs[1]);
    expect(latestJobOf(jobs, KIND_DESCRIBE)).toBe(jobs[0]);
    expect(latestGenerateJob(jobs)).toBe(jobs[1]);
    expect(wordsItsOwnStatus(KIND_DESCRIBE)).toBe(true);
    expect(wordsItsOwnStatus('bridge.verify')).toBe(false);
  });

  it("reads a result's description outcome", () => {
    expect(generateResult({ description: true, checked_against: 'none' })).toEqual({
      description: true,
      contributed: [],
    });
    expect(
      generateResult({ characters: 1, description: false, description_error: ' rate limited ' }),
    ).toEqual({
      characters: 1,
      description: false,
      descriptionError: 'rate limited',
      contributed: [],
    });
    // An error beside a written description says nothing.
    expect(generateResult({ description: true, description_error: 'x' })?.descriptionError).toBe(
      undefined,
    );
    expect(generateResult({ description: 'yes' })).toBeNull();
  });

  it('clamps a long description only', () => {
    expect(clampsDescription('x'.repeat(DESCRIPTION_CLAMP_CHARS))).toBe(false);
    expect(clampsDescription('x'.repeat(DESCRIPTION_CLAMP_CHARS + 1))).toBe(true);
  });

  it('labels the describing stage', () => {
    expect(KNOWN_STAGES).toContain('describing');
    expect(strings['bridge.stage.describing']).toBe('Writing the description');
  });
});
