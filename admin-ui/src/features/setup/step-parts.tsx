import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ArrowLeft, CircleCheck, LoaderCircle, Lock } from 'lucide-react';
import { scanActive } from '@/api/hooks';
import type { AdminLibrary, AdminSettings } from '@/api/types';
import { ProgressBar } from '@/components/progress-bar';
import { Button } from '@/components/ui/button';
import { toastError } from '@/lib/errors';
import { formatNumber, progressFraction } from '@/lib/format';
import { lockLabel, lockOf, type SettingId } from '@/features/settings/settings-model';
import { StepStop } from './step-actions';

/**
 * The step's buttons: Back on the left, Skip and the step's own action on the
 * right. Each runs once at a time; a failure is toasted and the step stays.
 */
export function StepFooter({
  back,
  onSkip,
  onContinue,
  last,
}: {
  back: (() => void) | null;
  onSkip: () => Promise<void>;
  onContinue: () => Promise<void>;
  last: boolean;
}) {
  const { t } = useTranslation();
  const [busy, setBusy] = useState<'skip' | 'continue' | null>(null);
  const run = async (which: 'skip' | 'continue', fn: () => Promise<void>) => {
    setBusy(which);
    try {
      await fn();
    } catch (err) {
      // A step that explains its own refusal throws StepStop after showing it.
      if (!(err instanceof StepStop)) toastError(t('setup.saveFailed'), err);
    } finally {
      setBusy(null);
    }
  };
  return (
    <div className="mt-6 flex flex-wrap items-center justify-between gap-3">
      <div>
        {back ? (
          <Button variant="ghost" onClick={back} disabled={busy !== null}>
            <ArrowLeft aria-hidden="true" />
            {t('setup.back')}
          </Button>
        ) : null}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Button variant="outline" disabled={busy !== null} onClick={() => void run('skip', onSkip)}>
          {busy === 'skip' ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
          {t(last ? 'setup.skipFinish' : 'setup.skip')}
        </Button>
        <Button disabled={busy !== null} onClick={() => void run('continue', onContinue)}>
          {busy === 'continue' ? (
            <LoaderCircle className="animate-spin" aria-hidden="true" />
          ) : null}
          {t(last ? 'setup.finish' : 'setup.continue')}
        </Button>
      </div>
    </div>
  );
}

/**
 * What sets a setting the environment or the desktop app sets ("Set by
 * AUDIOSILO_X", as Settings says), shown, not asked.
 */
export function LockedNote({ settings, id }: { settings: AdminSettings; id: SettingId }) {
  const { t } = useTranslation();
  const lock = lockOf(settings, id);
  if (!lock) return null;
  return (
    <span className="inline-flex items-center gap-1.5 text-[12.5px] text-muted-foreground">
      <Lock className="size-[15px]" aria-hidden="true" />
      {lockLabel(t, lock)}
    </span>
  );
}

/**
 * The scans the flow follows from the metadata step on: every library scanning
 * or waiting to, and the one the flow created once it's done.
 */
export function ScanStatus({
  libraries,
  createdId,
}: {
  libraries: AdminLibrary[];
  createdId: number | null;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const shown = libraries.filter((l) => scanActive(l) || l.id === createdId);
  if (shown.length === 0) return null;
  return (
    <section
      aria-label={t('setup.scan.label')}
      className="mb-5 flex flex-col gap-3 rounded-xl border bg-card px-5 py-4"
    >
      {shown.map((l) => {
        const active = scanActive(l);
        const fraction =
          l.scan.running && l.scan.total > 0
            ? progressFraction(l.scan.done, l.scan.total)
            : undefined;
        return (
          <div key={l.id} className="flex flex-col gap-1.5" role="status">
            <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-0.5 text-[13px]">
              <span className="inline-flex min-w-0 items-center gap-1.5 font-semibold">
                {active ? null : (
                  <CircleCheck className="size-[15px] text-success" aria-hidden="true" />
                )}
                <span className="truncate">{l.name}</span>
              </span>
              <span className="text-muted-foreground tabular-nums">
                {l.scan.running
                  ? t('setup.scan.running', {
                      done: formatNumber(l.scan.done, lang),
                      total: formatNumber(l.scan.total, lang),
                    })
                  : active
                    ? t('setup.scan.queued')
                    : t('setup.scan.done', {
                        count: l.book_count,
                        formatted: formatNumber(l.book_count, lang),
                      })}
              </span>
            </div>
            {active ? (
              <ProgressBar fraction={fraction} label={t('libraries.scanProgress')} />
            ) : null}
          </div>
        );
      })}
    </section>
  );
}
