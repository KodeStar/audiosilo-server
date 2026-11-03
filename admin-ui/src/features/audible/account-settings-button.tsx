import { useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Settings2 } from 'lucide-react';
import { AUDIBLE_POLICIES, type AudibleAccount, type AudiblePolicy } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Button } from '@/components/ui/button';
import { Field } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import { accountName } from './audible-model';
import type { Backups } from './use-audible';

/**
 * An account's backup settings (Bridge Phase 4b): what it backs up by itself
 * after each check (new purchases only: the books it already had are "Back up
 * everything"'s) and the placement template. The person and library stay the
 * Bridge's.
 */
export function AccountSettingsButton({
  account,
  backups,
}: {
  account: AudibleAccount;
  backups: Backups;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
        <Settings2 aria-hidden="true" />
        {t('audible.settings.action')}
      </Button>
      {open ? (
        <SettingsDialog account={account} backups={backups} onClose={() => setOpen(false)} />
      ) : null}
    </>
  );
}

function SettingsDialog({
  account,
  backups,
  onClose,
}: {
  account: AudibleAccount;
  backups: Backups;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const id = useId();
  const [policy, setPolicy] = useState<AudiblePolicy>(account.policy);
  const [template, setTemplate] = useState(account.template);
  return (
    <ConfirmDialog
      open
      onOpenChange={(o) => !o && onClose()}
      icon={Settings2}
      tone="brand"
      destructive={false}
      title={t('audible.settings.title', { name: accountName(t, account) })}
      description={t('audible.settings.description')}
      confirmLabel={t('audible.settings.save')}
      onConfirm={() =>
        backups.saveAccount(account, {
          ...(policy !== account.policy ? { policy } : {}),
          ...(template.trim() !== account.template ? { template: template.trim() } : {}),
        })
      }
    >
      <Field
        htmlFor={`${id}-policy`}
        label={t('audible.settings.policy')}
        description={t('audible.settings.policyHint')}
      >
        <NativeSelect
          id={`${id}-policy`}
          value={policy}
          onChange={(e) => setPolicy(e.target.value as AudiblePolicy)}
          aria-describedby={`${id}-policy-desc`}
        >
          {AUDIBLE_POLICIES.map((p) => (
            <option key={p} value={p}>
              {t(`audible.policy.${p}`)}
            </option>
          ))}
        </NativeSelect>
      </Field>
      <Field
        htmlFor={`${id}-template`}
        label={t('audible.settings.template')}
        description={t('audible.settings.templateHint')}
      >
        <Input
          id={`${id}-template`}
          value={template}
          autoComplete="off"
          spellCheck={false}
          placeholder="{Author}/{Series}/{ {PaddedSeq} - }{Title}"
          onChange={(e) => setTemplate(e.target.value)}
          aria-describedby={`${id}-template-desc`}
        />
      </Field>
      <p className="text-[12.5px] text-muted-foreground">{t('audible.legal')}</p>
    </ConfirmDialog>
  );
}
