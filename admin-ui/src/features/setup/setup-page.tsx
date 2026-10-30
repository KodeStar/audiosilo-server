import { useEffect, useRef, useState } from 'react';
import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Check, CircleDashed } from 'lucide-react';
import { api } from '@/api/client';
import {
  keys,
  saveSetup,
  useLibraries,
  useScanWatcher,
  useServerInfo,
  useSettings,
  useSetup,
} from '@/api/hooks';
import {
  SETUP_STEPS,
  type AdminSettings,
  type SetupMark,
  type SetupMarks,
  type SetupState,
  type SetupStepId,
} from '@/api/types';
import { LogoTile } from '@/components/logo';
import { QueryError } from '@/components/query-error';
import { buttonVariants } from '@/components/ui/button';
import { restartList } from '@/features/settings/settings-model';
import { readStorage, removeStorage, writeStorage } from '@/lib/storage';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { AbsStep, BridgeStep, PhoneStep } from './connect-steps';
import { FolderStep } from './folder-step';
import { NamingStep } from './naming-step';
import { BackupsStep, MetadataStep, ReachStep } from './settings-steps';
import {
  finishMarks,
  folderLost,
  hasBookLibrary,
  nextStep,
  parseDraft,
  previousStep,
  resumeStep,
  stepIndex,
  type SetupDraft,
} from './setup-model';
import { ScanStatus } from './step-parts';
import type { StepProps } from './step-actions';

const STEP: Record<SetupStepId, (p: StepProps) => React.ReactNode> = {
  folder: FolderStep,
  naming: NamingStep,
  metadata: MetadataStep,
  reach: ReachStep,
  abs: AbsStep,
  backups: BackupsStep,
  phone: PhoneStep,
  bridge: BridgeStep,
};

/** Where this browser keeps the library the flow hasn't created yet. */
const DRAFT_KEY = 'audiosilo.setup.draft';

/**
 * "Set up your server" (FIRST-RUN-PLAN.md Phase F1): the console's first-run
 * flow as a full page, outside the shell. Eight steps in order, each with Back
 * and Skip; `?step=` deep-links one (the Overview card's links), and without
 * it the flow resumes at the first step not yet done or skipped. Each step
 * writes through the APIs Settings and Libraries use, then marks itself
 * (PUT /admin/setup); the last one marks the flow finished.
 */
export function SetupPage() {
  const { t } = useTranslation();
  const setup = useSetup();
  const settings = useSettings();
  const libraries = useLibraries();
  const server = useServerInfo();
  // The shell isn't mounted here: this page is what refreshes after a scan.
  useScanWatcher();

  const failed = [setup, settings, libraries].find((q) => q.isError);
  return (
    <div className="min-h-dvh bg-background">
      <header className="border-b">
        <div className="mx-auto flex h-16 max-w-[1120px] items-center justify-between gap-3 px-4 md:px-6">
          <span className="flex min-w-0 items-center gap-2.5">
            <LogoTile />
            <span className="flex min-w-0 flex-col leading-tight">
              <b className="truncate font-display text-[15px] font-semibold">
                {server.data?.name || 'AudioSilo'}
              </b>
              <span className="text-[12px] text-muted-foreground">{t('setup.eyebrow')}</span>
            </span>
          </span>
          <Link to="/" className={buttonVariants({ variant: 'ghost', size: 'sm' })}>
            {t('setup.later')}
          </Link>
        </div>
      </header>
      <main id="main" className="mx-auto max-w-[1120px] px-4 pt-6 pb-24 md:px-6 md:pt-8">
        {failed ? (
          <QueryError
            title={t('setup.error')}
            error={failed.error}
            onRetry={() => void failed.refetch()}
          />
        ) : setup.data && settings.data && libraries.data ? (
          <Flow
            state={setup.data.state}
            props={{ env: setup.data, settings: settings.data, libraries: libraries.data }}
          />
        ) : (
          <div className="flex flex-col gap-4" role="status" aria-label={t('common.loading')}>
            <span className="skel h-9 w-64" />
            <span className="skel h-[320px] rounded-xl" />
          </div>
        )}
      </main>
    </div>
  );
}

function Flow({
  state,
  props,
}: {
  state: SetupState;
  props: Pick<StepProps, 'env' | 'settings' | 'libraries'>;
}) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const { step } = useSearch({ from: '/setup' });
  const [draft, setDraftState] = useState<SetupDraft | null>(() =>
    parseDraft(readStorage(DRAFT_KEY)),
  );
  // The draft as a step last set it: a step sets it and leaves in one handler,
  // before this renders again, and where the flow goes next depends on it.
  const draftNow = useRef(draft);
  const [createdId, setCreatedId] = useState<number | null>(null);
  const facts = { hasDraft: !!draft, hasLibraries: hasBookLibrary(props.libraries) };
  // A library made on this visit clears the draft before the list has
  // refetched: the folder isn't lost then.
  const lost = createdId === null && folderLost(state.steps, facts);
  const current = resumeStep(state.steps, lost, step);
  const next = nextStep(current, state, draft);
  const prev = previousStep(current);

  // Each step opens at the top with its heading focused, as a new page would.
  const heading = useRef<HTMLHeadingElement>(null);
  const first = useRef(true);
  useEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    window.scrollTo(0, 0);
    heading.current?.focus();
  }, [current]);

  const setDraft = (d: SetupDraft | null) => {
    draftNow.current = d;
    setDraftState(d);
    if (d) writeStorage(DRAFT_KEY, JSON.stringify(d));
    else removeStorage(DRAFT_KEY);
  };
  const go = (id: SetupStepId) => void navigate({ to: '/setup', search: { step: id } });

  const leave = async (mark: SetupMark) => {
    const to = nextStep(current, state, draftNow.current);
    const finishing = to === null && !state.finished;
    if (finishing) {
      // The marks as the server has them now, not as this tab last read them:
      // a step another tab recorded since must not be marked skipped over it.
      const fresh = await qc.fetchQuery({ queryKey: keys.setup, queryFn: api.setup });
      await saveSetup(qc, {
        steps: finishMarks(fresh.state.steps, current, mark, hasBookLibrary(props.libraries)),
        finished: true,
      });
    } else {
      await saveSetup(qc, { steps: { [current]: mark } });
    }
    if (to) return go(to);
    if (finishing) {
      // A restart-only choice (a local metadata copy, the HTTPS mode) is named
      // here, and stays on the Overview until the restart.
      const saved = qc.getQueryData<AdminSettings>(keys.settings) ?? props.settings;
      toast.add({
        title: t('setup.finished'),
        description: saved.restart_pending.length
          ? t('setup.finishedRestart', {
              list: restartList(t, saved, i18n.resolvedLanguage ?? 'en'),
            })
          : t('setup.finishedBody'),
        type: saved.restart_pending.length ? 'info' : 'success',
      });
    }
    void navigate({ to: '/' });
  };

  const Step = STEP[current];
  return (
    <div className="grid grid-cols-1 gap-6 lg:grid-cols-[220px_minmax(0,1fr)] lg:gap-10">
      <Stepper
        marks={lost ? { ...state.steps, folder: undefined } : state.steps}
        current={current}
      />
      <div className="min-w-0">
        <div className="mb-5 flex flex-col gap-2">
          <div className="eyebrow">
            {t('setup.stepOf', { n: stepIndex(current) + 1, total: SETUP_STEPS.length })}
          </div>
          <h1 id="setup-step-title" ref={heading} tabIndex={-1} className="display outline-none">
            {t(`setup.step.${current}.title`)}
          </h1>
          <p className="max-w-[640px] text-[15px] text-muted-foreground">
            {t(`setup.step.${current}.why`)}
          </p>
        </div>
        {stepIndex(current) >= stepIndex('metadata') ? (
          <ScanStatus libraries={props.libraries} createdId={createdId} />
        ) : null}
        <section
          aria-labelledby="setup-step-title"
          className="rounded-xl border bg-card px-4 py-5 md:px-6 md:py-6"
        >
          <Step
            key={current}
            {...props}
            draft={draft}
            setDraft={setDraft}
            leave={leave}
            back={prev ? () => go(prev) : null}
            last={next === null}
            onCreated={setCreatedId}
            folderLost={lost}
          />
        </section>
      </div>
    </div>
  );
}

/**
 * The steps as a list: each one's mark (done, skipped), the current one in
 * pink, any of them a click away. A column beside the step on a wide screen,
 * a row of numbered dots on a phone.
 */
function Stepper({ marks, current }: { marks: SetupMarks; current: SetupStepId }) {
  const { t } = useTranslation();
  return (
    <nav aria-label={t('setup.steps')}>
      <ol className="flex gap-1.5 overflow-x-auto pb-1 lg:flex-col lg:gap-0.5 lg:overflow-visible">
        {SETUP_STEPS.map((id, i) => {
          const mark = marks[id];
          const here = id === current;
          const Mark = here
            ? null
            : mark === 'done'
              ? Check
              : mark === 'skipped'
                ? CircleDashed
                : null;
          return (
            <li key={id} className="shrink-0">
              <Link
                to="/setup"
                search={{ step: id }}
                aria-current={here ? 'step' : undefined}
                className={cn(
                  'flex items-center gap-2.5 rounded-md p-1 text-[13.5px] transition-colors duration-(--dur-1) hover:bg-accent lg:px-2 lg:py-1.5',
                  here ? 'font-semibold text-foreground' : 'text-muted-foreground',
                )}
              >
                <span
                  className={cn(
                    'grid size-7 shrink-0 place-items-center rounded-full text-[12.5px] font-bold tabular-nums',
                    here
                      ? 'bg-brand text-brand-foreground'
                      : mark === 'done'
                        ? 'bg-success-soft text-success'
                        : 'shadow-[inset_0_0_0_1.5px_var(--border-strong)]',
                  )}
                  aria-hidden="true"
                >
                  {Mark ? <Mark className="size-4" /> : i + 1}
                </span>
                <span className="sr-only lg:not-sr-only">
                  {t(`setup.step.${id}.short`)}
                  {mark ? <span className="sr-only">, {t(`setup.mark.${mark}`)}</span> : null}
                </span>
              </Link>
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
