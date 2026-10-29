import { describe, expect, it } from 'vitest';
import type {
  BridgeEstimate,
  BridgeJob,
  BridgeJobState,
  BridgeStatus,
  ServerInfo,
} from '@/api/types';
import i18n from '@/i18n';
import en from '@/i18n/locales/en.json';
import { formatUsd } from '@/lib/format';
import { serverInfo } from '@/test/fixtures';
import { adminBook } from '@/test/library-fixtures';
import {
  batchCheck,
  batchPlan,
  contributedLinks,
  blocksGenerate,
  generateBlocker,
  bridgeJobLink,
  clampsDescription,
  contributeOffered,
  contributionsOn,
  DESCRIPTION_CLAMP_CHARS,
  describeOffered,
  destinationKey,
  detectAvailable,
  detectJobsKnown,
  detectPlan,
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
  isBookKind,
  KIND_CONTRIBUTE,
  KIND_CORRECT,
  KIND_DESCRIBE,
  KIND_DETECT,
  KIND_GENERATE,
  KNOWN_PARKS,
  KNOWN_STAGES,
  latestJobOf,
  parkHint,
  parkKey,
  planEstimate,
  roundEta,
  seriesBatchOrder,
  settleLimited,
  shareDimensions,
  shareOffered,
  shareRequest,
  stageLabel,
  titleCase,
  type BatchCheck,
  cancelledView,
} from './generate-model';
import { skipNotes } from './use-generate';

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
    can_describe: false,
    has: {
      community: { characters: false, recaps: false, description: false },
      local: { characters: false, recaps: false, description: false },
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

  it('follows a book chapter detection on any server that knows the Bridge, metadata or not', () => {
    expect(detectJobsKnown(server({ bridge: true }))).toBe(true);
    expect(detectJobsKnown(server({ bridge: false, metadata: false }))).toBe(true);
    expect(detectJobsKnown(server({}))).toBe(false);
    expect(detectJobsKnown(undefined)).toBe(false);
  });

  it('follows the one offer rule: covered by the community, else Regenerate over local data, else Generate', () => {
    const cov = (characters: boolean, recaps: boolean) => ({
      characters,
      recaps,
      description: false,
    });
    // [community characters, community recaps, local characters, local recaps] -> action.
    const table: [boolean, boolean, boolean, boolean, string][] = [
      // The community has both: covered, whatever was generated here before.
      [true, true, false, false, 'covered'],
      [true, true, true, false, 'covered'],
      [true, true, false, true, 'covered'],
      [true, true, true, true, 'covered'],
      // The community has one: the other is a gap Generate fills.
      [true, false, false, false, 'generate'],
      [false, true, false, false, 'generate'],
      [true, false, false, true, 'regenerate'],
      [false, true, true, false, 'regenerate'],
      // The community has neither.
      [false, false, false, false, 'generate'],
      [false, false, true, false, 'regenerate'],
      [false, false, false, true, 'regenerate'],
      [false, false, true, true, 'regenerate'],
    ];
    for (const [cc, cr, lc, lr, want] of table) {
      expect(
        generateAction({ community: cov(cc, cr), local: cov(lc, lr) }),
        `${cc}${cr}${lc}${lr}`,
      ).toBe(want);
    }
  });

  it("reads the newest finished generate job's community issues", () => {
    const issue = 'https://github.com/KodeStar/audiosilo-meta-community/issues/7';
    const done = (id: number, contributed: Record<string, unknown>) =>
      job('done', { id, result: { characters: 3, contributed } });
    expect(contributedLinks(undefined)).toEqual({});
    expect(contributedLinks([])).toEqual({});
    // An open job since doesn't hide the finished one's issues.
    expect(
      contributedLinks([
        job('running', { id: 9 }),
        done(8, { characters: issue, recaps: 'javascript:x' }),
      ]),
    ).toEqual({ characters: issue });
    // A newer finished job replaces them, even with none.
    expect(contributedLinks([done(9, {}), done(8, { recaps: issue })])).toEqual({});
    // Other kinds don't count.
    expect(contributedLinks([{ ...done(9, { recaps: issue }), kind: KIND_DESCRIBE }])).toEqual({});
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
    expect(latestJobOf(jobs, KIND_GENERATE)?.id).toBe(11);
    expect(latestJobOf([], KIND_GENERATE)).toBeUndefined();
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

  it("shows the Bridge's hint, else the error, but not under wording that has its own", () => {
    expect(parkHint('agent_unavailable', 'Start the agent.', 'parked')).toBe('Start the agent.');
    expect(parkHint('agent_unavailable', undefined, 'parked')).toBe('parked');
    expect(parkHint(undefined, undefined, '')).toBeUndefined();
    expect(parkHint('chapters_not_detected', 'then cancel this job', 'parked')).toBeUndefined();
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
    // So does a generate job's description step (Phase 1c), labelled, so the
    // Bridge's English restatement of it isn't shown.
    expect(
      generateProgress({ step: 'describing', message: 'Writing a description', stage: 'done' }),
    ).toEqual({ stage: 'describing' });
    expect(generateProgress({ step: 'describing', stage: 'contributing' })).toEqual({
      stage: 'describing',
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
              community: { characters: false, recaps: true, description: false },
              local: { characters: false, recaps: false, description: false },
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
              community: { characters: true, recaps: false, description: false },
              local: { characters: true, recaps: false, description: false },
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
              community: { characters: false, recaps: true, description: false },
              local: { characters: false, recaps: false, description: false },
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
      community: { characters: false, recaps: false, description: false },
      local: { characters: false, recaps: false, description: false },
      community_unknown: true,
    };
    expect(batchCheck(estimate({ has: unasked })).unknown).toBe(true);
    // Recaps of its own make it covered, whatever the community says.
    expect(
      batchCheck(
        estimate({
          has: { ...unasked, local: { characters: false, recaps: true, description: false } },
        }),
      ),
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
  // A Bridge that runs sidecars.describe and can write to the library.
  const can = { can_describe: true, writable: true };

  it('is offered for a book with generated content, no description and nothing open', () => {
    expect(describeOffered({ ...can, has: has({ characters: true }) }, [])).toBe(true);
    expect(describeOffered({ ...can, has: has({ recaps: true }) }, undefined)).toBe(true);
    // A finished or cancelled job is no reason to hold back.
    expect(
      describeOffered({ ...can, has: has({ recaps: true }) }, [
        open(KIND_DESCRIBE, 'failed'),
        open(KIND_GENERATE, 'done'),
      ]),
    ).toBe(true);
  });

  it('is not offered without a Bridge that can, content to write from, or with one already', () => {
    const local = has({ characters: true, recaps: true, description: false });
    expect(describeOffered(undefined, [])).toBe(false);
    expect(describeOffered({ ...can, can_describe: false, has: local }, [])).toBe(false);
    expect(describeOffered({ ...can, has: has({}) }, [])).toBe(false);
    expect(describeOffered({ ...can, has: has({ characters: true, description: true }) }, [])).toBe(
      false,
    );
    expect(
      describeOffered({ ...can, has: has({ characters: true }, { description: true }) }, []),
    ).toBe(false);
  });

  it('is not offered where the server would refuse it, or on a guess', () => {
    const local = has({ characters: true });
    // A library the Bridge can't write to: the job would be a 409.
    expect(describeOffered({ ...can, writable: false, has: local }, [])).toBe(false);
    // The estimate's open generate job, whatever the job list says (or before it loads).
    expect(describeOffered({ ...can, has: local, open_job: job('running') }, undefined)).toBe(
      false,
    );
    // A community that couldn't be asked may have a description of its own.
    expect(describeOffered({ ...can, has: { ...local, community_unknown: true } }, [])).toBe(false);
  });

  it('is not offered while a generate, describe or chapter detection job is open', () => {
    const e = { ...can, has: has({ characters: true }) };
    expect(describeOffered(e, [open(KIND_GENERATE)])).toBe(false);
    expect(describeOffered(e, [open(KIND_DESCRIBE, 'needs_attention')])).toBe(false);
    // A detection, running or parked, holds the book on the Bridge.
    expect(describeOffered(e, [open(KIND_DETECT)])).toBe(false);
    expect(describeOffered(e, [open(KIND_DETECT, 'needs_attention')])).toBe(false);
    // Another kind (a verify) doesn't count.
    expect(describeOffered(e, [open('bridge.verify')])).toBe(true);
  });

  it('polls a describe job like a generate one, and finds each kind', () => {
    expect(generatePollMs([open(KIND_DESCRIBE)])).toBe(GENERATE_POLL_MS);
    expect(generatePollMs([open(KIND_DESCRIBE, 'needs_attention')])).toBe(GENERATE_PARKED_POLL_MS);
    const jobs = [open(KIND_DESCRIBE), open(KIND_GENERATE, 'done')];
    expect(latestJobOf(jobs, KIND_GENERATE)).toBe(jobs[1]);
    expect(latestJobOf(jobs, KIND_DESCRIBE)).toBe(jobs[0]);
    expect(isBookKind(KIND_DESCRIBE)).toBe(true);
    expect(isBookKind(KIND_GENERATE)).toBe(true);
    expect(isBookKind('bridge.verify')).toBe(false);
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

describe('detecting chapters', () => {
  it('offers Detect chapters only while an online Bridge offers the chapters feature, metadata or not', () => {
    const features = ['bridge', 'chapters'];
    expect(detectAvailable(server({ bridge: true, features }))).toBe(true);
    expect(detectAvailable(server({ bridge: true, metadata: false, features }))).toBe(true);
    expect(detectAvailable(server({ bridge: false, features }))).toBe(false);
    expect(detectAvailable(server({ bridge: true }))).toBe(false);
    expect(detectAvailable(server({ bridge: true, features: null }))).toBe(false);
    expect(detectAvailable(undefined)).toBe(false);
  });

  const status = (over: Partial<BridgeStatus> = {}) =>
    ({
      libraries: [
        { id: 1, name: 'Books', root: '/b', mapping: { mapped: true, writable: true } },
        { id: 2, name: 'Read only', root: '/r', mapping: { mapped: true, writable: false } },
      ],
      estimates: { sidecars: { asr_seconds_per_audio_hour: 900 } },
      ...over,
    }) as BridgeStatus;

  it("quotes the transcription over the books' known lengths, as the server rounds it", () => {
    const books = [
      adminBook({ library_id: 1, duration: 7200 }),
      adminBook({ library_id: 1, duration: 3 * 3600 + 1 }),
      adminBook({ library_id: 1, duration: 0 }),
    ];
    // 900 s per audio hour: 0.5 h, then 0.75 h (rounded to hundredths per book).
    expect(detectPlan(status(), books)).toEqual({ asrHours: 1.25, notWritable: false });
    expect(detectPlan(status({ estimates: undefined }), books).asrHours).toBeNull();
    expect(detectPlan(status(), [adminBook({ library_id: 1, duration: 0 })]).asrHours).toBeNull();
    expect(detectPlan(undefined, books)).toEqual({ asrHours: null, notWritable: false });
    // A figure of 0 (the server's float when a Bridge sends none) is no
    // estimate, as the generate dialog reads it, never "About 0h".
    const none = status({
      estimates: { sidecars: { asr_seconds_per_audio_hour: 0 } } as BridgeStatus['estimates'],
    });
    expect(detectPlan(none, books).asrHours).toBeNull();
  });

  it("refuses a book whose library the Bridge can't write to, or doesn't map", () => {
    expect(detectPlan(status(), [adminBook({ library_id: 2, duration: 60 })]).notWritable).toBe(
      true,
    );
    expect(detectPlan(status(), [adminBook({ library_id: 3, duration: 60 })]).notWritable).toBe(
      true,
    );
    expect(
      detectPlan(status(), [
        adminBook({ library_id: 1, duration: 60 }),
        adminBook({ library_id: 2, duration: 60 }),
      ]).notWritable,
    ).toBe(true);
  });

  it('reads the chapters a detect job found, and follows its jobs on the book page', () => {
    expect(generateResult({ chapters: 24, files: ['a'], source: 'audio' })).toEqual({
      chapters: 24,
      contributed: [],
    });
    expect(isBookKind(KIND_DETECT)).toBe(true);
    expect(strings[stageLabel('chapter_detecting').key]).toBe(
      'Finding the chapters in the narration',
    );
    expect(parkKey('chapters_not_detected')).toBe('bridge.park.chapters_not_detected');
    expect(strings['bridge.kind.chapters.detect']).toBe('Chapters');
  });
});

describe('blocksGenerate', () => {
  const detect = (over: Partial<BridgeJob>) =>
    ({ kind: 'chapters.detect', state: 'queued', cancel_requested: false, ...over }) as BridgeJob;

  it('holds Generate back only while a detection is queued or working, as the server does', () => {
    for (const state of ['queued', 'claimed', 'running'] as BridgeJobState[]) {
      expect(blocksGenerate(detect({ state }))).toBe(true);
    }
    // Parked, it gives way (a generate job cancels it); being cancelled, it ends unwritten.
    expect(blocksGenerate(detect({ state: 'needs_attention' }))).toBe(false);
    expect(blocksGenerate(detect({ state: 'running', cancel_requested: true }))).toBe(false);
    expect(blocksGenerate(detect({ state: 'done' }))).toBe(false);
    expect(blocksGenerate(detect({ kind: 'sidecars.describe' }))).toBe(false);
  });

  it("reads the job list once loaded, else the estimate's open job", () => {
    const running = detect({ state: 'running' });
    expect(generateBlocker(undefined, running)).toBe(running);
    expect(generateBlocker([detect({ state: 'done' })], running)).toBeUndefined();
    expect(generateBlocker([running], undefined)).toBe(running);
  });
});

describe('skipNotes', () => {
  const t = i18n.getFixedT('en');
  const skip = (reason: string, n: number) => ({
    request: { kind: 'chapters.detect', library_id: 1, path: `A/Book ${n}` },
    reason,
  });

  it('words each reason once, naming its books, the first few then how many more', () => {
    const out = {
      skipped: [
        skip('parked', 1),
        ...[2, 3, 4, 5, 6, 7].map((n) => skip('generate_open', n)),
        skip('parked', 8),
      ],
    };
    expect(skipNotes(out, t, 'en')).toEqual([
      'Waiting for attention on the Bridge: Book 1 and Book 8.',
      'Being generated, which finds the chapters too: Book 2, Book 3, Book 4, Book 5, and 2 more.',
    ]);
  });
});

// Phase 2b: the contributions module's gates, Share with the community, and
// the results of the share and corrections jobs.

/** An online Bridge offering sidecars, with the contributions module on or off. */
function contributing(on: boolean): ServerInfo {
  const base = server({ bridge: true, features: ['bridge', 'sidecars', 'contribute'] });
  return { ...base, capabilities: { ...base.capabilities, contributions: on } };
}

const localOnly = {
  community: { characters: false, recaps: false, description: false },
  local: { characters: true, recaps: true, description: false },
};

describe('contributing (Phase 2b)', () => {
  it('reads the module from the capability, absent or off as off', () => {
    expect(contributionsOn(contributing(true))).toBe(true);
    expect(contributionsOn(contributing(false))).toBe(false);
    expect(contributionsOn(server({ bridge: true }))).toBe(false);
    expect(contributionsOn(undefined)).toBe(false);
  });

  it('offers the contribute switch only with the module on and a Bridge that can contribute', () => {
    expect(contributeOffered(contributing(true), true)).toBe(true);
    expect(contributeOffered(contributing(true), false)).toBe(false);
    expect(contributeOffered(contributing(false), true)).toBe(false);
  });

  it('follows the share and corrections kinds on the book page, polling while they work', () => {
    expect(isBookKind(KIND_CONTRIBUTE)).toBe(true);
    expect(isBookKind(KIND_CORRECT)).toBe(true);
    expect(generatePollMs([job('running', { kind: KIND_CONTRIBUTE })])).toBe(GENERATE_POLL_MS);
    expect(generatePollMs([job('queued', { kind: KIND_CORRECT })])).toBe(GENERATE_POLL_MS);
  });

  it("doesn't hold a description back for an open share or corrections job", () => {
    const e = estimate({
      can_describe: true,
      has: { ...localOnly, community_unknown: false },
    });
    expect(describeOffered(e, [job('running', { kind: KIND_CONTRIBUTE })])).toBe(true);
    expect(describeOffered(e, [job('running', { kind: KIND_CORRECT })])).toBe(true);
    expect(describeOffered(e, [job('running', { kind: KIND_GENERATE })])).toBe(false);
    // Nor for the estimate's open job when that is the share (its generate blocker).
    const sharing = { ...e, open_job: job('running', { kind: KIND_CONTRIBUTE }) };
    expect(describeOffered(sharing, undefined)).toBe(true);
    expect(describeOffered({ ...e, open_job: job('running', { kind: KIND_DETECT }) }, [])).toBe(
      false,
    );
    expect(describeOffered({ ...e, open_job: job('needs_attention') }, [])).toBe(false);
  });

  it("takes a finished share job's links, and drops them after a newer run kept here", () => {
    const issue = 'https://github.com/KodeStar/audiosilo-meta-community/issues/4';
    const share = job('done', {
      id: 12,
      kind: KIND_CONTRIBUTE,
      result: { contributed: { recaps: issue } },
    });
    expect(contributedLinks([share])).toEqual({ recaps: issue });
    // Newest first: a local regenerate since made new content, not yet offered.
    const regen = job('done', { id: 13, result: { characters: 3, recaps: 2 } });
    expect(contributedLinks([regen, share])).toEqual({});
    // A failed share since leaves the earlier links waiting.
    expect(contributedLinks([job('failed', { id: 14, kind: KIND_CONTRIBUTE }), share])).toEqual({
      recaps: issue,
    });
    // A share of one dimension leaves the run's issue for the other standing.
    const run = job('done', {
      id: 11,
      result: { characters: 3, contributed: { characters: 'https://x.test/c' } },
    });
    expect(contributedLinks([share, run])).toEqual({
      characters: 'https://x.test/c',
      recaps: issue,
    });
    // The newest issue for a dimension wins.
    const again = job('done', {
      id: 15,
      kind: KIND_CONTRIBUTE,
      result: { contributed: { recaps: 'https://x.test/r2' } },
    });
    expect(contributedLinks([again, share, run])).toEqual({
      characters: 'https://x.test/c',
      recaps: 'https://x.test/r2',
    });
    // Nothing before the newest run counts, even for a dimension still without an issue.
    const kept = job('done', { id: 10, result: { characters: 3, recaps: 2 } });
    const older = job('done', {
      id: 7,
      result: { characters: 2, contributed: { characters: 'https://x.test/old' } },
    });
    expect(contributedLinks([share, kept, older])).toEqual({ recaps: issue });
  });

  it('offers each dimension generated here that the community lacks and nobody filed', () => {
    expect(shareDimensions(localOnly, {})).toEqual(['characters', 'recaps']);
    expect(shareDimensions(localOnly, { characters: 'https://x.test/1' })).toEqual(['recaps']);
    expect(
      shareDimensions(
        { ...localOnly, community: { characters: true, recaps: false, description: false } },
        {},
      ),
    ).toEqual(['recaps']);
    expect(
      shareDimensions(
        { ...localOnly, local: { characters: false, recaps: false, description: false } },
        {},
      ),
    ).toEqual([]);
  });

  it('offers Share only with the module on, a Bridge that can contribute and nothing open', () => {
    const e = estimate({ has: localOnly });
    expect(shareOffered(contributing(true), e, [])).toEqual(['characters', 'recaps']);
    // The module off, the Bridge away, or it can't contribute: nothing.
    expect(shareOffered(contributing(false), e, [])).toEqual([]);
    expect(shareOffered(server({ bridge: true }), e, [])).toEqual([]);
    expect(
      shareOffered(contributing(true), estimate({ has: localOnly, can_contribute: false }), []),
    ).toEqual([]);
    // The community couldn't be asked: never "lacks".
    expect(
      shareOffered(
        contributing(true),
        estimate({ has: { ...localOnly, community_unknown: true } }),
        [],
      ),
    ).toEqual([]);
    // A generate or share job open for the book (the server refuses one then).
    expect(shareOffered(contributing(true), e, [job('running')])).toEqual([]);
    expect(shareOffered(contributing(true), e, [job('queued', { kind: KIND_CONTRIBUTE })])).toEqual(
      [],
    );
    expect(
      shareOffered(
        contributing(true),
        estimate({ has: localOnly, open_job: job('queued') }),
        undefined,
      ),
    ).toEqual([]);
    // A detection or a correction open doesn't stop it.
    expect(shareOffered(contributing(true), e, [job('running', { kind: KIND_CORRECT })])).toEqual([
      'characters',
      'recaps',
    ]);
    // Already filed by the last run: nothing left to offer.
    const filed = job('done', {
      result: { contributed: { characters: 'https://x.test/1', recaps: 'https://x.test/2' } },
    });
    expect(shareOffered(contributing(true), e, [filed])).toEqual([]);
    expect(shareOffered(contributing(true), undefined, [])).toEqual([]);
    // Before the book's jobs load (or when they can't), what was filed isn't known.
    expect(shareOffered(contributing(true), e, undefined)).toEqual([]);
  });

  it('queues the share for the dimensions still to offer', () => {
    expect(shareRequest({ library_id: 2, path: 'A/B' }, ['recaps'])).toEqual({
      kind: KIND_CONTRIBUTE,
      library_id: 2,
      path: 'A/B',
      params: { dimensions: ['recaps'] },
    });
  });

  it("reads a share job's result: links and what the community had", () => {
    expect(
      generateResult({
        library_id: 1,
        path: 'A/B',
        work_id: 'w1',
        contributed: {
          characters: 'https://github.com/x/y/issues/1',
          recaps: 'javascript:alert(1)',
        },
        covered: ['recaps', ''],
      }),
    ).toEqual({
      contributed: [{ what: 'characters', url: 'https://github.com/x/y/issues/1' }],
      covered: ['recaps'],
    });
    // Nothing filed, everything covered: still a result worth wording.
    expect(generateResult({ contributed: {}, covered: ['characters', 'recaps'] })).toEqual({
      contributed: [],
      covered: ['characters', 'recaps'],
    });
  });

  it("reads a corrections job's result: the issue (http(s) only), the count, who applies what", () => {
    expect(
      generateResult({
        issue_url: 'https://github.com/KodeStar/audiosilo-meta/issues/88',
        issue_number: 88,
        changes: 3,
        auto: ['title', 'published'],
        maintainer: ['author'],
      }),
    ).toEqual({
      contributed: [],
      issue: { url: 'https://github.com/KodeStar/audiosilo-meta/issues/88', number: 88 },
      changes: 3,
      auto: ['title', 'published'],
      maintainer: ['author'],
    });
    // The changes as a list count too; a bad link is dropped.
    expect(generateResult({ issue_url: 'data:text/html,x', changes: [{}, {}] })).toEqual({
      contributed: [],
      changes: 2,
    });
  });
});

describe('a share job and Generate (Phase 2b)', () => {
  it('holds Generate back while a share is queued or working, not once parked or stopping', () => {
    const share = (state: BridgeJobState, over: Partial<BridgeJob> = {}) =>
      job(state, { kind: KIND_CONTRIBUTE, cancel_requested: false, ...over });
    expect(blocksGenerate(share('queued'))).toBe(true);
    expect(blocksGenerate(share('running'))).toBe(true);
    expect(blocksGenerate(share('needs_attention'))).toBe(false);
    expect(blocksGenerate(share('running', { cancel_requested: true }))).toBe(false);
    expect(generateBlocker([share('running')], undefined)?.kind).toBe(KIND_CONTRIBUTE);
    // Before the job list loads, the estimate's open job says it.
    expect(generateBlocker(undefined, share('queued'))?.kind).toBe(KIND_CONTRIBUTE);
  });

  it('lets a share queued while the contributions module is off give way, as the server does', () => {
    const share = (state: BridgeJobState) =>
      job(state, { kind: KIND_CONTRIBUTE, cancel_requested: false });
    // Held: no claim hands it out, and the generate job cancels it.
    expect(blocksGenerate(share('queued'), false)).toBe(false);
    expect(generateBlocker([share('queued')], undefined, false)).toBeUndefined();
    expect(generateBlocker(undefined, share('queued'), false)).toBeUndefined();
    // One the Bridge took still blocks, and so does a queued chapter detection.
    expect(blocksGenerate(share('claimed'), false)).toBe(true);
    expect(blocksGenerate(share('running'), false)).toBe(true);
    expect(
      blocksGenerate(job('queued', { kind: KIND_DETECT, cancel_requested: false }), false),
    ).toBe(true);
    // The module on, a queued share blocks.
    expect(generateBlocker([share('queued')], undefined, true)?.kind).toBe(KIND_CONTRIBUTE);
  });

  it("never offers a second Share while the estimate's open job is one", () => {
    const e = estimate({
      has: localOnly,
      open_job: job('queued', { kind: KIND_CONTRIBUTE }),
    });
    expect(shareOffered(contributing(true), e, undefined)).toEqual([]);
  });
});

describe('cancelledView (BRIDGE-02)', () => {
  const cancelled = (
    over: Partial<Pick<BridgeJob, 'error' | 'cancel_requested' | 'progress'>>,
  ) => ({
    error: '',
    cancel_requested: true,
    progress: {},
    ...over,
  });

  it("gives the server's own reason as it is", () => {
    expect(
      cancelledView(cancelled({ error: 'the book moved onto another job of this kind' })),
    ).toEqual({ byAdmin: false, reason: 'the book moved onto another job of this kind' });
  });

  it("names an admin's cancel and the park, never the raw error", () => {
    const v = cancelledView(
      cancelled({
        error: 'supervisor: parked book requires a bounded recovery plan',
        progress: { park: { code: 'chapters_not_detected', hint: 'ignored: own wording' } },
      }),
    );
    expect(v).toEqual({ byAdmin: true, park: { code: 'chapters_not_detected' } });
    expect(
      cancelledView(
        cancelled({ progress: { park: { code: 'budget_exceeded', hint: 'Raise it.' } } }),
      ),
    ).toEqual({ byAdmin: true, park: { code: 'budget_exceeded', hint: 'Raise it.' } });
  });

  it('says nothing of an admin when none asked', () => {
    expect(cancelledView(cancelled({ cancel_requested: false, error: 'raw' }))).toEqual({
      byAdmin: false,
    });
  });
});
