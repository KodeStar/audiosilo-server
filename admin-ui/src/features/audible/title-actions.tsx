import { useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Ban, CloudDownload, FolderInput, LoaderCircle, Undo2 } from 'lucide-react';
import type { AudibleTitle } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Button } from '@/components/ui/button';
import { Field } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { canBackUp, cleanDest } from './audible-model';
import type { useBackups } from './use-audible';

/**
 * A title's backup actions (Bridge Phase 4b): Back up (a preorder's waits for
 * its release), "Not this book" (never backed up by a policy or Back up
 * everything, and its match isn't this title) and its undo, and the
 * destination: where in the library the backup goes instead of the account's
 * placement.
 */
export function TitleActions({
  title: x,
  backups,
}: {
  title: AudibleTitle;
  backups: ReturnType<typeof useBackups>;
}) {
  const { t } = useTranslation();
  const [editing, setEditing] = useState(false);
  const busy = backups.busy === backups.titleKey(x);
  const name = x.title || x.asin;
  return (
    <div className="flex flex-wrap items-center gap-2">
      {canBackUp(x) ? (
        <Button size="sm" variant="outline" disabled={busy} onClick={() => void backups.backUp(x)}>
          {busy ? (
            <LoaderCircle className="animate-spin" aria-hidden="true" />
          ) : (
            <CloudDownload aria-hidden="true" />
          )}
          {t('audible.backup.action')}
        </Button>
      ) : null}
      {x.rejected ? (
        <Button
          size="sm"
          variant="ghost"
          disabled={busy}
          onClick={() => void backups.setRejected(x, false)}
          aria-label={t('audible.reject.undoLabel', { name })}
        >
          <Undo2 aria-hidden="true" />
          {t('audible.reject.undo')}
        </Button>
      ) : (
        <Button
          size="sm"
          variant="ghost"
          disabled={busy}
          onClick={() => void backups.setRejected(x, true)}
          aria-label={t('audible.reject.label', { name })}
        >
          <Ban aria-hidden="true" />
          {t('audible.reject.action')}
        </Button>
      )}
      {!x.match || x.rejected ? (
        <Button
          size="sm"
          variant="ghost"
          onClick={() => setEditing(true)}
          aria-label={t('audible.dest.label', { name })}
        >
          <FolderInput aria-hidden="true" />
          {t('audible.dest.action')}
        </Button>
      ) : null}
      {editing ? (
        <DestDialog title={x} backups={backups} onClose={() => setEditing(false)} />
      ) : null}
    </div>
  );
}

function DestDialog({
  title: x,
  backups,
  onClose,
}: {
  title: AudibleTitle;
  backups: ReturnType<typeof useBackups>;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const id = useId();
  const [dest, setDest] = useState(x.dest_path);
  return (
    <ConfirmDialog
      open
      onOpenChange={(open) => !open && onClose()}
      icon={FolderInput}
      tone="brand"
      destructive={false}
      title={t('audible.dest.title', { name: x.title || x.asin })}
      description={t('audible.dest.description')}
      confirmLabel={t('audible.dest.save')}
      onConfirm={() => backups.setDest(x, cleanDest(dest))}
    >
      <Field htmlFor={id} label={t('audible.dest.field')} description={t('audible.dest.hint')}>
        <Input
          id={id}
          value={dest}
          autoComplete="off"
          spellCheck={false}
          placeholder="Author/Series/Title.m4b"
          onChange={(e) => setDest(e.target.value)}
          aria-describedby={`${id}-desc`}
        />
      </Field>
    </ConfirmDialog>
  );
}
