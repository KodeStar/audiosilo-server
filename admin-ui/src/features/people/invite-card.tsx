import { useTranslation } from 'react-i18next';
import { Clock, ShieldCheck, Users } from 'lucide-react';
import { useServerInfo } from '@/api/hooks';
import { CopyField } from '@/components/copy-field';
import { LogoTile } from '@/components/logo';
import { QrCode } from '@/components/qr-code';
import { formatDateTime, hostOf } from '@/lib/format';
import { serverLabel } from '@/lib/server-label';
import type { ShownInvite } from './invite-dialog';

/**
 * A freshly minted (or rotated) invite: the ink card with the QR code, then the
 * link and the code with copy buttons. The code is shown only this once; the
 * server keeps a hash. The QR is drawn in the browser, so the code never goes
 * back to the server in an image request. The card names the server the way the
 * person invited knows it, not by whatever address this browser happens to use.
 */
export function InviteCard({ invite }: { invite: ShownInvite }) {
  const { name, invite_url: url, auth_code: code, max_uses: maxUses, expires_at } = invite;
  const { t, i18n } = useTranslation();
  const server = useServerInfo();
  // The link's host is the server's public address, else the host it was asked on.
  const serverName = serverLabel(server.data?.name, hostOf(url));
  return (
    <div className="flex flex-col gap-4">
      <div className="invite-card relative overflow-hidden rounded-xl p-5 text-white md:p-6">
        <div className="mb-5 flex items-center gap-2.5">
          <LogoTile />
          <span className="flex min-w-0 flex-col">
            <b className="truncate font-display text-[15px]">{serverName}</b>
            <span className="text-[12px] opacity-70">{t('invite.card.from')}</span>
          </span>
        </div>
        <div className="flex flex-wrap items-end gap-5">
          <QrCode text={url} label={t('invite.card.qr', { name })} />
          <div className="flex min-w-[170px] flex-1 flex-col gap-2">
            <span className="font-display text-[24px] leading-[1.08] font-bold tracking-[-0.03em] [overflow-wrap:anywhere]">
              {t('invite.card.headline', { name })}
            </span>
            <span className="text-[13px] opacity-75">{t('invite.card.body')}</span>
          </div>
        </div>
      </div>

      <CopyField id="invite-link" label={t('invite.link')} value={url} />
      <CopyField id="invite-code" label={t('invite.code')} value={code}>
        <p className="text-[12.5px] text-muted-foreground">{t('invite.codeOnce')}</p>
      </CopyField>
      <ul className="flex flex-wrap gap-x-4 gap-y-2 text-[12.5px] text-muted-foreground">
        <li className="inline-flex items-center gap-1.5">
          <Users className="size-[15px]" aria-hidden="true" />
          {maxUses ? t('invite.uses', { count: maxUses }) : t('invite.usesUnlimited')}
        </li>
        <li className="inline-flex items-center gap-1.5">
          <Clock className="size-[15px]" aria-hidden="true" />
          {expires_at
            ? t('invite.expires', {
                time: formatDateTime(expires_at, i18n.resolvedLanguage ?? 'en'),
              })
            : t('invite.neverExpires')}
        </li>
        <li className="inline-flex items-center gap-1.5">
          <ShieldCheck className="size-[15px]" aria-hidden="true" />
          {t('invite.fragment')}
        </li>
      </ul>
    </div>
  );
}
