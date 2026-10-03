import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { z } from '@/lib/zod';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Share2 } from 'lucide-react';
import { api } from '@/api/client';
import { invalidatePeople } from '@/api/hooks';
import type { Share } from '@/api/types';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { Field } from '@/components/ui/field';
import { describedBy } from '@/lib/a11y';
import { Input } from '@/components/ui/input';
import { errorMessage, fieldMessage } from '@/lib/errors';
import { toast } from '@/lib/toast';

const schema = z.object({ name: z.string().trim().min(1, 'shares.nameRequired') });
type Values = z.infer<typeof schema>;

/** Creates a share, or (with `share`) renames one. */
export function ShareNameDialog({
  share,
  open,
  onOpenChange,
  onCreated,
}: {
  share?: Share;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated?: (id: number) => void;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={Share2}
        title={share ? t('shares.renameTitle', { name: share.name }) : t('shares.newTitle')}
        description={share ? undefined : t('shares.newDescription')}
      >
        {open ? (
          <ShareNameForm
            share={share}
            onDone={(id) => {
              onOpenChange(false);
              if (!share) onCreated?.(id);
            }}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function ShareNameForm({ share, onDone }: { share?: Share; onDone: (id: number) => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { name: share?.name ?? '' },
  });
  const { errors, isSubmitting } = form.formState;
  const onSubmit = form.handleSubmit(async ({ name }) => {
    try {
      const saved = share ? await api.renameShare(share.id, name) : await api.createShare(name);
      invalidatePeople(qc);
      toast.add({
        title: share ? t('shares.toast.renamed', { name }) : t('shares.toast.created', { name }),
        type: 'success',
      });
      onDone(saved.id);
    } catch (err) {
      form.setError('name', { message: errorMessage(err, t) });
    }
  });
  const nameError = fieldMessage(errors.name?.message, t);
  return (
    <form onSubmit={(e) => void onSubmit(e)} noValidate className="contents">
      <DialogBody>
        <Field htmlFor="share-name" label={t('shares.name')} error={nameError}>
          <Input
            id="share-name"
            autoComplete="off"
            placeholder={t('shares.namePlaceholder')}
            aria-invalid={nameError ? true : undefined}
            aria-describedby={describedBy('share-name', !!nameError, false)}
            {...form.register('name')}
          />
        </Field>
      </DialogBody>
      <DialogFormFooter
        busy={isSubmitting}
        submitLabel={share ? t('shares.saveName') : t('shares.create')}
      />
    </form>
  );
}
