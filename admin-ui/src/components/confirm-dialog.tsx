import { useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { LucideIcon } from 'lucide-react';
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFormFooter,
  type DialogTone,
} from '@/components/ui/dialog';
import { Field, FormError } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { errorMessage } from '@/lib/errors';

/**
 * Confirms one consequential action. With `typeToConfirm`, the action stays
 * disabled until that exact text is typed (irreversible deletes: the style
 * guide's type-to-confirm). `onConfirm` may reject; the dialog then stays open
 * and says why (an API error as `errorMessage` words it).
 */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  icon,
  tone = 'danger',
  confirmLabel,
  destructive = true,
  typeToConfirm,
  onConfirm,
  children,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  icon: LucideIcon;
  tone?: DialogTone;
  confirmLabel: string;
  destructive?: boolean;
  typeToConfirm?: string;
  /** Resolves when done (the dialog closes) or throws a message to show. */
  onConfirm: () => Promise<void>;
  children?: React.ReactNode;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent title={title} description={description} icon={icon} tone={tone}>
        {open ? (
          <ConfirmBody
            confirmLabel={confirmLabel}
            destructive={destructive}
            typeToConfirm={typeToConfirm}
            onConfirm={async () => {
              await onConfirm();
              onOpenChange(false);
            }}
          >
            {children}
          </ConfirmBody>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function ConfirmBody({
  confirmLabel,
  destructive,
  typeToConfirm,
  onConfirm,
  children,
}: {
  confirmLabel: string;
  destructive: boolean;
  typeToConfirm?: string;
  onConfirm: () => Promise<void>;
  children?: React.ReactNode;
}) {
  const { t } = useTranslation();
  const id = useId();
  const [typed, setTyped] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const ready = typeToConfirm === undefined || typed.trim() === typeToConfirm;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!ready || busy) return;
    setBusy(true);
    setError(undefined);
    try {
      await onConfirm();
    } catch (err) {
      setError(errorMessage(err, t));
      setBusy(false);
    }
  };

  return (
    <form onSubmit={(e) => void submit(e)} className="contents" noValidate>
      {children || typeToConfirm !== undefined || error ? (
        <DialogBody className="flex flex-col gap-4">
          {children}
          {typeToConfirm !== undefined ? (
            <Field
              htmlFor={id}
              label={
                <>
                  {t('confirm.typeBefore')}{' '}
                  <span className="rounded-[5px] bg-muted px-1.5 py-px font-mono">
                    {typeToConfirm}
                  </span>{' '}
                  {t('confirm.typeAfter')}
                </>
              }
            >
              <Input
                id={id}
                autoComplete="off"
                spellCheck={false}
                value={typed}
                onChange={(e) => setTyped(e.target.value)}
                aria-invalid={typed && !typeToConfirm.startsWith(typed.trim()) ? true : undefined}
              />
            </Field>
          ) : null}
          <FormError>{error}</FormError>
        </DialogBody>
      ) : null}
      <DialogFormFooter
        busy={busy}
        disabled={!ready}
        variant={destructive ? 'destructive' : 'default'}
        submitLabel={confirmLabel}
      />
    </form>
  );
}
