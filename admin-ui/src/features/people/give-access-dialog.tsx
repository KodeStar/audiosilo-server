import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Share2 } from 'lucide-react';
import { invalidatePeople, useLibraries, useShares } from '@/api/hooks';
import type { Share, User } from '@/api/types';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import { RadioCards, type RadioCardOption } from '@/components/ui/radio-cards';
import { errorMessage } from '@/lib/errors';
import { formatNumber } from '@/lib/format';
import { toast } from '@/lib/toast';
import { grantAccess } from './grant-access';
import { accessValue, parseAccessChoice, wholeLibraryOf } from './people-model';

/** Give a person a whole library or a share they don't have yet. */
export function GiveAccessDialog({
  open,
  onOpenChange,
  user,
  granted,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  user: User;
  granted: Share[];
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={Share2}
        title={t('user.access.giveTitle', { name: user.username })}
        description={t('user.access.giveDescription')}
      >
        {open ? (
          <GiveAccessForm user={user} granted={granted} onDone={() => onOpenChange(false)} />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function GiveAccessForm({
  user,
  granted,
  onDone,
}: {
  user: User;
  granted: Share[];
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const libraries = useLibraries();
  const shares = useShares();
  const [choice, setChoice] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const grantedIds = new Set(granted.map((s) => s.id));
  const grantedLibs = new Set(granted.map(wholeLibraryOf).filter((id) => id !== undefined));
  const options: RadioCardOption<string>[] = [
    ...(libraries.data ?? [])
      .filter((l) => !grantedLibs.has(l.id))
      .map((l) => ({
        value: accessValue({ kind: 'library', id: l.id }),
        title: l.name,
        description: t('user.access.libraryOption', {
          count: l.book_count,
          formatted: formatNumber(l.book_count, lang),
        }),
      })),
    ...(shares.data ?? [])
      .filter((s) => wholeLibraryOf(s) === undefined && !grantedIds.has(s.id))
      .map((s) => ({
        value: accessValue({ kind: 'share', id: s.id }),
        title: s.name,
        description: t('invite.access.shareBody', { count: s.paths?.length ?? 0 }),
      })),
  ];
  const loading = libraries.isPending || shares.isPending;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const picked = choice ? parseAccessChoice(choice) : undefined;
    if (!picked) return;
    setBusy(true);
    setError(undefined);
    try {
      await grantAccess(user.id, picked);
      invalidatePeople(qc);
      toast.add({
        title: t('user.access.given', { what: options.find((o) => o.value === choice)?.title }),
        description: t('user.access.changedBody', { name: user.username }),
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
      <DialogBody className="flex flex-col gap-3">
        {loading ? (
          <div className="skel h-24" role="status" aria-label={t('common.loading')} />
        ) : options.length === 0 ? (
          <p className="text-muted-foreground">
            {t('user.access.nothingLeft', { name: user.username })}
          </p>
        ) : (
          <RadioCards
            label={t('user.access.giveTitle', { name: user.username })}
            value={choice}
            onValueChange={setChoice}
            options={options}
          />
        )}
        <FormError>{error}</FormError>
      </DialogBody>
      <DialogFormFooter busy={busy} disabled={!choice} submitLabel={t('user.access.giveAction')} />
    </form>
  );
}
