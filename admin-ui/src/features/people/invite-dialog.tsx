import { useRef, useState } from 'react';
import { useForm } from 'react-hook-form';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Info, Send, UserPlus } from 'lucide-react';
import { api } from '@/api/client';
import { invalidatePeople, useLibraries, useShares } from '@/api/hooks';
import type { AuthCode, InviteCreated, User } from '@/api/types';
import { Notice } from '@/components/notice';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogBody,
  DialogClose,
  DialogContent,
  DialogFooter,
  DialogFormFooter,
} from '@/components/ui/dialog';
import { Field, FormError } from '@/components/ui/field';
import { describedBy } from '@/lib/a11y';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import { RadioCards, type RadioCardOption } from '@/components/ui/radio-cards';
import { errorMessage, toastError } from '@/lib/errors';
import { formatNumber } from '@/lib/format';
import { InviteCard } from './invite-card';
import { grantAccess } from './grant-access';
import { accessValue, inviteStatus, parseAccessChoice, wholeLibraryOf } from './people-model';

const USES = [1, 3, 5, 10, 0] as const; // 0 = unlimited
const DAYS = [1, 7, 30, 0] as const; // 0 = never

/** A minted or rotated invite, ready to show, with whom it pairs. */
export interface ShownInvite extends InviteCreated {
  name: string;
}

/**
 * "Invite someone": either a new person (an account with no password, what they
 * can listen to, and an invite) or another device for an existing `user`. Ends on
 * the invite card with the QR code, link and code.
 */
export function InviteDialog({
  open,
  onOpenChange,
  user,
  codes,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Invite an existing account (pair another device) instead of a new person. */
  user?: User;
  /** That account's invites, to warn that a new one replaces the active one. */
  codes?: AuthCode[];
}) {
  const { t } = useTranslation();
  const [shown, setShown] = useState<ShownInvite>();
  // Back to the form only once the close has finished: swapping the content while
  // the dialog animates out mounts a fresh popup that Base UI never unmounts.
  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      onOpenChangeComplete={(o) => !o && setShown(undefined)}
    >
      {shown ? (
        <InviteResultContent invite={shown} />
      ) : (
        <DialogContent
          icon={UserPlus}
          title={user ? t('invite.titleFor', { name: user.username }) : t('invite.title')}
          description={
            user ? t('invite.descriptionFor', { name: user.username }) : t('invite.description')
          }
        >
          {open ? <InviteForm user={user} codes={codes} onCreated={setShown} /> : null}
        </DialogContent>
      )}
    </Dialog>
  );
}

/** The invite card in a dialog, with Done. Shared with Rotate. */
export function InviteResultContent({ invite }: { invite: ShownInvite }) {
  const { t } = useTranslation();
  return (
    <DialogContent
      icon={Send}
      tone="success"
      title={t('invite.readyTitle', { name: invite.name })}
      description={t('invite.readyDescription')}
    >
      <DialogBody>
        <InviteCard invite={invite} />
      </DialogBody>
      <DialogFooter>
        <DialogClose render={<Button />}>{t('common.done')}</DialogClose>
      </DialogFooter>
    </DialogContent>
  );
}

interface Values {
  name: string;
  access: string;
  uses: string;
  days: string;
}

function InviteForm({
  user,
  codes,
  onCreated,
}: {
  user?: User;
  codes?: AuthCode[];
  onCreated: (invite: ShownInvite) => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const libraries = useLibraries();
  const shares = useShares();
  const named = (shares.data ?? []).filter((s) => wholeLibraryOf(s) === undefined);
  const libs = libraries.data ?? [];

  const accessOptions: RadioCardOption<string>[] = [
    ...(libs.length
      ? [
          {
            value: 'all',
            title: t('invite.access.all'),
            description: t('invite.access.allBody', { count: libs.length }),
          },
        ]
      : []),
    ...(libs.length > 1
      ? libs.map((l) => ({
          value: accessValue({ kind: 'library', id: l.id }),
          title: l.name,
          description: t('invite.access.libraryBody', {
            count: l.book_count,
            formatted: formatNumber(l.book_count, lang),
          }),
        }))
      : []),
    ...named.map((s) => ({
      value: accessValue({ kind: 'share', id: s.id }),
      title: s.name,
      description: t('invite.access.shareBody', { count: s.paths?.length ?? 0 }),
    })),
    { value: 'none', title: t('invite.access.none'), description: t('invite.access.noneBody') },
  ];

  const form = useForm<Values>({ defaultValues: { name: '', access: '', uses: '5', days: '1' } });
  // A new person's account, once created: retrying after a failed invite reuses
  // it instead of tripping over its own username.
  const createdUser = useRef<User>(undefined);
  const { errors, isSubmitting } = form.formState;
  const access = form.watch('access') || accessOptions[0].value;
  const hasActive = (codes ?? []).some((c) => inviteStatus(c, Date.now()) === 'active');

  const onSubmit = form.handleSubmit(async (v) => {
    const maxUses = Number(v.uses);
    const ttlDays = Number(v.days);
    let target = user ?? createdUser.current;
    if (!target) {
      const name = v.name.trim();
      if (!name) {
        form.setError('name', { message: t('invite.nameRequired') });
        return;
      }
      try {
        target = await api.createUser({ username: name, password: '', role: 'user' });
        createdUser.current = target;
      } catch (err) {
        form.setError('name', { message: errorMessage(err, t) });
        return;
      }
      try {
        await grantChosen(target.id, access);
      } catch (err) {
        // The account exists either way; say what's missing and keep going.
        toastError(t('invite.accessFailed'), err);
      }
    }
    try {
      const created = await api.createInvite(target.id, { max_uses: maxUses, ttl_days: ttlDays });
      invalidatePeople(qc);
      onCreated({ ...created, name: target.username });
    } catch (err) {
      invalidatePeople(qc);
      form.setError('root', { message: errorMessage(err, t) });
    }
  });

  // "All libraries" grants each library there is now (in parallel: a handful of
  // requests); a single library or share is one grant; "Decide later" is none.
  const grantChosen = (userId: number, choice: string) => {
    if (choice === 'all') {
      return Promise.all(libs.map((l) => grantAccess(userId, { kind: 'library', id: l.id })));
    }
    const picked = parseAccessChoice(choice);
    return picked ? grantAccess(userId, picked) : Promise.resolve();
  };

  const nameError = errors.name?.message;
  return (
    <form onSubmit={(e) => void onSubmit(e)} noValidate className="contents">
      <DialogBody className="flex flex-col gap-4">
        {!user && !createdUser.current ? (
          <>
            <Field
              htmlFor="invite-name"
              label={t('invite.name')}
              description={t('invite.nameHint')}
              error={nameError}
            >
              <Input
                id="invite-name"
                autoComplete="off"
                placeholder={t('invite.namePlaceholder')}
                aria-invalid={nameError ? true : undefined}
                aria-describedby={describedBy('invite-name', !!nameError, true)}
                {...form.register('name')}
              />
            </Field>
            <div className="flex flex-col gap-1.5">
              <span className="text-[13px] font-semibold">{t('invite.access.label')}</span>
              <RadioCards
                label={t('invite.access.label')}
                value={access}
                onValueChange={(v) => form.setValue('access', v)}
                options={accessOptions}
              />
            </div>
          </>
        ) : null}
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <Field htmlFor="invite-uses" label={t('invite.devices')}>
            <NativeSelect id="invite-uses" {...form.register('uses')}>
              {USES.map((n) => (
                <option key={n} value={n}>
                  {n ? t('invite.uses', { count: n }) : t('invite.usesUnlimited')}
                </option>
              ))}
            </NativeSelect>
          </Field>
          <Field htmlFor="invite-days" label={t('invite.expiresAfter')}>
            <NativeSelect id="invite-days" {...form.register('days')}>
              {DAYS.map((n) => (
                <option key={n} value={n}>
                  {n ? t('invite.days', { count: n }) : t('invite.never')}
                </option>
              ))}
            </NativeSelect>
          </Field>
        </div>
        {user && hasActive ? (
          <Notice tone="info" icon={Info}>
            {t('invite.replaces', { name: user.username })}
          </Notice>
        ) : null}
        <FormError>{errors.root?.message}</FormError>
      </DialogBody>
      <DialogFormFooter busy={isSubmitting} submitLabel={t('invite.create')} />
    </form>
  );
}
