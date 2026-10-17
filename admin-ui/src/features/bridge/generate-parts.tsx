import { useId, useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { LockKeyhole, X } from 'lucide-react';
import type { BridgeJob } from '@/api/types';
import { Notice } from '@/components/notice';
import { SettingRow } from '@/components/setting-row';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Switch } from '@/components/ui/switch';
import { cancelJob } from './bridge-actions';
import { stateLook } from './bridge-model';
import type { Contribute } from './generate-model';

// The pieces the book's generate dialog and the series batch share, and the
// Community card's generate and describe sections.

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

/** The contribute choice: this server only (off, the default) or offered to the community too. */
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
