import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { KeyRound } from 'lucide-react';
import { ApiError, WRONG_CURRENT_PASSWORD, api } from '@/api/client';
import { invalidatePeople, notePasswordChosen } from '@/api/hooks';
import { MIN_PASSWORD } from '@/api/types';
import { Notice } from '@/components/notice';
import { Field } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { describedBy } from '@/lib/a11y';
import { errorMessage } from '@/lib/errors';
import { useCurrentUser, useSession } from '@/lib/session';
import { accountPlan, type AccountDraft, type AccountField } from './setup-model';
import { StepFooter } from './step-parts';
import { StepStop, type StepProps } from './step-actions';

/** The refusals a field shows as its own (the rest are toasted). */
const USERNAME_CODES = ['username_taken', 'username_required'];

/**
 * The account step, the first: your account (server#369). The admin chooses the username and
 * password they sign in with: the username through the admin user PATCH, the
 * password through the account's own change (which asks for the current one and
 * keeps this session). While the admin still has the password the server
 * printed in its log (env.printed_password) a new one is asked for; otherwise
 * it is optional. Skip keeps both.
 */
export function AccountStep({ env, leave, back, last }: StepProps) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const me = useCurrentUser();
  const { updateUser } = useSession();
  const printed = env.printed_password ?? false;
  const [draft, setDraft] = useState<AccountDraft>({
    username: me.username,
    password: '',
    repeat: '',
    current: '',
  });
  const [errors, setErrors] = useState<Partial<Record<AccountField, string>>>({});
  const set = (field: AccountField) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setDraft((d) => ({ ...d, [field]: e.target.value }));
  const fail = (field: AccountField, message: string): never => {
    setErrors({ [field]: message });
    throw new StepStop();
  };

  const onContinue = async () => {
    const plan = accountPlan(draft, me.username, printed);
    setErrors(Object.fromEntries(Object.entries(plan.errors).map(([f, k]) => [f, t(k)])));
    if (Object.keys(plan.errors).length) throw new StepStop();
    // The password first: its current-password check is the one a typo
    // fails, and a refusal then leaves the name as it was (a rename can't be
    // checked before it is made).
    if (plan.password) {
      try {
        await api.setOwnPassword(plan.password.next, plan.password.current);
      } catch (err) {
        if (err instanceof ApiError && err.code === WRONG_CURRENT_PASSWORD) {
          fail('current', t('setup.account.currentWrong'));
        }
        if (err instanceof ApiError && err.code === 'password_too_short') {
          fail('password', errorMessage(err, t));
        }
        throw err;
      }
      // Done: a retry (the rename's refusal below) doesn't send it again, and
      // the step (now, or opened again with Back) no longer asks for one.
      notePasswordChosen(qc);
      setDraft((d) => ({ ...d, password: '', repeat: '', current: '' }));
    }
    if (plan.rename) {
      try {
        const u = await api.updateUser(me.id, { username: plan.rename });
        // The console names the admin as the server now does.
        updateUser({ ...me, username: u.username });
        invalidatePeople(qc);
      } catch (err) {
        if (err instanceof ApiError && USERNAME_CODES.includes(err.code ?? '')) {
          fail('username', errorMessage(err, t));
        }
        throw err;
      }
    }
    await leave('done');
  };

  const field = (
    id: string,
    name: AccountField,
    label: string,
    input: React.InputHTMLAttributes<HTMLInputElement>,
    description?: string,
  ) => (
    <Field htmlFor={id} label={label} description={description} error={errors[name]}>
      <Input
        id={id}
        value={draft[name]}
        onChange={set(name)}
        aria-invalid={errors[name] ? true : undefined}
        aria-describedby={describedBy(id, !!errors[name], !!description)}
        {...input}
      />
    </Field>
  );

  return (
    <>
      <div className="flex max-w-[460px] flex-col gap-5">
        {printed ? (
          <Notice tone="warn" icon={KeyRound} role="status" title={t('setup.account.printed')}>
            {t('setup.account.printedBody')}
          </Notice>
        ) : null}
        {field(
          'setup-username',
          'username',
          t('setup.account.username'),
          { autoComplete: 'username', autoCapitalize: 'none', spellCheck: false },
          t('setup.account.usernameBody'),
        )}
        {field(
          'setup-password',
          'password',
          t('user.password.new'),
          { type: 'password', autoComplete: 'new-password' },
          t(printed ? 'user.password.hint' : 'setup.account.passwordOptional', {
            count: MIN_PASSWORD,
          }),
        )}
        {draft.password !== '' ? (
          <>
            {field('setup-repeat', 'repeat', t('setup.account.repeat'), {
              type: 'password',
              autoComplete: 'new-password',
            })}
            {field(
              'setup-current',
              'current',
              t('setup.account.current'),
              { type: 'password', autoComplete: 'current-password' },
              t('setup.account.currentBody'),
            )}
          </>
        ) : null}
      </div>
      <StepFooter
        back={back}
        onSkip={() => leave('skipped')}
        onContinue={onContinue}
        last={last}
        skipLabel={t('setup.account.keep', { name: me.username })}
      />
    </>
  );
}
