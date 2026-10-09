import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ExternalLink, HeartHandshake } from 'lucide-react';
import { api } from '@/api/client';
import { keys, useSupportStatus } from '@/api/hooks';
import type { SupportAction, SupportStatus } from '@/api/types';
import { Button, buttonVariants } from '@/components/ui/button';
import { toastError } from '@/lib/errors';
import { SPONSOR_URL } from '@/lib/support';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';

/**
 * The Overview's support card: AudioSilo is free, and here is where to sponsor
 * it. The server says when it shows (never on a new server; GET /admin/support).
 * "I've donated" hides it for good and "Not now" for six months, for every admin
 * on the server; both are taken on trust. Opening GitHub Sponsors hides nothing.
 */
export function SupportCard() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const support = useSupportStatus();
  const [busy, setBusy] = useState(false);
  if (!support.data?.show) return null;

  const answer = async (action: SupportAction) => {
    setBusy(true);
    try {
      qc.setQueryData<SupportStatus>(keys.support, await api.answerSupport(action));
      toast.add({
        title: t(`support.done.${action}`),
        description: t(`support.done.${action}Body`),
        type: 'success',
      });
    } catch (err) {
      toastError(t('support.failed'), err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section
      className="flex flex-col gap-3 rounded-xl border bg-card p-5"
      aria-labelledby="support-heading"
    >
      <h3 id="support-heading" className="h3 flex items-center gap-2">
        <HeartHandshake className="size-[18px] text-muted-foreground" aria-hidden="true" />
        {t('support.title')}
      </h3>
      <p className="text-[13.5px] text-muted-foreground">{t('support.body')}</p>
      {/* The two answers wrap together, under the link, in the narrow column. */}
      <div className="flex flex-wrap items-center gap-2">
        <a
          href={SPONSOR_URL}
          target="_blank"
          rel="noreferrer noopener"
          className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
        >
          {t('support.sponsor')}
          <ExternalLink aria-hidden="true" />
        </a>
        <div className="flex gap-1">
          <Button variant="ghost" size="sm" disabled={busy} onClick={() => void answer('donated')}>
            {t('support.donated')}
          </Button>
          <Button variant="ghost" size="sm" disabled={busy} onClick={() => void answer('snooze')}>
            {t('support.notNow')}
          </Button>
        </div>
      </div>
    </section>
  );
}
