import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { CircleCheck } from 'lucide-react';
import { ApiError, WRONG_CURRENT_PASSWORD, api } from '@/api/client';
import { invalidatePeople, notePasswordChosen, saveSetup } from '@/api/hooks';
import { MIN_PASSWORD } from '@/api/types';
import { Notice } from '@/components/notice';
import { Field } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { describedBy } from '@/lib/a11y';
import { errorMessage } from '@/lib/errors';
import { useCurrentUser, useSession } from '@/lib/session';
import { PrintedPasswordNotice } from './printed-password-notice';
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
 * it is optional. Skip keeps both. A password changed when the rename is then
 * refused stays said (server#391), and Skip then keeps the name only: the step
 * is done.
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
  // The password changed on this visit, whatever happens to the name after it.
  const [passwordChanged, setPasswordChanged] = useState(false);
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
      setPasswordChanged(true);
      setDraft((d) => ({ ...d, password: '', repeat: '', current: '' }));
    }
    // Save the completed step before trying the name, so a reload while the
    // rename is pending resumes after the account step.
    let recorded = env.state.steps.account === 'done';
    if (plan.rename) {
      if ((plan.password || passwordChanged) && !recorded) {
        await saveSetup(qc, { steps: { account: 'done' } });
        recorded = true;
      }
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
    await leave(recorded ? undefined : 'done');
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
        {passwordChanged ? (
          <Notice
            tone="info"
            icon={CircleCheck}
            role="status"
            title={t('setup.account.passwordChanged')}
          >
            {t('setup.account.passwordChangedBody', { name: me.username })}
          </Notice>
        ) : printed ? (
          <PrintedPasswordNotice />
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
        // With the password changed the step is done: Skip keeps only the name.
        onSkip={() => {
          if (env.state.steps.account === 'done') return leave();
          return leave(passwordChanged ? 'done' : 'skipped');
        }}
        onContinue={onContinue}
        last={last}
        skipLabel={t(passwordChanged ? 'setup.account.keepName' : 'setup.account.keep', {
          name: me.username,
        })}
      />
    </>
  );
}
