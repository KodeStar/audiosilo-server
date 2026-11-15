import { useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { invalidatePeople } from '@/api/hooks';
import type { AdminUser } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import { FormError } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { errorMessage } from '@/lib/errors';
import { toast } from '@/lib/toast';

/**
 * A person's email (server#235): a single sign-on whose verified email matches
 * this account, and no other, links to it on its own (never an admin's). Not
 * for household members: they never sign in with single sign-on.
 */
export function EmailCard({ user: u }: { user: AdminUser }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [email, setEmail] = useState(u.email);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const id = useId();
  const errorId = `${id}-error`;
  // The saved value moved on (saved here, or by another admin): start from it.
  const [saved, setSaved] = useState(u.email);
  if (saved !== u.email) {
    setSaved(u.email);
    setEmail(u.email);
  }
  const changed = email.trim() !== u.email;

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.updateUser(u.id, { email: email.trim() });
      invalidatePeople(qc);
      toast.add({
        title: email.trim() ? t('user.email.saved') : t('user.email.cleared'),
        type: 'success',
      });
    } catch (err) {
      setError(errorMessage(err, t));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card className="flex flex-col gap-3 p-5" aria-labelledby={`${id}-title`}>
      <h2 id={`${id}-title`} className="h3">
        {t('user.email.title')}
      </h2>
      <p className="text-[13.5px] text-muted-foreground">
        {u.role === 'admin' ? t('user.email.bodyAdmin') : t('user.email.body')}
      </p>
      <form className="flex flex-col gap-2" onSubmit={onSubmit} noValidate>
        <div className="flex gap-2">
          <Input
            type="email"
            autoComplete="off"
            spellCheck={false}
            aria-label={t('user.email.title')}
            placeholder={t('user.email.placeholder')}
            value={email}
            maxLength={254}
            onChange={(e) => setEmail(e.target.value)}
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? errorId : undefined}
          />
          <Button type="submit" variant="outline" disabled={busy || !changed}>
            {t('user.email.save')}
          </Button>
        </div>
        <div id={errorId}>
          <FormError>{error}</FormError>
        </div>
      </form>
    </Card>
  );
}
