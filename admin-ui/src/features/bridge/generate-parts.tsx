import { useId } from 'react';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { LockKeyhole } from 'lucide-react';
import { Notice } from '@/components/notice';
import { SettingRow } from '@/components/setting-row';
import { Switch } from '@/components/ui/switch';
import type { Contribute } from './generate-model';

// The pieces the book's generate dialog and the series batch share.

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
