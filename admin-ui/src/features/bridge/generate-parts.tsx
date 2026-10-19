import { useId, useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import { LockKeyhole, Unplug, X } from 'lucide-react';
import type { BridgeJob } from '@/api/types';
import { Notice } from '@/components/notice';
import { SettingRow } from '@/components/setting-row';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Switch } from '@/components/ui/switch';
import { formatDuration, formatNumber } from '@/lib/format';
import { cancelJob } from './bridge-actions';
import { stateLook } from './bridge-model';
import type { Contribute } from './generate-model';

// The pieces the book's generate dialog and the series batch share, the
// Community card's generate and describe sections, and Health's Detect chapters.

/**
 * A Community card section that follows a job: its heading, with the job's
 * state badge when a job is passed (the caller passes one only while it shows).
 */
export function JobSection({
  id,
  title,
  job,
  children,
}: {
  id: string;
  title: string;
  job?: BridgeJob;
  children: ReactNode;
}) {
  const { t } = useTranslation();
  const look = job ? stateLook(job) : undefined;
  return (
    <section className="flex flex-col gap-2.5 border-t pt-3.5" aria-labelledby={id}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h4 id={id} className="text-[13px] font-semibold">
          {title}
        </h4>
        {look ? <Badge variant={look.tone}>{t(look.key)}</Badge> : null}
      </div>
      {children}
    </section>
  );
}

/** Cancels an open job, disabled while the request is out. */
export function JobCancelButton({ job, ariaLabel }: { job: BridgeJob; ariaLabel: string }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [cancelling, setCancelling] = useState(false);
  return (
    <Button
      variant="ghost"
      size="sm"
      disabled={cancelling}
      onClick={() => {
        setCancelling(true);
        void cancelJob(qc, job).finally(() => setCancelling(false));
      }}
      aria-label={ariaLabel}
    >
      <X aria-hidden="true" />
      {t('bridge.jobs.cancel')}
    </Button>
  );
}

/**
 * The contribute choice: this server only (off) or offered to the community too
 * (on, the default whenever it is offered: Phase 2b).
 */
export function ContributeSwitch({
  value,
  onChange,
}: {
  value: Contribute;
  onChange: (value: Contribute) => void;
}) {
  const { t } = useTranslation();
  const id = useId();
  return (
    <div className="rounded-lg border px-4">
      <SettingRow
        title={t('bridge.generate.contribute')}
        description={t('bridge.generate.contributeBody')}
        htmlFor={id}
        descriptionId={`${id}-desc`}
      >
        <Switch
          id={id}
          aria-describedby={`${id}-desc`}
          checked={value === 'issue'}
          onCheckedChange={(on) => onChange(on ? 'issue' : 'local')}
        />
      </SettingRow>
    </div>
  );
}

/** Nothing can be queued while the Bridge can't write to the library: why, and where to fix it. */
export function NotWritableNotice() {
  const { t } = useTranslation();
  return (
    <Notice
      tone="bad"
      icon={LockKeyhole}
      role="alert"
      title={t('bridge.generate.notWritable')}
      actions={
        <Link
          to="/server/{-$section}"
          params={{ section: undefined }}
          search={{ topic: 'bridge' }}
          className="text-[13px] font-semibold text-brand-ink hover:underline"
        >
          {t('bridge.generate.openSettings')}
        </Link>
      }
    >
      {t('bridge.generate.notWritableBody')}
    </Notice>
  );
}

/** The Bridge went offline while a confirmation is open: what is queued waits for it. */
export function OfflineNotice() {
  const { t } = useTranslation();
  return (
    <Notice tone="warn" icon={Unplug} title={t('bridge.generate.offline')}>
      {t('bridge.generate.offlineBody')}
    </Notice>
  );
}

/** A figure the Bridge's figures can't give yet. */
export function NoEstimate() {
  const { t } = useTranslation();
  return <span className="text-muted-foreground">{t('bridge.generate.noEstimate')}</span>;
}

/** A confirmation's figure (children) for every run it queues, naming how many books when more than one. */
export function ForBooks({ count, children }: { count: number; children: string }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  return count > 1
    ? t('bridge.generate.forBooks', { count, n: formatNumber(count, lang), what: children })
    : children;
}

/** A confirmation's Length fact for seconds of audio, none while the length is unknown. */
// eslint-disable-next-line react-refresh/only-export-components -- a FactList row, not a component
export function lengthRow(t: TFunction, lang: string, seconds: number): [string, ReactNode][] {
  return seconds > 0 ? [[t('bridge.generate.length'), formatDuration(seconds, lang)]] : [];
}
