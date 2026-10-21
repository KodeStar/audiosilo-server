import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Globe, Lock, Network, ShieldCheck, TriangleAlert } from 'lucide-react';
import { api } from '@/api/client';
import { invalidateLibraries, noteScanStarted, useSystem } from '@/api/hooks';
import type { MetadataMode } from '@/api/types';
import { Notice } from '@/components/notice';
import { SettingRow } from '@/components/setting-row';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { FormError } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import { RadioCards } from '@/components/ui/radio-cards';
import { Switch } from '@/components/ui/switch';
import { SettingBadges } from '@/features/settings/settings-form';
import {
  lockLabel,
  lockOf,
  METADATA_MODES,
  outOfRange,
  type SettingId,
} from '@/features/settings/settings-model';
import { ScheduleInput } from '@/features/settings/schedule-input';
import { errorMessage } from '@/lib/errors';
import { regionOptions } from '@/lib/regions';
import { toast } from '@/lib/toast';
import {
  addressMismatch,
  editable,
  mergePatches,
  proxyToTrust,
  reachOffers,
  seenAs,
  unlockedPatch,
} from './setup-model';
import { LockedNote, StepFooter } from './step-parts';
import { StepStop, useSaveStepSettings, type StepProps } from './step-actions';

/**
 * A setting's badge: "Set by your config" with a lock for one the environment
 * or the desktop app sets (which variable in its tooltip), else the restart
 * badges Settings shows.
 */
function Badges({ settings, id }: Pick<StepProps, 'settings'> & { id: SettingId }) {
  const { t } = useTranslation();
  const lock = lockOf(settings, id);
  if (!lock) return <SettingBadges settings={settings} id={id} />;
  return (
    <Badge variant="outline" title={lockLabel(t, lock)}>
      <Lock aria-hidden="true" />
      {t('setup.locked')}
    </Badge>
  );
}

/**
 * Creates the draft's library (kind books, with the naming step's source) once
 * its region is chosen (decision 5), then follows its scan. Nothing without a
 * draft. A refusal (a name taken, a folder gone) is toasted by the footer and
 * the step stays.
 */
function useCreateDraftLibrary({ draft, setDraft, onCreated }: StepProps) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  return async () => {
    if (!draft) return;
    const lib = await api.createLibrary({
      kind: 'books',
      name: draft.name,
      root: draft.root,
      ...(draft.source ? { metadata_source: draft.source } : {}),
    });
    invalidateLibraries(qc);
    noteScanStarted(qc, lib.id);
    onCreated(lib.id);
    setDraft(null);
    toast.add({
      title: t('libraries.toast.added', { name: lib.name }),
      description: t('setup.metadata.scanning'),
      type: 'success',
    });
  };
}

/**
 * Step 3: community metadata. On or off (the server's main outbound traffic),
 * the marketplace, and the service or a local copy. Then the library is
 * created, so its first scan and first matches use them; Skip keeps the
 * settings as they are and still creates it.
 */
export function MetadataStep(props: StepProps) {
  const { settings, leave, back, last, draft } = props;
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const save = useSaveStepSettings();
  const create = useCreateDraftLibrary(props);
  const m = settings.metadata;
  const [enabled, setEnabled] = useState(m.enabled && m.available);
  const [region, setRegion] = useState(m.region);
  const [mode, setMode] = useState<MetadataMode>(m.mode);

  const onContinue = async () => {
    // Without the service the switch shows off but can't be changed: the saved
    // setting stays as it is, for when the service is configured.
    const draft = m.available ? { enabled, region, mode } : { region, mode };
    await save(unlockedPatch(settings, 'metadata', draft));
    await create();
    await leave('done');
  };
  const onSkip = async () => {
    await create();
    await leave('skipped');
  };

  return (
    <>
      <div className="flex flex-col">
        <div className="divide-y">
          <SettingRow
            title={t('settings.metadata.toggle')}
            badges={<Badges settings={settings} id="metadata.enabled" />}
            htmlFor="setup-metadata"
            description={t('setup.metadata.toggleBody')}
            descriptionId="setup-metadata-desc"
          >
            <Switch
              id="setup-metadata"
              checked={enabled}
              disabled={!m.available || !editable(settings, 'metadata.enabled')}
              onCheckedChange={setEnabled}
              aria-describedby="setup-metadata-desc"
            />
          </SettingRow>
          {enabled ? (
            <>
              <SettingRow
                title={t('settings.metadata.region')}
                badges={<Badges settings={settings} id="metadata.region" />}
                htmlFor="setup-region"
                description={t('setup.metadata.regionBody')}
                descriptionId="setup-region-desc"
              >
                <NativeSelect
                  id="setup-region"
                  className="w-full sm:w-[260px]"
                  value={region}
                  disabled={!editable(settings, 'metadata.region')}
                  onChange={(e) => setRegion(e.target.value)}
                  aria-describedby="setup-region-desc"
                >
                  <option value="">{t('settings.metadata.regionNone')}</option>
                  {regionOptions(lang).map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </NativeSelect>
              </SettingRow>
              <div className="flex flex-col gap-3 py-4">
                <div className="flex flex-wrap items-center gap-2">
                  <b className="font-semibold">{t('settings.metadata.mode')}</b>
                  <Badges settings={settings} id="metadata.mode" />
                </div>
                <RadioCards
                  label={t('settings.metadata.mode')}
                  value={mode}
                  onValueChange={setMode}
                  options={METADATA_MODES.map((v) => ({
                    value: v,
                    title: t(`settings.metadata.mode.${v}`),
                    description: t(`settings.metadata.mode.${v}Body`),
                    disabled: !editable(settings, 'metadata.mode'),
                  }))}
                />
              </div>
            </>
          ) : null}
        </div>
        {!m.available ? (
          <Notice tone="warn" icon={TriangleAlert} title={t('settings.metadata.unavailable')}>
            {t('settings.metadata.unavailableBody')}
          </Notice>
        ) : null}
        {draft ? (
          <p className="mt-4 text-[13px] text-muted-foreground">
            {t('setup.metadata.creates', { name: draft.name })}
          </p>
        ) : null}
      </div>
      <StepFooter back={back} onSkip={onSkip} onContinue={onContinue} last={last} />
    </>
  );
}

/**
 * Step 4: how people reach the server. The address this browser uses, offered
 * as the public or the home address (pairing QR codes are built from them); a
 * reverse proxy whose address isn't trusted yet, offered for trusted_proxies;
 * and the HTTPS mode with what it means for phones (restart settings, marked).
 */
export function ReachStep({ env, settings, leave, back, last }: StepProps) {
  const { t } = useTranslation();
  const save = useSaveStepSettings();
  const g = settings.general;
  const n = settings.network;
  const [publicUrl, setPublicUrl] = useState(g.public_url);
  const [lanUrl, setLanUrl] = useState(g.lan_url);
  const [proxies, setProxies] = useState(n.trusted_proxies);
  const [homeHttp, setHomeHttp] = useState(n.tls_home_http);
  const [error, setError] = useState<string | null>(null);
  const origin = window.location.origin;
  const peer = proxyToTrust(env.request, proxies);
  const canPublic = editable(settings, 'general.public_url');
  const canLan = editable(settings, 'general.lan_url');
  const canProxies = editable(settings, 'network.trusted_proxies');

  const onContinue = async () => {
    setError(null);
    try {
      await save(
        mergePatches(
          unlockedPatch(settings, 'general', {
            public_url: publicUrl.trim(),
            lan_url: lanUrl.trim(),
          }),
          unlockedPatch(settings, 'network', { trusted_proxies: proxies, tls_home_http: homeHttp }),
        ),
      );
    } catch (err) {
      setError(errorMessage(err, t));
      throw new StepStop();
    }
    await leave('done');
  };

  const fields = {
    lan: { can: canLan, set: setLanUrl, value: lanUrl },
    public: { can: canPublic, set: setPublicUrl, value: publicUrl },
  };
  const offers = reachOffers(window.location.host).map((which) => ({ which, ...fields[which] }));

  return (
    <>
      <div className="flex flex-col gap-5">
        <div className="flex flex-col gap-3 rounded-xl border bg-muted/40 px-4 py-3.5">
          <span className="inline-flex flex-wrap items-center gap-x-2 gap-y-1">
            <Globe className="size-[15px] text-muted-foreground" aria-hidden="true" />
            {t('setup.reach.browser')}
            <code className="font-mono text-[12.5px] [overflow-wrap:anywhere]">{origin}</code>
          </span>
          {offers.length > 0 ? (
            <div className="flex flex-wrap gap-2">
              {offers.map((b) => (
                <Button
                  key={b.which}
                  type="button"
                  size="sm"
                  variant={b === offers[0] ? 'secondary' : 'outline'}
                  disabled={!b.can || b.value === origin}
                  onClick={() => b.set(origin)}
                >
                  {t(`setup.reach.use.${b.which}`)}
                </Button>
              ))}
            </div>
          ) : null}
          {addressMismatch(env.request, origin) ? (
            <span className="text-[12.5px] text-muted-foreground">
              {t('setup.reach.seenAs', { address: seenAs(env.request) })}
            </span>
          ) : null}
        </div>

        <div className="divide-y">
          <SettingRow
            title={t('settings.general.public_url')}
            badges={<Badges settings={settings} id="general.public_url" />}
            htmlFor="setup-public-url"
            description={t('setup.reach.publicBody')}
          >
            <Input
              id="setup-public-url"
              className="w-full font-mono text-[13px] sm:w-[300px]"
              placeholder="https://books.example.com"
              value={publicUrl}
              disabled={!canPublic}
              onChange={(e) => setPublicUrl(e.target.value)}
            />
          </SettingRow>
          <SettingRow
            title={t('settings.general.lan_url')}
            badges={<Badges settings={settings} id="general.lan_url" />}
            htmlFor="setup-lan-url"
            description={t('setup.reach.lanBody')}
          >
            <Input
              id="setup-lan-url"
              className="w-full font-mono text-[13px] sm:w-[300px]"
              placeholder="http://192.168.1.20:8080"
              value={lanUrl}
              disabled={!canLan}
              onChange={(e) => setLanUrl(e.target.value)}
            />
          </SettingRow>
        </div>

        {peer ? (
          <Notice
            tone="warn"
            icon={Network}
            title={t('setup.reach.proxy')}
            actions={
              <Button
                size="sm"
                variant="outline"
                disabled={!canProxies}
                onClick={() => setProxies((p) => [...p, peer])}
              >
                {t('setup.reach.trust', { peer })}
              </Button>
            }
          >
            {t('setup.reach.proxyBody', { peer })}
            {canProxies ? null : (
              <>
                {' '}
                <LockedNote settings={settings} id="network.trusted_proxies" />
              </>
            )}
          </Notice>
        ) : null}
        {proxies.length !== n.trusted_proxies.length ? (
          <p className="text-[13px] text-muted-foreground" role="status">
            {t('setup.reach.trusted', { list: proxies.join(', ') })}
          </p>
        ) : null}

        <section
          aria-labelledby="setup-https"
          className="flex flex-col gap-2 rounded-xl border px-4 py-3.5"
        >
          <div className="flex flex-wrap items-center gap-2">
            <ShieldCheck className="size-[15px] text-muted-foreground" aria-hidden="true" />
            <h3 id="setup-https" className="font-semibold">
              {t('setup.reach.https', { mode: t(`settings.network.mode.${n.tls_mode}`) })}
            </h3>
            <Badges settings={settings} id="network.tls_mode" />
          </div>
          <p className="text-[13px] text-muted-foreground">
            {t(`setup.reach.phones.${n.tls_mode}`)}
          </p>
          {n.tls_mode === 'selfsigned' ? (
            <SettingRow
              title={t('settings.network.tls_home_http')}
              badges={<Badges settings={settings} id="network.tls_home_http" />}
              htmlFor="setup-home-http"
              description={t('settings.network.tls_home_httpBody')}
              descriptionId="setup-home-http-desc"
            >
              <Switch
                id="setup-home-http"
                checked={homeHttp}
                disabled={!editable(settings, 'network.tls_home_http')}
                onCheckedChange={setHomeHttp}
                aria-describedby="setup-home-http-desc"
              />
            </SettingRow>
          ) : null}
          <Link
            to="/server/{-$section}"
            params={{ section: undefined }}
            search={{ topic: 'network' }}
            target="_blank"
            className="text-[13px] font-semibold text-brand-ink hover:underline"
          >
            {t('setup.reach.httpsSettings')}
          </Link>
        </section>
        <FormError>{error}</FormError>
      </div>
      <StepFooter back={back} onSkip={() => leave('skipped')} onContinue={onContinue} last={last} />
    </>
  );
}

/**
 * Step 6: backups and updates. The schedule and how many to keep (the
 * defaults: daily at 03:00, the newest 7), the folder they go to (set only in
 * config, so shown), and the daily update check (another outbound call).
 */
export function BackupsStep({ settings, leave, back, last }: StepProps) {
  const { t } = useTranslation();
  const save = useSaveStepSettings();
  const system = useSystem();
  const b = settings.backups;
  const [schedule, setSchedule] = useState(b.schedule);
  const [keep, setKeep] = useState(String(b.keep));
  const [updates, setUpdates] = useState(settings.general.update_check);
  const [error, setError] = useState<string | null>(null);
  const dir = b.dir || system.data?.backups?.dir || '';
  const keepBad = outOfRange(keep, 1, 365);

  const onContinue = async () => {
    setError(null);
    if (keepBad && editable(settings, 'backups.keep')) {
      setError(t('setup.backups.keepRange'));
      throw new StepStop();
    }
    try {
      await save(
        mergePatches(
          unlockedPatch(settings, 'backups', { schedule, keep: Number(keep) }),
          unlockedPatch(settings, 'general', { update_check: updates }),
        ),
      );
    } catch (err) {
      setError(errorMessage(err, t));
      throw new StepStop();
    }
    await leave('done');
  };

  return (
    <>
      <div className="flex flex-col">
        <div className="divide-y">
          <SettingRow
            title={t('settings.backups.schedule')}
            badges={<Badges settings={settings} id="backups.schedule" />}
            htmlFor="setup-backup-schedule"
            description={t('settings.backups.scheduleBody')}
          >
            <ScheduleInput
              id="setup-backup-schedule"
              value={schedule}
              onChange={setSchedule}
              disabled={!editable(settings, 'backups.schedule')}
            />
          </SettingRow>
          <SettingRow
            title={t('settings.backups.keep')}
            badges={<Badges settings={settings} id="backups.keep" />}
            htmlFor="setup-backup-keep"
            description={t('settings.backups.keepBody')}
          >
            <Input
              id="setup-backup-keep"
              type="number"
              inputMode="numeric"
              min={1}
              max={365}
              className="w-[110px] tabular-nums"
              value={keep}
              disabled={!editable(settings, 'backups.keep')}
              aria-invalid={keepBad ? true : undefined}
              onChange={(e) => setKeep(e.target.value)}
            />
          </SettingRow>
          <SettingRow title={t('settings.backups.dir')} description={t('setup.backups.dirBody')}>
            <code className="font-mono text-[12.5px] [overflow-wrap:anywhere]">
              {dir || t('setup.backups.dirDefault')}
            </code>
          </SettingRow>
          <SettingRow
            title={t('settings.general.update_check')}
            badges={<Badges settings={settings} id="general.update_check" />}
            htmlFor="setup-updates"
            description={t('settings.general.update_checkBody')}
            descriptionId="setup-updates-desc"
          >
            <Switch
              id="setup-updates"
              checked={updates}
              disabled={!editable(settings, 'general.update_check')}
              onCheckedChange={setUpdates}
              aria-describedby="setup-updates-desc"
            />
          </SettingRow>
        </div>
        <FormError>{error}</FormError>
      </div>
      <StepFooter back={back} onSkip={() => leave('skipped')} onContinue={onContinue} last={last} />
    </>
  );
}
