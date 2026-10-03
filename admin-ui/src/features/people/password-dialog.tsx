import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { z } from 'zod';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { KeyRound, ShieldCheck } from 'lucide-react';
import { api } from '@/api/client';
import { invalidatePeople } from '@/api/hooks';
import type { User } from '@/api/types';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { Field } from '@/components/ui/field';
import { describedBy } from '@/lib/a11y';
import { Input } from '@/components/ui/input';
import { errorMessage, fieldMessage } from '@/lib/errors';
import { toast } from '@/lib/toast';

/** The server's rule (auth.ErrPasswordTooShort); checked here first so the field can say so. */
const MIN_PASSWORD = 8;

const schema = z
  .object({
    password: z.string().min(MIN_PASSWORD, 'errors.passwordTooShort'),
    confirm: z.string(),
  })
  .refine((v) => v.password === v.confirm, {
    path: ['confirm'],
    message: 'user.password.mismatch',
  });
type Values = z.infer<typeof schema>;

/**
 * Sets or changes a person's password. With `promote`, it also makes them an
 * admin in the same request (an admin must have a password).
 */
export function PasswordDialog({
  open,
  onOpenChange,
  user,
  promote = false,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  user: User;
  promote?: boolean;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={promote ? ShieldCheck : KeyRound}
        title={
          promote
            ? t('user.password.promoteTitle', { name: user.username })
            : user.has_password
              ? t('user.password.changeTitle', { name: user.username })
              : t('user.password.setTitle', { name: user.username })
        }
        description={
          promote ? t('user.password.promoteDescription') : t('user.password.description')
        }
      >
        {open ? (
          <PasswordForm user={user} promote={promote} onDone={() => onOpenChange(false)} />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function PasswordForm({
  user,
  promote,
  onDone,
}: {
  user: User;
  promote: boolean;
  onDone: () => void;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { password: '', confirm: '' },
  });
  const { errors, isSubmitting } = form.formState;

  const onSubmit = form.handleSubmit(async (v) => {
    try {
      // Password first in the same PATCH: the server applies it before the role,
      // so promoting a password-less account passes the admin-password rule.
      await api.updateUser(
        user.id,
        promote ? { password: v.password, role: 'admin' } : { password: v.password },
      );
      invalidatePeople(qc);
      toast.add({
        title: promote
          ? t('user.account.nowAdmin', { name: user.username })
          : t('user.password.saved', { name: user.username }),
        type: 'success',
      });
      onDone();
    } catch (err) {
      form.setError('password', { message: errorMessage(err, t) });
    }
  });

  const pwError = fieldMessage(errors.password?.message, t);
  const confirmError = fieldMessage(errors.confirm?.message, t);

  return (
    <form onSubmit={(e) => void onSubmit(e)} noValidate className="contents">
      <DialogBody className="flex flex-col gap-4">
        <Field
          htmlFor="new-password"
          label={t('user.password.new')}
          description={t('user.password.hint', { count: MIN_PASSWORD })}
          error={pwError}
        >
          <Input
            id="new-password"
            type="password"
            autoComplete="new-password"
            aria-invalid={pwError ? true : undefined}
            aria-describedby={describedBy('new-password', !!pwError, true)}
            {...form.register('password')}
          />
        </Field>
        <Field htmlFor="confirm-password" label={t('user.password.confirm')} error={confirmError}>
          <Input
            id="confirm-password"
            type="password"
            autoComplete="new-password"
            aria-invalid={confirmError ? true : undefined}
            aria-describedby={describedBy('confirm-password', !!confirmError, false)}
            {...form.register('confirm')}
          />
        </Field>
      </DialogBody>
      <DialogFormFooter
        busy={isSubmitting}
        submitLabel={promote ? t('user.password.promoteAction') : t('user.password.save')}
      />
    </form>
  );
}
