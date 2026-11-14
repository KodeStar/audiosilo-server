import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { MapPin } from 'lucide-react';
import type { AdminSettings } from '@/api/types';
import { Button } from '@/components/ui/button';
import { useSaveSettings } from '@/features/settings/use-save-settings';
import { toastError } from '@/lib/errors';
import { browserNearestRegion } from '@/lib/nearest-region';
import { regionLabel, regionName } from '@/lib/regions';
import { offerRegion } from './setup-model';

/**
 * Settings > Community metadata's offer of the marketplace nearest the admin,
 * while the server says none was ever chosen (server#192): a row under the
 * marketplace field that saves it in one click, or keeps "No preference" (also
 * a choice, so the offer goes). An offer, not a changed field: the card has
 * nothing unsaved until the admin acts (a preselected field would hold the
 * leave-page prompt up on every visit).
 */
export function RegionSuggestion({ settings }: { settings: AdminSettings }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const save = useSaveSettings();
  const offered = offerRegion(settings);
  const [nearest] = useState(() => (offered ? browserNearestRegion() : null));
  const [busy, setBusy] = useState(false);
  // The US store is what "No preference" already takes first: nothing to offer.
  if (!offered || nearest === null || nearest === 'us') return null;

  const choose = async (region: string) => {
    setBusy(true);
    try {
      await save({ metadata: { region } });
    } catch (err) {
      toastError(t('setup.region.failed'), err);
    } finally {
      setBusy(false);
    }
  };
  const name = regionName(nearest, lang);
  return (
    <div className="flex flex-wrap items-center gap-3 py-4" role="status">
      <MapPin className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
      <span className="min-w-0 flex-1 basis-60 text-[13px]">
        {t('setup.region.nearest', { region: regionLabel(nearest, lang) })}
      </span>
      <div className="flex flex-wrap gap-2">
        <Button type="button" size="sm" disabled={busy} onClick={() => void choose(nearest)}>
          {t('setup.region.use', { region: name })}
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          disabled={busy}
          onClick={() => void choose('')}
        >
          {t('setup.region.keepNone')}
        </Button>
      </div>
    </div>
  );
}
