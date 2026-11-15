import { useTranslation } from 'react-i18next';
import { Info } from 'lucide-react';
import { useOIDCStatus } from '@/api/hooks';
import type { AdminSettings } from '@/api/types';
import { CopyField } from '@/components/copy-field';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { SettingRow } from '@/components/setting-row';
import { StatusText } from '@/components/status-text';
import { Card, CardHeader } from '@/components/ui/card';
import { SettingBadges, SettingsForm } from './settings-form';
import { signinLook, type SettingId } from './settings-model';

/**
 * Settings > Sign-in: Sign in with OpenID Connect (server#235). The provider is
 * read only here (config.yaml auth.oidc or AUDIOSILO_OIDC_*, applied at a
 * restart); who may sign in and as what saves here and applies at once.
 */
export function SigninTopic({ settings }: { settings: AdminSettings }) {
  const { t } = useTranslation();
  return (
    <>
      <ProviderCard settings={settings} />
      <SettingsForm
        settings={settings}
        section="signin"
        title={t('settings.signin.who')}
        description={t('settings.signin.whoBody')}
        fields={[
          { name: 'auto_create', kind: 'switch' },
          { name: 'allowed_group', kind: 'text', placeholder: t('settings.signin.anyone') },
          { name: 'admin_group', kind: 'text', placeholder: t('settings.signin.noGroup') },
        ]}
        visible={(name, draft) => name !== 'allowed_group' || draft.auto_create === true}
      />
    </>
  );
}

/** The provider as configured, its status and the redirect URI to register. */
function ProviderCard({ settings }: { settings: AdminSettings }) {
  const { t } = useTranslation();
  const status = useOIDCStatus();
  const s = settings.signin;
  const fact = (id: SettingId, value: string, fallback?: string) => (
    <SettingRow
      key={id}
      title={t(`settings.${id}`)}
      badges={<SettingBadges settings={settings} id={id} />}
    >
      <span className="font-mono text-[12.5px] [overflow-wrap:anywhere]">
        {value || (
          <span className="font-sans text-muted-foreground">
            {fallback ?? t('settings.signin.unset')}
          </span>
        )}
      </span>
    </SettingRow>
  );
  return (
    <Card aria-labelledby="signin-title">
      <CardHeader
        titleId="signin-title"
        title={t('settings.signin.card')}
        description={t('settings.signin.cardBody')}
      />
      <div className="divide-y px-5">
        <StatusRow status={status} />
        {status.data?.redirect_uri ? (
          <div className="py-4">
            <CopyField
              id="signin-redirect"
              label={t('settings.signin.redirect')}
              value={status.data.redirect_uri}
            >
              <span className="text-[12.5px] text-muted-foreground">
                {t('settings.signin.redirectBody')}
              </span>
            </CopyField>
          </div>
        ) : status.data ? (
          <SettingRow
            title={t('settings.signin.redirect')}
            description={t('settings.signin.redirectNone')}
          >
            <span className="text-muted-foreground">{t('settings.signin.unset')}</span>
          </SettingRow>
        ) : null}
        <SettingRow
          title={t('settings.signin.enabled')}
          badges={<SettingBadges settings={settings} id="signin.enabled" />}
        >
          <StatusText tone={s.enabled ? 'ok' : 'off'} className="font-semibold">
            {s.enabled ? t('settings.signin.on') : t('settings.signin.off')}
          </StatusText>
        </SettingRow>
        {fact('signin.name', s.name, t('settings.signin.defaultName'))}
        {fact('signin.issuer', s.issuer)}
        {fact('signin.client_id', s.client_id)}
        <SettingRow
          title={t('settings.signin.secret')}
          description={t('settings.signin.secretBody')}
        >
          <span className="text-muted-foreground">
            {s.client_secret_set ? t('settings.signin.secretSet') : t('settings.signin.secretNone')}
          </span>
        </SettingRow>
        {fact('signin.scopes', (s.scopes ?? []).join(' '), 'openid profile email groups')}
        {fact('signin.username_claim', s.username_claim, 'preferred_username')}
        {fact('signin.groups_claim', s.groups_claim, 'groups')}
        {fact('signin.ca_file', s.ca_file, t('settings.signin.systemCAs'))}
      </div>
      <Notice tone="info" icon={Info} className="mx-5 mb-5">
        {t('settings.signin.how')}
      </Notice>
    </Card>
  );
}

function StatusRow({ status }: { status: ReturnType<typeof useOIDCStatus> }) {
  const { t } = useTranslation();
  if (status.isError) {
    return (
      <div className="py-4">
        <QueryError
          title={t('settings.signin.statusError')}
          error={status.error}
          onRetry={() => void status.refetch()}
        />
      </div>
    );
  }
  const s = status.data;
  const look = s && signinLook(s);
  const detail = !s
    ? undefined
    : !s.configured
      ? s.problem
      : s.discovery?.state === 'error'
        ? s.discovery.error
        : undefined;
  return (
    <SettingRow
      title={t('settings.signin.status')}
      description={detail ? <span className="[overflow-wrap:anywhere]">{detail}</span> : undefined}
    >
      {look ? (
        <StatusText tone={look.tone} colored className="font-semibold">
          {t(look.key)}
        </StatusText>
      ) : (
        <span
          className="skel inline-block h-5 w-36"
          role="status"
          aria-label={t('common.loading')}
        />
      )}
    </SettingRow>
  );
}
