import { useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { BadgeCheck, CircleAlert, Link2, UserPlus, X } from 'lucide-react';
import { ApiError, api } from '@/api/client';
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
import { accountLabel } from './household-model';
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
  const rows = pending.data?.pending ?? [];

  // A poll that fails keeps the list it had; the error stands in for none.
  let list: React.ReactNode = null;
  if (pending.isError && !pending.data) {
    list = (
      <div className="mb-6">
        <QueryError
          title={t('people.pending.error')}
          error={pending.error}
          onRetry={() => void pending.refetch()}
        />
      </div>
    );
  } else if (rows.length) {
    list = (
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
      </Card>
    );
  }

  // The dialogs keep one place in the tree whatever the list shows (the card,
  // the error or nothing, always the first child): a refused action reads the
  // list again, which may leave it empty (its row went meanwhile) or fail, and
  // a dialog that moved would be mounted again without what it said. Each
  // closes only for the row it was opened on, never one opened since (Cancel
  // works while an action is on its way).
  return (
    <>
      {list}
      <LinkDialog
        row={linking}
        linked={pending.data?.linked_user_ids}
        onOpenChange={(o) => !o && setLinking(null)}
        onDone={(id) => setLinking((cur) => (cur?.id === id ? null : cur))}
      />
      <CreateDialog
        row={creating}
        onOpenChange={(o) => !o && setCreating(null)}
        onDone={(id) => setCreating((cur) => (cur?.id === id ? null : cur))}
      />
      <ConfirmDialog
        open={dismissing !== null}
        onOpenChange={(o) =>
          !o && setDismissing((cur) => (cur?.id === dismissing?.id ? null : cur))
        }
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
    </>
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
  // The refusal in words, and its code (which says where a form shows it).
  const [failure, setFailure] = useState<{ message: string; code?: string } | null>(null);

  async function submit(run: () => Promise<AdminUser>, toastKey: string) {
    if (busy) return;
    setBusy(true);
    setFailure(null);
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
      setFailure({
        message: errorMessage(err, t),
        code: err instanceof ApiError ? err.code : undefined,
      });
      setBusy(false);
    }
  }
  return { busy, error: failure?.message ?? null, code: failure?.code, submit };
}

/**
 * Links a waiting sign-in to an account the admin chooses. `onDone` gets the
 * row a finished link was for, so it closes that dialog, never one opened
 * since (Cancel works while a link is on its way).
 */
function LinkDialog({
  row,
  linked,
  onOpenChange,
  onDone,
}: {
  row: OIDCPendingSignIn | null;
  linked: OIDCPendingList['linked_user_ids'];
  onOpenChange: (open: boolean) => void;
  onDone: (id: number) => void;
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
          <LinkForm key={row.id} row={row} linked={linked} onDone={() => onDone(row.id)} />
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
  const [choice, setChoice] = useState<number>();
  // The admin's pick, else the suggestion; one the list no longer offers
  // (linked to this provider meanwhile, a refusal's re-read says) is none.
  const wanted = choice ?? suggestedAccount(candidates, row)?.id;
  const chosen = candidates.find((u) => u.id === wanted)?.id;
  const { busy, error, submit } = useRowSubmit(onDone);
  // No accounts to choose from because they couldn't be read: say so.
  const loadError = users.isError && !users.data ? errorMessage(users.error, t) : null;
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
            onChange={(e) => setChoice(Number(e.target.value))}
          >
            <option value="" disabled>
              {t('people.pending.choose')}
            </option>
            {candidates.map((u) => {
              const name = accountLabel(u);
              return (
                <option key={u.id} value={u.id}>
                  {u.role === 'admin' ? t('people.pending.adminOption', { name }) : name}
                </option>
              );
            })}
          </NativeSelect>
        </Field>
        <FormError>{error ?? loadError}</FormError>
      </DialogBody>
      <DialogFormFooter
        busy={busy}
        disabled={chosen === undefined}
        submitLabel={t('people.pending.link')}
      />
    </form>
  );
}

/**
 * Makes an ordinary account for a waiting sign-in, its username prefilled from
 * the sign-in. `onDone` as LinkDialog's.
 */
function CreateDialog({
  row,
  onOpenChange,
  onDone,
}: {
  row: OIDCPendingSignIn | null;
  onOpenChange: (open: boolean) => void;
  onDone: (id: number) => void;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={row !== null} onOpenChange={onOpenChange}>
      <DialogContent
        icon={UserPlus}
        title={t('people.pending.createTitle', { name: row ? pendingName(row) : '' })}
        description={t('people.pending.createBody')}
      >
        {row ? <CreateForm key={row.id} row={row} onDone={() => onDone(row.id)} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function CreateForm({ row, onDone }: { row: OIDCPendingSignIn; onDone: () => void }) {
  const { t } = useTranslation();
  const [username, setUsername] = useState(row.username);
  const { busy, error, code, submit } = useRowSubmit(onDone);
  const id = useId();
  // A taken or missing username is the field's; any other refusal (the row
  // gone, linked meanwhile) is the form's.
  const fieldError = code === 'username_taken' || code === 'username_required' ? error : null;

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
          error={fieldError ?? undefined}
        >
          <Input
            id={id}
            autoComplete="off"
            spellCheck={false}
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            aria-invalid={fieldError ? true : undefined}
            aria-describedby={describedBy(id, Boolean(fieldError), true)}
          />
        </Field>
        {fieldError ? null : <FormError>{error}</FormError>}
      </DialogBody>
      <DialogFormFooter busy={busy} submitLabel={t('people.pending.createAction')} />
    </form>
  );
}
