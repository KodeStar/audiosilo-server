import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { RotateCw, Sparkles } from 'lucide-react';
import { useServerInfo, useSettings } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { Button } from '@/components/ui/button';
import { STORY_CARD_ID } from '@/features/book/story-model';
import { isOpen } from './bridge-model';
import { GenerateDialog } from './generate-dialog';
import {
  generateAction,
  generateAvailable,
  generateBlocker,
  generateJobsKnown,
  KIND_CONTRIBUTE,
  KIND_DETECT,
  KIND_READALONG,
} from './generate-model';
import { BridgeSettingsLink, JobActions, JobSection } from './generate-parts';
import { GenerateStatus } from './generate-status';
import { generateNeed } from './setup-model';
import { useBookGenerateJob, useBridgeStatus, useGenerateEstimate } from './use-generate';

/** The blocker kinds generating waits for in the section, with the line that says so. */
const WAIT_KEYS: Partial<
  Record<string, 'bridge.generate.afterDetect' | 'bridge.generate.afterReadalong'>
> = {
  [KIND_DETECT]: 'bridge.generate.afterDetect',
  [KIND_READALONG]: 'bridge.generate.afterReadalong',
};

/**
 * The Community card's characters-and-recaps part: the book's generate job
 * while one is open (its stage, ETA, cost; why it's parked, with a link into
 * the Bridge) or how the last one ended, and the action that opens the
 * confirmation. Offered only while an online Bridge has the sidecars feature
 * and metadata is on, and not while the book's chapters are being found or
 * its read-along is being made (a queued or working chapters.detect or
 * readalong.align job, whose status shows instead: the server refuses a
 * generate job until it has finished). When the community already has
 * both characters and recaps a line says so, with a link to the Characters and
 * recaps card, where the action would be (generateAction's `covered`). While
 * the online Bridge's setup checklist says a step generating needs is still to
 * do (speech recognition, the agent's credential, this library's mapping), a
 * line names it, with a link to Settings > Bridge, instead of the action
 * (Phase 2b). Renders nothing when there is nothing to show.
 */
export function GenerateSection({ book }: { book: AdminBook }) {
  const { t } = useTranslation();
  const server = useServerInfo().data;
  const available = generateAvailable(server);
  const { job, jobs } = useBookGenerateJob(book.library_id, book.path, generateJobsKnown(server));
  // The brief estimate: enough for what the book has and so which action to offer.
  const estimate = useGenerateEstimate(book.library_id, book.path, available, { brief: true });
  const parked = job?.state === 'needs_attention';
  const [open, setOpen] = useState(false);

  const has = estimate.data?.has;
  const action = has ? generateAction(has) : undefined;
  // Files generated here before show nowhere once the community covers both.
  const generated = action === 'regenerate';
  const jobOpen = job ? isOpen(job.state) : false;
  // The book's chapters being found, or its files being shared with the
  // community: the server refuses a generate job until that has finished (a
  // detection's transcription is reused), so it stands where the action would:
  // a detection with its status, a share with a line (the Share part shows its
  // status). A share queued while the contributions module is off is held, and
  // the server cancels it for a generate job: the toggle is the settings
  // envelope's (the server's rule), not the capability, which also needs the
  // Bridge; it counts as on until the settings arrive.
  const contributions = useSettings().data?.modules.contributions ?? true;
  const blocker = jobOpen
    ? undefined
    : generateBlocker(jobs, estimate.data?.open_job, contributions);
  // A detection or a read-along being made: the same audio on the Bridge, so
  // generating waits for it, its status in the section.
  const waitKey = blocker ? WAIT_KEYS[blocker.kind] : undefined;
  const waiting = waitKey ? blocker : undefined;
  const sharing = blocker?.kind === KIND_CONTRIBUTE;
  const idle = !jobOpen && !blocker;
  // The Bridge's status: its page for a parked job's link, its checklist for
  // what generating still needs (asked only when that line could show).
  const bridgeOnline = Boolean(server?.capabilities.bridge && server.capabilities.metadata);
  const status = useBridgeStatus(parked || (bridgeOnline && idle && action !== 'covered'));
  const uiUrl = status?.ui_url;
  const need = bridgeOnline ? generateNeed(status, book.library_id) : undefined;
  // The action: with an answer that offers it, no job open, and not while the
  // Bridge is away (it couldn't run it).
  const ready = available && idle;
  // In its place, why there is none: the community has both, or the Bridge
  // still needs setting up for it (named; said whether or not the Bridge
  // offers the kind yet, since that is why it doesn't).
  const covered = ready && action === 'covered';
  const why = need !== undefined && idle && action !== 'covered';
  const offer = ready && !why && (action === 'generate' || action === 'regenerate');
  // A cancelled job says nothing worth a line.
  const showJob = job && job.state !== 'cancelled';
  // An open dialog stays (it says when the Bridge went away), whatever else changed.
  if (!open && !showJob && !offer && !covered && !generated && !blocker && !why) return null;

  return (
    <JobSection
      id="generate-title"
      title={t('bridge.generate.title')}
      job={waiting ?? (showJob ? job : undefined)}
    >
      {generated && !jobOpen ? (
        <p className="text-[12.5px] text-muted-foreground">{t('bridge.generate.localNote')}</p>
      ) : null}
      {covered ? (
        <p className="text-[12.5px] text-muted-foreground">
          {t('bridge.generate.covered')}{' '}
          <a
            href={`#${STORY_CARD_ID}`}
            onClick={(e) => {
              e.preventDefault();
              jumpTo(STORY_CARD_ID);
            }}
            className="font-semibold text-brand-ink hover:underline"
          >
            {t('bridge.generate.coveredLink')}
          </a>
        </p>
      ) : null}
      {why ? (
        <p className="text-[12.5px] text-muted-foreground">
          {t(`bridge.generate.needs.${need}`)} <BridgeSettingsLink />
        </p>
      ) : null}
      {sharing ? (
        <p className="text-[12.5px] text-muted-foreground">{t('bridge.generate.afterShare')}</p>
      ) : null}
      {waiting && waitKey ? (
        <>
          <p className="text-[12.5px] text-muted-foreground">{t(waitKey)}</p>
          <GenerateStatus job={waiting} />
        </>
      ) : showJob && job ? (
        <GenerateStatus job={job} uiUrl={uiUrl} />
      ) : null}
      <JobActions job={job} cancelAria={t('bridge.generate.cancelAria')}>
        {offer ? (
          <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
            {action === 'regenerate' ? (
              <RotateCw aria-hidden="true" />
            ) : (
              <Sparkles aria-hidden="true" />
            )}
            {action === 'regenerate'
              ? t('bridge.generate.regenerate')
              : t('bridge.generate.action')}
          </Button>
        ) : null}
      </JobActions>
      <GenerateDialog
        open={open}
        onOpenChange={setOpen}
        book={{ library_id: book.library_id, path: book.path, title: book.title }}
        regenerate={action === 'regenerate'}
        offline={!available}
      />
    </JobSection>
  );
}

/**
 * Scrolls the element into view (smoothly, unless reduced motion is asked for)
 * and moves focus to it, so a keyboard or screen reader user lands there too.
 */
function jumpTo(id: string) {
  const el = document.getElementById(id);
  if (!el) return;
  const reduce = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
  el.scrollIntoView({ behavior: reduce ? 'auto' : 'smooth', block: 'start' });
  el.focus({ preventScroll: true });
}
