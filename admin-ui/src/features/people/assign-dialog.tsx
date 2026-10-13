import { useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { House, Info, TriangleAlert } from 'lucide-react';
import { api } from '@/api/client';
import { invalidateDevices, invalidatePeople, useUsers } from '@/api/hooks';
import type { AdminUser, MemberAccess, MemberKind } from '@/api/types';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { Field, FormError } from '@/components/ui/field';
import { NativeSelect } from '@/components/ui/native-select';
import { RadioCards } from '@/components/ui/radio-cards';
import { errorMessage } from '@/lib/errors';
import { toast } from '@/lib/toast';
import {
  assignPatch,
  assignTargets,
  defaultAccess,
  displayName,
  namedApart,
} from './household-model';

/**
 * "Assign to someone": put a person in another person's household, choosing
 * whether they're a kid or a grown-up and what they can listen to. For a member
 * this moves them to a new owner.
 */
export function AssignDialog({
  open,
  onOpenChange,
  user,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  user: AdminUser;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={House}
        title={t('user.household.assignTitle', { name: user.username })}
        description={t('user.household.assignDescription')}
      >
        {open ? <AssignForm user={user} onDone={() => onOpenChange(false)} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function AssignForm({ user, onDone }: { user: AdminUser; onDone: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const users = useUsers();
  const id = useId();
  const targets = users.data ? assignTargets(users.data, user) : [];
  const [ownerId, setOwnerId] = useState('');
  const [kind, setKind] = useState<MemberKind>(user.member_kind ?? 'grownup');
  // The access follows the kind (a kid starts on shared collections) until it's picked.
  const [access, setAccess] = useState<MemberAccess>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const owner = targets.find((u) => String(u.id) === ownerId);
  // Until someone is picked the copy has its own strings: "the person you pick"
  // can't stand in for a name ("von/di/de {{owner}}" breaks the grammar).
  const ownerLabel = owner ? displayName(owner) : undefined;
  const ownerText = (key: string, vars: Record<string, string> = {}) =>
    ownerLabel
      ? t(`user.household.${key}`, { ...vars, owner: ownerLabel })
      : t(`user.household.unpicked.${key}`, vars);
  const chosenAccess = access ?? user.member_access ?? defaultAccess(kind);
  // A kid has no password, API keys or pairing invites: the server clears them.
  const clears = kind === 'kid' && user.member_kind !== 'kid';

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!owner) return;
    setBusy(true);
    setError(undefined);
    try {
      await api.updateUser(user.id, assignPatch(owner.id, kind, chosenAccess));
      invalidatePeople(qc);
      // A kid loses their API keys, and a move signs them out of the devices
      // their old owner set up: the device lists change too.
      invalidateDevices(qc);
      toast.add({
        title: t('user.household.assigned', { name: user.username, owner: displayName(owner) }),
        type: 'success',
      });
      onDone();
    } catch (err) {
      setError(errorMessage(err, t));
      setBusy(false);
    }
  };

  return (
    <form onSubmit={(e) => void submit(e)} className="contents">
      <DialogBody className="flex flex-col gap-4">
        {users.isPending ? (
          <div className="skel h-24" role="status" aria-label={t('common.loading')} />
        ) : users.isError && !users.data ? (
          <QueryError
            title={t('people.error')}
            error={users.error}
            onRetry={() => void users.refetch()}
          />
        ) : targets.length === 0 ? (
          <p className="text-muted-foreground">
            {t('user.household.noOwners', { name: user.username })}
          </p>
        ) : (
          <>
            <Field htmlFor={`${id}-owner`} label={t('user.household.owner')}>
              <NativeSelect
                id={`${id}-owner`}
                value={ownerId}
                onChange={(e) => setOwnerId(e.target.value)}
              >
                <option value="" disabled>
                  {t('user.household.ownerPick')}
                </option>
                {targets.map((u) => (
                  <option key={u.id} value={String(u.id)}>
                    {namedApart(displayName(u), u.username)
                      ? `${displayName(u)} (${u.username})`
                      : u.username}
                  </option>
                ))}
              </NativeSelect>
            </Field>
            <div className="flex flex-col gap-1.5">
              <span className="text-[13px] font-semibold">{t('user.household.kind')}</span>
              <RadioCards
                label={t('user.household.kind')}
                value={kind}
                onValueChange={setKind}
                className="grid grid-cols-1 gap-2 sm:grid-cols-2"
                options={(['kid', 'grownup'] as const).map((k) => ({
                  value: k,
                  title: t(`people.kind.${k}`),
                  description: t(`user.household.kindBody.${k}`),
                }))}
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <span className="text-[13px] font-semibold">{t('user.household.access')}</span>
              <RadioCards
                label={t('user.household.access')}
                value={chosenAccess}
                onValueChange={setAccess}
                options={(['all', 'collections'] as const).map((a) => ({
                  value: a,
                  title: ownerText(`accessOption.${a}`),
                  description: ownerText(`accessOptionBody.${a}`),
                }))}
              />
            </div>
            {clears ? (
              <Notice tone="warn" icon={TriangleAlert}>
                {t('user.household.kidClears', { name: user.username })}
              </Notice>
            ) : null}
            {chosenAccess === 'collections' ? (
              <Notice tone="info" icon={Info}>
                {ownerText('collectionsStart', { name: user.username })}
              </Notice>
            ) : null}
          </>
        )}
        <FormError>{error}</FormError>
      </DialogBody>
      <DialogFormFooter
        busy={busy}
        disabled={!owner}
        submitLabel={t('user.household.assignAction')}
      />
    </form>
  );
}
