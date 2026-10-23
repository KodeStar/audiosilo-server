import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { ArrowRight, ExternalLink, EyeOff, Puzzle } from 'lucide-react';
import { useModules } from '@/api/hooks';
import type { AdminModule, AdminSettings, ModuleBridgeState } from '@/api/types';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { SettingRow } from '@/components/setting-row';
import { StatusText, type StatusTone } from '@/components/status-text';
import { buttonVariants } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { CardEmpty } from './card-empty';
import { InstantSwitch, SettingBadges } from './settings-form';
import { lockOf, type SettingId } from './settings-model';

/** Where the User Guide explains modules (docs-users/admin/server.md, "Modules"). */
export const MODULES_DOCS_URL = 'https://docs.audiosilo.app/users/admin/server#modules';

const BRIDGE_TONE: Record<ModuleBridgeState, StatusTone> = {
  ready: 'ok',
  offline: 'off',
  no_feature: 'warn',
};

/**
 * Settings > Modules: the optional features the server ships off, one card
 * each (GET /admin/modules, in the server's order). A card's switch saves the
 * module's toggle at once (locked when the environment sets it), says where it
 * stands with the Bridge it needs, and notes when it is on but still hidden
 * from listeners because the Bridge isn't ready.
 */
export function ModulesTopic({ settings }: { settings: AdminSettings }) {
  const { t } = useTranslation();
  const modules = useModules();
  if (modules.isError)
    return (
      <QueryError
        title={t('settings.modules.error')}
        error={modules.error}
        onRetry={() => void modules.refetch()}
      />
    );
  if (!modules.data)
    return (
      <div className="skel h-[220px] rounded-xl" role="status" aria-label={t('common.loading')} />
    );
  if (modules.data.modules.length === 0)
    return (
      <Card aria-label={t('settings.topic.modules')}>
        <CardEmpty
          icon={Puzzle}
          title={t('settings.modules.none')}
          body={t('settings.modules.noneBody')}
        />
      </Card>
    );
  return (
    <>
      <p className="text-[13px] text-muted-foreground">{t('settings.modules.intro')}</p>
      {modules.data.modules.map((m) => (
        <ModuleCard key={m.id} module={m} settings={settings} />
      ))}
    </>
  );
}

function ModuleCard({ module: m, settings }: { module: AdminModule; settings: AdminSettings }) {
  const { t } = useTranslation();
  const id = `modules.${m.id}` as SettingId;
  const name = t(`settings.module.${m.id}.name`, { defaultValue: m.id });
  // The settings envelope is the toggle's truth (a save updates it at once);
  // the module list may lag it by a refetch.
  const enabled = settings.modules[m.id] ?? m.enabled;
  // Listeners see it while on and its Bridge requirement is met (the server's
  // modules.Active), read from that toggle, not the list's possibly older one.
  const offered = enabled && (m.bridge_state === undefined || m.bridge_state === 'ready');
  const titleId = `module-${m.id}-title`;
  const switchId = `module-${m.id}-switch`;
  return (
    <Card aria-labelledby={titleId}>
      <CardHeader
        titleId={titleId}
        title={name}
        description={t(`settings.module.${m.id}.blurb`, { defaultValue: '' }) || undefined}
        action={
          <a
            href={MODULES_DOCS_URL}
            target="_blank"
            rel="noopener noreferrer"
            aria-label={t('settings.modules.learnMoreAbout', { name })}
            className={cn(buttonVariants({ variant: 'ghost', size: 'sm' }))}
          >
            {t('settings.modules.learnMore')}
            <ExternalLink aria-hidden="true" />
          </a>
        }
      />
      <div className="divide-y px-5">
        <SettingRow
          title={t('settings.modules.toggle', { name })}
          badges={<SettingBadges settings={settings} id={id} />}
          htmlFor={switchId}
          description={m.bridge_state ? <BridgeLine id={m.id} state={m.bridge_state} /> : undefined}
          descriptionId={`${switchId}-desc`}
        >
          <InstantSwitch
            id={switchId}
            checked={enabled}
            disabled={Boolean(lockOf(settings, id))}
            describedBy={m.bridge_state ? `${switchId}-desc` : undefined}
            patch={(on) => ({ modules: { [m.id]: on } })}
            failedTitle={t('settings.modules.failed', { name })}
            onSaved={(on) =>
              toast.add({
                title: on
                  ? t('settings.modules.on', { name })
                  : t('settings.modules.off', { name }),
                type: 'success',
              })
            }
          />
        </SettingRow>
      </div>
      {enabled && !offered ? (
        <Notice tone="info" icon={EyeOff} role="status" className="mx-5 mb-5">
          {/* An admin-only module (Contributions) isn't hidden from listeners: it waits. */}
          {t(`settings.module.${m.id}.hidden`, { defaultValue: t('settings.modules.hidden') })}
        </Notice>
      ) : null}
    </Card>
  );
}

/** Where the module stands with the Bridge feature it needs. */
function BridgeLine({ id, state }: { id: string; state: ModuleBridgeState }) {
  const { t } = useTranslation();
  const words =
    state === 'no_feature'
      ? t(`settings.module.${id}.needs`, {
          defaultValue: t('settings.modules.bridge.no_feature'),
        })
      : t(`settings.modules.bridge.${state}`);
  return (
    <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <StatusText tone={BRIDGE_TONE[state]}>{words}</StatusText>
      {state === 'ready' ? null : (
        <Link
          to="/server/{-$section}"
          params={{ section: undefined }}
          search={{ topic: 'bridge' }}
          className="inline-flex items-center gap-1 font-semibold text-brand-ink hover:underline"
        >
          {t('settings.modules.bridge.open')}
          <ArrowRight className="size-3.5" aria-hidden="true" />
        </Link>
      )}
    </span>
  );
}
