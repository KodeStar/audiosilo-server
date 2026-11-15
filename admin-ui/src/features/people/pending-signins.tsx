import { useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { BadgeCheck, CircleAlert, Link2, UserPlus, X } from 'lucide-react';
import { api } from '@/api/client';
import { invalidatePeople, keys, useOIDCPending, useUsers } from '@/api/hooks';
import type { AdminUser, OIDCPendingList, OIDCPendingSignIn } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { Field, FormError } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import { describedBy } from '@/lib/a11y';
import { errorMessage } from '@/lib/errors';
import { formatRelative } from '@/lib/format';
import { toast } from '@/lib/toast';
import { displayName, namedApart } from './household-model';
import {
  groupsShown,
  linkCandidates,
  pendingName,
  reasonKey,
  suggestedAccount,
} from './pending-model';

/**
 * People > Waiting sign-ins (server#235): the single sign-ons no account took
 * (an unverified email, no match, an admin's email...), newest first. Each can
 * be linked to an account, given a new one, or dismissed (the person's next
 * attempt records them again). Shown only while there are some.
 */
export function PendingSignInsCard() {
  const { t } = useTranslation();
  const pending = useOIDCPending();
  const titleId = useId();
  const [linking, setLinking] = useState<OIDCPendingSignIn | null>(null);
  const [creating, setCreating] = useState<OIDCPendingSignIn | null>(null);
  const [dismissing, setDismissing] = useState<OIDCPendingSignIn | null>(null);
  const qc = useQueryClient();

  if (pending.isError) {
    return (
      <div className="mb-6">
        <QueryError
          title={t('people.pending.error')}
          error={pending.error}
          onRetry={() => void pending.refetch()}
        />
      </div>
    );
  }
  const rows = pending.data?.pending ?? [];
  if (!rows.length) return null;

  return (
    <Card className="mb-6" aria-labelledby={titleId}>
      <CardHeader
        titleId={titleId}
        title={
          <span className="inline-flex items-center gap-2">
            {t('people.pending.title')}
            <Badge variant="warning">{rows.length}</Badge>
          </span>
        }
        description={t('people.pending.description')}
      />
      <ul className="divide-y">
        {rows.map((p) => (
          <PendingRow
            key={p.id}
            row={p}
            onLink={() => setLinking(p)}
            onCreate={() => setCreating(p)}
            onDismiss={() => setDismissing(p)}
          />
        ))}
      </ul>
      <LinkDialog
        row={linking}
        linked={pending.data?.linked_user_ids}
        onOpenChange={(o) => !o && setLinking(null)}
      />
      <CreateDialog row={creating} onOpenChange={(o) => !o && setCreating(null)} />
      <ConfirmDialog
        open={dismissing !== null}
        onOpenChange={(o) => !o && setDismissing(null)}
        icon={X}
        title={t('people.pending.dismissTitle', {
          name: dismissing ? pendingName(dismissing) : '',
        })}
        description={t('people.pending.dismissBody')}
        confirmLabel={t('people.pending.dismiss')}
        onConfirm={async () => {
          if (!dismissing) return;
          try {
            await api.dismissPending(dismissing.id);
          } finally {
            // Gone meanwhile (404) or not: the list says what is left.
            void qc.invalidateQueries({ queryKey: keys.oidcPending });
          }
          toast.add({ title: t('people.pending.dismissed'), type: 'success' });
        }}
      />
    </Card>
  );
}

function PendingRow({
  row: p,
  onLink,
  onCreate,
  onDismiss,
}: {
  row: OIDCPendingSignIn;
  onLink: () => void;
  onCreate: () => void;
  onDismiss: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const name = pendingName(p);
  const { shown, more } = groupsShown(p);
  return (
    <li className="flex flex-wrap items-start gap-x-6 gap-y-3 px-5 py-4">
      <div className="flex min-w-0 flex-1 basis-72 flex-col gap-1 text-[13px]">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <b className="truncate text-[14.5px]">{name}</b>
          {p.username && p.username !== name ? (
            <span className="truncate text-muted-foreground">{p.username}</span>
          ) : null}
        </div>
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          {p.email ? (
            <>
              <span className="truncate [overflow-wrap:anywhere]">{p.email}</span>
              {p.email_verified ? (
                <Badge variant="success">
                  <BadgeCheck aria-hidden="true" />
                  {t('people.pending.verified')}
                </Badge>
              ) : (
                <Badge variant="warning">
                  <CircleAlert aria-hidden="true" />
                  {t('people.pending.unverified')}
                </Badge>
              )}
            </>
          ) : (
            <span className="text-muted-foreground">{t('people.pending.noEmail')}</span>
          )}
        </div>
        <span>{t(reasonKey(p.reason))}</span>
        {shown.length ? (
          <span className="text-muted-foreground [overflow-wrap:anywhere]">
            {more
              ? t('people.pending.groupsMore', { groups: shown.join(', '), count: more })
              : t('people.pending.groups', { groups: shown.join(', ') })}
          </span>
        ) : null}
        <span className="text-[12px] text-subtle-foreground">
          {t('people.pending.seen', {
            first: formatRelative(p.first_seen_at, lang),
            last: formatRelative(p.last_seen_at, lang),
            count: p.attempts,
          })}
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm" variant="outline" onClick={onLink}>
          <Link2 aria-hidden="true" />
          {t('people.pending.link')}
        </Button>
        <Button size="sm" variant="outline" onClick={onCreate}>
          <UserPlus aria-hidden="true" />
          {t('people.pending.create')}
        </Button>
        <Button
          size="sm"
          variant="ghost"
          onClick={onDismiss}
          aria-label={t('people.pending.dismissAria', { name })}
        >
          <X aria-hidden="true" />
          {t('people.pending.dismiss')}
        </Button>
      </div>
    </li>
  );
}

/**
 * A link or a create from a waiting row: the account it names toasted on
 * success, the route's refusal in words otherwise. Done or refused, the list
 * and People are read again for what is left (the row taken, someone new or
 * newly linked, or a row gone meanwhile).
 */
function useRowSubmit(onDone: () => void) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(run: () => Promise<AdminUser>, toastKey: string) {
    if (busy) return;
    setBusy(true);
    setError(null);
    const settled = () => {
      void qc.invalidateQueries({ queryKey: keys.oidcPending });
      invalidatePeople(qc);
    };
    try {
      const u = await run();
      settled();
      toast.add({ title: t(toastKey, { name: u.username }), type: 'success' });
      onDone();
    } catch (err) {
      settled();
      setError(errorMessage(err, t));
      setBusy(false);
    }
  }
  return { busy, error, submit };
}

/** Links a waiting sign-in to an account the admin chooses. */
function LinkDialog({
  row,
  linked,
  onOpenChange,
}: {
  row: OIDCPendingSignIn | null;
  linked: OIDCPendingList['linked_user_ids'];
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={row !== null} onOpenChange={onOpenChange}>
      <DialogContent
        icon={Link2}
        title={t('people.pending.linkTitle', { name: row ? pendingName(row) : '' })}
        description={t('people.pending.linkBody')}
      >
        {row ? (
          <LinkForm key={row.id} row={row} linked={linked} onDone={() => onOpenChange(false)} />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function LinkForm({
  row,
  linked,
  onDone,
}: {
  row: OIDCPendingSignIn;
  linked: OIDCPendingList['linked_user_ids'];
  onDone: () => void;
}) {
  const { t } = useTranslation();
  const users = useUsers();
  const candidates = users.data ? linkCandidates(users.data, row, linked) : [];
  const noCandidates = Boolean(users.data) && !candidates.length;
  const [choice, setChoice] = useState<number | undefined>(undefined);
  const chosen = choice ?? suggestedAccount(candidates, row)?.id;
  const { busy, error, submit } = useRowSubmit(onDone);
  const id = useId();

  function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (chosen === undefined) return;
    void submit(() => api.linkPending(row.id, chosen), 'people.pending.linked');
  }

  return (
    <form onSubmit={onSubmit} noValidate className="contents">
      <DialogBody className="flex flex-col gap-4">
        <Field
          htmlFor={id}
          label={t('people.pending.account')}
          description={noCandidates ? t('people.pending.noCandidates') : undefined}
        >
          <NativeSelect
            id={id}
            value={chosen ?? ''}
            disabled={!candidates.length}
            aria-describedby={describedBy(id, false, noCandidates)}
            onChange={(e) => setChoice(e.target.value ? Number(e.target.value) : undefined)}
          >
            <option value="">{t('people.pending.choose')}</option>
            {candidates.map((u) => {
              const name = namedApart(displayName(u), u.username)
                ? `${displayName(u)} (${u.username})`
                : u.username;
              return (
                <option key={u.id} value={u.id}>
                  {u.role === 'admin' ? t('people.pending.adminOption', { name }) : name}
                </option>
              );
            })}
          </NativeSelect>
        </Field>
        <FormError>{error}</FormError>
      </DialogBody>
      <DialogFormFooter
        busy={busy}
        disabled={chosen === undefined}
        submitLabel={t('people.pending.link')}
      />
    </form>
  );
}

/** Makes an ordinary account for a waiting sign-in, its username prefilled from the sign-in. */
function CreateDialog({
  row,
  onOpenChange,
}: {
  row: OIDCPendingSignIn | null;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={row !== null} onOpenChange={onOpenChange}>
      <DialogContent
        icon={UserPlus}
        title={t('people.pending.createTitle', { name: row ? pendingName(row) : '' })}
        description={t('people.pending.createBody')}
      >
        {row ? <CreateForm key={row.id} row={row} onDone={() => onOpenChange(false)} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function CreateForm({ row, onDone }: { row: OIDCPendingSignIn; onDone: () => void }) {
  const { t } = useTranslation();
  const [username, setUsername] = useState(row.username);
  const { busy, error, submit } = useRowSubmit(onDone);
  const id = useId();

  function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    void submit(() => api.createFromPending(row.id, username.trim()), 'people.pending.created');
  }

  return (
    <form onSubmit={onSubmit} noValidate className="contents">
      <DialogBody className="flex flex-col gap-4">
        <Field
          htmlFor={id}
          label={t('people.pending.username')}
          description={t('people.pending.usernameHint')}
          error={error ?? undefined}
        >
          <Input
            id={id}
            autoComplete="off"
            spellCheck={false}
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            aria-invalid={error ? true : undefined}
            aria-describedby={describedBy(id, Boolean(error), true)}
          />
        </Field>
      </DialogBody>
      <DialogFormFooter busy={busy} submitLabel={t('people.pending.createAction')} />
    </form>
  );
}
