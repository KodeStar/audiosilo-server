import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { ExternalLink, Headphones } from 'lucide-react';
import type { AudibleAccount } from '@/api/types';
import { FactList } from '@/components/fact-list';
import { QueryError } from '@/components/query-error';
import { StatusText } from '@/components/status-text';
import { buttonVariants } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { counted, formatNumber, formatRelative } from '@/lib/format';
import { cn } from '@/lib/utils';
import { CardEmpty } from '@/features/settings/card-empty';
import { bridgeTabLink } from '@/features/bridge/bridge-model';
import { ACCOUNTS_TAB } from '@/features/bridge/setup-model';
import { accountName } from './audible-model';
import { AccountSettingsButton } from './account-settings-button';
import { BackupAllButton } from './backup-all-button';
import { CheckNowButton } from './check-now-button';
import { RemoveAccountButton } from './remove-account-button';
import { useAudibleAccounts, useBackups, useCheckNow, type Backups } from './use-audible';

/**
 * Settings > Bridge's Audible accounts (Bridge Phase 4a), while the audible
 * module is on: each account the Bridge signed in to, the person and library it
 * is mapped to, when it was last checked and its titles counted. Signing in and
 * mapping happen on the Bridge's own Accounts tab (the server never holds an
 * Audible credential), so the card links there; Remove only takes an account
 * the Bridge can no longer remove (unpaired, paired again) off this server.
 * Phase 4b: the account's backup policy and template (Backup settings) and Back
 * up everything.
 */
export function AudibleAccountsCard({ uiUrl }: { uiUrl?: string }) {
  const { t } = useTranslation();
  const accounts = useAudibleAccounts();
  const { checkNow, busy } = useCheckNow();
  const backups = useBackups();
  const accountsTab = bridgeTabLink(uiUrl, ACCOUNTS_TAB);
  const list = accounts.data?.accounts ?? [];

  return (
    <Card aria-labelledby="audible-accounts-title">
      <CardHeader
        titleId="audible-accounts-title"
        title={t('audible.accounts.title')}
        description={t('audible.accounts.description')}
        action={
          list.length > 0 ? (
            <Link
              to="/server/{-$section}"
              params={{ section: undefined }}
              search={{ topic: 'audible' }}
              className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
            >
              {t('audible.accounts.viewTitles')}
            </Link>
          ) : undefined
        }
      />
      {accounts.isError ? (
        <div className="p-5">
          <QueryError
            title={t('audible.accounts.error')}
            error={accounts.error}
            onRetry={() => void accounts.refetch()}
          />
        </div>
      ) : accounts.isPending ? (
        <div className="p-5">
          <div className="skel h-20 rounded-lg" role="status" aria-label={t('common.loading')} />
        </div>
      ) : list.length === 0 ? (
        <CardEmpty
          icon={Headphones}
          title={t('audible.accounts.none')}
          body={t('audible.accounts.noneBody')}
          action={
            accountsTab ? (
              <a
                href={accountsTab}
                target="_blank"
                rel="noopener noreferrer"
                className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
              >
                {t('audible.accounts.openAccounts')}
                <ExternalLink aria-hidden="true" />
              </a>
            ) : undefined
          }
        />
      ) : (
        <ul className="flex flex-col divide-y" aria-label={t('audible.accounts.title')}>
          {list.map((a) => (
            <AccountRow
              key={a.id}
              account={a}
              busy={busy === a.id}
              onCheck={() => void checkNow(a)}
              backups={backups}
            />
          ))}
        </ul>
      )}
    </Card>
  );
}

function AccountRow({
  account: a,
  busy,
  onCheck,
  backups,
}: {
  account: AudibleAccount;
  busy: boolean;
  onCheck: () => void;
  backups: Backups;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const counts = a.titles;
  return (
    <li className="flex flex-wrap items-start gap-x-3 gap-y-2 px-5 py-3.5">
      <div className="flex min-w-0 flex-1 basis-64 flex-col gap-2">
        <span className="flex flex-wrap items-center gap-2">
          <b className="font-semibold">{accountName(t, a)}</b>
          {a.paused ? (
            <StatusText tone="warn" colored className="text-[12.5px] font-semibold">
              {t('audible.account.paused')}
            </StatusText>
          ) : (
            <StatusText
              tone={a.last_poll_at ? 'ok' : 'off'}
              colored
              className="text-[12.5px] font-semibold"
            >
              {a.last_poll_at
                ? t('audible.account.checked', { when: formatRelative(a.last_poll_at, lang) })
                : t('audible.account.notChecked')}
            </StatusText>
          )}
        </span>
        <FactList
          rows={[
            [t('audible.account.person'), a.user.display_name],
            [
              t('audible.account.library'),
              a.library ? (
                a.library.name
              ) : (
                <span className="text-muted-foreground">{t('audible.account.pausedBody')}</span>
              ),
            ],
            [t('audible.account.store'), a.marketplace.toUpperCase()],
            [t('audible.account.policy'), t(`audible.policy.${a.policy}`)],
            [
              t('audible.account.template'),
              a.template ? (
                <span className="font-mono text-[12.5px] [overflow-wrap:anywhere]">
                  {a.template}
                </span>
              ) : (
                <span className="text-muted-foreground">{t('audible.account.templateAuto')}</span>
              ),
            ],
            [
              t('audible.account.titles'),
              counts
                ? t('audible.account.counts', {
                    ...counted(counts.total, lang),
                    matched: formatNumber(counts.matched, lang),
                  })
                : '',
            ],
          ]}
        />
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <CheckNowButton busy={busy} paused={a.paused} onCheck={onCheck} />
        <AccountSettingsButton account={a} backups={backups} />
        <BackupAllButton account={a} backups={backups} />
        <RemoveAccountButton account={a} />
      </div>
    </li>
  );
}
